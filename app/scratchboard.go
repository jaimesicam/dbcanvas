package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// scratchboard.go — the boards a person chose to keep on the Scratchboard page
// (web/src/pages/Scratchboard.jsx).
//
// Nothing is kept unless the "scratchboard" preference is on (settings.go),
// and it is off by default: a board is a place to paste certificates, Secrets and
// connection strings, and the default is that none of that outlives the browser tab.
// With the preference on, each board is still temporary until it is marked to be
// kept; only those are sent here. Turning the preference off deletes every board
// this account kept (handleUpdateSettings).
//
// A board is stored whole — its name, view and cards — as one JSON document the page
// writes, sealed at rest (encryption.go). The server never reads inside it.
//
// rev counts a board's saves. The page sends the rev it last saw (?rev=N); when the
// stored board has moved on — the same board saved from another tab or browser — the
// save is refused with the stored copy, and the page asks which one to keep, rather
// than the last tab to save quietly overwriting the other's work.

const scratchboardSchema = `
CREATE TABLE IF NOT EXISTS scratchboards (
  board_id   TEXT PRIMARY KEY,
  data       TEXT NOT NULL,
  rev        INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL
);`

const (
	scratchboardMaxBytes  = 8 << 20 // one board's document
	scratchboardMaxBoards = 50      // kept boards
)

// The page names its boards; the id is only ever a key.
var scratchboardIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func aadScratchboard(boardID string) string {
	return "scratchboards.data:" + boardID
}

// ScratchboardDoc is one kept board: the page's own document, as it sent it.
type ScratchboardDoc struct {
	ID        string          `json:"id"`
	Data      json.RawMessage `json:"data"`
	Rev       int64           `json:"rev"`
	UpdatedAt string          `json:"updatedAt"`
}

// ------------------------------------------------------------------ store

func (s *Store) Scratchboards() ([]ScratchboardDoc, error) {
	rows, err := s.db.Query(`SELECT board_id, data, rev, updated_at FROM scratchboards ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScratchboardDoc{}
	for rows.Next() {
		var d ScratchboardDoc
		var sealed string
		if err := rows.Scan(&d.ID, &sealed, &d.Rev, &d.UpdatedAt); err != nil {
			return nil, err
		}
		pt, err := s.openVal(aadScratchboard(d.ID), sealed)
		if err != nil {
			return nil, err
		}
		d.Data = json.RawMessage(pt)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Scratchboard is one kept board, or ok false when there is none.
func (s *Store) Scratchboard(boardID string) (ScratchboardDoc, bool, error) {
	d := ScratchboardDoc{ID: boardID}
	var sealed string
	err := s.db.QueryRow(`SELECT data, rev, updated_at FROM scratchboards WHERE board_id = ?`, boardID).
		Scan(&sealed, &d.Rev, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	pt, err := s.openVal(aadScratchboard(boardID), sealed)
	if err != nil {
		return d, false, err
	}
	d.Data = json.RawMessage(pt)
	return d, true, nil
}

func (s *Store) countScratchboards(except string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM scratchboards WHERE board_id != ?`, except).Scan(&n)
	return n, err
}

// errScratchboardMoved is a conditional save against a rev that is no longer the stored one.
var errScratchboardMoved = errors.New("the board was saved from somewhere else")

// SaveScratchboard stores a board and returns its new rev. With baseRev >= 0 the save
// only happens when the stored rev is baseRev (0: no stored board); otherwise it is
// errScratchboardMoved. baseRev < 0 saves unconditionally.
func (s *Store) SaveScratchboard(boardID, data string, baseRev int64) (int64, string, error) {
	sealed, err := s.sealVal(aadScratchboard(boardID), data)
	if err != nil {
		return 0, "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	var cur int64
	err = tx.QueryRow(`SELECT rev FROM scratchboards WHERE board_id = ?`, boardID).Scan(&cur)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, "", err
	}
	if baseRev >= 0 && cur != baseRev {
		return cur, "", errScratchboardMoved
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`INSERT INTO scratchboards (board_id, data, rev, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(board_id) DO UPDATE SET data = excluded.data, rev = excluded.rev, updated_at = excluded.updated_at`,
		boardID, sealed, cur+1, now); err != nil {
		return 0, "", err
	}
	return cur + 1, now, tx.Commit()
}

func (s *Store) DeleteScratchboard(boardID string) error {
	_, err := s.db.Exec(`DELETE FROM scratchboards WHERE board_id = ?`, boardID)
	return err
}

// DeleteScratchboards removes every kept board.
func (s *Store) DeleteScratchboards() error {
	_, err := s.db.Exec(`DELETE FROM scratchboards`)
	return err
}

// ------------------------------------------------------------------ handlers

// scratchboardKeep is whether boards are kept at all; ok is false when the request is
// not signed in.
func (a *App) scratchboardKeep(w http.ResponseWriter, r *http.Request) (keep, ok bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return false, false
	}
	return a.userSettingsFor(u.ID).Scratchboard == ScratchboardStoreOn, true
}

func (a *App) handleScratchboards(w http.ResponseWriter, r *http.Request) {
	keep, ok := a.scratchboardKeep(w, r)
	if !ok {
		return
	}
	if !keep {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "boards": []ScratchboardDoc{}})
		return
	}
	boards, err := a.store.Scratchboards()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read the kept boards")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "boards": boards})
}

func (a *App) handleSaveScratchboard(w http.ResponseWriter, r *http.Request) {
	keep, ok := a.scratchboardKeep(w, r)
	if !ok {
		return
	}
	if !keep {
		writeErr(w, http.StatusConflict, "keeping Scratchboard boards is off in Settings")
		return
	}
	id := r.PathValue("bid")
	if !scratchboardIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "invalid board id")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, scratchboardMaxBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(raw) > scratchboardMaxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "the board is larger than 8 MiB — remove a card or two to keep it")
		return
	}
	if !json.Valid(raw) || len(raw) == 0 || raw[0] != '{' {
		writeErr(w, http.StatusBadRequest, "a board is a JSON object")
		return
	}
	if n, err := a.store.countScratchboards(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the board")
		return
	} else if n >= scratchboardMaxBoards {
		writeErr(w, http.StatusConflict, "you already keep 50 boards — delete one or mark it temporary first")
		return
	}
	base := int64(-1)
	if v := r.URL.Query().Get("rev"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "rev is a whole number")
			return
		}
		base = n
	}
	rev, at, err := a.store.SaveScratchboard(id, string(raw), base)
	if errors.Is(err, errScratchboardMoved) {
		// The stored copy goes back with the refusal, so the page can offer it.
		cur, ok, _ := a.store.Scratchboard(id)
		body := map[string]any{"error": "this board was saved from another tab or browser since this one loaded it", "conflict": true, "rev": rev}
		if ok {
			body["board"] = cur
		}
		writeJSON(w, http.StatusConflict, body)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the board")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "rev": rev, "updatedAt": at})
}

// handleDeleteScratchboard is allowed with keeping off too: deleting is never refused.
func (a *App) handleDeleteScratchboard(w http.ResponseWriter, r *http.Request) {
	_, ok := a.scratchboardKeep(w, r)
	if !ok {
		return
	}
	id := r.PathValue("bid")
	if !scratchboardIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "invalid board id")
		return
	}
	if err := a.store.DeleteScratchboard(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete the board")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
