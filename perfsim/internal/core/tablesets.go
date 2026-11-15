package core

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Table sets: a database (a schema on PostgreSQL, a database on MongoDB) holding many
// small tables, built so it can be dropped again — all at once, or one table at a time.
//
// It is a facility rather than a stressor because both halves are the test. Creating a
// thousand tables fills the data dictionary, table_open_cache, the relcache or WiredTiger's
// file handles; dropping them is where servers stall — a DROP DATABASE that holds a global
// lock, a buffer pool scanned once per dropped table, a DROP SCHEMA CASCADE that runs out
// of lock slots. Both run as one job with a band on the timeline, and both are timed.
//
// Each set lives in a database of its own, named with the sim's prefix, so dropping a
// whole set can never reach the sim's own schema or anybody else's.

// TableSetPrefix starts the name of every database a table set lives in. A database is
// only ever dropped when its name matches TableSetName.
const TableSetPrefix = "perfsim_"

var TableSetName = regexp.MustCompile(`^perfsim_[a-z0-9_]{1,40}$`)

// Limits on one set: enough to make any server notice, not enough to fill a disk by a
// mistyped zero.
const (
	MaxSetTables = 100000
	MaxSetRows   = 1000000
	// MaxSetCells bounds tables × rows, about 20 GB of the small rows the engines write.
	MaxSetCells = 200_000_000
)

// TableSet is one database the sim made for many tables.
type TableSet struct {
	DB     string `json:"db"`
	Tables int    `json:"tables"`
	Rows   int64  `json:"rows"` // the engine's estimate where an exact count would be a scan
}

// TableSetter is implemented by the engines that have tables. It is optional, so an
// engine without them (Valkey) simply does not offer the section.
type TableSetter interface {
	// TableSetWords names the container and its members in the engine's own language:
	// "database"/"table" on MySQL, "schema"/"table" on PostgreSQL, "database"/"collection"
	// on MongoDB.
	TableSetWords() (container, member string)
	TableSets(ctx context.Context) ([]TableSet, error)
	// CreateTableSet makes tables named t00001… in db, each with rows rows, skipping
	// tables that already exist. done is called with the number of tables finished.
	CreateTableSet(ctx context.Context, db string, tables, rows int, done func(int)) error
	// SetTables lists the tables in db, for the one-at-a-time drop.
	SetTables(ctx context.Context, db string) ([]string, error)
	DropSetTable(ctx context.Context, db, table string) error
	// DropTableSet drops the whole container in one statement.
	DropTableSet(ctx context.Context, db string) error
}

// SetTableName is the name of a set's i-th table, from 1.
func SetTableName(i int) string { return fmt.Sprintf("t%05d", i) }

type tableSetJob struct {
	Op     string  `json:"op"` // create | drop
	DB     string  `json:"db"`
	Mode   string  `json:"mode,omitempty"` // drop: database | tables
	Done   int     `json:"done"`
	Total  int     `json:"total"`
	Rows   int     `json:"rows,omitempty"`
	Start  int64   `json:"start"`
	Rate   float64 `json:"rate"` // tables per second so far
	cancel context.CancelFunc
}

// tableSetState is the part of State this section reads.
type tableSetState struct {
	Container string       `json:"container"`
	Member    string       `json:"member"`
	Sets      []TableSet   `json:"sets"`
	Job       *tableSetJob `json:"job"`
}

func (h *Hub) tableSetter() TableSetter {
	ts, _ := h.eng.(TableSetter)
	return ts
}

func (h *Hub) refreshTableSets(ctx context.Context) {
	ts := h.tableSetter()
	if ts == nil {
		return
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	sets, err := ts.TableSets(c)
	cancel()
	if err != nil {
		return
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].DB < sets[j].DB })
	h.mu.Lock()
	h.tableSets = sets
	h.mu.Unlock()
}

// tableSetStateLocked is called with h.mu held.
func (h *Hub) tableSetStateLocked() *tableSetState {
	ts := h.tableSetter()
	if ts == nil {
		return nil
	}
	c, m := ts.TableSetWords()
	s := &tableSetState{Container: c, Member: m, Sets: h.tableSets}
	if s.Sets == nil {
		s.Sets = []TableSet{}
	}
	if j := h.tsJob; j != nil {
		cp := *j
		if el := time.Since(time.Unix(j.Start, 0)).Seconds(); el > 0 {
			cp.Rate = float64(j.Done) / el
		}
		s.Job = &cp
	}
	return s
}

// beginTableSetJob registers the one job a hub runs at a time, with its timeline band.
func (h *Hub) beginTableSetJob(j *tableSetJob, label string) (context.Context, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tsJob != nil {
		return nil, nil, errf("%s is still %s; wait for it or cancel it", h.tsJob.DB, map[string]string{"create": "being created", "drop": "being dropped"}[h.tsJob.Op])
	}
	ctx, cancel := context.WithCancel(h.ctx)
	j.cancel, j.Start = cancel, time.Now().Unix()
	h.tsJob = j
	h.seq++
	seq := h.seq
	h.bands = append(h.bands, Band{Seq: seq, ID: "tableset", Label: label, Start: j.Start, Color: 3})
	end := func() {
		h.mu.Lock()
		now := time.Now().Unix()
		for i := range h.bands {
			if h.bands[i].Seq == seq {
				h.bands[i].End = &now
			}
		}
		h.tsJob = nil
		h.mu.Unlock()
		cancel()
	}
	return ctx, end, nil
}

