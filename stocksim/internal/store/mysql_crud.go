package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// The CRUD half of the MySQL store. Every List* follows one shape — a WHERE
// clause built from ListQuery, a COUNT(*) for the total, then the page — so
// the three of them stay comparable and the frontend can drive all three
// tables with one function.

const securityCols = `id, symbol, name, sector, currency, shares_outstanding,
	open_price, last_price, day_high, day_low, day_volume, listed, created_at, updated_at`

func scanSecurity(sc interface{ Scan(...any) error }) (Security, error) {
	var s Security
	err := sc.Scan(&s.ID, &s.Symbol, &s.Name, &s.Sector, &s.Currency, &s.Shares,
		&s.OpenPrice, &s.LastPrice, &s.DayHigh, &s.DayLow, &s.DayVolume, &s.Listed,
		&s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// isDuplicate recognises MySQL error 1062 so a repeated symbol surfaces as
// ErrConflict (→ 409) rather than a driver string the UI would show raw.
func isDuplicate(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func (s *mysqlStore) ListSecurities(ctx context.Context, q ListQuery) ([]Security, int, error) {
	where, args := []string{"1=1"}, []any{}
	if t := strings.TrimSpace(q.Search); t != "" {
		where = append(where, `(symbol LIKE ? ESCAPE '\\' OR name LIKE ? ESCAPE '\\')`)
		p := "%" + likeEscape(t) + "%"
		args = append(args, p, p)
	}
	if f := strings.TrimSpace(q.Filter); f != "" {
		where = append(where, "sector = ?")
		args = append(args, f)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM securities WHERE "+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := clampLimit(q.Limit, 50, 500)
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+securityCols+" FROM securities WHERE "+clause+" ORDER BY symbol LIMIT ? OFFSET ?",
		append(args, limit, max0(q.Offset))...)
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

func (s *mysqlStore) GetSecurity(ctx context.Context, id string) (Security, error) {
	sec, err := scanSecurity(s.db.QueryRowContext(ctx,
		"SELECT "+securityCols+" FROM securities WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return Security{}, ErrNotFound
	}
	return sec, err
}

func (s *mysqlStore) CreateSecurity(ctx context.Context, sec Security) (Security, error) {
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
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sec.ID, sec.Symbol, sec.Name, sec.Sector, sec.Currency, sec.Shares,
		sec.OpenPrice, sec.LastPrice, sec.DayHigh, sec.DayLow, sec.DayVolume, sec.Listed,
		sec.CreatedAt, sec.UpdatedAt)
	if isDuplicate(err) {
		return Security{}, fmt.Errorf("symbol %s: %w", sec.Symbol, ErrConflict)
	}
	return sec, err
}

// UpdateSecurity writes only the user-editable fields. Price and volume are
// deliberately excluded from the CRUD path in normal operation — those belong
// to ApplyQuotes — except for open_price, which a user may legitimately reset
// to re-baseline the day's change calculation.
func (s *mysqlStore) UpdateSecurity(ctx context.Context, sec Security) (Security, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE securities SET
		symbol=?, name=?, sector=?, currency=?, shares_outstanding=?, open_price=?,
		listed=?, updated_at=? WHERE id=?`,
		sec.Symbol, sec.Name, sec.Sector, sec.Currency, sec.Shares, sec.OpenPrice,
		sec.Listed, time.Now().UTC(), sec.ID)
	if isDuplicate(err) {
		return Security{}, fmt.Errorf("symbol %s: %w", sec.Symbol, ErrConflict)
	}
	if err != nil {
		return Security{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Zero rows can mean "no such id" or "no values changed"; distinguish
		// with a read so an unchanged save is not reported as a 404.
		if _, gerr := s.GetSecurity(ctx, sec.ID); gerr != nil {
			return Security{}, ErrNotFound
		}
	}
	return s.GetSecurity(ctx, sec.ID)
}

// DeleteSecurity removes the instrument and the rows that only make sense
// alongside it (its ticks, its open orders, its holdings). Executed trades are
// kept on purpose: they are the audit trail of what happened, and a report run
// after a delisting should still account for the cash that moved.
func (s *mysqlStore) DeleteSecurity(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM securities WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, stmt := range []string{
		"DELETE FROM price_ticks WHERE security_id = ?",
		"DELETE FROM holdings WHERE security_id = ?",
		"DELETE FROM orders WHERE security_id = ? AND status = 'open'",
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ------------------------------------------------------------- portfolios

const portfolioCols = `id, name, owner, cash, created_at, updated_at`

func scanPortfolio(sc interface{ Scan(...any) error }) (Portfolio, error) {
	var p Portfolio
	err := sc.Scan(&p.ID, &p.Name, &p.Owner, &p.Cash, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (s *mysqlStore) ListPortfolios(ctx context.Context, q ListQuery) ([]Portfolio, int, error) {
	where, args := []string{"1=1"}, []any{}
	if t := strings.TrimSpace(q.Search); t != "" {
		where = append(where, `(name LIKE ? ESCAPE '\\' OR owner LIKE ? ESCAPE '\\')`)
		p := "%" + likeEscape(t) + "%"
		args = append(args, p, p)
	}
	if f := strings.TrimSpace(q.Filter); f != "" {
		where = append(where, "owner = ?")
		args = append(args, f)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM portfolios WHERE "+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := clampLimit(q.Limit, 50, 500)
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+portfolioCols+" FROM portfolios WHERE "+clause+" ORDER BY owner, name LIMIT ? OFFSET ?",
		append(args, limit, max0(q.Offset))...)
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

func (s *mysqlStore) GetPortfolio(ctx context.Context, id string) (Portfolio, error) {
	p, err := scanPortfolio(s.db.QueryRowContext(ctx,
		"SELECT "+portfolioCols+" FROM portfolios WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return Portfolio{}, ErrNotFound
	}
	return p, err
}

func (s *mysqlStore) CreatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error) {
	now := time.Now().UTC()
	p.ID, p.CreatedAt, p.UpdatedAt = newID(), now, now
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO portfolios (id, name, owner, cash, created_at, updated_at) VALUES (?,?,?,?,?,?)",
		p.ID, p.Name, p.Owner, p.Cash, p.CreatedAt, p.UpdatedAt)
	return p, err
}

func (s *mysqlStore) UpdatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE portfolios SET name=?, owner=?, cash=?, updated_at=? WHERE id=?",
		p.Name, p.Owner, p.Cash, time.Now().UTC(), p.ID)
	if err != nil {
		return Portfolio{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.GetPortfolio(ctx, p.ID); gerr != nil {
			return Portfolio{}, ErrNotFound
		}
	}
	return s.GetPortfolio(ctx, p.ID)
}

// DeletePortfolio removes the account, its positions and its open orders,
// keeping executed trades for the same audit reason as DeleteSecurity.
func (s *mysqlStore) DeletePortfolio(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM portfolios WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, stmt := range []string{
		"DELETE FROM holdings WHERE portfolio_id = ?",
		"DELETE FROM orders WHERE portfolio_id = ? AND status = 'open'",
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ----------------------------------------------------------------- orders

const orderCols = `id, portfolio_id, security_id, symbol, owner, side, order_type,
	quantity, limit_price, status, created_at, filled_at`

func scanOrder(sc interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var filled sql.NullTime
	err := sc.Scan(&o.ID, &o.PortfolioID, &o.SecurityID, &o.Symbol, &o.Owner, &o.Side,
		&o.OrderType, &o.Quantity, &o.LimitPrice, &o.Status, &o.CreatedAt, &filled)
	if filled.Valid {
		t := filled.Time
		o.FilledAt = &t
	}
	return o, err
}

func (s *mysqlStore) ListOrders(ctx context.Context, q ListQuery) ([]Order, int, error) {
	where, args := []string{"1=1"}, []any{}
	if t := strings.TrimSpace(q.Search); t != "" {
		where = append(where, `(symbol LIKE ? ESCAPE '\\' OR owner LIKE ? ESCAPE '\\')`)
		p := "%" + likeEscape(t) + "%"
		args = append(args, p, p)
	}
	if f := strings.TrimSpace(q.Filter); f != "" {
		where = append(where, "status = ?")
		args = append(args, f)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM orders WHERE "+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := clampLimit(q.Limit, 50, 500)
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+orderCols+" FROM orders WHERE "+clause+" ORDER BY created_at DESC LIMIT ? OFFSET ?",
		append(args, limit, max0(q.Offset))...)
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

func (s *mysqlStore) GetOrder(ctx context.Context, id string) (Order, error) {
	o, err := scanOrder(s.db.QueryRowContext(ctx,
		"SELECT "+orderCols+" FROM orders WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return Order{}, ErrNotFound
	}
	return o, err
}

func (s *mysqlStore) CreateOrder(ctx context.Context, o Order) (Order, error) {
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
		VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL)`,
		o.ID, o.PortfolioID, o.SecurityID, o.Symbol, o.Owner, o.Side, o.OrderType,
		o.Quantity, o.LimitPrice, o.Status, o.CreatedAt)
	return o, err
}

// UpdateOrder allows editing an order's terms. Cancelling is an update to
// status, not a delete, so the order stays visible in the book's history.
func (s *mysqlStore) UpdateOrder(ctx context.Context, o Order) (Order, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE orders SET side=?, order_type=?, quantity=?, limit_price=?, status=? WHERE id=?",
		o.Side, o.OrderType, o.Quantity, o.LimitPrice, o.Status, o.ID)
	if err != nil {
		return Order{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.GetOrder(ctx, o.ID); gerr != nil {
			return Order{}, ErrNotFound
		}
	}
	return s.GetOrder(ctx, o.ID)
}

func (s *mysqlStore) DeleteOrder(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM orders WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// -------------------------------------------------------- simulation writes

// AppendTicks inserts a batch of price observations in one multi-row INSERT.
// Called every second by the price agent, so it is the hottest write path.
func (s *mysqlStore) AppendTicks(ctx context.Context, ticks []Tick) error {
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
		sb.WriteString("(?,?,?,?,?,?)")
		args = append(args, newID(), t.SecurityID, t.Symbol, t.TS, t.Price, t.Volume)
	}
	_, err := s.db.ExecContext(ctx, sb.String(), args...)
	return err
}

func (s *mysqlStore) RecentTicks(ctx context.Context, securityID string, limit int) ([]Tick, error) {
	limit = clampLimit(limit, 60, 1000)
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, security_id, symbol, ts, price, volume FROM price_ticks "+
			"WHERE security_id = ? ORDER BY ts DESC LIMIT ?", securityID, limit)
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
	// Returned oldest-first so a sparkline can draw straight through them.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// ApplyQuotes writes the price agent's new prices onto the securities rows,
// touching only price/volume columns so a concurrent CRUD edit to a name or
// sector survives. GREATEST/LEAST maintain the session high and low without a
// read-modify-write race between the agent and a user edit.
func (s *mysqlStore) ApplyQuotes(ctx context.Context, quotes []Quote) error {
	if len(quotes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE securities SET
		last_price = ?,
		day_high = GREATEST(day_high, ?),
		day_low  = CASE WHEN day_low = 0 THEN ? ELSE LEAST(day_low, ?) END,
		day_volume = day_volume + ?,
		updated_at = ?
		WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC()
	for _, q := range quotes {
		if _, err := stmt.ExecContext(ctx, q.Price, q.Price, q.Price, q.Price,
			q.Volume, now, q.SecurityID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *mysqlStore) OpenOrders(ctx context.Context, limit int) ([]Order, error) {
	limit = clampLimit(limit, 100, 1000)
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+orderCols+" FROM orders WHERE status = 'open' ORDER BY created_at ASC LIMIT ?", limit)
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

// RecordFill settles an order in one transaction: the order moves to filled,
// the trade is recorded, the holding is upserted with a recalculated average
// cost, and the portfolio's cash moves. All four or none — a half-settled fill
// would show as money that vanished.
//
// The average-cost expression only recalculates on a buy; a sell reduces the
// quantity and leaves the basis alone, which is what makes unrealised P/L on
// the remaining shares stay meaningful. When a sell closes the position
// entirely the basis is reset to zero, so a later repurchase starts clean
// rather than inheriting a stale average.
//
// This arithmetic is only well-defined while quantity stays non-negative,
// which is enforced upstream by sim.settlementBlock — a position that crossed
// zero would produce a negative average cost and nonsensical P/L.
func (s *mysqlStore) RecordFill(ctx context.Context, o Order, t Trade) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		"UPDATE orders SET status = ?, filled_at = ? WHERE id = ? AND status = 'open'",
		OrderFilled, t.TS, o.ID)
	if err != nil {
		return err
	}
	// Zero rows means someone else (a user cancelling from the UI, or another
	// match pass) already moved this order. Abandon quietly rather than
	// double-filling it.
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO trades
		(id, order_id, portfolio_id, security_id, symbol, side, quantity, price, ts)
		VALUES (?,?,?,?,?,?,?,?,?)`,
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
		VALUES (?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			avg_cost = CASE
				WHEN quantity + ? = 0 THEN 0
				WHEN ? > 0 THEN (avg_cost * quantity + ? * ?) / (quantity + ?)
				ELSE avg_cost END,
			quantity = quantity + ?,
			updated_at = VALUES(updated_at)`,
		o.PortfolioID, o.SecurityID, o.Symbol, signedQty, t.Price, t.TS,
		signedQty, signedQty, t.Price, signedQty, signedQty,
		signedQty); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE portfolios SET cash = cash + ?, updated_at = ? WHERE id = ?",
		cashDelta, t.TS, o.PortfolioID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *mysqlStore) ListHoldings(ctx context.Context, portfolioID string) ([]Holding, error) {
	// A fully-closed position keeps its row (so the average-cost reset is
	// durable and a repurchase reuses the same key) but is not a holding any
	// more — listing it would put a row of zeros in the report and the
	// holdings CSV.
	q := `SELECT h.portfolio_id, p.owner, h.security_id, h.symbol, h.quantity,
			h.avg_cost, IFNULL(s.last_price,0), h.updated_at
		FROM holdings h
		LEFT JOIN portfolios p ON p.id = h.portfolio_id
		LEFT JOIN securities s ON s.id = h.security_id
		WHERE h.quantity <> 0`
	args := []any{}
	if portfolioID != "" {
		q += " AND h.portfolio_id = ?"
		args = append(args, portfolioID)
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
		var owner sql.NullString
		if err := rows.Scan(&h.PortfolioID, &owner, &h.SecurityID, &h.Symbol,
			&h.Quantity, &h.AvgCost, &h.LastPrice, &h.UpdatedAt); err != nil {
			return nil, err
		}
		h.Owner = owner.String
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *mysqlStore) CountOrdersByStatus(ctx context.Context) (map[string]int64, error) {
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

func (s *mysqlStore) RecentTrades(ctx context.Context, limit int) ([]Trade, error) {
	limit = clampLimit(limit, 50, 1000)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, order_id, portfolio_id, security_id, symbol, side, quantity, price, ts
		 FROM trades ORDER BY ts DESC LIMIT ?`, limit)
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

func (s *mysqlStore) TradeTotals(ctx context.Context) (int64, int64, error) {
	var count, volume int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*), IFNULL(SUM(quantity),0) FROM trades").Scan(&count, &volume)
	return count, volume, err
}

// ReportData gathers everything the printable report and the CSV exports need
// in one pass, so the printed page is internally consistent rather than
// stitched together from several reads racing the simulation. The assembly is
// shared across all four engines — see reportFrom.
func (s *mysqlStore) ReportData(ctx context.Context, limitTrades int) (Report, error) {
	return reportFrom(ctx, s, limitTrades)
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
