package sim

import (
	"context"
	"encoding/json"
	"log"

	"carsim/internal/store"
)

// explainNode mirrors just the fields this app cares about from PostgreSQL's
// EXPLAIN (FORMAT JSON) plan tree — a nested structure, since a real query plan
// wraps the scan node in aggregate/sort/limit nodes above it.
type explainNode struct {
	NodeType  string        `json:"Node Type"`
	IndexName string        `json:"Index Name"`
	PlanRows  float64       `json:"Plan Rows"`
	Plans     []explainNode `json:"Plans"`
}

type explainPlanRow struct {
	Plan explainNode `json:"Plan"`
}

// findScanNode walks the plan tree depth-first for the first node whose type
// contains "Scan" — the actual access-method decision this app's
// query-education panel cares about (Index Scan / Index Only Scan / Bitmap
// Index Scan vs Seq Scan), skipping over wrapping GroupAggregate/Sort/Limit
// nodes a GROUP BY query like the region scatter-search adds on top.
func findScanNode(n explainNode) (explainNode, bool) {
	if containsScan(n.NodeType) {
		return n, true
	}
	for _, child := range n.Plans {
		if found, ok := findScanNode(child); ok {
			return found, true
		}
	}
	return explainNode{}, false
}

func containsScan(nodeType string) bool {
	for _, suffix := range []string{"Scan"} {
		if len(nodeType) >= len(suffix) && nodeType[len(nodeType)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}

// verifyExplain runs EXPLAIN (FORMAT JSON) over the just-executed query (pgx
// prepares EXPLAIN statements over the server's extended protocol the same as
// any other statement, so the $N placeholders and args work unchanged) and
// records a QuerySample carrying the real access method PostgreSQL chose — the
// "verified" query-education sample, sampled at Profile.ExplainRate because
// explaining a query is itself extra server work and shouldn't distort the
// throughput being measured. This is Airline Sim's/Hotel Sim's targeted-vs-
// scatter explain panel, reframed once more around indexing: a targeted lookup
// should show an Index Scan against idx_ri_location_date; the deliberate
// region-only browse shows a much wider Index Scan (or a Seq Scan) against
// idx_ri_region_date across every location in that region.
func (e *Engine) verifyExplain(ctx context.Context, query string, args []any, kind string, ms float64) {
	rows, err := e.Store.DB.QueryContext(ctx, "EXPLAIN (FORMAT JSON) "+query, args...)
	if err != nil {
		log.Printf("carsim: explain: %v", err)
		e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: kind, SQLText: query, DurationMs: ms})
		return
	}
	defer rows.Close()
	if !rows.Next() {
		e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: kind, SQLText: query, DurationMs: ms})
		return
	}
	var raw string
	if rows.Scan(&raw) != nil {
		e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: kind, SQLText: query, DurationMs: ms})
		return
	}
	var planRows []explainPlanRow
	if err := json.Unmarshal([]byte(raw), &planRows); err != nil || len(planRows) == 0 {
		e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: kind, SQLText: query, DurationMs: ms})
		return
	}
	scan, found := findScanNode(planRows[0].Plan)
	if !found {
		e.Store.RecordQuerySample(ctx, store.QuerySample{Kind: kind, SQLText: query, DurationMs: ms, RowsExamined: int64(planRows[0].Plan.PlanRows)})
		return
	}
	indexUsed := scan.IndexName
	if indexUsed == "" {
		indexUsed = "(none: " + scan.NodeType + ")"
	}
	e.Store.RecordQuerySample(ctx, store.QuerySample{
		Kind: kind, SQLText: query, DurationMs: ms, RowsExamined: int64(scan.PlanRows), IndexUsed: indexUsed,
	})
}
