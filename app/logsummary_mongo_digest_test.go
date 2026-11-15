package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func lsTestRecord(t *testing.T, line string) lsMongoRecord {
	t.Helper()
	var raw struct {
		Attr map[string]json.RawMessage `json:"attr"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatal(err)
	}
	return lsMongoRecord{ID: 51803, Attr: raw.Attr}
}

// Two finds that differ only in their values are one shape; a different field is another.
func TestMongoShapeIgnoresLiterals(t *testing.T) {
	a := lsTestRecord(t, `{"attr":{"type":"command","ns":"shop.orders","command":{"find":"orders","filter":{"status":"open","total":{"$gt":10}},"sort":{"ts":-1},"lsid":{"id":1},"$db":"shop","batchSize":5}}}`)
	b := lsTestRecord(t, `{"attr":{"type":"command","ns":"shop.orders","command":{"find":"orders","filter":{"total":{"$gt":99.5},"status":"closed"},"sort":{"ts":1},"$db":"shop","batchSize":500}}}`)
	c := lsTestRecord(t, `{"attr":{"type":"command","ns":"shop.orders","command":{"find":"orders","filter":{"customer":{"$oid":"64b7f0c2e1d3a1b2c3d4e5f6"}},"$db":"shop"}}}`)
	opA, shA, sampleA, intA := lsMongoShape(a)
	_, shB, _, _ := lsMongoShape(b)
	_, shC, _, _ := lsMongoShape(c)
	if opA != "find" || intA {
		t.Fatalf("op %q internal %v", opA, intA)
	}
	if shA != shB {
		t.Errorf("same query, two shapes:\n%s\n%s", shA, shB)
	}
	if shA == shC {
		t.Errorf("different predicate, one shape: %s", shA)
	}
	if strings.Contains(shA, "open") || strings.Contains(shA, "10") || strings.Contains(shA, "orders") {
		t.Errorf("literal leaked into the shape: %s", shA)
	}
	if shC != "{filter: {customer: ?}}" {
		t.Errorf("extended JSON not collapsed: %s", shC)
	}
	if strings.Contains(sampleA, "lsid") || !strings.Contains(sampleA, "open") {
		t.Errorf("sample should keep values and drop the session: %s", sampleA)
	}
}

// The oplog fetcher's tailing getMore waits by design and must not rank as a slow query.
func TestMongoShapeMarksTailingCursorsInternal(t *testing.T) {
	r := lsTestRecord(t, `{"attr":{"type":"command","ns":"local.oplog.rs","appName":"OplogFetcher","command":{"getMore":1,"collection":"oplog.rs"},"originatingCommand":{"find":"oplog.rs","filter":{"ts":{"$gte":{"$timestamp":{"t":1,"i":1}}}},"tailable":true,"awaitData":true}}}`)
	op, shape, _, internal := lsMongoShape(r)
	if op != "getMore:find" || !internal || shape != "{filter: {ts: {$gte: ?}}}" {
		t.Errorf("op %q shape %q internal %v", op, shape, internal)
	}
}

func TestMongoDigestOnWorkload(t *testing.T) {
	b := lsLoadScenario(t, "m07-workload")
	seen := 0
	for _, s := range b.Sources {
		if s.Mongo == nil || s.Mongo.Ops == 0 {
			continue
		}
		seen++
		d := s.Mongo.Digest
		if len(d) == 0 {
			t.Fatalf("%s: %d slow ops and an empty digest", s.Name, s.Mongo.Ops)
		}
		var ops int
		var prev int64 = -1
		for i, sh := range d {
			ops += sh.Ops
			if sh.Internal {
				prev = -1 // the internal block is ranked on its own
				continue
			}
			if prev >= 0 && sh.Millis > prev {
				t.Errorf("%s: row %d (%d ms) above row %d (%d ms) — not worst first", s.Name, i, sh.Millis, i-1, prev)
			}
			prev = sh.Millis
			var h int64
			for _, p := range sh.Hist {
				h += p[1]
			}
			if h != int64(sh.Ops) {
				t.Errorf("%s: %s histogram holds %d of %d ops", s.Name, sh.Key, h, sh.Ops)
			}
			if sh.MaxMs*int64(sh.Ops) < sh.Millis {
				t.Errorf("%s: %s max %d ms × %d ops < total %d ms", s.Name, sh.Key, sh.MaxMs, sh.Ops, sh.Millis)
			}
		}
		if ops > s.Mongo.Ops {
			t.Errorf("%s: digest holds %d ops, the scan saw %d", s.Name, ops, s.Mongo.Ops)
		}
	}
	if seen == 0 {
		t.Fatal("no source carried slow queries")
	}
}

// A pipeline is spelled stage by stage; a list of values is one element whatever its length.
func TestMongoShapeSpellsPipelines(t *testing.T) {
	r := lsTestRecord(t, `{"attr":{"type":"command","ns":"shop.orders","command":{"aggregate":"orders","pipeline":[{"$match":{"sku":{"$in":[1,2,3,4]}}},{"$group":{"_id":"$sku","n":{"$sum":1}}}],"cursor":{}}}}`)
	_, shape, _, _ := lsMongoShape(r)
	want := "{pipeline: [{$match: {sku: {$in: [?]}}}, {$group: {_id: ?, n: {$sum: ?}}}]}"
	if shape != want {
		t.Errorf("got  %s\nwant %s", shape, want)
	}
}
