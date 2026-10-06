package sim

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"supportsim/internal/corpus"
	"supportsim/internal/search"
	"supportsim/internal/store"
)

// mongoBackend is the desk on MongoDB: documents with an embedding field, searched
// through mongot ($vectorSearch, $search, $rankFusion / $scoreFusion).
type mongoBackend struct {
	St     *store.Store
	search *search.Searcher
	mirror *search.Mirror
	ws     *mongoWorkshop
}

// NewMongoBackend wraps a store.
func NewMongoBackend(st *store.Store, mirror *search.Mirror) Backend {
	b := &mongoBackend{St: st, mirror: mirror, search: search.NewSearcher(st, mirror)}
	b.ws = &mongoWorkshop{St: st}
	return b
}

func (b *mongoBackend) Engine() string                            { return "mongodb" }
func (b *mongoBackend) Info(ctx context.Context) store.ServerInfo { return b.St.Info(ctx) }
func (b *mongoBackend) Prepare(ctx context.Context) error         { return b.St.EnsureCollections(ctx) }
func (b *mongoBackend) Searcher() Searcher                        { return b.search }
func (b *mongoBackend) Workshop() WorkshopBackend                 { return b.ws }

func (b *mongoBackend) SaveArticle(ctx context.Context, a corpus.Article, vec []float32) error {
	doc := bson.D{{Key: "_id", Value: a.Slug}, {Key: "title", Value: a.Title}, {Key: "team", Value: a.Team},
		{Key: "summary", Value: a.Summary}, {Key: "steps", Value: a.Steps}, {Key: "embedding", Value: vec}}
	_, err := b.St.C(store.KB).ReplaceOne(ctx, bson.M{"_id": a.Slug}, doc, options.Replace().SetUpsert(true))
	return err
}

func (b *mongoBackend) TicketCount(ctx context.Context) (int64, error) {
	return b.St.C(store.Tickets).CountDocuments(ctx, bson.M{})
}

func (b *mongoBackend) NextTicketNumber(ctx context.Context) (int64, error) {
	return b.St.NextTicketNumber(ctx)
}

func ticketDoc(r TicketRecord) bson.D {
	t := r.T
	d := bson.D{
		{Key: "_id", Value: r.ID}, {Key: "subject", Value: t.Subject}, {Key: "body", Value: t.Body},
		{Key: "customer", Value: t.Customer}, {Key: "plan", Value: t.Plan}, {Key: "createdAt", Value: r.Created},
		{Key: "status", Value: r.Status}, {Key: "team", Value: r.Team},
	}
	if r.ResolvedKB != "" {
		d = append(d, bson.E{Key: "resolvedKB", Value: r.ResolvedKB}, bson.E{Key: "resolvedBy", Value: r.ResolvedBy}, bson.E{Key: "resolvedAt", Value: r.Created.Add(2 * time.Hour)})
	}
	if r.SuggestedKB != "" {
		d = append(d, bson.E{Key: "suggestedKB", Value: r.SuggestedKB}, bson.E{Key: "suggestedCosine", Value: r.SuggestedCos})
	}
	if r.DuplicateOf != nil {
		d = append(d, bson.E{Key: "duplicateOf", Value: r.DuplicateOf})
	}
	return append(d,
		bson.E{Key: "truth", Value: bson.D{{Key: "archetype", Value: t.Archetype}, {Key: "team", Value: t.TruthTeam}, {Key: "kb", Value: t.TruthKB}}},
		bson.E{Key: "embedding", Value: r.Vec},
	)
}

func (b *mongoBackend) InsertTickets(ctx context.Context, recs []TicketRecord) error {
	docs := make([]any, len(recs))
	for i, r := range recs {
		docs[i] = ticketDoc(r)
	}
	if len(docs) == 1 {
		_, err := b.St.C(store.Tickets).InsertOne(ctx, docs[0])
		return err
	}
	_, err := b.St.C(store.Tickets).InsertMany(ctx, docs)
	return err
}

type ticketRow struct {
	ID         int64     `bson:"_id"`
	Subject    string    `bson:"subject"`
	Team       string    `bson:"team"`
	Status     string    `bson:"status"`
	Plan       string    `bson:"plan"`
	Customer   string    `bson:"customer"`
	ResolvedKB string    `bson:"resolvedKB"`
	CreatedAt  time.Time `bson:"createdAt"`
	Embedding  []float32 `bson:"embedding"`
}

func (r ticketRow) doc() *search.Doc {
	return &search.Doc{ID: r.ID, Title: r.Subject, Team: r.Team, Status: r.Status, KB: r.ResolvedKB, Plan: r.Plan, Customer: r.Customer,
		CreatedAt: r.CreatedAt, Vec: r.Embedding}
}

