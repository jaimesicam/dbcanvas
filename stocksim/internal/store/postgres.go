package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// pgStore implements Store against PostgreSQL.
//
// Placement is decided once, at open, and there are two possible outcomes:
//
//   - A database of its own, named by Config.Database, with the tables in that
//     database's public schema. This is the preferred layout and the one that
//     matches every sibling engine — MySQL gets its own database, MongoDB gets
//     its own database, so PostgreSQL should too. It needs CREATEDB.
//   - Failing that, a *schema* named by Config.Database inside whatever
//     database the DSN already points at. A managed PostgreSQL instance
//     usually hands you one database and no right to create another, so this
//     is the only thing that reliably works there.
//
// Either way search_path is pinned to the chosen schema on every connection,
// so no query below has to know which of the two happened. Location() reports
// the outcome in words, because "where did my data actually land" is not a
// question a user should have to answer by hand.
type pgStore struct {
	db       *sql.DB
	name     string // the namespace the user asked for — what Database() reports
	database string // the database actually connected to
	schema   string // the schema the tables actually live in
	owned    bool   // true when database == name, i.e. we got a database of our own
}

func openPostgres(ctx context.Context, c Config) (Store, error) {
	base := c.DSN
	if base == "" {
		base = pgDSN(c)
	}

	dsn, database, schema, owned := pgResolvePlacement(ctx, base, c.Database)
	// Pin search_path so unqualified table names in every query below resolve
	// to our schema, without threading the name through each statement.
	dsn = pgWithSearchPath(dsn, schema)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	return &pgStore{
		db: db, name: c.Database, database: database, schema: schema, owned: owned,
	}, nil
}

// pgResolvePlacement decides between the two layouts described on pgStore and
// returns the DSN to actually use.
//
// It has to connect to find out, and it is called before main's own
// wait-for-database loop, so an unreachable server here means "not up yet"
// rather than "cannot create". It retries for the same minute that loop would
// have spent waiting. Only after that does it give up and take the schema
// layout, which works on any server and so is the safe thing to be wrong
// about — the alternative, connecting to a database we never confirmed
// exists, fails permanently.
func pgResolvePlacement(ctx context.Context, base, name string) (dsn, database, schema string, owned bool) {
	deadline := time.Now().Add(time.Minute)
	for {
		ok, retryable := pgTryOwnDatabase(ctx, base, name)
		switch {
		case ok:
			return pgWithDatabase(base, name), name, "public", true
		case !retryable:
			log.Printf("stocksim: no database of our own (%q): using a schema "+
				"named %q inside the connected database instead", name, name)
			return base, pgDatabaseOf(base), name, false
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			log.Printf("stocksim: postgres not reachable yet; falling back to a schema "+
				"named %q inside the connected database", name)
			return base, pgDatabaseOf(base), name, false
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// pgTryOwnDatabase reports whether this app can have the named database to
// itself, creating it if it does not exist and the role is allowed to.
//
// Existence is not enough, and assuming it was is a mistake worth naming: every
// role can see every row of pg_database, so a `stocksim` database belonging to
// somebody else on a shared server reads as "already there". Connecting to it
// would then fail on the first CREATE TABLE, having bypassed the schema
// fallback that would have worked. So the answer is only yes once we have
// connected to the database and confirmed we may create objects in it.
//
// retryable distinguishes "the server went away mid-check", worth another
// attempt, from "the server answered and the answer was no", which will not
// change however long we wait.
func pgTryOwnDatabase(ctx context.Context, base, name string) (ok, retryable bool) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		return false, false // a DSN the driver cannot parse will not start parsing
	}
	defer admin.Close()

	var exists bool
	if err := admin.QueryRowContext(cctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return false, true
	}
	if !exists {
		// CREATE DATABASE cannot run inside a transaction and takes no
		// parameters, hence the quoted identifier. A role without CREATEDB
		// fails here, which is the expected outcome on a managed instance and
		// not an error worth failing the deployment over.
		if _, err := admin.ExecContext(cctx, "CREATE DATABASE "+pgQuoteIdent(name)); err != nil {
			// Another replica of this app may have won the race with the check
			// above, in which case the probe below still decides.
			if err2 := admin.QueryRowContext(cctx,
				"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err2 != nil || !exists {
				log.Printf("stocksim: cannot create database %q: %v", name, err)
				return false, false
			}
		} else {
			log.Printf("stocksim: created database %q", name)
		}
	}

	own, err := sql.Open("pgx", pgWithDatabase(base, name))
	if err != nil {
		return false, false
	}
	defer own.Close()
	var mayCreate bool
	if err := own.QueryRowContext(cctx,
		"SELECT has_schema_privilege(current_user, 'public', 'CREATE')").Scan(&mayCreate); err != nil {
		// Cannot connect to it, or cannot read the catalogue through it: either
		// way this is not a database we can use. Not retryable — the admin
		// connection above just worked, so the server itself is up.
		log.Printf("stocksim: database %q is not usable by this role: %v", name, err)
		return false, false
	}
	if !mayCreate {
		log.Printf("stocksim: database %q exists but this role cannot create objects in it", name)
		return false, false
	}
	return true, false
}

func pgDSN(c Config) string {
	port := c.Port
	if port == 0 {
		port = DefaultPort(EnginePostgres)
	}
	ssl := "prefer"
	switch c.TLS {
	case "require":
		ssl = "require"
	case "disable":
		ssl = "disable"
	}
	q := url.Values{}
	q.Set("sslmode", ssl)
	q.Set("connect_timeout", "10")
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.User, c.Password),
		Host:     fmt.Sprintf("%s:%d", c.Host, port),
		Path:     "/postgres",
		RawQuery: q.Encode(),
	}
	out := u.String()
	if extra := strings.TrimSpace(c.Params); extra != "" {
		out += "&" + strings.TrimPrefix(extra, "&")
	}
	return out
}

