package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// mongoStore implements Store against MongoDB.
//
// The relational schema maps onto collections of the same names, with the
// app's string ids used directly as _id. Two things differ from the SQL stores
// and are called out where they bite:
//
//   - There are no joins. ListHoldings therefore denormalises owner and last
//     price at read time from two small lookups, which is affordable because
//     both collections are tiny by construction.
//   - There are no multi-document transactions on a standalone mongod (they
//     need a replica set). RecordFill is therefore ordered so that a failure
//     part-way leaves the least damaging state — see its comment.
type mongoStore struct {
	client *mongo.Client
	db     *mongo.Database
	name   string
}

// mongoCollections is the complete set this app owns, and the list Wipe,
// DropSchema and Objects iterate.
var mongoCollections = []string{
	"securities", "price_ticks", "portfolios", "orders", "trades", "holdings",
	"metrics", "sim_state", "agents", "events",
}

func openMongo(ctx context.Context, c Config) (Store, error) {
	uri := c.DSN
	if uri == "" {
		uri = mongoURI(c)
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	client, err := mongo.Connect(cctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("open mongodb: %w", err)
	}
	return &mongoStore{client: client, db: client.Database(c.Database), name: c.Database}, nil
}

func mongoURI(c Config) string {
	port := c.Port
	if port == 0 {
		port = DefaultPort(EngineMongoDB)
	}
	q := url.Values{}
	q.Set("authSource", "admin")
	// directConnection keeps the driver from trying to discover a replica set
	// that a standalone target does not have.
	q.Set("directConnection", "true")
	if c.TLS == "require" {
		q.Set("tls", "true")
		q.Set("tlsInsecure", "true") // self-signed is the norm for a lab target
	}
	u := &url.URL{
		Scheme: "mongodb",
		Host:   fmt.Sprintf("%s:%d", c.Host, port),
		// The trailing slash is required: the driver rejects a URI whose query
		// string follows the host directly ("must have a / before the query ?").
		Path:     "/",
		RawQuery: q.Encode(),
	}
	if c.User != "" {
		u.User = url.UserPassword(c.User, c.Password)
	}
	out := u.String()
	if extra := strings.TrimSpace(c.Params); extra != "" {
		out += "&" + strings.TrimPrefix(extra, "&")
	}
	return out
}

func (s *mongoStore) Engine() string   { return EngineMongoDB }
func (s *mongoStore) Database() string { return s.name }
func (s *mongoStore) Close() error     { return s.client.Disconnect(context.Background()) }

func (s *mongoStore) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.client.Ping(cctx, nil)
}

func (s *mongoStore) ServerVersion(ctx context.Context) (string, error) {
	var res struct {
		Version string `bson:"version"`
	}
	err := s.client.Database("admin").
		RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&res)
	return res.Version, err
}

// EnsureSchema creates the indexes the app relies on. Collections themselves
// are created implicitly on first write, so there is nothing to declare — the
// indexes are the schema here.
func (s *mongoStore) EnsureSchema(ctx context.Context) error {
	idx := map[string][]mongo.IndexModel{
		"securities": {
			{Keys: bson.D{{Key: "symbol", Value: 1}}, Options: options.Index().SetUnique(true)},
			{Keys: bson.D{{Key: "sector", Value: 1}}},
		},
		"price_ticks": {{Keys: bson.D{{Key: "securityId", Value: 1}, {Key: "ts", Value: -1}}}},
		"portfolios":  {{Keys: bson.D{{Key: "owner", Value: 1}}}},
		"orders": {
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "createdAt", Value: 1}}},
			{Keys: bson.D{{Key: "portfolioId", Value: 1}}},
			{Keys: bson.D{{Key: "securityId", Value: 1}}},
		},
		"trades": {
			{Keys: bson.D{{Key: "ts", Value: -1}}},
			{Keys: bson.D{{Key: "securityId", Value: 1}}},
		},
		"holdings": {
			{Keys: bson.D{{Key: "portfolioId", Value: 1}, {Key: "securityId", Value: 1}},
				Options: options.Index().SetUnique(true)},
		},
		"events": {{Keys: bson.D{{Key: "seq", Value: 1}}}},
	}
	for coll, models := range idx {
		if _, err := s.db.Collection(coll).Indexes().CreateMany(ctx, models); err != nil {
			return fmt.Errorf("create indexes on %s: %w", coll, err)
		}
	}
	return nil
}

