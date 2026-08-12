package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// valkeyStore implements Store against Valkey (or Redis).
//
// There are no tables here, so the "schema" is a key layout. Everything lives
// under Config.Database as a prefix — that prefix is mandatory and validated
// non-empty by Config.Validate, because a blank one would turn DropSchema's
// SCAN pattern into "*", i.e. the entire keyspace of a server this app may not
// own.
//
//	<p>:sec:<id>            HASH    one security
//	<p>:sec:all             ZSET    security ids, scored 0, ordered lexically by member
//	<p>:sec:bysym           HASH    SYMBOL -> id, the uniqueness constraint
//	<p>:tick:<securityId>   STREAM  price ticks, XADD with MAXLEN ~ 500
//	<p>:pf:<id>             HASH    one portfolio
//	<p>:pf:all              ZSET    portfolio ids
//	<p>:ord:<id>            HASH    one order
//	<p>:ord:all             ZSET    order ids, scored by creation time (desc listing)
//	<p>:ord:open            ZSET    open order ids, scored by creation time
//	<p>:trade:<id>          HASH    one trade
//	<p>:trade:all           ZSET    trade ids, scored by timestamp
//	<p>:hold:<pfId>         HASH    securityId -> "qty:avgCost"
//	<p>:hold:pfs            SET     portfolio ids that have any holdings
//	<p>:events              STREAM  the activity feed
//	<p>:metrics:<id>        STRING  JSON blob
//	<p>:state:<id>          STRING  JSON blob
//	<p>:agents              HASH    agent -> JSON heartbeat
//
// Secondary indexes are maintained by hand on every write. That is the cost of
// a key-value store, and it is why each Create/Delete below touches more than
// one key inside a pipeline.
type valkeyStore struct {
	c      redis.UniversalClient
	prefix string
}

func openValkey(ctx context.Context, c Config) (Store, error) {
	addrs := strings.Split(c.Host, ",")
	for i := range addrs {
		addrs[i] = strings.TrimSpace(addrs[i])
		// A bare host with no port gets the default; an "host:port" pair is
		// left alone. VALKEY_ADDRS from a linked node already carries ports.
		if addrs[i] != "" && !strings.Contains(addrs[i], ":") {
			port := c.Port
			if port == 0 {
				port = DefaultPort(EngineValkey)
			}
			addrs[i] = fmt.Sprintf("%s:%d", addrs[i], port)
		}
	}
	opts := &redis.UniversalOptions{
		Addrs:    addrs,
		Password: c.Password,
		Username: c.User, // empty means legacy AUTH with just the password
	}
	// NewUniversalClient transparently picks standalone vs cluster from the
	// address count, the same way trafficsim's store does.
	return &valkeyStore{c: redis.NewUniversalClient(opts), prefix: c.Database}, nil
}

func (s *valkeyStore) k(parts ...string) string {
	return s.prefix + ":" + strings.Join(parts, ":")
}

func (s *valkeyStore) Engine() string   { return EngineValkey }
func (s *valkeyStore) Database() string { return s.prefix }
func (s *valkeyStore) Close() error     { return s.c.Close() }

// Location: Valkey has no databases to speak of in cluster mode, so the key
// prefix is the whole of the namespace and worth naming as such.
func (s *valkeyStore) Location() string {
	return fmt.Sprintf("keys under prefix %q", s.prefix+":")
}

func (s *valkeyStore) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.c.Ping(cctx).Err()
}

func (s *valkeyStore) ServerVersion(ctx context.Context) (string, error) {
	info, err := s.c.Info(ctx, "server").Result()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(info, "\n") {
		for _, key := range []string{"valkey_version:", "redis_version:"} {
			if strings.HasPrefix(line, key) {
				return strings.TrimSpace(strings.TrimPrefix(line, key)), nil
			}
		}
	}
	return "unknown", nil
}

// EnsureSchema has almost nothing to do — keys spring into existence on write.
// It writes a marker so that a freshly-prepared prefix is distinguishable from
// an empty one, and so Objects has something to report before the first tick.
func (s *valkeyStore) EnsureSchema(ctx context.Context) error {
	return s.c.Set(ctx, s.k("meta"), time.Now().UTC().Format(time.RFC3339), 0).Err()
}

