package main

// Database Explorer — the HTTP surface.
//
// Every handler here begins the same way and it is not boilerplate: resolve the
// caller, then resolve the connection id *for that caller* (dexResolve), which
// re-reads the stack, re-checks ownership and re-derives the endpoint from the
// deployments as they are right now. No handler trusts a connection id because it
// arrived in a request, none of them accepts a host, user or password, and none of
// them returns one — dexConnection has no password field to return.
//
// The adapter is opened per request and closed with it. That costs a connection
// handshake on each call and buys something worth more on a lab canvas: a node that
// was restarted, redeployed or destroyed between two clicks is discovered on the next
// one rather than through a pool full of dead connections.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// dexAsked reads the write arm off a GET. Only "1" counts, so a stray parameter
// cannot unlock anything, and the administrator setting still has to agree.
func dexAsked(r *http.Request) bool { return r.URL.Query().Get("allowWrites") == "1" }

// dexWithConn is the shared preamble: authenticate, authorize, open, and hand the
// adapter to fn with a bounded context. Read-only policies apply as declared.
func (a *App) dexWithConn(w http.ResponseWriter, r *http.Request, connID string, timeoutS int,
	fn func(ctx context.Context, ad dexAdapter, t dexTarget)) {
	a.dexWithConnRW(w, r, connID, timeoutS, false, fn)
}

// dexWithConnRW is dexWithConn with the option of lifting a read-only policy. Only
// the two write paths pass true, and passing it is a request rather than a decision:
// dexUnlock still has to agree, which it does only for a policy-read-only connection
// on an installation whose administrator has unlocked writes.
func (a *App) dexWithConnRW(w http.ResponseWriter, r *http.Request, connID string, timeoutS int,
	allowWrites bool, fn func(ctx context.Context, ad dexAdapter, t dexTarget)) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if strings.TrimSpace(connID) == "" {
		writeErr(w, http.StatusBadRequest, "a connection is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(dexClampTimeout(timeoutS))*time.Second)
	defer cancel()

	t, err := a.dexResolve(ctx, u, connID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	unlocked := a.dexUnlock(t, allowWrites)
	open := a.dexOpen
	if unlocked {
		open = a.dexOpenUnlocked
	}
	ad, err := open(ctx, t)
	if err != nil {
		// A connection failure is the database's answer, not a server fault, and it
		// is the single most useful thing to show: "could not reach it, here is what
		// it said".
		writeJSON(w, http.StatusOK, map[string]any{
			"connection": t.Conn,
			"error":      dexAsDexError(t.Conn.Engine, err),
		})
		return
	}
	defer ad.Close()
	if unlocked {
		t.Conn.ReadOnly = false
	}
	fn(ctx, ad, t)
}

// GET /api/dbexplorer/connections
func (a *App) handleDexConnections(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{
		"stacks": a.dexConnections(ctx, u),
		// The warning travels with the payload rather than being hard-coded in the
		// frontend, so the sentence a user reads about PMM's databases and the rule
		// the server enforces about them are written in one place.
		"pmmWarning": dexPMMWarning,
	})
}

// GET /api/dbexplorer/connections/{cid}
func (a *App) handleDexConnectionInfo(w http.ResponseWriter, r *http.Request) {
	a.dexWithConn(w, r, r.PathValue("cid"), 30, func(ctx context.Context, ad dexAdapter, t dexTarget) {
		conn := t.Conn
		if v, err := ad.Version(ctx); err == nil && v != "" {
			conn.Version = v
		}
		writeJSON(w, http.StatusOK, map[string]any{"connection": conn})
	})
}

// GET /api/dbexplorer/connections/{cid}/databases
func (a *App) handleDexDatabases(w http.ResponseWriter, r *http.Request) {
	a.dexWithConn(w, r, r.PathValue("cid"), 60, func(ctx context.Context, ad dexAdapter, t dexTarget) {
		nodes, err := ad.ListDatabases(ctx)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"error": dexAsDexError(t.Conn.Engine, err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
	})
}

// GET /api/dbexplorer/connections/{cid}/schemas?database=
func (a *App) handleDexSchemas(w http.ResponseWriter, r *http.Request) {
	a.dexWithConn(w, r, r.PathValue("cid"), 60, func(ctx context.Context, ad dexAdapter, t dexTarget) {
		nodes, err := ad.ListSchemas(ctx, r.URL.Query().Get("database"))
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"error": dexAsDexError(t.Conn.Engine, err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
	})
}

// GET /api/dbexplorer/connections/{cid}/objects?database=&schema=&folder=&cursor=&filter=
func (a *App) handleDexObjects(w http.ResponseWriter, r *http.Request) {
	a.dexWithConn(w, r, r.PathValue("cid"), 60, func(ctx context.Context, ad dexAdapter, t dexTarget) {
		q := r.URL.Query()
		page, err := ad.ListObjects(ctx, q.Get("database"), q.Get("schema"), q.Get("folder"), q.Get("cursor"), q.Get("filter"))
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"error": dexAsDexError(t.Conn.Engine, err)})
			return
		}
		if len(page.Folders) > 0 {
			dexSortTreeNodes(page.Nodes, page.Folders)
		}
		writeJSON(w, http.StatusOK, page)
	})
}

