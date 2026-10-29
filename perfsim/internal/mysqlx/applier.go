package mysqlx

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Several dials control a replica's applier: Stop the applier keeps it stopped,
// while Delayed replica and Single-threaded applier stop it briefly to change a
// setting and start it again. Left to themselves they undo each other: a delay
// change would restart an applier another dial is holding stopped, and two
// releases at once fail because one starts the thread the other has to change.
// So every change to a member's applier goes through that member's lock, and
// the applier is started again only when no dial still wants it stopped.
type applierState struct {
	mu    sync.Mutex
	holds int
}

func (e *Engine) applierOf(m string) *applierState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.applier == nil {
		e.applier = map[string]*applierState{}
	}
	a := e.applier[m]
	if a == nil {
		a = &applierState{}
		e.applier[m] = a
	}
	return a
}

// holdApplier stops the member's applier and keeps it stopped until the
// matching releaseApplier.
//
// The hold counts from before the STOP is sent: if the STOP is slow (it waits
// for the applier, which may be waiting on a lock) and the caller gives up,
// the server can still complete it, and the matching release must then start
// the applier again.
func (e *Engine) holdApplier(ctx context.Context, db *sql.DB, m string) error {
	if err := e.applierFree(ctx, db); err != nil {
		return err
	}
	a := e.applierOf(m)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.holds++
	_, err := db.ExecContext(ctx, e.w("stop"))
	return err
}

func (e *Engine) releaseApplier(ctx context.Context, db *sql.DB, m string) error {
	a := e.applierOf(m)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.holds > 0 {
		a.holds--
	}
	if a.holds > 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, e.w("start"))
	return err
}

// withApplierStopped runs fn with the member's applier stopped, and starts it
// again afterwards unless a dial is holding it stopped.
func (e *Engine) withApplierStopped(ctx context.Context, db *sql.DB, m string, fn func() error) error {
	if err := e.applierFree(ctx, db); err != nil {
		return err
	}
	a := e.applierOf(m)
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := db.ExecContext(ctx, e.w("stop")); err != nil {
		return err
	}
	err := fn()
	if a.holds == 0 {
		if _, err2 := db.ExecContext(ctx, e.w("start")); err == nil {
			err = err2
		}
	}
	return err
}

// applierFree refuses to stop an applier that is waiting on a lock. STOP
// REPLICA waits for the applier to stop, and an applier waiting on FLUSH TABLES
// WITH READ LOCK or a metadata lock does not stop until that is released: the
// STOP would hang, and if given up on, still land later, after the dial has
// moved on. Saying so at once is the safe answer.
func (e *Engine) applierFree(ctx context.Context, db *sql.DB) error {
	row, ok := queryRow(ctx, db, e.w("status"))
	if !ok {
		return nil
	}
	for _, k := range []string{"Replica_SQL_Running_State", "Slave_SQL_Running_State"} {
		if st := row[k]; strings.Contains(strings.ToLower(st), "lock") {
			return fmt.Errorf("its applier is waiting for a lock (%s); release whatever holds it first", st)
		}
	}
	// With parallel workers the coordinator only says it is waiting for them;
	// the worker is the thread that waits on the lock.
	var state sql.NullString
	err := db.QueryRowContext(ctx, `SELECT STATE FROM information_schema.PROCESSLIST
		WHERE USER = 'system user' AND STATE LIKE '%lock%' LIMIT 1`).Scan(&state)
	if err == nil && state.Valid {
		return fmt.Errorf("its applier is waiting for a lock (%s); release whatever holds it first", state.String)
	}
	return nil
}

// undo is a change a dial made to a member, recorded on the target before the
// change is made. Releasing a dial applies its undo and deletes the record; a
// record still there when the sim starts means the sim was killed before it
// could release, and Init applies it then. A lock or an open transaction needs
// no record: it ends with the connection that held it.
type undo struct {
	Kind    string `json:"kind"` // start | delay | var
	Member  string `json:"member"`
	Var     string `json:"var,omitempty"`
	Value   string `json:"value,omitempty"`
	Applier bool   `json:"applier,omitempty"` // the variable can only change with the applier stopped
}

var varName = regexp.MustCompile(`^[a-z_]+$`)

func (e *Engine) saveUndo(ctx context.Context, key string, u undo) {
	b, _ := json.Marshal(u)
	if err := e.Save(ctx, key, b); err != nil {
		log.Printf("perfsim: recording how to undo %s: %v", key, err)
	}
}

// restore applies one undo record (or, with new values, the change itself).
func (e *Engine) restore(ctx context.Context, u undo) error {
	db := e.member(u.Member)
	if db == nil {
		return fmt.Errorf("%s is no longer a member", u.Member)
	}
	switch u.Kind {
	case "start":
		return e.releaseApplier(ctx, db, u.Member)
	case "delay":
		d, err := strconv.Atoi(u.Value)
		if err != nil {
			return err
		}
		return e.withApplierStopped(ctx, db, u.Member, func() error {
			_, err := db.ExecContext(ctx, fmt.Sprintf(e.w("delay"), d))
			return err
		})
	case "var":
		if !varName.MatchString(u.Var) || strings.TrimFunc(u.Value, func(r rune) bool { return r >= '0' && r <= '9' }) != "" {
			return fmt.Errorf("refusing to set %q to %q", u.Var, u.Value)
		}
		set := func() error { _, err := db.ExecContext(ctx, "SET GLOBAL "+u.Var+" = "+u.Value); return err }
		if u.Applier {
			return e.withApplierStopped(ctx, db, u.Member, set)
		}
		return set()
	}
	return fmt.Errorf("unknown undo %q", u.Kind)
}

// replayUndo applies what a killed sim left behind. See undo.
func (e *Engine) replayUndo(ctx context.Context) {
	all, err := e.Load(ctx, "undo:")
	if err != nil {
		return
	}
	for key, b := range all {
		var u undo
		if json.Unmarshal(b, &u) != nil {
			e.Delete(ctx, key)
			continue
		}
		if u.Kind == "start" {
			// A stopped applier from before: start it, whatever the count says now.
			if db := e.member(u.Member); db != nil {
				_, err = db.ExecContext(ctx, e.w("start"))
			}
		} else {
			err = e.restore(ctx, u)
		}
		if err != nil {
			log.Printf("perfsim: could not undo %s left by an earlier run: %v", key, err)
			continue
		}
		log.Printf("perfsim: undid %s left by an earlier run", key)
		e.Delete(ctx, key)
	}
}