// scanDelete removes every key under this app's prefix using SCAN + UNLINK in
// batches. Never KEYS (which blocks the server), never FLUSHDB (which is not
// ours to call) — the prefix is the whole safety boundary.
func (s *valkeyStore) scanDelete(ctx context.Context) error {
	var cursor uint64
	for {
		keys, next, err := s.c.Scan(ctx, cursor, s.prefix+":*", 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := s.c.Unlink(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func (s *valkeyStore) Wipe(ctx context.Context) error {
	if err := s.scanDelete(ctx); err != nil {
		return err
	}
	return s.EnsureSchema(ctx)
}

// DropSchema removes everything including the marker. There is no "database"
// to leave behind here, so unlike the SQL stores this genuinely removes the
// app's entire footprint — bounded strictly by the prefix.
func (s *valkeyStore) DropSchema(ctx context.Context) error {
	return s.scanDelete(ctx)
}

// valkeyGroups maps a reported object name to the key pattern that backs it,
// so the dashboard's Schema panel stays meaningful on an engine with no tables.
var valkeyGroups = []struct{ name, suffix string }{
	{"securities", "sec:*"},
	{"price_ticks", "tick:*"},
	{"portfolios", "pf:*"},
	{"orders", "ord:*"},
	{"trades", "trade:*"},
	{"holdings", "hold:*"},
	{"metrics", "metrics:*"},
	{"sim_state", "state:*"},
	{"agents", "agents"},
	{"events", "events"},
}

// Objects reports one row per logical group with its key count. "Rows" is a
// key count rather than a record count for the grouped patterns, which is the
// closest honest analogue; the entity groups report their index cardinality
// instead so the numbers match what the UI shows elsewhere.
func (s *valkeyStore) Objects(ctx context.Context) ([]ObjectInfo, error) {
	counts := map[string]int64{}
	var cursor uint64
	for {
		keys, next, err := s.c.Scan(ctx, cursor, s.prefix+":*", 500).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			rest := strings.TrimPrefix(key, s.prefix+":")
			for _, g := range valkeyGroups {
				pat := strings.TrimSuffix(g.suffix, "*")
				if (strings.HasSuffix(g.suffix, "*") && strings.HasPrefix(rest, pat)) ||
					rest == g.suffix {
					counts[g.name]++
					break
				}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	// Prefer real cardinalities where an index exists — a user comparing the
	// panel against the securities table should see the same number.
	if n, err := s.c.ZCard(ctx, s.k("sec", "all")).Result(); err == nil && n > 0 {
		counts["securities"] = n
	}
	if n, err := s.c.ZCard(ctx, s.k("pf", "all")).Result(); err == nil && n > 0 {
		counts["portfolios"] = n
	}
	if n, err := s.c.ZCard(ctx, s.k("ord", "all")).Result(); err == nil && n > 0 {
		counts["orders"] = n
	}
	if n, err := s.c.ZCard(ctx, s.k("trade", "all")).Result(); err == nil && n > 0 {
		counts["trades"] = n
	}
	if n, err := s.c.XLen(ctx, s.k("events")).Result(); err == nil && n > 0 {
		counts["events"] = n
	}

	var out []ObjectInfo
	for _, g := range valkeyGroups {
		if counts[g.name] == 0 {
			continue
		}
		out = append(out, ObjectInfo{Name: g.name, Kind: "keyspace", Rows: counts[g.name]})
	}
	return out, nil
}

// ------------------------------------------------------------------ helpers

func vFloat(m map[string]string, key string) float64 {
	f, _ := strconv.ParseFloat(m[key], 64)
	return f
}
func vInt(m map[string]string, key string) int64 {
	n, _ := strconv.ParseInt(m[key], 10, 64)
	return n
}
func vTime(m map[string]string, key string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, m[key])
	return t
}
func fs(f float64) string   { return strconv.FormatFloat(f, 'f', -1, 64) }
func is(n int64) string     { return strconv.FormatInt(n, 10) }
func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// paginate applies offset/limit to an already-ordered id slice.
func paginate(ids []string, offset, limit int) []string {
	if offset >= len(ids) {
		return nil
	}
	end := offset + limit
	if end > len(ids) {
		end = len(ids)
	}
	return ids[offset:end]
}

// ---------------------------------------------------------------- securities

func (s *valkeyStore) securityFrom(m map[string]string) Security {
	return Security{
		ID: m["id"], Symbol: m["symbol"], Name: m["name"], Sector: m["sector"],
		Currency: m["currency"], Shares: vInt(m, "shares"),
		OpenPrice: vFloat(m, "openPrice"), LastPrice: vFloat(m, "lastPrice"),
		DayHigh: vFloat(m, "dayHigh"), DayLow: vFloat(m, "dayLow"),
		DayVolume: vInt(m, "dayVolume"), Listed: m["listed"] == "1",
		CreatedAt: vTime(m, "createdAt"), UpdatedAt: vTime(m, "updatedAt"),
	}
}

func (s *valkeyStore) allSecurities(ctx context.Context) ([]Security, error) {
	ids, err := s.c.ZRange(ctx, s.k("sec", "all"), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	pipe := s.c.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, s.k("sec", id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	out := make([]Security, 0, len(ids))
	for _, c := range cmds {
		m := c.Val()
		if len(m) == 0 {
			continue
		}
		out = append(out, s.securityFrom(m))
	}
	return out, nil
}

func (s *valkeyStore) ListSecurities(ctx context.Context, q ListQuery) ([]Security, int, error) {
	all, err := s.allSecurities(ctx)
	if err != nil {
		return nil, 0, err
	}
	// Filtering happens in the app: Valkey has no secondary-index query, and
	// building one for free-text search would cost more than it saves at this
	// scale (tens of instruments).
	term := strings.ToLower(strings.TrimSpace(q.Search))
	sector := strings.TrimSpace(q.Filter)
	filtered := all[:0:0]
	for _, sec := range all {
		if term != "" && !strings.Contains(strings.ToLower(sec.Symbol), term) &&
			!strings.Contains(strings.ToLower(sec.Name), term) {
			continue
		}
		if sector != "" && sec.Sector != sector {
			continue
		}
		filtered = append(filtered, sec)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Symbol < filtered[j].Symbol })
	total := len(filtered)
	limit := clampLimit(q.Limit, 50, 500)
	off := max0(q.Offset)
	if off >= total {
		return []Security{}, total, nil
	}
	end := off + limit
	if end > total {
		end = total
	}
	return filtered[off:end], total, nil
}

func (s *valkeyStore) GetSecurity(ctx context.Context, id string) (Security, error) {
	m, err := s.c.HGetAll(ctx, s.k("sec", id)).Result()
	if err != nil {
		return Security{}, err
	}
	if len(m) == 0 {
		return Security{}, ErrNotFound
	}
	return s.securityFrom(m), nil
}

func (s *valkeyStore) CreateSecurity(ctx context.Context, sec Security) (Security, error) {
	// HSETNX on the symbol index is the uniqueness constraint: it succeeds for
	// exactly one writer, which is what makes a duplicate symbol a conflict
	// rather than a silent overwrite.
	sec.ID = newID()
	ok, err := s.c.HSetNX(ctx, s.k("sec", "bysym"), sec.Symbol, sec.ID).Result()
	if err != nil {
		return Security{}, err
	}
	if !ok {
		return Security{}, fmt.Errorf("symbol %s: %w", sec.Symbol, ErrConflict)
	}
	now := time.Now().UTC()
	sec.CreatedAt, sec.UpdatedAt = now, now
	if sec.DayLow == 0 {
		sec.DayLow = sec.LastPrice
	}
	if sec.DayHigh == 0 {
		sec.DayHigh = sec.LastPrice
	}
	pipe := s.c.TxPipeline()
	pipe.HSet(ctx, s.k("sec", sec.ID), s.securityFields(sec))
	pipe.ZAdd(ctx, s.k("sec", "all"), redis.Z{Score: 0, Member: sec.ID})
	if _, err := pipe.Exec(ctx); err != nil {
		s.c.HDel(ctx, s.k("sec", "bysym"), sec.Symbol) // release the claimed symbol
		return Security{}, err
	}
	return sec, nil
}

func (s *valkeyStore) securityFields(sec Security) []any {
	listed := "0"
	if sec.Listed {
		listed = "1"
	}
	return []any{
		"id", sec.ID, "symbol", sec.Symbol, "name", sec.Name, "sector", sec.Sector,
		"currency", sec.Currency, "shares", is(sec.Shares),
		"openPrice", fs(sec.OpenPrice), "lastPrice", fs(sec.LastPrice),
		"dayHigh", fs(sec.DayHigh), "dayLow", fs(sec.DayLow),
		"dayVolume", is(sec.DayVolume), "listed", listed,
		"createdAt", ts(sec.CreatedAt), "updatedAt", ts(sec.UpdatedAt),
	}
}

func (s *valkeyStore) UpdateSecurity(ctx context.Context, sec Security) (Security, error) {
	cur, err := s.GetSecurity(ctx, sec.ID)
	if err != nil {
		return Security{}, err
	}
	if cur.Symbol != sec.Symbol {
		ok, err := s.c.HSetNX(ctx, s.k("sec", "bysym"), sec.Symbol, sec.ID).Result()
		if err != nil {
			return Security{}, err
		}
		if !ok {
			return Security{}, fmt.Errorf("symbol %s: %w", sec.Symbol, ErrConflict)
		}
		s.c.HDel(ctx, s.k("sec", "bysym"), cur.Symbol)
	}
	listed := "0"
	if sec.Listed {
		listed = "1"
	}
	if err := s.c.HSet(ctx, s.k("sec", sec.ID),
		"symbol", sec.Symbol, "name", sec.Name, "sector", sec.Sector,
		"currency", sec.Currency, "shares", is(sec.Shares),
		"openPrice", fs(sec.OpenPrice), "listed", listed,
		"updatedAt", ts(time.Now().UTC())).Err(); err != nil {
		return Security{}, err
	}
	return s.GetSecurity(ctx, sec.ID)
}

func (s *valkeyStore) DeleteSecurity(ctx context.Context, id string) error {
	sec, err := s.GetSecurity(ctx, id)
	if err != nil {
		return err
	}
	pipe := s.c.TxPipeline()
	pipe.Unlink(ctx, s.k("sec", id), s.k("tick", id))
	pipe.HDel(ctx, s.k("sec", "bysym"), sec.Symbol)
	pipe.ZRem(ctx, s.k("sec", "all"), id)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	// Holdings in this security, and its open orders, go too — trades stay.
	pfIDs, _ := s.c.SMembers(ctx, s.k("hold", "pfs")).Result()
	for _, pf := range pfIDs {
		s.c.HDel(ctx, s.k("hold", pf), id)
	}
	openIDs, _ := s.c.ZRange(ctx, s.k("ord", "open"), 0, -1).Result()
	for _, oid := range openIDs {
		if s.c.HGet(ctx, s.k("ord", oid), "securityId").Val() == id {
			s.deleteOrderKeys(ctx, oid)
		}
	}
	return nil
}

// ---------------------------------------------------------------- portfolios

func (s *valkeyStore) portfolioFrom(m map[string]string) Portfolio {
	return Portfolio{
		ID: m["id"], Name: m["name"], Owner: m["owner"], Cash: vFloat(m, "cash"),
		CreatedAt: vTime(m, "createdAt"), UpdatedAt: vTime(m, "updatedAt"),
	}
}

func (s *valkeyStore) allPortfolios(ctx context.Context) ([]Portfolio, error) {
	ids, err := s.c.ZRange(ctx, s.k("pf", "all"), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	pipe := s.c.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, s.k("pf", id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	out := make([]Portfolio, 0, len(ids))
	for _, c := range cmds {
		if m := c.Val(); len(m) > 0 {
			out = append(out, s.portfolioFrom(m))
		}
	}
	return out, nil
}

func (s *valkeyStore) ListPortfolios(ctx context.Context, q ListQuery) ([]Portfolio, int, error) {
	all, err := s.allPortfolios(ctx)
	if err != nil {
		return nil, 0, err
	}
	term := strings.ToLower(strings.TrimSpace(q.Search))
	owner := strings.TrimSpace(q.Filter)
	filtered := all[:0:0]
	for _, p := range all {
		if term != "" && !strings.Contains(strings.ToLower(p.Name), term) &&
			!strings.Contains(strings.ToLower(p.Owner), term) {
			continue
		}
		if owner != "" && p.Owner != owner {
			continue
		}
		filtered = append(filtered, p)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Owner != filtered[j].Owner {
			return filtered[i].Owner < filtered[j].Owner
		}
		return filtered[i].Name < filtered[j].Name
	})
	total := len(filtered)
	limit := clampLimit(q.Limit, 50, 500)
	off := max0(q.Offset)
	if off >= total {
		return []Portfolio{}, total, nil
	}
	end := off + limit
	if end > total {
		end = total
	}
	return filtered[off:end], total, nil
}

func (s *valkeyStore) GetPortfolio(ctx context.Context, id string) (Portfolio, error) {
	m, err := s.c.HGetAll(ctx, s.k("pf", id)).Result()
	if err != nil {
		return Portfolio{}, err
	}
	if len(m) == 0 {
		return Portfolio{}, ErrNotFound
	}
	return s.portfolioFrom(m), nil
}

func (s *valkeyStore) CreatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error) {
	now := time.Now().UTC()
	p.ID, p.CreatedAt, p.UpdatedAt = newID(), now, now
	pipe := s.c.TxPipeline()
	pipe.HSet(ctx, s.k("pf", p.ID), "id", p.ID, "name", p.Name, "owner", p.Owner,
		"cash", fs(p.Cash), "createdAt", ts(p.CreatedAt), "updatedAt", ts(p.UpdatedAt))
	pipe.ZAdd(ctx, s.k("pf", "all"), redis.Z{Score: 0, Member: p.ID})
	_, err := pipe.Exec(ctx)
	return p, err
}

func (s *valkeyStore) UpdatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error) {
	if _, err := s.GetPortfolio(ctx, p.ID); err != nil {
		return Portfolio{}, err
	}
	if err := s.c.HSet(ctx, s.k("pf", p.ID), "name", p.Name, "owner", p.Owner,
		"cash", fs(p.Cash), "updatedAt", ts(time.Now().UTC())).Err(); err != nil {
		return Portfolio{}, err
	}
	return s.GetPortfolio(ctx, p.ID)
}

func (s *valkeyStore) DeletePortfolio(ctx context.Context, id string) error {
	if _, err := s.GetPortfolio(ctx, id); err != nil {
		return err
	}
	pipe := s.c.TxPipeline()
	pipe.Unlink(ctx, s.k("pf", id), s.k("hold", id))
	pipe.ZRem(ctx, s.k("pf", "all"), id)
	pipe.SRem(ctx, s.k("hold", "pfs"), id)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	openIDs, _ := s.c.ZRange(ctx, s.k("ord", "open"), 0, -1).Result()
	for _, oid := range openIDs {
		if s.c.HGet(ctx, s.k("ord", oid), "portfolioId").Val() == id {
			s.deleteOrderKeys(ctx, oid)
		}
	}
	return nil
}

// -------------------------------------------------------------------- orders

func (s *valkeyStore) orderFrom(m map[string]string) Order {
	o := Order{
		ID: m["id"], PortfolioID: m["portfolioId"], SecurityID: m["securityId"],
		Symbol: m["symbol"], Owner: m["owner"], Side: m["side"], OrderType: m["orderType"],
		Quantity: vInt(m, "quantity"), LimitPrice: vFloat(m, "limitPrice"),
		Status: m["status"], CreatedAt: vTime(m, "createdAt"),
	}
	if m["filledAt"] != "" {
		t := vTime(m, "filledAt")
		o.FilledAt = &t
	}
	return o
}

func (s *valkeyStore) ordersByIDs(ctx context.Context, ids []string) []Order {
	if len(ids) == 0 {
		return []Order{}
	}
	pipe := s.c.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, s.k("ord", id))
	}
	pipe.Exec(ctx)
	out := make([]Order, 0, len(ids))
	for _, c := range cmds {
		if m := c.Val(); len(m) > 0 {
			out = append(out, s.orderFrom(m))
		}
	}
	return out
}

func (s *valkeyStore) ListOrders(ctx context.Context, q ListQuery) ([]Order, int, error) {
	// ZREVRANGE gives newest-first directly, matching the SQL stores'
	// ORDER BY created_at DESC. When a filter is present the whole index has
	// to be materialised, which is bounded by the sim's own order volume.
	ids, err := s.c.ZRevRange(ctx, s.k("ord", "all"), 0, -1).Result()
	if err != nil {
		return nil, 0, err
	}
	all := s.ordersByIDs(ctx, ids)
	term := strings.ToLower(strings.TrimSpace(q.Search))
	status := strings.TrimSpace(q.Filter)
	filtered := all[:0:0]
	for _, o := range all {
		if term != "" && !strings.Contains(strings.ToLower(o.Symbol), term) &&
			!strings.Contains(strings.ToLower(o.Owner), term) {
			continue
		}
		if status != "" && o.Status != status {
			continue
		}
		filtered = append(filtered, o)
	}
	total := len(filtered)
	limit := clampLimit(q.Limit, 50, 500)
	off := max0(q.Offset)
	if off >= total {
		return []Order{}, total, nil
	}
	end := off + limit
	if end > total {
		end = total
	}
	return filtered[off:end], total, nil
}

func (s *valkeyStore) GetOrder(ctx context.Context, id string) (Order, error) {
	m, err := s.c.HGetAll(ctx, s.k("ord", id)).Result()
	if err != nil {
		return Order{}, err
	}
	if len(m) == 0 {
		return Order{}, ErrNotFound
	}
	return s.orderFrom(m), nil
}

func (s *valkeyStore) CreateOrder(ctx context.Context, o Order) (Order, error) {
	o.ID = newID()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	if o.Status == "" {
		o.Status = OrderOpen
	}
	score := float64(o.CreatedAt.UnixNano())
	pipe := s.c.TxPipeline()
	pipe.HSet(ctx, s.k("ord", o.ID),
		"id", o.ID, "portfolioId", o.PortfolioID, "securityId", o.SecurityID,
		"symbol", o.Symbol, "owner", o.Owner, "side", o.Side, "orderType", o.OrderType,
		"quantity", is(o.Quantity), "limitPrice", fs(o.LimitPrice),
		"status", o.Status, "createdAt", ts(o.CreatedAt), "filledAt", "")
	pipe.ZAdd(ctx, s.k("ord", "all"), redis.Z{Score: score, Member: o.ID})
	if o.Status == OrderOpen {
		pipe.ZAdd(ctx, s.k("ord", "open"), redis.Z{Score: score, Member: o.ID})
	}
	_, err := pipe.Exec(ctx)
	return o, err
}

func (s *valkeyStore) UpdateOrder(ctx context.Context, o Order) (Order, error) {
	if _, err := s.GetOrder(ctx, o.ID); err != nil {
		return Order{}, err
	}
	pipe := s.c.TxPipeline()
	pipe.HSet(ctx, s.k("ord", o.ID), "side", o.Side, "orderType", o.OrderType,
		"quantity", is(o.Quantity), "limitPrice", fs(o.LimitPrice), "status", o.Status)
	// The open index has to follow the status, or the match agent keeps
	// picking up orders a user has already cancelled.
	if o.Status == OrderOpen {
		pipe.ZAdd(ctx, s.k("ord", "open"),
			redis.Z{Score: float64(o.CreatedAt.UnixNano()), Member: o.ID})
	} else {
		pipe.ZRem(ctx, s.k("ord", "open"), o.ID)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return Order{}, err
	}
	return s.GetOrder(ctx, o.ID)
}

func (s *valkeyStore) deleteOrderKeys(ctx context.Context, id string) {
	pipe := s.c.TxPipeline()
	pipe.Unlink(ctx, s.k("ord", id))
	pipe.ZRem(ctx, s.k("ord", "all"), id)
	pipe.ZRem(ctx, s.k("ord", "open"), id)
	pipe.Exec(ctx)
}

func (s *valkeyStore) DeleteOrder(ctx context.Context, id string) error {
	if _, err := s.GetOrder(ctx, id); err != nil {
		return err
	}
	s.deleteOrderKeys(ctx, id)
	return nil
}

// --------------------------------------------------------- simulation writes

// AppendTicks writes one stream entry per tick, capped with MAXLEN ~ so the
// history stays bounded without an explicit cleanup job. This is the one place
// the key-value model is a better fit than a table: a capped stream is exactly
// what a price history wants.
func (s *valkeyStore) AppendTicks(ctx context.Context, ticks []Tick) error {
	if len(ticks) == 0 {
		return nil
	}
	pipe := s.c.Pipeline()
	for _, t := range ticks {
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: s.k("tick", t.SecurityID),
			MaxLen: 500, Approx: true,
			Values: map[string]any{
				"symbol": t.Symbol, "price": fs(t.Price),
				"volume": is(t.Volume), "ts": ts(t.TS),
			},
		})
	}
	_, err := pipe.Exec(ctx)
	return err
}

// TicksBefore reads back through the security's stream. at is a stream id
// bound rather than a field comparison, which works because AppendTicks writes
// in time order and lets Valkey do the seek — but note the stream is capped at
// 500 entries per security (see AppendTicks), so "history" here is the last few
// minutes and nothing more. That cap is why this engine has neither a size
// target nor a working set; see CanGrowToSize.
func (s *valkeyStore) TicksBefore(ctx context.Context, securityID string, at time.Time, limit int) ([]Tick, error) {
	n := int64(clampLimit(limit, 60, 1000))
	upper := "+"
	if !at.IsZero() {
		upper = strconv.FormatInt(at.UTC().UnixMilli(), 10)
	}
	entries, err := s.c.XRevRangeN(ctx, s.k("tick", securityID), upper, "-", n).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Tick, 0, len(entries))
	// XREVRANGE is newest-first; walk backwards to return oldest-first so a
	// sparkline can draw straight through the result.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		vals := map[string]string{}
		for k, v := range e.Values {
			vals[k], _ = v.(string)
		}
		out = append(out, Tick{
			ID: e.ID, SecurityID: securityID, Symbol: vals["symbol"],
			TS: vTime(vals, "ts"), Price: vFloat(vals, "price"), Volume: vInt(vals, "volume"),
		})
	}
	return out, nil
}

