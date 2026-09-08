package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kept pt-stalk captures.
//
// A capture used to live at one fixed path inside the node — /tmp/ptstalk.tar.gz —
// so every new one destroyed the last, and all of them died with the container.
// That makes the single most useful thing you can do with pt-stalk impossible:
// take one before a change, take another after, and compare them. dbcanvas now
// copies each finished capture into its own data directory under a timestamp and
// keeps an index of them, so a node has a history rather than a latest.
//
// The tarball goes on disk rather than into SQLite. They are a couple of
// megabytes each, and a blob column would put every one of them into every
// backup and every read of that database.

// ptStalkArchive is one kept capture.
type ptStalkArchive struct {
	ID         int64  `json:"id"`
	StackID    int64  `json:"stackId"`
	NodeID     string `json:"nodeId"`
	Host       string `json:"host"`
	CapturedAt string `json:"capturedAt"`
	SizeBytes  int64  `json:"sizeBytes"`
	Note       string `json:"note"`
	Path       string `json:"-"`
	// Filled in for the picker, which lists captures from every stack.
	StackName string `json:"stackName,omitempty"`
	NodeLabel string `json:"nodeLabel,omitempty"`
}

// ptStalkArchiveDir is where the tarballs live, beside the SQLite file so a
// single volume holds all of dbcanvas's state.
func ptStalkArchiveDir() string {
	dir := filepath.Dir(envOr("DB_PATH", "dbcanvas.db"))
	if dir == "" || dir == "." {
		dir = "."
	}
	return filepath.Join(dir, "ptstalk")
}

// ---- store ----