func (s *mongoStore) Wipe(ctx context.Context) error {
	for _, c := range mongoCollections {
		if _, err := s.db.Collection(c).DeleteMany(ctx, bson.M{}); err != nil {
			return fmt.Errorf("empty %s: %w", c, err)
		}
	}
	return nil
}

// DropSchema drops this app's collections and leaves the database itself in
// place — same reasoning as the SQL stores, which see.
func (s *mongoStore) DropSchema(ctx context.Context) error {
	for _, c := range mongoCollections {
		if err := s.db.Collection(c).Drop(ctx); err != nil {
			return fmt.Errorf("drop %s: %w", c, err)
		}
	}
	return nil
}

func (s *mongoStore) Objects(ctx context.Context) ([]ObjectInfo, error) {
	existing, err := s.db.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, n := range existing {
		present[n] = true
	}
	var out []ObjectInfo
	for _, name := range mongoCollections {
		if !present[name] {
			continue
		}
		var stats struct {
			Count      int64 `bson:"count"`
			Size       int64 `bson:"size"`
			TotalIndex int64 `bson:"totalIndexSize"`
		}
		// collStats is deprecated in favour of $collStats but remains the only
		// one-call way to get count and size together on every supported
		// server; a failure here degrades to zeroes rather than losing the row.
		s.db.RunCommand(ctx, bson.D{{Key: "collStats", Value: name}}).Decode(&stats)
		out = append(out, ObjectInfo{
			Name: name, Kind: "collection",
			Rows: stats.Count, Bytes: stats.Size + stats.TotalIndex,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------- documents

// The BSON documents mirror the Go structs, with _id carrying the app's own
// string id. Field names use the same camelCase as the JSON tags so a document
// read straight out of the shell is recognisable next to the API's output.

type mongoSecurity struct {
	ID        string    `bson:"_id"`
	Symbol    string    `bson:"symbol"`
	Name      string    `bson:"name"`
	Sector    string    `bson:"sector"`
	Currency  string    `bson:"currency"`
	Shares    int64     `bson:"sharesOutstanding"`
	OpenPrice float64   `bson:"openPrice"`
	LastPrice float64   `bson:"lastPrice"`
	DayHigh   float64   `bson:"dayHigh"`
	DayLow    float64   `bson:"dayLow"`
	DayVolume int64     `bson:"dayVolume"`
	Listed    bool      `bson:"listed"`
	CreatedAt time.Time `bson:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt"`
}

func (m mongoSecurity) toSecurity() Security {
	return Security{
		ID: m.ID, Symbol: m.Symbol, Name: m.Name, Sector: m.Sector, Currency: m.Currency,
		Shares: m.Shares, OpenPrice: m.OpenPrice, LastPrice: m.LastPrice,
		DayHigh: m.DayHigh, DayLow: m.DayLow, DayVolume: m.DayVolume, Listed: m.Listed,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

type mongoPortfolio struct {
	ID        string    `bson:"_id"`
	Name      string    `bson:"name"`
	Owner     string    `bson:"owner"`
	Cash      float64   `bson:"cash"`
	CreatedAt time.Time `bson:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt"`
}

type mongoOrder struct {
	ID          string     `bson:"_id"`
	PortfolioID string     `bson:"portfolioId"`
	SecurityID  string     `bson:"securityId"`
	Symbol      string     `bson:"symbol"`
	Owner       string     `bson:"owner"`
	Side        string     `bson:"side"`
	OrderType   string     `bson:"orderType"`
	Quantity    int64      `bson:"quantity"`
	LimitPrice  float64    `bson:"limitPrice"`
	Status      string     `bson:"status"`
	CreatedAt   time.Time  `bson:"createdAt"`
	FilledAt    *time.Time `bson:"filledAt,omitempty"`
}

func (m mongoOrder) toOrder() Order {
	return Order{
		ID: m.ID, PortfolioID: m.PortfolioID, SecurityID: m.SecurityID, Symbol: m.Symbol,
		Owner: m.Owner, Side: m.Side, OrderType: m.OrderType, Quantity: m.Quantity,
		LimitPrice: m.LimitPrice, Status: m.Status, CreatedAt: m.CreatedAt, FilledAt: m.FilledAt,
	}
}

// mongoRegex builds a case-insensitive contains-match, escaping the term so a
// user searching for "." or "*" gets a literal match rather than a wildcard.
func mongoRegex(term string) primitive.Regex {
	var b strings.Builder
	for _, r := range term {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return primitive.Regex{Pattern: b.String(), Options: "i"}
}

func mongoNotFound(err error) bool { return errors.Is(err, mongo.ErrNoDocuments) }

func mongoIsDuplicate(err error) bool {
	return mongo.IsDuplicateKeyError(err)
}

// ---------------------------------------------------------------- securities

func (s *mongoStore) ListSecurities(ctx context.Context, q ListQuery) ([]Security, int, error) {
	filter := bson.M{}
	if t := strings.TrimSpace(q.Search); t != "" {
		filter["$or"] = []bson.M{
			{"symbol": mongoRegex(t)}, {"name": mongoRegex(t)},
		}
	}
	if f := strings.TrimSpace(q.Filter); f != "" {
		filter["sector"] = f
	}
	total, err := s.db.Collection("securities").CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "symbol", Value: 1}}).
		SetLimit(int64(clampLimit(q.Limit, 50, 500))).
		SetSkip(int64(max0(q.Offset)))
	cur, err := s.db.Collection("securities").Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)
	out := []Security{}
	for cur.Next(ctx) {
		var m mongoSecurity
		if err := cur.Decode(&m); err != nil {
			return nil, 0, err
		}
		out = append(out, m.toSecurity())
	}
	return out, int(total), cur.Err()
}

func (s *mongoStore) GetSecurity(ctx context.Context, id string) (Security, error) {
	var m mongoSecurity
	err := s.db.Collection("securities").FindOne(ctx, bson.M{"_id": id}).Decode(&m)
	if mongoNotFound(err) {
		return Security{}, ErrNotFound
	}
	return m.toSecurity(), err
}

func (s *mongoStore) CreateSecurity(ctx context.Context, sec Security) (Security, error) {
	now := time.Now().UTC()
	sec.ID, sec.CreatedAt, sec.UpdatedAt = newID(), now, now
	if sec.DayLow == 0 {
		sec.DayLow = sec.LastPrice
	}
	if sec.DayHigh == 0 {
		sec.DayHigh = sec.LastPrice
	}
	_, err := s.db.Collection("securities").InsertOne(ctx, mongoSecurity{
		ID: sec.ID, Symbol: sec.Symbol, Name: sec.Name, Sector: sec.Sector,
		Currency: sec.Currency, Shares: sec.Shares, OpenPrice: sec.OpenPrice,
		LastPrice: sec.LastPrice, DayHigh: sec.DayHigh, DayLow: sec.DayLow,
		DayVolume: sec.DayVolume, Listed: sec.Listed,
		CreatedAt: sec.CreatedAt, UpdatedAt: sec.UpdatedAt,
	})
	if mongoIsDuplicate(err) {
		return Security{}, fmt.Errorf("symbol %s: %w", sec.Symbol, ErrConflict)
	}
	return sec, err
}

func (s *mongoStore) UpdateSecurity(ctx context.Context, sec Security) (Security, error) {
	res, err := s.db.Collection("securities").UpdateOne(ctx, bson.M{"_id": sec.ID}, bson.M{
		"$set": bson.M{
			"symbol": sec.Symbol, "name": sec.Name, "sector": sec.Sector,
			"currency": sec.Currency, "sharesOutstanding": sec.Shares,
			"openPrice": sec.OpenPrice, "listed": sec.Listed,
			"updatedAt": time.Now().UTC(),
		},
	})
	if mongoIsDuplicate(err) {
		return Security{}, fmt.Errorf("symbol %s: %w", sec.Symbol, ErrConflict)
	}
	if err != nil {
		return Security{}, err
	}
	if res.MatchedCount == 0 {
		return Security{}, ErrNotFound
	}
	return s.GetSecurity(ctx, sec.ID)
}

func (s *mongoStore) DeleteSecurity(ctx context.Context, id string) error {
	res, err := s.db.Collection("securities").DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	// Trades are kept deliberately — see the MySQL store's DeleteSecurity.
	s.db.Collection("price_ticks").DeleteMany(ctx, bson.M{"securityId": id})
	s.db.Collection("holdings").DeleteMany(ctx, bson.M{"securityId": id})
	s.db.Collection("orders").DeleteMany(ctx, bson.M{"securityId": id, "status": OrderOpen})
	return nil
}

// ---------------------------------------------------------------- portfolios

func (s *mongoStore) ListPortfolios(ctx context.Context, q ListQuery) ([]Portfolio, int, error) {
	filter := bson.M{}
	if t := strings.TrimSpace(q.Search); t != "" {
		filter["$or"] = []bson.M{{"name": mongoRegex(t)}, {"owner": mongoRegex(t)}}
	}
	if f := strings.TrimSpace(q.Filter); f != "" {
		filter["owner"] = f
	}
	total, err := s.db.Collection("portfolios").CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "owner", Value: 1}, {Key: "name", Value: 1}}).
		SetLimit(int64(clampLimit(q.Limit, 50, 500))).
		SetSkip(int64(max0(q.Offset)))
	cur, err := s.db.Collection("portfolios").Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)
	out := []Portfolio{}
	for cur.Next(ctx) {
		var m mongoPortfolio
		if err := cur.Decode(&m); err != nil {
			return nil, 0, err
		}
		out = append(out, Portfolio{ID: m.ID, Name: m.Name, Owner: m.Owner,
			Cash: m.Cash, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt})
	}
	return out, int(total), cur.Err()
}