// pgWithSearchPath appends a search_path option to a DSN in either supported
// form. Uses the URL's own query when it parses as one, so a user-supplied
// connection string keeps whatever else it carries.
func pgWithSearchPath(dsn, schema string) string {
	opt := "-c search_path=" + schema + ",public"
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		q := u.Query()
		if q.Get("options") != "" {
			return dsn // the caller pinned their own options; do not fight them
		}
		q.Set("options", opt)
		u.RawQuery = q.Encode()
		return u.String()
	}
	if strings.Contains(dsn, "options=") {
		return dsn
	}
	return dsn + " options='" + opt + "'"
}

// pgWithDatabase repoints a DSN at a different database. In keyword/value form
// the appended dbname wins, because libpq — and pgx, which follows it — takes
// the last occurrence of a repeated keyword.
func pgWithDatabase(dsn, name string) string {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		u.Path = "/" + name
		return u.String()
	}
	return dsn + " dbname='" + strings.ReplaceAll(name, `'`, `\'`) + "'"
}

// pgDatabaseOf reports which database a DSN names, for Location()'s benefit
// only. An unparseable or database-less DSN yields "" rather than a guess.
func pgDatabaseOf(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		return strings.TrimPrefix(u.Path, "/")
	}
	for _, f := range strings.Fields(dsn) {
		if v, ok := strings.CutPrefix(f, "dbname="); ok {
			return strings.Trim(v, "'\"")
		}
	}
	return ""
}

func (s *pgStore) Engine() string   { return EnginePostgres }
func (s *pgStore) Database() string { return s.name }
func (s *pgStore) Close() error     { return s.db.Close() }

// Location spells out which of pgStore's two layouts is in effect. This is the
// one engine where the answer is not obvious from the namespace name alone.
func (s *pgStore) Location() string {
	if s.owned {
		return fmt.Sprintf("database %q (schema %s)", s.database, s.schema)
	}
	where := s.database
	if where == "" {
		where = "the connected database"
	} else {
		where = fmt.Sprintf("database %q", where)
	}
	return fmt.Sprintf("schema %q inside %s", s.schema, where)
}