// TickSpan reads the two ends of one security's stream. Both ends are O(1) for
// a stream, and the answer only ever covers the capped window described on
// TicksBefore.
func (s *valkeyStore) TickSpan(ctx context.Context, securityID string) (time.Time, time.Time, error) {
	key := s.k("tick", securityID)
	first, err := s.c.XRangeN(ctx, key, "-", "+", 1).Result()
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	last, err := s.c.XRevRangeN(ctx, key, "+", "-", 1).Result()
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if len(first) == 0 || len(last) == 0 {
		return time.Time{}, time.Time{}, nil
	}
	entryTime := func(e redis.XMessage) time.Time {
		vals := map[string]string{}
		for k, v := range e.Values {
			vals[k], _ = v.(string)
		}
		return vTime(vals, "ts")
	}
	return entryTime(first[0]), entryTime(last[0]), nil
}

func (s *valkeyStore) ApplyQuotes(ctx context.Context, quotes []Quote) error {
	if len(quotes) == 0 {
		return nil
	}
	now := ts(time.Now().UTC())
	// Read-modify-write for the high/low, because Valkey has no conditional
	// numeric update. The price agent is the only writer of these fields, so
	// there is no contention in practice.
	pipe := s.c.Pipeline()
	cur := make([]*redis.SliceCmd, len(quotes))
	for i, q := range quotes {
		cur[i] = pipe.HMGet(ctx, s.k("sec", q.SecurityID), "dayHigh", "dayLow")
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return err
	}
	wpipe := s.c.Pipeline()
	for i, q := range quotes {
		vals := cur[i].Val()
		high, low := 0.0, 0.0
		if len(vals) == 2 {
			if s0, ok := vals[0].(string); ok {
				high, _ = strconv.ParseFloat(s0, 64)
			}
			if s1, ok := vals[1].(string); ok {
				low, _ = strconv.ParseFloat(s1, 64)
			}
		}
		if q.Price > high {
			high = q.Price
		}
		if low == 0 || q.Price < low {
			low = q.Price
		}
		wpipe.HSet(ctx, s.k("sec", q.SecurityID),
			"lastPrice", fs(q.Price), "dayHigh", fs(high), "dayLow", fs(low),
			"updatedAt", now)
		wpipe.HIncrBy(ctx, s.k("sec", q.SecurityID), "dayVolume", q.Volume)
	}
	_, err := wpipe.Exec(ctx)
	return err
}