func (s *mongoStore) GetPortfolio(ctx context.Context, id string) (Portfolio, error) {
	var m mongoPortfolio
	err := s.db.Collection("portfolios").FindOne(ctx, bson.M{"_id": id}).Decode(&m)
	if mongoNotFound(err) {
		return Portfolio{}, ErrNotFound
	}
	return Portfolio{ID: m.ID, Name: m.Name, Owner: m.Owner, Cash: m.Cash,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}, err
}

func (s *mongoStore) CreatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error) {
	now := time.Now().UTC()
	p.ID, p.CreatedAt, p.UpdatedAt = newID(), now, now
	_, err := s.db.Collection("portfolios").InsertOne(ctx, mongoPortfolio{
		ID: p.ID, Name: p.Name, Owner: p.Owner, Cash: p.Cash,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	})
	return p, err
}

func (s *mongoStore) UpdatePortfolio(ctx context.Context, p Portfolio) (Portfolio, error) {
	res, err := s.db.Collection("portfolios").UpdateOne(ctx, bson.M{"_id": p.ID}, bson.M{
		"$set": bson.M{"name": p.Name, "owner": p.Owner, "cash": p.Cash,
			"updatedAt": time.Now().UTC()},
	})
	if err != nil {
		return Portfolio{}, err
	}
	if res.MatchedCount == 0 {
		return Portfolio{}, ErrNotFound
	}
	return s.GetPortfolio(ctx, p.ID)
}

