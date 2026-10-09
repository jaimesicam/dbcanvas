package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// activitydeep.go — deep instrumentation: the performance_schema consumers and instruments that do
// cost a busy server something, switched on for one MySQL-family node, for a set time, and back
// off by themselves. What it gives the Activity view: DDL progress (stage events), what a session
// waits on beyond locks, socket I/O per session.
//
// Only what was off is switched on, and that exact list is what is switched back — recorded in
// app_settings ("deep:<stack>:<node>") so an app restart in the middle reverts it at startup
// rather than leaving a node instrumented for good. performance_schema's setup is runtime state;
// a server restart resets it, so a key whose node is gone is simply dropped.

const deepMaxMinutes = 60

type deepRecord struct {
	deepState
	Consumers   []string    `json:"consumers"`
	Instruments [][3]string `json:"instruments"` // name, enabled, timed — as they were
}

func deepKey(stackID int64, nid string) string { return fmt.Sprintf("deep:%d:%s", stackID, nid) }

func (a *App) deepRecordOf(stackID int64, nid string) *deepRecord {
	v, _ := a.store.AppSetting(deepKey(stackID, nid))
	if v == "" {
		return nil
	}
	var r deepRecord
	if json.Unmarshal([]byte(v), &r) != nil {
		return nil
	}
	return &r
}

func (a *App) deepStateOf(stackID int64, nid string) *deepState {
	r := a.deepRecordOf(stackID, nid)
	if r == nil {
		return nil
	}
	return &r.deepState
}

func sqlList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "'" + strings.ReplaceAll(n, "'", "''") + "'"
	}
	return strings.Join(q, ",")
}

// deepEnable switches the deep instruments on for minutes and records what it changed.
func (a *App) deepEnable(ctx context.Context, st Stack, nid string, minutes int) (*deepState, error) {
	c, ok := a.dbConnFor(st, nid)
	if !ok || c.Engine != "mysql" {
		return nil, errors.New("deep instrumentation is for MySQL-family nodes")
	}
	if r := a.deepRecordOf(st.ID, nid); r != nil {
		// Already on: only the timer moves.
		r.Until = time.Now().Add(time.Duration(minutes) * time.Minute).UnixMilli()
		b, _ := json.Marshal(r)
		a.store.SetAppSetting(deepKey(st.ID, nid), string(b))
		return &r.deepState, nil
	}
	var pats []string
	for _, p := range deepInstruments {
		pats = append(pats, "NAME LIKE '"+p+"'")
	}
	var probe struct {
		On   string            `json:"on"`
		Cons []string          `json:"cons"`
		Ins  []json.RawMessage `json:"ins"`
	}
	q := fmt.Sprintf(`SELECT JSON_OBJECT('on', CAST(@@performance_schema AS CHAR),
	  'cons', (SELECT JSON_ARRAYAGG(NAME) FROM performance_schema.setup_consumers WHERE ENABLED = 'NO' AND NAME IN (%s)),
	  'ins', (SELECT JSON_ARRAYAGG(JSON_ARRAY(NAME, ENABLED, TIMED)) FROM performance_schema.setup_instruments WHERE (%s) AND (ENABLED = 'NO' OR TIMED = 'NO')))`,
		sqlList(deepConsumers), strings.Join(pats, " OR "))
	if err := a.queryJSON(ctx, c, "", q, &probe); err != nil {
		return nil, fmt.Errorf("performance_schema is not available on this server: %v", err)
	}
	if probe.On != "1" && !strings.EqualFold(probe.On, "ON") {
		return nil, errors.New("performance_schema is off on this server, and turning it on needs a restart")
	}
	rec := &deepRecord{Consumers: probe.Cons}
	var names []string
	for _, raw := range probe.Ins {
		var t [3]string
		if json.Unmarshal(raw, &t) == nil {
			rec.Instruments = append(rec.Instruments, t)
			names = append(names, t[0])
		}
	}
	var stmts []string
	if len(names) > 0 {
		stmts = append(stmts, "UPDATE performance_schema.setup_instruments SET ENABLED = 'YES', TIMED = 'YES' WHERE NAME IN ("+sqlList(names)+")")
	}
	if len(rec.Consumers) > 0 {
		stmts = append(stmts, "UPDATE performance_schema.setup_consumers SET ENABLED = 'YES' WHERE NAME IN ("+sqlList(rec.Consumers)+")")
	}
	now := time.Now()
	rec.Since = now.UnixMilli()
	rec.Until = now.Add(time.Duration(minutes) * time.Minute).UnixMilli()
	rec.Enabled = append(append([]string{}, rec.Consumers...), fmt.Sprintf("%d instruments (%s)", len(names), strings.Join(deepInstruments, ", ")))
	if s := a.store.LatestSamples(st.ID, now.Add(-2*time.Minute).Unix()); s != nil {
		if smp, ok := s[nid]; ok {
			rec.QPSBase = smp.QPS
		}
	}
	b, _ := json.Marshal(rec)
	// Recorded before it is applied: a crash between the two reverts something that may not
	// have changed, which is harmless; the other order could leave it on.
	if err := a.store.SetAppSetting(deepKey(st.ID, nid), string(b)); err != nil {
		return nil, err
	}
	if len(stmts) > 0 {
		if err := a.execSQL(ctx, c, "mysql", strings.Join(stmts, ";\n")+";"); err != nil {
			a.store.DeleteAppSetting(deepKey(st.ID, nid))
			return nil, err
		}
	}
	return &rec.deepState, nil
}