func (s *valkeyStore) OpenOrders(ctx context.Context, limit int) ([]Order, error) {
	ids, err := s.c.ZRange(ctx, s.k("ord", "open"), 0,
		int64(clampLimit(limit, 100, 1000))-1).Result()
	if err != nil {
		return nil, err
	}
	return s.ordersByIDs(ctx, ids), nil
}

// RecordFill settles inside a MULTI/EXEC, so the order move, the trade, the
// holding and the cash change all land or none do. The compare-and-set that
// prevents a double fill cannot live inside the transaction (Valkey has no
// conditional write), so it is done first with HGET + a status check; the
// match agent is the only filler, which makes the window academic here.
func (s *valkeyStore) RecordFill(ctx context.Context, o Order, t Trade) error {
	status, err := s.c.HGet(ctx, s.k("ord", o.ID), "status").Result()
	if err == redis.Nil || status != OrderOpen {
		return nil // already moved, or gone
	}
	if err != nil {
		return err
	}

	signedQty := t.Quantity
	cashDelta := -t.Notional()
	if o.Side == SideSell {
		signedQty = -t.Quantity
		cashDelta = t.Notional()
	}

	// Existing position, needed for the weighted average.
	var qty int64
	var avg float64
	if raw, err := s.c.HGet(ctx, s.k("hold", o.PortfolioID), o.SecurityID).Result(); err == nil {
		qty, avg = parseHolding(raw)
	}
	newQty := qty + signedQty
	newAvg := avg
	switch {
	case newQty == 0:
		newAvg = 0
	case signedQty > 0:
		newAvg = (avg*float64(qty) + t.Price*float64(signedQty)) / float64(newQty)
	}

	tradeID := newID()
	pipe := s.c.TxPipeline()
	pipe.HSet(ctx, s.k("ord", o.ID), "status", OrderFilled, "filledAt", ts(t.TS))
	pipe.ZRem(ctx, s.k("ord", "open"), o.ID)
	pipe.HSet(ctx, s.k("trade", tradeID),
		"id", tradeID, "orderId", o.ID, "portfolioId", o.PortfolioID,
		"securityId", o.SecurityID, "symbol", o.Symbol, "side", o.Side,
		"quantity", is(t.Quantity), "price", fs(t.Price), "ts", ts(t.TS))
	pipe.ZAdd(ctx, s.k("trade", "all"), redis.Z{Score: float64(t.TS.UnixNano()), Member: tradeID})
	pipe.HSet(ctx, s.k("hold", o.PortfolioID), o.SecurityID,
		formatHolding(newQty, newAvg, o.Symbol))
	pipe.SAdd(ctx, s.k("hold", "pfs"), o.PortfolioID)
	pipe.HIncrByFloat(ctx, s.k("pf", o.PortfolioID), "cash", cashDelta)
	pipe.HSet(ctx, s.k("pf", o.PortfolioID), "updatedAt", ts(t.TS))
	_, err = pipe.Exec(ctx)
	return err
}