func (s *pgStore) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.db.PingContext(cctx)
}

func (s *pgStore) ServerVersion(ctx context.Context) (string, error) {
	var v string
	if err := s.db.QueryRowContext(ctx, "SHOW server_version").Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// pgQuoteIdent double-quotes an identifier for DDL where a bind parameter is
// not allowed. Same helper carsim carries.
func pgQuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func (s *pgStore) EnsureSchema(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(cctx,
		"CREATE SCHEMA IF NOT EXISTS "+pgQuoteIdent(s.schema)); err != nil {
		// As with MySQL: a user without CREATE on the database is normal
		// against a managed instance where an administrator already made the
		// schema. Only a genuinely absent schema is fatal.
		var exists bool
		if qerr := s.db.QueryRowContext(cctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)",
			s.schema).Scan(&exists); qerr != nil || !exists {
			return fmt.Errorf("schema %q does not exist and could not be created "+
				"(%w) — create it first, or grant this user CREATE privileges", s.schema, err)
		}
	}
	for _, stmt := range pgCreateStmts {
		if _, err := s.db.ExecContext(cctx, stmt); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
	}
	return nil
}

// Wipe empties this app's tables in one statement. RESTART IDENTITY resets the
// events sequence so the feed cursor starts clean; CASCADE is required because
// TRUNCATE refuses on a table referenced by another, even though nothing here
// declares a foreign key.
func (s *pgStore) Wipe(ctx context.Context) error {
	names := make([]string, 0, len(pgTables))
	for _, t := range pgTables {
		names = append(names, pgQuoteIdent(s.schema)+"."+pgQuoteIdent(t))
	}
	_, err := s.db.ExecContext(ctx,
		"TRUNCATE TABLE "+strings.Join(names, ", ")+" RESTART IDENTITY CASCADE")
	return err
}

// DropSchema removes this app's tables and leaves the schema in place, for the
// same reason the MySQL store leaves the database alone — see its doc comment.
func (s *pgStore) DropSchema(ctx context.Context) error {
	for _, t := range pgTables {
		if _, err := s.db.ExecContext(ctx,
			"DROP TABLE IF EXISTS "+pgQuoteIdent(s.schema)+"."+pgQuoteIdent(t)+" CASCADE"); err != nil {
			return fmt.Errorf("drop %s: %w", t, err)
		}
	}
	return nil
}