func (s *mongoStore) DeletePortfolio(ctx context.Context, id string) error {
	res, err := s.db.Collection("portfolios").DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	s.db.Collection("holdings").DeleteMany(ctx, bson.M{"portfolioId": id})
	s.db.Collection("orders").DeleteMany(ctx, bson.M{"portfolioId": id, "status": OrderOpen})
	return nil
}

// -------------------------------------------------------------------- orders

func (s *mongoStore) ListOrders(ctx context.Context, q ListQuery) ([]Order, int, error) {
	filter := bson.M{}
	if t := strings.TrimSpace(q.Search); t != "" {
		filter["$or"] = []bson.M{{"symbol": mongoRegex(t)}, {"owner": mongoRegex(t)}}
	}
	if f := strings.TrimSpace(q.Filter); f != "" {
		filter["status"] = f
	}
	total, err := s.db.Collection("orders").CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: -1}}).
		SetLimit(int64(clampLimit(q.Limit, 50, 500))).
		SetSkip(int64(max0(q.Offset)))
	cur, err := s.db.Collection("orders").Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, err
	}
	defer cur.Close(ctx)
	out := []Order{}
	for cur.Next(ctx) {
		var m mongoOrder
		if err := cur.Decode(&m); err != nil {
			return nil, 0, err
		}
		out = append(out, m.toOrder())
	}
	return out, int(total), cur.Err()
}