// A holding is stored as one field per security: "qty:avgCost:symbol". Packing
// it into a single HASH field keeps a portfolio's whole position set to one
// key, which is what makes ListHoldings a handful of reads instead of one per
// instrument.
func formatHolding(qty int64, avg float64, symbol string) string {
	return is(qty) + ":" + fs(avg) + ":" + symbol
}

func parseHolding(raw string) (int64, float64) {
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) < 2 {
		return 0, 0
	}
	q, _ := strconv.ParseInt(parts[0], 10, 64)
	a, _ := strconv.ParseFloat(parts[1], 64)
	return q, a
}

func parseHoldingSymbol(raw string) string {
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func (s *valkeyStore) ListHoldings(ctx context.Context, portfolioID string) ([]Holding, error) {
	owners := map[string]string{}
	if pfs, err := s.allPortfolios(ctx); err == nil {
		for _, p := range pfs {
			owners[p.ID] = p.Owner
		}
	}
	prices := map[string]float64{}
	if secs, err := s.allSecurities(ctx); err == nil {
		for _, sec := range secs {
			prices[sec.ID] = sec.LastPrice
		}
	}

	pfIDs := []string{portfolioID}
	if portfolioID == "" {
		var err error
		if pfIDs, err = s.c.SMembers(ctx, s.k("hold", "pfs")).Result(); err != nil {
			return nil, err
		}
	}
	out := []Holding{}
	for _, pf := range pfIDs {
		m, err := s.c.HGetAll(ctx, s.k("hold", pf)).Result()
		if err != nil {
			return nil, err
		}
		for secID, raw := range m {
			qty, avg := parseHolding(raw)
			if qty == 0 {
				continue
			}
			out = append(out, Holding{
				PortfolioID: pf, Owner: owners[pf], SecurityID: secID,
				Symbol: parseHoldingSymbol(raw), Quantity: qty, AvgCost: avg,
				LastPrice: prices[secID],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out, nil
}

func (s *valkeyStore) CountOrdersByStatus(ctx context.Context) (map[string]int64, error) {
	ids, err := s.c.ZRange(ctx, s.k("ord", "all"), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	pipe := s.c.Pipeline()
	cmds := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGet(ctx, s.k("ord", id), "status")
	}
	pipe.Exec(ctx)
	out := map[string]int64{}
	for _, c := range cmds {
		if v := c.Val(); v != "" {
			out[v]++
		}
	}
	return out, nil
}

func (s *valkeyStore) tradesByIDs(ctx context.Context, ids []string) []Trade {
	pipe := s.c.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, s.k("trade", id))
	}
	pipe.Exec(ctx)
	out := make([]Trade, 0, len(ids))
	for _, c := range cmds {
		m := c.Val()
		if len(m) == 0 {
			continue
		}
		out = append(out, Trade{
			ID: m["id"], OrderID: m["orderId"], PortfolioID: m["portfolioId"],
			SecurityID: m["securityId"], Symbol: m["symbol"], Side: m["side"],
			Quantity: vInt(m, "quantity"), Price: vFloat(m, "price"), TS: vTime(m, "ts"),
		})
	}
	return out
}

func (s *valkeyStore) RecentTrades(ctx context.Context, limit int) ([]Trade, error) {
	ids, err := s.c.ZRevRange(ctx, s.k("trade", "all"), 0,
		int64(clampLimit(limit, 50, 1000))-1).Result()
	if err != nil {
		return nil, err
	}
	return s.tradesByIDs(ctx, ids), nil
}

// PruneOrders walks the ord:all sorted set, which is scored by creation time,
// so everything old enough is one ZRANGEBYSCORE away and nothing newer is even
// looked at. Only terminal orders are removed; an open one that happens to be
// old stays in the book and simply stays in the set.
//
// This matters more here than on the SQL engines. Valkey holds all of this in
// memory, and CountOrdersByStatus reads every order's status on every call — so
// an unpruned deployment does not merely get slower, it grows until the server
// is killed for it.
func (s *valkeyStore) PruneOrders(ctx context.Context, before time.Time, limit int) (map[string]int64, error) {
	out := map[string]int64{}
	ids, err := s.c.ZRangeByScore(ctx, s.k("ord", "all"), &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(before.UTC().UnixNano(), 10),
		Count: int64(limit),
	}).Result()
	if err != nil || len(ids) == 0 {
		return out, err
	}

	// One pipelined read of every candidate's status, then delete only those in
	// a terminal state.
	pipe := s.c.Pipeline()
	statuses := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		statuses[i] = pipe.HGet(ctx, s.k("ord", id), "status")
	}
	pipe.Exec(ctx)

	terminal := map[string]bool{}
	for _, st := range TerminalOrderStatuses {
		terminal[st] = true
	}
	del := s.c.TxPipeline()
	var found int
	for i, id := range ids {
		status := statuses[i].Val()
		if !terminal[status] {
			continue
		}
		del.Del(ctx, s.k("ord", id))
		del.ZRem(ctx, s.k("ord", "all"), id)
		del.ZRem(ctx, s.k("ord", "open"), id)
		out[status]++
		found++
	}
	if found == 0 {
		return out, nil
	}
	if _, err := del.Exec(ctx); err != nil {
		return map[string]int64{}, err
	}
	return out, nil
}

func (s *valkeyStore) TradeTotals(ctx context.Context) (int64, int64, error) {
	count, err := s.c.ZCard(ctx, s.k("trade", "all")).Result()
	if err != nil {
		return 0, 0, err
	}
	ids, err := s.c.ZRange(ctx, s.k("trade", "all"), 0, -1).Result()
	if err != nil {
		return count, 0, err
	}
	pipe := s.c.Pipeline()
	cmds := make([]*redis.StringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGet(ctx, s.k("trade", id), "quantity")
	}
	pipe.Exec(ctx)
	var volume int64
	for _, c := range cmds {
		n, _ := strconv.ParseInt(c.Val(), 10, 64)
		volume += n
	}
	return count, volume, nil
}

// ------------------------------------------------- metrics / state / agents

func (s *valkeyStore) PutMetrics(ctx context.Context, id string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.c.Set(ctx, s.k("metrics", id), string(b), 0).Err()
}

func (s *valkeyStore) GetMetrics(ctx context.Context, id string) (json.RawMessage, error) {
	v, err := s.c.Get(ctx, s.k("metrics", id)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(v), nil
}

func (s *valkeyStore) PutState(ctx context.Context, id string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.c.Set(ctx, s.k("state", id), string(b), 0).Err()
}

func (s *valkeyStore) GetState(ctx context.Context, id string) (json.RawMessage, error) {
	v, err := s.c.Get(ctx, s.k("state", id)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(v), nil
}

func (s *valkeyStore) Heartbeat(ctx context.Context, agent, status, detail string) error {
	now := time.Now().UTC()
	b, err := json.Marshal(AgentHeartbeat{
		Agent: agent, Status: status, LastTick: now,
		Detail: truncate(detail, 255), UpdatedAt: now,
	})
	if err != nil {
		return err
	}
	return s.c.HSet(ctx, s.k("agents"), agent, string(b)).Err()
}

func (s *valkeyStore) AllHeartbeats(ctx context.Context) ([]AgentHeartbeat, error) {
	m, err := s.c.HGetAll(ctx, s.k("agents")).Result()
	if err != nil {
		return nil, err
	}
	var out []AgentHeartbeat
	for _, raw := range m {
		var h AgentHeartbeat
		if json.Unmarshal([]byte(raw), &h) == nil {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out, nil
}

// The event feed is a stream, and the stream's own entry ids are the cursor —
// no separate sequence is needed, unlike the MongoDB store.
func (s *valkeyStore) AppendEvent(ctx context.Context, e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	return s.c.XAdd(ctx, &redis.XAddArgs{
		Stream: s.k("events"),
		MaxLen: 1000, Approx: true,
		Values: map[string]any{
			"kind": e.Kind, "symbol": e.Symbol,
			"message": truncate(e.Message, 512), "ts": ts(e.TS),
		},
	}).Err()
}

func (s *valkeyStore) EventsSince(ctx context.Context, afterID string, limit int) ([]Event, error) {
	start := "-"
	if afterID != "" {
		// XRANGE is inclusive; "(" makes it exclusive so a cursor is not
		// replayed. Supported since Redis 6.2 / all Valkey versions.
		start = "(" + afterID
	}
	entries, err := s.c.XRangeN(ctx, s.k("events"), start, "+",
		int64(clampLimit(limit, 50, 500))).Result()
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, e := range entries {
		vals := map[string]string{}
		for k, v := range e.Values {
			vals[k], _ = v.(string)
		}
		out = append(out, Event{
			ID: e.ID, TS: vTime(vals, "ts"), Kind: vals["kind"],
			Symbol: vals["symbol"], Message: vals["message"],
		})
	}
	return out, nil
}

func (s *valkeyStore) ReportData(ctx context.Context, limitTrades int) (Report, error) {
	return reportFrom(ctx, s, limitTrades)
}