func (b *mongoBackend) LoadTickets(ctx context.Context, put func(*search.Doc)) error {
	cur, err := b.St.C(store.Tickets).Find(ctx, bson.M{"status": bson.M{"$ne": "probe"}})
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var d ticketRow
		if cur.Decode(&d) == nil {
			put(d.doc())
		}
	}
	return cur.Err()
}

func (b *mongoBackend) UpdateTicket(ctx context.Context, id any, s TicketSet) error {
	set := bson.M{}
	if s.Status != "" {
		set["status"] = s.Status
	}
	if s.Team != "" {
		set["team"] = s.Team
	}
	if s.ResolvedKB != "" {
		set["resolvedKB"] = s.ResolvedKB
	}
	if s.Resolved {
		set["resolvedAt"] = time.Now()
	}
	if s.Incident != 0 {
		set["incident"] = s.Incident
	}
	_, err := b.St.C(store.Tickets).UpdateByID(ctx, id, bson.M{"$set": set})
	return err
}

func (b *mongoBackend) TicketArchetype(ctx context.Context, id any) string {
	var prev struct {
		Truth struct {
			Archetype string `bson:"archetype"`
		} `bson:"truth"`
	}
	if b.St.C(store.Tickets).FindOne(ctx, bson.M{"_id": id}).Decode(&prev) != nil {
		return ""
	}
	return prev.Truth.Archetype
}

// Prune deletes the oldest resolved tickets beyond keep (mongot follows deletes too).
func (b *mongoBackend) Prune(ctx context.Context, keep int64) []any {
	n, err := b.St.C(store.Tickets).CountDocuments(ctx, bson.M{})
	if err != nil || n <= keep {
		return nil
	}
	cur, err := b.St.C(store.Tickets).Find(ctx, bson.M{"status": "resolved"},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}).SetLimit(n-keep).SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil
	}
	var ids []any
	for cur.Next(ctx) {
		var r struct {
			ID int64 `bson:"_id"`
		}
		if cur.Decode(&r) == nil {
			ids = append(ids, r.ID)
		}
	}
	cur.Close(ctx)
	if len(ids) == 0 {
		return nil
	}
	if _, err := b.St.C(store.Tickets).DeleteMany(ctx, bson.M{"_id": bson.M{"$in": ids}}); err != nil {
		return nil
	}
	return ids
}

// EnsureSearch creates the four search indexes and reports whether mongot has made
// them queryable.
func (b *mongoBackend) EnsureSearch(ctx context.Context) SearchState {
	err := b.St.EnsureSearchIndexes(ctx)
	if errors.Is(err, store.ErrNoSearch) || store.IsNoSearch(err) {
		return SearchState{Err: "This cluster has no mongot, so it cannot create search indexes. The desk runs on an in-app brute-force scan instead — turn on Vector search on the MongoDB frame (PSMDB 8.3+) to use $vectorSearch."}
	}
	st := SearchState{}
	if err != nil {
		st.Err = err.Error()
	}
	ix, serr := b.St.SearchIndexStatus(ctx)
	if serr != nil {
		st.Err = serr.Error()
		return st
	}
	st.Indexes = ix
	st.Ready = len(ix) > 0
	for _, s := range ix {
		st.Ready = st.Ready && s.Queryable
	}
	if !st.Ready {
		st.Building = "mongot is building the search indexes: " + summarize(ix)
	}
	return st
}

// Freshness inserts a probe ticket and polls $vectorSearch until it comes back. That
// gap is mongot reading the insert off the change stream and adding it to its index
// — invisible in mongod, and the reason search is "eventually consistent".
func (b *mongoBackend) Freshness(ctx context.Context, v []float32, text string) Freshness {
	id := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	out := Freshness{Engine: "mongot", Text: text}
	t0 := time.Now()
	_, err := b.St.C(store.Tickets).InsertOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "subject", Value: text}, {Key: "status", Value: "probe"},
		{Key: "createdAt", Value: time.Now()}, {Key: "embedding", Value: v}})
	out.InsertMs = msSince(t0)
	if err != nil {
		out.Text = "insert failed: " + err.Error()
		return out
	}
	defer b.St.C(store.Tickets).DeleteOne(context.Background(), bson.M{"_id": id})
	q := search.VectorQuery{Collection: store.Tickets, Vector: v, K: 1, NumCandidates: 20, Filter: search.Filter{Status: []string{"probe"}}}
	out.Pipeline = search.Shell(store.Tickets, search.VectorPipeline(q))
	if !b.search.Ready() {
		out.Engine, out.Found = "app", true
		return out
	}
	t1 := time.Now()
	for time.Since(t1) < 30*time.Second && ctx.Err() == nil {
		out.Attempts++
		r := b.search.Vector(ctx, q)
		if len(r.Hits) > 0 && fmt.Sprint(r.Hits[0].ID) == id {
			out.Found = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	out.FoundMs = msSince(t1)
	return out
}