func (s *mongoStore) GetOrder(ctx context.Context, id string) (Order, error) {
	var m mongoOrder
	err := s.db.Collection("orders").FindOne(ctx, bson.M{"_id": id}).Decode(&m)
	if mongoNotFound(err) {
		return Order{}, ErrNotFound
	}
	return m.toOrder(), err
}

func (s *mongoStore) CreateOrder(ctx context.Context, o Order) (Order, error) {
	o.ID = newID()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	if o.Status == "" {
		o.Status = OrderOpen
	}
	_, err := s.db.Collection("orders").InsertOne(ctx, mongoOrder{
		ID: o.ID, PortfolioID: o.PortfolioID, SecurityID: o.SecurityID, Symbol: o.Symbol,
		Owner: o.Owner, Side: o.Side, OrderType: o.OrderType, Quantity: o.Quantity,
		LimitPrice: o.LimitPrice, Status: o.Status, CreatedAt: o.CreatedAt,
	})
	return o, err
}

func (s *mongoStore) UpdateOrder(ctx context.Context, o Order) (Order, error) {
	res, err := s.db.Collection("orders").UpdateOne(ctx, bson.M{"_id": o.ID}, bson.M{
		"$set": bson.M{"side": o.Side, "orderType": o.OrderType, "quantity": o.Quantity,
			"limitPrice": o.LimitPrice, "status": o.Status},
	})
	if err != nil {
		return Order{}, err
	}
	if res.MatchedCount == 0 {
		return Order{}, ErrNotFound
	}
	return s.GetOrder(ctx, o.ID)
}

func (s *mongoStore) DeleteOrder(ctx context.Context, id string) error {
	res, err := s.db.Collection("orders").DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// --------------------------------------------------------- simulation writes

func (s *mongoStore) AppendTicks(ctx context.Context, ticks []Tick) error {
	if len(ticks) == 0 {
		return nil
	}
	docs := make([]any, 0, len(ticks))
	for _, t := range ticks {
		docs = append(docs, bson.M{
			"_id": newID(), "securityId": t.SecurityID, "symbol": t.Symbol,
			"ts": t.TS, "price": t.Price, "volume": t.Volume,
		})
	}
	_, err := s.db.Collection("price_ticks").InsertMany(ctx, docs)
	return err
}

func (s *mongoStore) RecentTicks(ctx context.Context, securityID string, limit int) ([]Tick, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "ts", Value: -1}}).
		SetLimit(int64(clampLimit(limit, 60, 1000)))
	cur, err := s.db.Collection("price_ticks").Find(ctx, bson.M{"securityId": securityID}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Tick{}
	for cur.Next(ctx) {
		var t struct {
			ID         string    `bson:"_id"`
			SecurityID string    `bson:"securityId"`
			Symbol     string    `bson:"symbol"`
			TS         time.Time `bson:"ts"`
			Price      float64   `bson:"price"`
			Volume     int64     `bson:"volume"`
		}
		if err := cur.Decode(&t); err != nil {
			return nil, err
		}
		out = append(out, Tick{ID: t.ID, SecurityID: t.SecurityID, Symbol: t.Symbol,
			TS: t.TS, Price: t.Price, Volume: t.Volume})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, cur.Err()
}

// ApplyQuotes uses $max/$min for the session high and low so concurrent
// updates cannot lose an extreme, and touches only the price fields — a
// concurrent CRUD edit to a name or sector survives.
func (s *mongoStore) ApplyQuotes(ctx context.Context, quotes []Quote) error {
	if len(quotes) == 0 {
		return nil
	}
	now := time.Now().UTC()
	models := make([]mongo.WriteModel, 0, len(quotes))
	for _, q := range quotes {
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": q.SecurityID}).
			SetUpdate(bson.M{
				"$set": bson.M{"lastPrice": q.Price, "updatedAt": now},
				"$max": bson.M{"dayHigh": q.Price},
				"$inc": bson.M{"dayVolume": q.Volume},
			}))
	}
	if _, err := s.db.Collection("securities").
		BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false)); err != nil {
		return err
	}
	// dayLow needs a conditional: $min would clamp against the seeded 0 and
	// pin every low to zero forever, so it is applied only where the stored
	// low is 0 (unset) or genuinely higher than this quote.
	lows := make([]mongo.WriteModel, 0, len(quotes))
	for _, q := range quotes {
		lows = append(lows, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": q.SecurityID, "$or": []bson.M{
				{"dayLow": 0}, {"dayLow": bson.M{"$gt": q.Price}},
			}}).
			SetUpdate(bson.M{"$set": bson.M{"dayLow": q.Price}}))
	}
	_, err := s.db.Collection("securities").BulkWrite(ctx, lows, options.BulkWrite().SetOrdered(false))
	return err
}