func (h *Hub) setTableSetDone(n int) {
	h.mu.Lock()
	if h.tsJob != nil {
		h.tsJob.Done = n
	}
	h.mu.Unlock()
}

// CreateTableSet starts building a set in the background.
func (h *Hub) CreateTableSet(db string, tables, rows int) error {
	ts := h.tableSetter()
	if ts == nil {
		return errf("this target has no tables to create")
	}
	if !TableSetName.MatchString(db) {
		return errf("the name must start with %q and hold only lower-case letters, digits and _ (at most 40 after the prefix)", TableSetPrefix)
	}
	if tables < 1 || tables > MaxSetTables {
		return errf("tables must be between 1 and %d", MaxSetTables)
	}
	if rows < 0 || rows > MaxSetRows {
		return errf("rows per table must be between 0 and %d", MaxSetRows)
	}
	if int64(tables)*int64(rows) > MaxSetCells {
		return errf("%d tables × %d rows is more than the %d rows one set may hold", tables, rows, MaxSetCells)
	}
	_, member := ts.TableSetWords()
	ctx, end, err := h.beginTableSetJob(&tableSetJob{Op: "create", DB: db, Total: tables, Rows: rows},
		fmt.Sprintf("Create %s: %d %ss", db, tables, member))
	if err != nil {
		return err
	}
	rowWord := "rows"
	if member == "collection" {
		rowWord = "documents"
	}
	h.event("neutral", fmt.Sprintf("Creating %s: %d %ss of %d %s each", db, tables, member, rows, rowWord))
	go func() {
		defer end()
		t0 := time.Now()
		err := ts.CreateTableSet(ctx, db, tables, rows, h.setTableSetDone)
		el := time.Since(t0)
		switch {
		case ctx.Err() != nil:
			h.event("warn", fmt.Sprintf("%s: creation cancelled after %s; what was made stays", db, fmtDur(el)))
		case err != nil:
			h.event("bad", fmt.Sprintf("%s: creation failed after %s: %v", db, fmtDur(el), err))
		default:
			h.event("ok", fmt.Sprintf("%s ready: %d %ss in %s (%.0f/s)", db, tables, member, fmtDur(el), float64(tables)/max(el.Seconds(), 0.001)))
		}
		h.refreshTableSets(h.ctx)
	}()
	return nil
}

// DropTableSet drops a set: the whole container in one statement (mode "database"), or
// each table in turn and then the empty container (mode "tables").
func (h *Hub) DropTableSet(db, mode string) error {
	ts := h.tableSetter()
	if ts == nil {
		return errf("this target has no tables to drop")
	}
	if !TableSetName.MatchString(db) {
		return errf("only a table set the sim made can be dropped")
	}
	if mode != "tables" {
		mode = "database"
	}
	container, member := ts.TableSetWords()
	var names []string
	if mode == "tables" {
		lctx, cancel := context.WithTimeout(h.ctx, time.Minute)
		var err error
		names, err = ts.SetTables(lctx, db)
		cancel()
		if err != nil {
			return err
		}
	}
	how := "the whole " + container
	if mode == "tables" {
		how = fmt.Sprintf("%d %ss one at a time", len(names), member)
	}
	ctx, end, err := h.beginTableSetJob(&tableSetJob{Op: "drop", DB: db, Mode: mode, Total: max(len(names), 1)},
		fmt.Sprintf("Drop %s: %s", db, how))
	if err != nil {
		return err
	}
	h.event("neutral", fmt.Sprintf("Dropping %s: %s", db, how))
	go func() {
		defer end()
		t0 := time.Now()
		var err error
		if mode == "tables" {
			for i, n := range names {
				if ctx.Err() != nil {
					break
				}
				if err = ts.DropSetTable(ctx, db, n); err != nil {
					err = fmt.Errorf("%s: %w", n, err)
					break
				}
				h.setTableSetDone(i + 1)
			}
		}
		el := time.Since(t0)
		if err == nil && ctx.Err() == nil {
			// The container goes last, empty — or, in database mode, with everything in it.
			d0 := time.Now()
			err = ts.DropTableSet(ctx, db)
			if mode == "database" {
				el = time.Since(d0)
				h.setTableSetDone(1)
			}
		}
		switch {
		case ctx.Err() != nil:
			h.event("warn", fmt.Sprintf("%s: drop cancelled after %s; the rest is still there", db, fmtDur(el)))
		case err != nil:
			h.event("bad", fmt.Sprintf("%s: drop failed after %s: %v", db, fmtDur(el), err))
		case mode == "tables":
			h.event("ok", fmt.Sprintf("%s dropped %s by %s in %s (%.0f/s)", db, member, member, fmtDur(el), float64(len(names))/max(el.Seconds(), 0.001)))
		default:
			h.event("ok", fmt.Sprintf("%s dropped as one %s in %s", db, container, fmtDur(el)))
		}
		h.refreshTableSets(h.ctx)
	}()
	return nil
}

// CancelTableSet stops the running create or drop where it is.
func (h *Hub) CancelTableSet() {
	h.mu.Lock()
	if h.tsJob != nil {
		h.tsJob.cancel()
	}
	h.mu.Unlock()
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < 90*time.Second:
		return fmt.Sprintf("%.1f s", d.Seconds())
	default:
		return fmt.Sprintf("%.1f min", d.Minutes())
	}
}