func (s *pgStore) Objects(ctx context.Context) ([]ObjectInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.relname,
		       GREATEST(c.reltuples, 0)::BIGINT,
		       pg_total_relation_size(c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r'
		ORDER BY c.relname`, s.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owned := map[string]bool{}
	for _, t := range pgTables {
		owned[t] = true
	}
	var out []ObjectInfo
	for rows.Next() {
		var o ObjectInfo
		if err := rows.Scan(&o.Name, &o.Rows, &o.Bytes); err != nil {
			return nil, err
		}
		if !owned[o.Name] {
			continue
		}
		o.Kind = "table"
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- securities

func (s *pgStore) ListSecurities(ctx context.Context, q ListQuery) ([]Security, int, error) {
	where, args := []string{"TRUE"}, []any{}
	if t := strings.TrimSpace(q.Search); t != "" {
		args = append(args, "%"+likeEscape(t)+"%")
		where = append(where, fmt.Sprintf(`(symbol ILIKE $%d OR name ILIKE $%d)`, len(args), len(args)))
	}
	if f := strings.TrimSpace(q.Filter); f != "" {
		args = append(args, f)
		where = append(where, fmt.Sprintf("sector = $%d", len(args)))
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM securities WHERE "+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := clampLimit(q.Limit, 50, 500)
	args = append(args, limit, max0(q.Offset))
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		"SELECT "+securityCols+" FROM securities WHERE %s ORDER BY symbol LIMIT $%d OFFSET $%d",
		clause, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Security{}
	for rows.Next() {
		sec, err := scanSecurity(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, sec)
	}
	return out, total, rows.Err()
}

func (s *pgStore) GetSecurity(ctx context.Context, id string) (Security, error) {
	sec, err := scanSecurity(s.db.QueryRowContext(ctx,
		"SELECT "+securityCols+" FROM securities WHERE id = $1", id))
	if err == sql.ErrNoRows {
		return Security{}, ErrNotFound
	}
	return sec, err
}

// pgIsUnique recognises SQLSTATE 23505 so a duplicate symbol becomes
// ErrConflict, matching what the MySQL store does with error 1062.
func pgIsUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}

func (s *pgStore) CreateSecurity(ctx context.Context, sec Security) (Security, error) {
	now := time.Now().UTC()
	sec.ID, sec.CreatedAt, sec.UpdatedAt = newID(), now, now
	if sec.DayLow == 0 {
		sec.DayLow = sec.LastPrice
	}
	if sec.DayHigh == 0 {
		sec.DayHigh = sec.LastPrice
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO securities
		(id, symbol, name, sector, currency, shares_outstanding, open_price, last_price,
		 day_high, day_low, day_volume, listed, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		sec.ID, sec.Symbol, sec.Name, sec.Sector, sec.Currency, sec.Shares,
		sec.OpenPrice, sec.LastPrice, sec.DayHigh, sec.DayLow, sec.DayVolume, sec.Listed,
		sec.CreatedAt, sec.UpdatedAt)
	if pgIsUnique(err) {
		return Security{}, fmt.Errorf("symbol %s: %w", sec.Symbol, ErrConflict)
	}
	return sec, err
}

func (s *pgStore) UpdateSecurity(ctx context.Context, sec Security) (Security, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE securities SET
		symbol=$1, name=$2, sector=$3, currency=$4, shares_outstanding=$5, open_price=$6,
		listed=$7, updated_at=$8 WHERE id=$9`,
		sec.Symbol, sec.Name, sec.Sector, sec.Currency, sec.Shares, sec.OpenPrice,
		sec.Listed, time.Now().UTC(), sec.ID)
	if pgIsUnique(err) {
		return Security{}, fmt.Errorf("symbol %s: %w", sec.Symbol, ErrConflict)
	}
	if err != nil {
		return Security{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Security{}, ErrNotFound
	}
	return s.GetSecurity(ctx, sec.ID)
}

func (s *pgStore) DeleteSecurity(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM securities WHERE id = $1", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	// Trades are kept on purpose — see the MySQL store's DeleteSecurity.
	for _, stmt := range []string{
		"DELETE FROM price_ticks WHERE security_id = $1",
		"DELETE FROM holdings WHERE security_id = $1",
		"DELETE FROM orders WHERE security_id = $1 AND status = 'open'",
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- portfolios

func (s *pgStore) ListPortfolios(ctx context.Context, q ListQuery) ([]Portfolio, int, error) {
	where, args := []string{"TRUE"}, []any{}
	if t := strings.TrimSpace(q.Search); t != "" {
		args = append(args, "%"+likeEscape(t)+"%")
		where = append(where, fmt.Sprintf(`(name ILIKE $%d OR owner ILIKE $%d)`, len(args), len(args)))
	}
	if f := strings.TrimSpace(q.Filter); f != "" {
		args = append(args, f)
		where = append(where, fmt.Sprintf("owner = $%d", len(args)))
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM portfolios WHERE "+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := clampLimit(q.Limit, 50, 500)
	args = append(args, limit, max0(q.Offset))
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		"SELECT "+portfolioCols+" FROM portfolios WHERE %s ORDER BY owner, name LIMIT $%d OFFSET $%d",
		clause, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Portfolio{}
	for rows.Next() {
		p, err := scanPortfolio(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, total, rows.Err()
}

func (s *pgStore) GetPortfolio(ctx context.Context, id string) (Portfolio, error) {
	p, err := scanPortfolio(s.db.QueryRowContext(ctx,
		"SELECT "+portfolioCols+" FROM portfolios WHERE id = $1", id))
	if err == sql.ErrNoRows {
		return Portfolio{}, ErrNotFound
	}
	return p, err
}

func (s *pgStore) CreatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error) {
	now := time.Now().UTC()
	p.ID, p.CreatedAt, p.UpdatedAt = newID(), now, now
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO portfolios (id, name, owner, cash, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6)",
		p.ID, p.Name, p.Owner, p.Cash, p.CreatedAt, p.UpdatedAt)
	return p, err
}

func (s *pgStore) UpdatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE portfolios SET name=$1, owner=$2, cash=$3, updated_at=$4 WHERE id=$5",
		p.Name, p.Owner, p.Cash, time.Now().UTC(), p.ID)
	if err != nil {
		return Portfolio{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Portfolio{}, ErrNotFound
	}
	return s.GetPortfolio(ctx, p.ID)
}

func (s *pgStore) DeletePortfolio(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM portfolios WHERE id = $1", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, stmt := range []string{
		"DELETE FROM holdings WHERE portfolio_id = $1",
		"DELETE FROM orders WHERE portfolio_id = $1 AND status = 'open'",
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// -------------------------------------------------------------------- orders

func (s *pgStore) ListOrders(ctx context.Context, q ListQuery) ([]Order, int, error) {
	where, args := []string{"TRUE"}, []any{}
	if t := strings.TrimSpace(q.Search); t != "" {
		args = append(args, "%"+likeEscape(t)+"%")
		where = append(where, fmt.Sprintf(`(symbol ILIKE $%d OR owner ILIKE $%d)`, len(args), len(args)))
	}
	if f := strings.TrimSpace(q.Filter); f != "" {
		args = append(args, f)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM orders WHERE "+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := clampLimit(q.Limit, 50, 500)
	args = append(args, limit, max0(q.Offset))
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		"SELECT "+orderCols+" FROM orders WHERE %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d",
		clause, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, o)
	}
	return out, total, rows.Err()
}

func (s *pgStore) GetOrder(ctx context.Context, id string) (Order, error) {
	o, err := scanOrder(s.db.QueryRowContext(ctx,
		"SELECT "+orderCols+" FROM orders WHERE id = $1", id))
	if err == sql.ErrNoRows {
		return Order{}, ErrNotFound
	}
	return o, err
}

func (s *pgStore) CreateOrder(ctx context.Context, o Order) (Order, error) {
	o.ID = newID()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	if o.Status == "" {
		o.Status = OrderOpen
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO orders
		(id, portfolio_id, security_id, symbol, owner, side, order_type, quantity,
		 limit_price, status, created_at, filled_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULL)`,
		o.ID, o.PortfolioID, o.SecurityID, o.Symbol, o.Owner, o.Side, o.OrderType,
		o.Quantity, o.LimitPrice, o.Status, o.CreatedAt)
	return o, err
}

func (s *pgStore) UpdateOrder(ctx context.Context, o Order) (Order, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE orders SET side=$1, order_type=$2, quantity=$3, limit_price=$4, status=$5 WHERE id=$6",
		o.Side, o.OrderType, o.Quantity, o.LimitPrice, o.Status, o.ID)
	if err != nil {
		return Order{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Order{}, ErrNotFound
	}
	return s.GetOrder(ctx, o.ID)
}

func (s *pgStore) DeleteOrder(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM orders WHERE id = $1", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --------------------------------------------------------- simulation writes

func (s *pgStore) AppendTicks(ctx context.Context, ticks []Tick) error {
	if len(ticks) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO price_ticks (id, security_id, symbol, ts, price, volume) VALUES ")
	args := make([]any, 0, len(ticks)*6)
	for i, t := range ticks {
		if i > 0 {
			sb.WriteString(",")
		}
		b := i * 6
		sb.WriteString(fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d)", b+1, b+2, b+3, b+4, b+5, b+6))
		args = append(args, newID(), t.SecurityID, t.Symbol, t.TS, t.Price, t.Volume)
	}
	_, err := s.db.ExecContext(ctx, sb.String(), args...)
	return err
}

func (s *pgStore) RecentTicks(ctx context.Context, securityID string, limit int) ([]Tick, error) {
	limit = clampLimit(limit, 60, 1000)
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, security_id, symbol, ts, price, volume FROM price_ticks "+
			"WHERE security_id = $1 ORDER BY ts DESC LIMIT $2", securityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tick{}
	for rows.Next() {
		var t Tick
		if err := rows.Scan(&t.ID, &t.SecurityID, &t.Symbol, &t.TS, &t.Price, &t.Volume); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (s *pgStore) ApplyQuotes(ctx context.Context, quotes []Quote) error {
	if len(quotes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE securities SET
		last_price = $1,
		day_high = GREATEST(day_high, $1),
		day_low  = CASE WHEN day_low = 0 THEN $1 ELSE LEAST(day_low, $1) END,
		day_volume = day_volume + $2,
		updated_at = $3
		WHERE id = $4`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC()
	for _, q := range quotes {
		if _, err := stmt.ExecContext(ctx, q.Price, q.Volume, now, q.SecurityID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *pgStore) OpenOrders(ctx context.Context, limit int) ([]Order, error) {
	limit = clampLimit(limit, 100, 1000)
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+orderCols+" FROM orders WHERE status = 'open' ORDER BY created_at ASC LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RecordFill mirrors the MySQL store's settlement exactly — see its doc comment
// for the average-cost rules and why they only hold while quantity stays
// non-negative.
func (s *pgStore) RecordFill(ctx context.Context, o Order, t Trade) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		"UPDATE orders SET status = $1, filled_at = $2 WHERE id = $3 AND status = 'open'",
		OrderFilled, t.TS, o.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // someone else already moved this order
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO trades
		(id, order_id, portfolio_id, security_id, symbol, side, quantity, price, ts)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		newID(), o.ID, o.PortfolioID, o.SecurityID, o.Symbol, o.Side,
		t.Quantity, t.Price, t.TS); err != nil {
		return err
	}

	signedQty := t.Quantity
	cashDelta := -t.Notional()
	if o.Side == SideSell {
		signedQty = -t.Quantity
		cashDelta = t.Notional()
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO holdings
		(portfolio_id, security_id, symbol, quantity, avg_cost, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (portfolio_id, security_id) DO UPDATE SET
			avg_cost = CASE
				WHEN holdings.quantity + $4 = 0 THEN 0
				WHEN $4 > 0 THEN (holdings.avg_cost * holdings.quantity + $5 * $4) / (holdings.quantity + $4)
				ELSE holdings.avg_cost END,
			quantity = holdings.quantity + $4,
			updated_at = EXCLUDED.updated_at`,
		o.PortfolioID, o.SecurityID, o.Symbol, signedQty, t.Price, t.TS); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE portfolios SET cash = cash + $1, updated_at = $2 WHERE id = $3",
		cashDelta, t.TS, o.PortfolioID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *pgStore) ListHoldings(ctx context.Context, portfolioID string) ([]Holding, error) {
	q := `SELECT h.portfolio_id, COALESCE(p.owner,''), h.security_id, h.symbol, h.quantity,
			h.avg_cost, COALESCE(s.last_price,0), h.updated_at
		FROM holdings h
		LEFT JOIN portfolios p ON p.id = h.portfolio_id
		LEFT JOIN securities s ON s.id = h.security_id
		WHERE h.quantity <> 0`
	args := []any{}
	if portfolioID != "" {
		args = append(args, portfolioID)
		q += " AND h.portfolio_id = $1"
	}
	q += " ORDER BY p.owner, h.symbol"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Holding{}
	for rows.Next() {
		var h Holding
		if err := rows.Scan(&h.PortfolioID, &h.Owner, &h.SecurityID, &h.Symbol,
			&h.Quantity, &h.AvgCost, &h.LastPrice, &h.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *pgStore) CountOrdersByStatus(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM orders GROUP BY status")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

func (s *pgStore) RecentTrades(ctx context.Context, limit int) ([]Trade, error) {
	limit = clampLimit(limit, 50, 1000)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, order_id, portfolio_id, security_id, symbol, side, quantity, price, ts
		 FROM trades ORDER BY ts DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Trade{}
	for rows.Next() {
		var t Trade
		if err := rows.Scan(&t.ID, &t.OrderID, &t.PortfolioID, &t.SecurityID,
			&t.Symbol, &t.Side, &t.Quantity, &t.Price, &t.TS); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ------------------------------------------------- metrics / state / agents

func (s *pgStore) putBlob(ctx context.Context, table, id string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO "+pgQuoteIdent(table)+" (id, payload, updated_at) VALUES ($1,$2,$3) "+
			"ON CONFLICT (id) DO UPDATE SET payload = EXCLUDED.payload, updated_at = EXCLUDED.updated_at",
		id, string(b), time.Now().UTC())
	return err
}

func (s *pgStore) getBlob(ctx context.Context, table, id string) (json.RawMessage, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		"SELECT payload FROM "+pgQuoteIdent(table)+" WHERE id = $1", id).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(payload), nil
}

func (s *pgStore) PutMetrics(ctx context.Context, id string, payload any) error {
	return s.putBlob(ctx, "metrics", id, payload)
}
func (s *pgStore) GetMetrics(ctx context.Context, id string) (json.RawMessage, error) {
	return s.getBlob(ctx, "metrics", id)
}
func (s *pgStore) PutState(ctx context.Context, id string, payload any) error {
	return s.putBlob(ctx, "sim_state", id, payload)
}
func (s *pgStore) GetState(ctx context.Context, id string) (json.RawMessage, error) {
	return s.getBlob(ctx, "sim_state", id)
}

func (s *pgStore) Heartbeat(ctx context.Context, agent, status, detail string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (agent_name, status, last_tick, detail, updated_at)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (agent_name) DO UPDATE SET status = EXCLUDED.status,
			last_tick = EXCLUDED.last_tick, detail = EXCLUDED.detail,
			updated_at = EXCLUDED.updated_at`,
		agent, status, now, truncate(detail, 255), now)
	return err
}

func (s *pgStore) AllHeartbeats(ctx context.Context) ([]AgentHeartbeat, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT agent_name, status, last_tick, detail, updated_at FROM agents ORDER BY agent_name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentHeartbeat
	for rows.Next() {
		var h AgentHeartbeat
		if err := rows.Scan(&h.Agent, &h.Status, &h.LastTick, &h.Detail, &h.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *pgStore) AppendEvent(ctx context.Context, e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO events (ts, kind, symbol, message) VALUES ($1,$2,$3,$4)",
		e.TS, e.Kind, e.Symbol, truncate(e.Message, 512))
	return err
}

func (s *pgStore) EventsSince(ctx context.Context, afterID string, limit int) ([]Event, error) {
	after, _ := strconv.ParseInt(afterID, 10, 64)
	limit = clampLimit(limit, 50, 500)
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, ts, kind, symbol, message FROM events WHERE id > $1 ORDER BY id ASC LIMIT $2",
		after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var id int64
		if err := rows.Scan(&id, &e.TS, &e.Kind, &e.Symbol, &e.Message); err != nil {
			return nil, err
		}
		e.ID = strconv.FormatInt(id, 10)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) TradeTotals(ctx context.Context) (int64, int64, error) {
	var count, volume int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(SUM(quantity),0) FROM trades").Scan(&count, &volume)
	return count, volume, err
}

func (s *pgStore) ReportData(ctx context.Context, limitTrades int) (Report, error) {
	return reportFrom(ctx, s, limitTrades)
}