// deepDisable puts back exactly what deepEnable changed.
func (a *App) deepDisable(ctx context.Context, st Stack, nid string) error {
	r := a.deepRecordOf(st.ID, nid)
	if r == nil {
		return nil
	}
	c, ok := a.dbConnFor(st, nid)
	if ok {
		var stmts []string
		for _, ins := range r.Instruments {
			stmts = append(stmts, fmt.Sprintf("UPDATE performance_schema.setup_instruments SET ENABLED = '%s', TIMED = '%s' WHERE NAME = '%s'",
				ins[1], ins[2], strings.ReplaceAll(ins[0], "'", "''")))
		}
		if len(r.Consumers) > 0 {
			stmts = append(stmts, "UPDATE performance_schema.setup_consumers SET ENABLED = 'NO' WHERE NAME IN ("+sqlList(r.Consumers)+")")
		}
		if len(stmts) > 0 {
			if err := a.execSQL(ctx, c, "mysql", strings.Join(stmts, ";\n")+";"); err != nil {
				return err
			}
		}
	}
	// Not running: performance_schema's setup did not survive the restart anyway.
	return a.store.DeleteAppSetting(deepKey(st.ID, nid))
}

// startDeepReverter turns deep instrumentation off when its time is up — and, run at startup,
// whatever was left on by an app that stopped in the middle.
func (a *App) startDeepReverter() {
	go func() {
		for {
			keys, _ := a.store.AppSettingKeys("deep:")
			for _, k := range keys {
				var stackID int64
				var nid string
				parts := strings.SplitN(strings.TrimPrefix(k, "deep:"), ":", 2)
				if len(parts) != 2 {
					continue
				}
				fmt.Sscan(parts[0], &stackID)
				nid = parts[1]
				r := a.deepRecordOf(stackID, nid)
				st, err := a.store.GetStack(stackID)
				if err != nil {
					a.store.DeleteAppSetting(k)
					continue
				}
				if r != nil && time.Now().UnixMilli() < r.Until {
					continue
				}
				ctx, cancel := context.WithTimeout(withEngine(context.Background(), a.depEngine(st, nid)), 30*time.Second)
				if err := a.deepDisable(ctx, st, nid); err != nil {
					log.Printf("deep instrumentation on stack %d node %s: revert failed: %v", stackID, nid, err)
				} else {
					a.recordStackEvent(stackID, nid, "action", "info", "Deep instrumentation off on "+nodeLabel(buildDoc(st), nid)+" (time up)", "", "")
				}
				cancel()
			}
			time.Sleep(30 * time.Second)
		}
	}()
}

// handleActivityDeep is POST …/activity/deep {"minutes": N} — 0 switches it off.
func (a *App) handleActivityDeep(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	var in struct{ Minutes int }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil || in.Minutes < 0 || in.Minutes > deepMaxMinutes {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("minutes is 0 (off) to %d", deepMaxMinutes))
		return
	}
	ctx, cancel := context.WithTimeout(withEngine(r.Context(), a.depEngine(st, nid)), 30*time.Second)
	defer cancel()
	label := nodeLabel(buildDoc(st), nid)
	if in.Minutes == 0 {
		if err := a.deepDisable(ctx, st, nid); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		a.recordStackEvent(st.ID, nid, "action", "info", "Deep instrumentation off on "+label, "", u.Username)
		writeJSON(w, http.StatusOK, map[string]any{"deep": nil})
		return
	}
	d, err := a.deepEnable(ctx, st, nid, in.Minutes)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	a.recordStackEvent(st.ID, nid, "action", "info", fmt.Sprintf("Deep instrumentation on %s for %d min", label, in.Minutes), strings.Join(d.Enabled, ", "), u.Username)
	writeJSON(w, http.StatusOK, map[string]any{"deep": d})
}