func (s *Store) InsertPTStalkArchive(a ptStalkArchive) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO ptstalk_archives (stack_id, node_id, host, captured_at, size_bytes, note, path)
		 VALUES (?,?,?,?,?,?,?)`,
		a.StackID, a.NodeID, a.Host, a.CapturedAt, a.SizeBytes, a.Note, a.Path)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const ptStalkArchiveCols = `id, stack_id, node_id, host, captured_at, size_bytes, note, path`

func scanPTStalkArchives(rows *sql.Rows) ([]ptStalkArchive, error) {
	defer rows.Close()
	var out []ptStalkArchive
	for rows.Next() {
		var a ptStalkArchive
		if err := rows.Scan(&a.ID, &a.StackID, &a.NodeID, &a.Host,
			&a.CapturedAt, &a.SizeBytes, &a.Note, &a.Path); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListPTStalkArchives returns kept captures newest first. A zero stackID lists
// every stack's, which is what the Stalk Summary picker wants — comparing a
// capture against one from a stack that has since been destroyed is a normal
// thing to want.
func (s *Store) ListPTStalkArchives(stackID int64, nodeID string) ([]ptStalkArchive, error) {
	q := `SELECT ` + ptStalkArchiveCols + ` FROM ptstalk_archives`
	var args []any
	var where []string
	if stackID > 0 {
		where = append(where, "stack_id = ?")
		args = append(args, stackID)
	}
	if nodeID != "" {
		where = append(where, "node_id = ?")
		args = append(args, nodeID)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY captured_at DESC, id DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanPTStalkArchives(rows)
}

func (s *Store) GetPTStalkArchive(id int64) (ptStalkArchive, error) {
	var a ptStalkArchive
	err := s.db.QueryRow(`SELECT `+ptStalkArchiveCols+` FROM ptstalk_archives WHERE id = ?`, id).
		Scan(&a.ID, &a.StackID, &a.NodeID, &a.Host, &a.CapturedAt, &a.SizeBytes, &a.Note, &a.Path)
	return a, err
}

func (s *Store) SetPTStalkArchiveNote(id int64, note string) error {
	_, err := s.db.Exec(`UPDATE ptstalk_archives SET note = ? WHERE id = ?`, note, id)
	return err
}

// DeletePTStalkArchive removes the row and returns the path so the caller can
// unlink the file. Row first: a file left behind is wasted space, a row pointing
// at a file that is gone is a broken listing.
func (s *Store) DeletePTStalkArchive(id int64) (string, error) {
	a, err := s.GetPTStalkArchive(id)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`DELETE FROM ptstalk_archives WHERE id = ?`, id)
	return a.Path, err
}

// ---- keeping a finished capture ----

// keepPTStalkCapture copies a just-finished capture out of the node and into
// dbcanvas's own storage. Best-effort by design: the capture itself has already
// succeeded and is downloadable from the node, so a failure to archive it is
// worth logging and nothing more.
func (a *App) keepPTStalkCapture(ctx context.Context, stackID int64, nodeID, containerID, host string) {
	data, err := a.readContainerFile(ctx, containerID, ptStalkFile)
	if err != nil || len(data) == 0 {
		return
	}
	dir := ptStalkArchiveDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	now := time.Now().UTC()
	name := fmt.Sprintf("ptstalk-%s-%s.tar.gz", sanitizeName(host), now.Format("20060102-150405"))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return
	}
	if _, err := a.store.InsertPTStalkArchive(ptStalkArchive{
		StackID: stackID, NodeID: nodeID, Host: host,
		CapturedAt: now.Format(time.RFC3339), SizeBytes: int64(len(data)), Path: path,
	}); err != nil {
		os.Remove(path)
	}
}

// ---- handlers ----

// handlePTStalkArchives lists the captures kept for one node.
func (a *App) handlePTStalkArchives(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningDBNode(w, r, "mysql")
	if !ok {
		return
	}
	list, err := a.store.ListPTStalkArchives(dep.StackID, r.PathValue("nid"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"archives": list})
}

// handleStalkArchives lists every kept capture the user owns, for the Stalk
// Summary picker. Enriched with stack and node names, because "ps-01 at 14:32"
// is what a person recognises and a node id is not.
func (a *App) handleStalkArchives(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	stacks, err := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out []ptStalkArchive
	for _, st := range stacks {
		list, err := a.store.ListPTStalkArchives(st.ID, "")
		if err != nil || len(list) == 0 {
			continue
		}
		// ListStacks does not carry the design — it is a large blob the stack
		// list has no use for — so the node labels need a second read, and only
		// for stacks that actually have captures. Without this the picker
		// silently shows every capture with a blank node.
		labels := map[string]string{}
		if full, err := a.store.GetStack(st.ID); err == nil {
			labels = nodeLabels(full)
		}
		for i := range list {
			list[i].StackName = st.Name
			list[i].NodeLabel = labels[list[i].NodeID]
			if list[i].NodeLabel == "" {
				list[i].NodeLabel = list[i].Host
			}
			out = append(out, list[i])
		}
	}
	// Newest first across every stack.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CapturedAt > out[j].CapturedAt })
	writeJSON(w, http.StatusOK, map[string]any{"archives": out})
}

// nodeLabels maps node id to the label drawn on the canvas.
func nodeLabels(st Stack) map[string]string {
	var doc designDoc
	_ = json.Unmarshal(st.Design, &doc)
	out := map[string]string{}
	for _, n := range doc.Nodes {
		out[n.ID] = n.Label
	}
	return out
}

// handleStalkFromArchive parses a kept capture by id.
func (a *App) handleStalkFromArchive(w http.ResponseWriter, r *http.Request) {
	arch, ok := a.loadOwnedArchive(w, r)
	if !ok {
		return
	}
	data, err := os.ReadFile(arch.Path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "the archive file is missing from storage")
		return
	}
	m, err := parsePtStalk(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Stamp identity from the record rather than the archive: two captures of
	// one host minutes apart must be distinguishable in a comparison, and the
	// note is what a user labelled the run with.
	m.Source.ArchiveID = arch.ID
	m.Source.Note = arch.Note
	if arch.CapturedAt != "" {
		m.Source.CapturedAt = arch.CapturedAt
	}
	writeJSON(w, http.StatusOK, m)
}

func (a *App) handleArchiveDownload(w http.ResponseWriter, r *http.Request) {
	arch, ok := a.loadOwnedArchive(w, r)
	if !ok {
		return
	}
	data, err := os.ReadFile(arch.Path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "the archive file is missing from storage")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", filepath.Base(arch.Path)))
	w.Write(data)
}

func (a *App) handleArchiveNote(w http.ResponseWriter, r *http.Request) {
	arch, ok := a.loadOwnedArchive(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	if err := a.store.SetPTStalkArchiveNote(arch.ID, strings.TrimSpace(body.Note)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleArchiveDelete(w http.ResponseWriter, r *http.Request) {
	arch, ok := a.loadOwnedArchive(w, r)
	if !ok {
		return
	}
	path, err := a.store.DeletePTStalkArchive(arch.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	os.Remove(path)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// loadOwnedArchive resolves {aid} and checks the caller owns the stack it came
// from. An archive outlives its node and can outlive its stack; when the stack
// is gone there is nobody left to check ownership against, so those are visible
// only to an admin.
func (a *App) loadOwnedArchive(w http.ResponseWriter, r *http.Request) (ptStalkArchive, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return ptStalkArchive{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("aid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad archive id")
		return ptStalkArchive{}, false
	}
	return a.ownedArchive(w, u, id)
}

// loadOwnedArchiveByID is the same check for an id that came from a request
// body rather than the path — the comparison endpoint takes a list of them.
func (a *App) loadOwnedArchiveByID(w http.ResponseWriter, r *http.Request, id int64) (ptStalkArchive, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return ptStalkArchive{}, false
	}
	return a.ownedArchive(w, u, id)
}

func (a *App) ownedArchive(w http.ResponseWriter, u User, id int64) (ptStalkArchive, bool) {
	arch, err := a.store.GetPTStalkArchive(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such archive")
		return ptStalkArchive{}, false
	}
	st, err := a.store.GetStack(arch.StackID)
	if err != nil {
		if u.Role == RoleAdmin {
			return arch, true
		}
		writeErr(w, http.StatusNotFound, "no such archive")
		return ptStalkArchive{}, false
	}
	if st.OwnerID != u.ID && u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "not your stack")
		return ptStalkArchive{}, false
	}
	return arch, true
}