// GET /api/dbexplorer/connections/{cid}/object?database=&schema=&name=&kind=&allowWrites=
//
// allowWrites is here for one reason: whether a row can be edited is part of what
// describing an object answers, and on a connection the caller has armed for writes
// the answer changes. It is the same arm as everywhere else, checked the same way.
func (a *App) handleDexObjectDetail(w http.ResponseWriter, r *http.Request) {
	a.dexWithConnRW(w, r, r.PathValue("cid"), 60, dexAsked(r), func(ctx context.Context, ad dexAdapter, t dexTarget) {
		q := r.URL.Query()
		ref := dexObjectRef{Database: q.Get("database"), Schema: q.Get("schema"), Name: q.Get("name"), Kind: q.Get("kind")}
		if strings.TrimSpace(ref.Name) == "" {
			writeErr(w, http.StatusBadRequest, "an object name is required")
			return
		}
		detail, err := ad.DescribeObject(ctx, ref.Database, ref)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"error": dexAsDexError(t.Conn.Engine, err)})
			return
		}
		writeJSON(w, http.StatusOK, detail)
	})
}

// POST /api/dbexplorer/query
func (a *App) handleDexQuery(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req dexQueryRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Limit = dexClampLimit(req.Limit)
	timeout := dexClampTimeout(req.TimeoutS)
	runID := dexSafeID(req.QueryID)

	// The query's context is detached from the request's deadline handling only in
	// the sense that it has its own: a client that disconnects still cancels it
	// (r.Context()), and so does an explicit Cancel through the registry.
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeout)*time.Second)
	defer cancel()
	dexRegisterRun(runID, u.ID, cancel)
	defer dexUnregisterRun(runID)

	t, err := a.dexResolve(ctx, u, req.ConnectionID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// Whether this submission may write to a policy-read-only connection is decided
	// here, from the administrator setting and the request together — never from the
	// connection the browser thinks it has.
	unlocked := a.dexUnlock(t, req.AllowWrites)
	open := a.dexOpen
	if unlocked {
		open = a.dexOpenUnlocked
	}
	ad, err := open(ctx, t)
	if err != nil {
		writeJSON(w, http.StatusOK, dexResult{Engine: t.Conn.Engine, Error: dexAsDexError(t.Conn.Engine, err)})
		return
	}
	defer ad.Close()

	res, err := ad.Query(ctx, req)
	if err != nil {
		res.Engine = t.Conn.Engine
		res.Error = dexAsDexError(t.Conn.Engine, err)
	}
	res.Limit = req.Limit
	res.Unlocked = unlocked
	if stmt := dexStatementOf(req); stmt != "" {
		a.dexRecordHistory(u, t, req, res, stmt)
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /api/dbexplorer/cancel
func (a *App) handleDexCancel(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		QueryID string `json:"queryId"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// A query that has already finished is not an error: the user pressed Cancel and
	// the thing they wanted (it not running) is true either way.
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": dexCancelRun(dexSafeID(req.QueryID), u)})
}

// POST /api/dbexplorer/mutate
func (a *App) handleDexMutate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req dexMutateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	a.dexWithConnRW(w, r, req.ConnectionID, 60, req.AllowWrites, func(ctx context.Context, ad dexAdapter, t dexTarget) {
		res, err := a.dexMutate(ctx, ad, t, req)
		if err != nil {
			res.Error = dexAsDexError(t.Conn.Engine, err)
		}
		if res.Executed && res.Preview != "" {
			a.store.DexAddHistory(u.ID, dexHistoryEntry{
				ConnectionID: t.Conn.ID, Connection: t.dexSummaryLine(), Engine: t.Conn.Engine,
				Database: req.Database, Schema: req.Schema, Statement: res.Preview,
				DurationMs: res.DurationMs, AffectedRows: res.AffectedRows,
				Success: res.Error == nil,
			})
		}
		writeJSON(w, http.StatusOK, res)
	})
}

// GET /api/dbexplorer/history
func (a *App) handleDexHistory(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := a.store.DexHistory(u.ID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the history")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// DELETE /api/dbexplorer/history — the whole list
func (a *App) handleDexClearHistory(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := a.store.DexClearHistory(u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not clear the history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// DELETE /api/dbexplorer/history/{hid} — one entry
func (a *App) handleDexDeleteHistory(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("hid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid history id")
		return
	}
	// Scoped to the caller in the DELETE itself, so another user's id simply
	// matches nothing rather than being checked and then acted on.
	if err := a.store.DexDeleteHistory(u.ID, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not delete the entry")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /api/dbexplorer/saved
func (a *App) handleDexSavedList(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	out, err := a.store.DexSavedQueries(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the saved queries")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /api/dbexplorer/saved
func (a *App) handleDexSaveQuery(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var q dexSavedQuery
	if err := decode(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	q.Name = strings.TrimSpace(q.Name)
	q.Statement = strings.TrimSpace(q.Statement)
	if q.Name == "" || q.Statement == "" {
		writeErr(w, http.StatusBadRequest, "a name and a statement are required")
		return
	}
	if len(q.Statement) > dexMaxStatement {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("the statement is too long (max %d bytes)", dexMaxStatement))
		return
	}
	saved, err := a.store.DexSaveQuery(u.ID, q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save the query")
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// DELETE /api/dbexplorer/saved/{sid}
func (a *App) handleDexDeleteSaved(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid saved-query id")
		return
	}
	if err := a.store.DexDeleteSaved(u.ID, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not delete the saved query")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// dexViewDataRequest builds the "View Data" submission for an object, per engine. It
// lives on the server so that the statement a click produces is the same one the
// editor would have run — and so that the row ceiling is applied to it like any
// other, rather than a table click being the one path that could fetch everything.
func dexViewDataRequest(engine string, ref dexObjectRef, limit int) dexQueryRequest {
	req := dexQueryRequest{Database: ref.Database, Schema: ref.Schema, Limit: dexClampLimit(limit)}
	switch engine {
	case dexMySQL:
		req.SQL = fmt.Sprintf("SELECT * FROM %s.%s LIMIT %d",
			dexQuoteIdent(dexMySQL, ref.Database), dexQuoteIdent(dexMySQL, ref.Name), req.Limit)
	case dexPostgres:
		schema := ref.Schema
		if schema == "" {
			schema = "public"
		}
		req.SQL = fmt.Sprintf("SELECT * FROM %s.%s LIMIT %d",
			dexQuoteIdent(dexPostgres, schema), dexQuoteIdent(dexPostgres, ref.Name), req.Limit)
	case dexClickHouse:
		req.SQL = fmt.Sprintf("SELECT * FROM %s.%s LIMIT %d",
			dexQuoteIdent(dexClickHouse, ref.Database), dexQuoteIdent(dexClickHouse, ref.Name), req.Limit)
	case dexMongoDB:
		req.Mongo.Collection = ref.Name
		req.Mongo.Operation = "find"
		req.Mongo.Filter = json.RawMessage("{}")
	case dexValkey:
		req.Mongo.Collection = ref.Name // the key
	}
	return req
}

// GET /api/dbexplorer/connections/{cid}/viewdata?database=&schema=&name=&kind=&limit=
//
// One round trip for the commonest action in the whole feature: click a table, see
// its rows. It runs the same query the editor would, through the same adapter, and
// returns the statement alongside the result so the editor tab can show what it ran.
func (a *App) handleDexViewData(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	a.dexWithConnRW(w, r, r.PathValue("cid"), 60, dexAsked(r), func(ctx context.Context, ad dexAdapter, t dexTarget) {
		ref := dexObjectRef{Database: q.Get("database"), Schema: q.Get("schema"), Name: q.Get("name"), Kind: q.Get("kind")}
		if strings.TrimSpace(ref.Name) == "" {
			writeErr(w, http.StatusBadRequest, "an object name is required")
			return
		}
		req := dexViewDataRequest(t.Conn.Engine, ref, limit)
		req.ConnectionID = t.Conn.ID
		res, err := ad.Query(ctx, req)
		if err != nil {
			res.Error = dexAsDexError(t.Conn.Engine, err)
		}
		res.Limit = req.Limit
		if stmt := dexStatementOf(req); stmt != "" {
			a.dexRecordHistory(u, t, req, res, stmt)
		}
		writeJSON(w, http.StatusOK, map[string]any{"request": req, "result": res})
	})
}

// POST /api/dbexplorer/expose — give a Kubernetes endpoint an address the load tools
// can dial, by creating a companion Service beside the operator's own.
// DELETE removes it again.
func (a *App) handleDexExpose(w http.ResponseWriter, r *http.Request) {
	a.dexExposeAction(w, r, true)
}

func (a *App) handleDexUnexpose(w http.ResponseWriter, r *http.Request) {
	a.dexExposeAction(w, r, false)
}

func (a *App) dexExposeAction(w http.ResponseWriter, r *http.Request, on bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		ConnectionID string `json:"connectionId"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	// Resolved through the same path as everything else, so this cannot be pointed
	// at a cluster the caller may not reach.
	t, err := a.dexResolve(ctx, u, req.ConnectionID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if t.K8s == nil {
		writeErr(w, http.StatusBadRequest, "only a database inside a Kubernetes cluster can be exposed this way")
		return
	}
	if !on {
		if err := a.k8sUnexposeEndpoint(ctx, *t.K8s); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, k8sExposeResult{
			Message: t.K8s.Label + " is back to being reachable only inside the cluster",
		})
		return
	}
	res, err := a.k8sExposeEndpoint(ctx, *t.K8s)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
