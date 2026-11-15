package core

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSets is an Engine with table sets held in memory. Only the methods the table-set
// path calls are implemented; the embedded nil Engine panics on anything else, which is
// what a test wants if the path reaches further than it should.
type fakeSets struct {
	Engine
	mu      sync.Mutex
	sets    map[string]map[string]int
	dropped []string // in order, "db.table" or "db"
	whole   int      // DropTableSet calls
}

func (f *fakeSets) TableSetWords() (string, string) { return "database", "table" }

func (f *fakeSets) TableSets(ctx context.Context) ([]TableSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []TableSet
	for db, ts := range f.sets {
		s := TableSet{DB: db, Tables: len(ts)}
		for _, r := range ts {
			s.Rows += int64(r)
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeSets) CreateTableSet(ctx context.Context, db string, tables, rows int, done func(int)) error {
	f.mu.Lock()
	if f.sets[db] == nil {
		f.sets[db] = map[string]int{}
	}
	f.mu.Unlock()
	for i := 1; i <= tables; i++ {
		f.mu.Lock()
		f.sets[db][SetTableName(i)] = rows
		f.mu.Unlock()
		done(i)
	}
	return nil
}

func (f *fakeSets) SetTables(ctx context.Context, db string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.sets[db] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeSets) DropSetTable(ctx context.Context, db, table string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sets[db], table)
	f.dropped = append(f.dropped, db+"."+table)
	return nil
}

func (f *fakeSets) DropTableSet(ctx context.Context, db string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sets, db)
	f.dropped = append(f.dropped, db)
	f.whole++
	return nil
}

func waitIdle(t *testing.T, h *Hub) {
	t.Helper()
	for i := 0; i < 200; i++ {
		h.mu.Lock()
		busy := h.tsJob != nil
		h.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the table-set job never finished")
}

func newFakeSetHub() (*Hub, *fakeSets) {
	f := &fakeSets{sets: map[string]map[string]int{}}
	return NewHub(context.Background(), f), f
}

func TestTableSetCreateThenDropOneAtATime(t *testing.T) {
	h, f := newFakeSetHub()
	if err := h.CreateTableSet("perfsim_tables", 1000, 10); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	st := h.State(0).TableSets
	if st == nil || len(st.Sets) != 1 || st.Sets[0].Tables != 1000 || st.Sets[0].Rows != 10000 {
		t.Fatalf("after create: %+v", st)
	}
	if err := h.DropTableSet("perfsim_tables", "tables"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	// Every table on its own, in order, and the emptied database last.
	if len(f.dropped) != 1001 || f.dropped[0] != "perfsim_tables.t00001" || f.dropped[1000] != "perfsim_tables" {
		t.Fatalf("dropped %d things, first %q last %q", len(f.dropped), f.dropped[0], f.dropped[len(f.dropped)-1])
	}
	if len(h.State(0).TableSets.Sets) != 0 {
		t.Error("the set is still listed after its drop")
	}
	// Both jobs left a closed band on the timeline.
	n := 0
	for _, b := range h.State(0).Bands {
		if b.ID == "tableset" {
			n++
			if b.End == nil {
				t.Errorf("band %q never closed", b.Label)
			}
		}
	}
	if n != 2 {
		t.Errorf("%d table-set bands, want 2", n)
	}
}

func TestTableSetDropWholeDatabase(t *testing.T) {
	h, f := newFakeSetHub()
	h.CreateTableSet("perfsim_x", 50, 0)
	waitIdle(t, h)
	if err := h.DropTableSet("perfsim_x", "database"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if f.whole != 1 || len(f.dropped) != 1 {
		t.Errorf("a whole-database drop dropped %v", f.dropped)
	}
}

// Only a database named as a set can be created or dropped: the sim's own schema, and
// anything that is not the sim's, are out of reach whatever the browser sends.
func TestTableSetNamesAreGuarded(t *testing.T) {
	h, f := newFakeSetHub()
	for _, bad := range []string{"perfsim", "mysql", "perfsim_", "perfsim_Upper", "perfsim_a;drop", "perfsim_" + strings.Repeat("a", 41), "other_db"} {
		if h.CreateTableSet(bad, 1, 1) == nil {
			t.Errorf("created %q", bad)
			waitIdle(t, h)
		}
		if h.DropTableSet(bad, "database") == nil {
			t.Errorf("dropped %q", bad)
			waitIdle(t, h)
		}
	}
	if h.CreateTableSet("perfsim_ok", 0, 1) == nil || h.CreateTableSet("perfsim_ok", 10, -1) == nil ||
		h.CreateTableSet("perfsim_ok", MaxSetTables, MaxSetRows) == nil {
		t.Error("an out-of-range size was accepted")
	}
	if f.whole != 0 || len(f.sets) != 0 {
		t.Errorf("the guard let something through: %v %v", f.dropped, f.sets)
	}
}

func TestTableSetOneJobAtATime(t *testing.T) {
	h, _ := newFakeSetHub()
	h.mu.Lock()
	h.tsJob = &tableSetJob{Op: "create", DB: "perfsim_busy", cancel: func() {}}
	h.mu.Unlock()
	if err := h.CreateTableSet("perfsim_other", 1, 1); err == nil || !strings.Contains(err.Error(), "perfsim_busy") {
		t.Errorf("a second job started: %v", err)
	}
}