func (s *mongoStore) OpenOrders(ctx context.Context, limit int) ([]Order, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "createdAt", Value: 1}}).
		SetLimit(int64(clampLimit(limit, 100, 1000)))
	cur, err := s.db.Collection("orders").Find(ctx, bson.M{"status": OrderOpen}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Order{}
	for cur.Next(ctx) {
		var m mongoOrder
		if err := cur.Decode(&m); err != nil {
			return nil, err
		}
		out = append(out, m.toOrder())
	}
	return out, cur.Err()
}

// RecordFill settles an order without a transaction, because a standalone
// mongod has none. The order is moved to filled FIRST, conditionally on it
// still being open — that compare-and-set is what prevents a double fill, and
// it is the step whose failure is least damaging: if the process died
// immediately after, the order would read as filled with no trade behind it,
// which is visible and harmless, whereas the reverse order could pay out the
// same order twice. The remaining writes are individually idempotent-safe.
func (s *mongoStore) RecordFill(ctx context.Context, o Order, t Trade) error {
	res, err := s.db.Collection("orders").UpdateOne(ctx,
		bson.M{"_id": o.ID, "status": OrderOpen},
		bson.M{"$set": bson.M{"status": OrderFilled, "filledAt": t.TS}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return nil // already moved by someone else
	}

	if _, err := s.db.Collection("trades").InsertOne(ctx, bson.M{
		"_id": newID(), "orderId": o.ID, "portfolioId": o.PortfolioID,
		"securityId": o.SecurityID, "symbol": o.Symbol, "side": o.Side,
		"quantity": t.Quantity, "price": t.Price, "ts": t.TS,
	}); err != nil {
		return err
	}

	signedQty := t.Quantity
	cashDelta := -t.Notional()
	if o.Side == SideSell {
		signedQty = -t.Quantity
		cashDelta = t.Notional()
	}

	// Without a transaction the weighted average has to be computed read-then-
	// write. The read is cheap and the only writer of a given (portfolio,
	// security) pair in practice is the single match agent, so the race is
	// theoretical here; it is called out rather than hidden.
	holdings := s.db.Collection("holdings")
	key := bson.M{"portfolioId": o.PortfolioID, "securityId": o.SecurityID}
	var cur struct {
		Quantity int64   `bson:"quantity"`
		AvgCost  float64 `bson:"avgCost"`
	}
	_ = holdings.FindOne(ctx, key).Decode(&cur)

	newQty := cur.Quantity + signedQty
	newAvg := cur.AvgCost
	switch {
	case newQty == 0:
		newAvg = 0
	case signedQty > 0:
		newAvg = (cur.AvgCost*float64(cur.Quantity) + t.Price*float64(signedQty)) / float64(newQty)
	}
	if _, err := holdings.UpdateOne(ctx, key, bson.M{
		"$set": bson.M{"symbol": o.Symbol, "quantity": newQty,
			"avgCost": newAvg, "updatedAt": t.TS},
	}, options.Update().SetUpsert(true)); err != nil {
		return err
	}

	_, err = s.db.Collection("portfolios").UpdateOne(ctx, bson.M{"_id": o.PortfolioID},
		bson.M{"$inc": bson.M{"cash": cashDelta}, "$set": bson.M{"updatedAt": t.TS}})
	return err
}

// ListHoldings joins by hand: Mongo has no joins worth using here, and both
// lookup collections are small enough (tens of rows) that reading them whole
// beats an aggregation pipeline for both speed and legibility.
func (s *mongoStore) ListHoldings(ctx context.Context, portfolioID string) ([]Holding, error) {
	owners := map[string]string{}
	if pfs, _, err := s.ListPortfolios(ctx, ListQuery{Limit: 500}); err == nil {
		for _, p := range pfs {
			owners[p.ID] = p.Owner
		}
	}
	prices := map[string]float64{}
	if secs, _, err := s.ListSecurities(ctx, ListQuery{Limit: 500}); err == nil {
		for _, sec := range secs {
			prices[sec.ID] = sec.LastPrice
		}
	}

	filter := bson.M{"quantity": bson.M{"$ne": 0}}
	if portfolioID != "" {
		filter["portfolioId"] = portfolioID
	}
	cur, err := s.db.Collection("holdings").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "symbol", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Holding{}
	for cur.Next(ctx) {
		var h struct {
			PortfolioID string    `bson:"portfolioId"`
			SecurityID  string    `bson:"securityId"`
			Symbol      string    `bson:"symbol"`
			Quantity    int64     `bson:"quantity"`
			AvgCost     float64   `bson:"avgCost"`
			UpdatedAt   time.Time `bson:"updatedAt"`
		}
		if err := cur.Decode(&h); err != nil {
			return nil, err
		}
		out = append(out, Holding{
			PortfolioID: h.PortfolioID, Owner: owners[h.PortfolioID],
			SecurityID: h.SecurityID, Symbol: h.Symbol, Quantity: h.Quantity,
			AvgCost: h.AvgCost, LastPrice: prices[h.SecurityID], UpdatedAt: h.UpdatedAt,
		})
	}
	return out, cur.Err()
}

func (s *mongoStore) CountOrdersByStatus(ctx context.Context) (map[string]int64, error) {
	cur, err := s.db.Collection("orders").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$status"},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := map[string]int64{}
	for cur.Next(ctx) {
		var row struct {
			ID string `bson:"_id"`
			N  int64  `bson:"n"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		out[row.ID] = row.N
	}
	return out, cur.Err()
}

func (s *mongoStore) RecentTrades(ctx context.Context, limit int) ([]Trade, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "ts", Value: -1}}).
		SetLimit(int64(clampLimit(limit, 50, 1000)))
	cur, err := s.db.Collection("trades").Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Trade{}
	for cur.Next(ctx) {
		var t struct {
			ID          string    `bson:"_id"`
			OrderID     string    `bson:"orderId"`
			PortfolioID string    `bson:"portfolioId"`
			SecurityID  string    `bson:"securityId"`
			Symbol      string    `bson:"symbol"`
			Side        string    `bson:"side"`
			Quantity    int64     `bson:"quantity"`
			Price       float64   `bson:"price"`
			TS          time.Time `bson:"ts"`
		}
		if err := cur.Decode(&t); err != nil {
			return nil, err
		}
		out = append(out, Trade{ID: t.ID, OrderID: t.OrderID, PortfolioID: t.PortfolioID,
			SecurityID: t.SecurityID, Symbol: t.Symbol, Side: t.Side,
			Quantity: t.Quantity, Price: t.Price, TS: t.TS})
	}
	return out, cur.Err()
}

func (s *mongoStore) TradeTotals(ctx context.Context) (int64, int64, error) {
	cur, err := s.db.Collection("trades").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "volume", Value: bson.D{{Key: "$sum", Value: "$quantity"}}},
		}}},
	})
	if err != nil {
		return 0, 0, err
	}
	defer cur.Close(ctx)
	if cur.Next(ctx) {
		var row struct {
			Count  int64 `bson:"count"`
			Volume int64 `bson:"volume"`
		}
		if err := cur.Decode(&row); err != nil {
			return 0, 0, err
		}
		return row.Count, row.Volume, nil
	}
	return 0, 0, cur.Err()
}

// ------------------------------------------------- metrics / state / agents

func (s *mongoStore) putBlob(ctx context.Context, coll, id string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.Collection(coll).UpdateOne(ctx, bson.M{"_id": id},
		bson.M{"$set": bson.M{"payload": string(b), "updatedAt": time.Now().UTC()}},
		options.Update().SetUpsert(true))
	return err
}

func (s *mongoStore) getBlob(ctx context.Context, coll, id string) (json.RawMessage, error) {
	var doc struct {
		Payload string `bson:"payload"`
	}
	err := s.db.Collection(coll).FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if mongoNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(doc.Payload), nil
}

func (s *mongoStore) PutMetrics(ctx context.Context, id string, payload any) error {
	return s.putBlob(ctx, "metrics", id, payload)
}
func (s *mongoStore) GetMetrics(ctx context.Context, id string) (json.RawMessage, error) {
	return s.getBlob(ctx, "metrics", id)
}
func (s *mongoStore) PutState(ctx context.Context, id string, payload any) error {
	return s.putBlob(ctx, "sim_state", id, payload)
}
func (s *mongoStore) GetState(ctx context.Context, id string) (json.RawMessage, error) {
	return s.getBlob(ctx, "sim_state", id)
}

func (s *mongoStore) Heartbeat(ctx context.Context, agent, status, detail string) error {
	now := time.Now().UTC()
	_, err := s.db.Collection("agents").UpdateOne(ctx, bson.M{"_id": agent},
		bson.M{"$set": bson.M{"status": status, "lastTick": now,
			"detail": truncate(detail, 255), "updatedAt": now}},
		options.Update().SetUpsert(true))
	return err
}

func (s *mongoStore) AllHeartbeats(ctx context.Context) ([]AgentHeartbeat, error) {
	cur, err := s.db.Collection("agents").Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []AgentHeartbeat
	for cur.Next(ctx) {
		var h struct {
			Agent     string    `bson:"_id"`
			Status    string    `bson:"status"`
			LastTick  time.Time `bson:"lastTick"`
			Detail    string    `bson:"detail"`
			UpdatedAt time.Time `bson:"updatedAt"`
		}
		if err := cur.Decode(&h); err != nil {
			return nil, err
		}
		out = append(out, AgentHeartbeat{Agent: h.Agent, Status: h.Status,
			LastTick: h.LastTick, Detail: h.Detail, UpdatedAt: h.UpdatedAt})
	}
	return out, cur.Err()
}

// AppendEvent assigns its own monotonic sequence number, because the event
// feed is read by a cursor and an ObjectId — while roughly ordered — is not a
// number the poller can compare with ">". The counter lives in sim_state, so
// it survives a restart and resets with a Wipe like everything else.
func (s *mongoStore) AppendEvent(ctx context.Context, e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	var counter struct {
		Seq int64 `bson:"seq"`
	}
	err := s.db.Collection("sim_state").FindOneAndUpdate(ctx,
		bson.M{"_id": "eventSeq"},
		bson.M{"$inc": bson.M{"seq": 1}},
		options.FindOneAndUpdate().SetUpsert(true).
			SetReturnDocument(options.After)).Decode(&counter)
	if err != nil {
		return err
	}
	_, err = s.db.Collection("events").InsertOne(ctx, bson.M{
		"_id": newID(), "seq": counter.Seq, "ts": e.TS,
		"kind": e.Kind, "symbol": e.Symbol, "message": truncate(e.Message, 512),
	})
	return err
}

func (s *mongoStore) EventsSince(ctx context.Context, afterID string, limit int) ([]Event, error) {
	after, _ := strconv.ParseInt(afterID, 10, 64)
	opts := options.Find().
		SetSort(bson.D{{Key: "seq", Value: 1}}).
		SetLimit(int64(clampLimit(limit, 50, 500)))
	cur, err := s.db.Collection("events").Find(ctx, bson.M{"seq": bson.M{"$gt": after}}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []Event
	for cur.Next(ctx) {
		var e struct {
			Seq     int64     `bson:"seq"`
			TS      time.Time `bson:"ts"`
			Kind    string    `bson:"kind"`
			Symbol  string    `bson:"symbol"`
			Message string    `bson:"message"`
		}
		if err := cur.Decode(&e); err != nil {
			return nil, err
		}
		out = append(out, Event{ID: strconv.FormatInt(e.Seq, 10), TS: e.TS,
			Kind: e.Kind, Symbol: e.Symbol, Message: e.Message})
	}
	return out, cur.Err()
}

func (s *mongoStore) ReportData(ctx context.Context, limitTrades int) (Report, error) {
	return reportFrom(ctx, s, limitTrades)
}
