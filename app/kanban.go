package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// kanban.go — Kanban boards: the Kanban page (web/src/pages/Kanban.jsx).
//
// A person keeps as many boards as they like. Each board has columns, in an order
// they choose, and each column has cards, in an order they choose: a card dropped
// third in a column is third, and stays third. Order is an integer position per
// column (and per board, for columns); a move is one transaction that takes the
// item out, puts it in at the index asked for, and renumbers what is left, so the
// stored order is always exactly what the person last saw.
//
// A board is its owner's. Marked shared, every signed-in user may see and change
// it — its cards, its columns — though only the owner renames it, shares it or
// deletes it. Everything a person writes on a board (its name, column names, card
// titles and descriptions) is sealed at rest like the rest of what they type
// (encryption.go).
//
// rev counts every change to a board, so a page that has it open asks "anything
// since rev N?" every few seconds and gets a one-line answer when nothing moved.

const kanbanSchema = `
CREATE TABLE IF NOT EXISTS kanban_boards (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  shared     INTEGER NOT NULL DEFAULT 0,
  rev        INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kanban_boards_owner ON kanban_boards(owner_id);
CREATE TABLE IF NOT EXISTS kanban_columns (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  board_id  INTEGER NOT NULL REFERENCES kanban_boards(id) ON DELETE CASCADE,
  name      TEXT NOT NULL,
  position  INTEGER NOT NULL,
  wip_limit INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_kanban_columns_board ON kanban_columns(board_id, position);
CREATE TABLE IF NOT EXISTS kanban_cards (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  board_id    INTEGER NOT NULL REFERENCES kanban_boards(id) ON DELETE CASCADE,
  column_id   INTEGER NOT NULL REFERENCES kanban_columns(id) ON DELETE CASCADE,
  title       TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  labels      TEXT NOT NULL DEFAULT '[]',
  assignee_id INTEGER NOT NULL DEFAULT 0,
  due         TEXT NOT NULL DEFAULT '',
  position    INTEGER NOT NULL,
  created_by  INTEGER NOT NULL DEFAULT 0,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kanban_cards_column ON kanban_cards(column_id, position);`

const (
	kanbanNameMax  = 120
	kanbanTitleMax = 300
	kanbanDescMax  = 20000
	kanbanLabelMax = 8
	kanbanLabelLen = 32
	kanbanColsMax  = 30
	kanbanCardsMax = 2000 // per board
)

// kanbanLabelColors is the palette a label is drawn in (Kanban.jsx has the same).
var kanbanLabelColors = map[string]bool{
	"red": true, "orange": true, "yellow": true, "green": true, "teal": true, "blue": true, "purple": true, "pink": true, "gray": true,
}

type KanbanBoard struct {
	ID        int64  `json:"id"`
	OwnerID   int64  `json:"ownerId"`
	OwnerName string `json:"ownerName"`
	Name      string `json:"name"`
	Shared    bool   `json:"shared"`
	Rev       int64  `json:"rev"`
	Cards     int    `json:"cards"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type KanbanColumn struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
	WIPLimit int    `json:"wipLimit"`
}

type KanbanLabel struct {
	Color string `json:"color"`
	Text  string `json:"text"`
}

type KanbanCard struct {
	ID          int64         `json:"id"`
	ColumnID    int64         `json:"columnId"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Labels      []KanbanLabel `json:"labels"`
	AssigneeID  int64         `json:"assigneeId"`
	Due         string        `json:"due"`
	Position    int           `json:"position"`
	CreatedBy   int64         `json:"createdBy"`
	CreatedAt   string        `json:"createdAt"`
	UpdatedAt   string        `json:"updatedAt"`
}

var errKanbanNotFound = errors.New("board not found")

// ------------------------------------------------------------------ store

func (s *Store) kanbanSeal(table, col string, boardID int64, v string) (string, error) {
	return s.sealVal(aadID(table, col, boardID), v)
}

func (s *Store) kanbanOpen(table, col string, boardID int64, v string) (string, error) {
	return s.openVal(aadID(table, col, boardID), v)
}

// bumpBoard records a change: a new rev for anyone polling.
func bumpBoard(tx *sql.Tx, boardID int64) error {
	_, err := tx.Exec(`UPDATE kanban_boards SET rev = rev + 1, updated_at = ? WHERE id = ?`, nowRFC3339(), boardID)
	return err
}

// ListKanbanBoards is the boards a user may open: their own and the shared ones.
func (s *Store) ListKanbanBoards(userID int64) ([]KanbanBoard, error) {
	rows, err := s.db.Query(`SELECT b.id, b.owner_id, b.name, b.shared, b.rev, b.created_at, b.updated_at,
		(SELECT COUNT(*) FROM kanban_cards c WHERE c.board_id = b.id)
		FROM kanban_boards b WHERE b.owner_id = ? OR b.shared = 1 ORDER BY b.owner_id != ?, b.updated_at DESC`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KanbanBoard{}
	for rows.Next() {
		b, err := s.scanKanbanBoard(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if u, err := s.GetUser(out[i].OwnerID); err == nil {
			out[i].OwnerName = u.displayName()
		}
	}
	return out, nil
}

func (s *Store) scanKanbanBoard(row interface{ Scan(...any) error }, withCount bool) (KanbanBoard, error) {
	var b KanbanBoard
	var shared int
	dest := []any{&b.ID, &b.OwnerID, &b.Name, &shared, &b.Rev, &b.CreatedAt, &b.UpdatedAt}
	if withCount {
		dest = append(dest, &b.Cards)
	}
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return b, errKanbanNotFound
		}
		return b, err
	}
	b.Shared = shared != 0
	var err error
	b.Name, err = s.kanbanOpen("kanban_boards", "name", b.ID, b.Name)
	return b, err
}

func (s *Store) GetKanbanBoard(id int64) (KanbanBoard, error) {
	b, err := s.scanKanbanBoard(s.db.QueryRow(`SELECT id, owner_id, name, shared, rev, created_at, updated_at,
		(SELECT COUNT(*) FROM kanban_cards c WHERE c.board_id = kanban_boards.id) FROM kanban_boards WHERE id = ?`, id), true)
	if err == nil {
		if u, err := s.GetUser(b.OwnerID); err == nil {
			b.OwnerName = u.displayName()
		}
	}
	return b, err
}

// CreateKanbanBoard makes a board with the given columns, left to right.
func (s *Store) CreateKanbanBoard(ownerID int64, name string, shared bool, columns []string) (KanbanBoard, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return KanbanBoard{}, err
	}
	defer tx.Rollback()
	now := nowRFC3339()
	res, err := tx.Exec(`INSERT INTO kanban_boards (owner_id, name, shared, created_at, updated_at) VALUES (?,'',?,?,?)`,
		ownerID, boolInt(shared), now, now)
	if err != nil {
		return KanbanBoard{}, err
	}
	id, _ := res.LastInsertId()
	sealed, err := s.kanbanSeal("kanban_boards", "name", id, name)
	if err != nil {
		return KanbanBoard{}, err
	}
	if _, err := tx.Exec(`UPDATE kanban_boards SET name = ? WHERE id = ?`, sealed, id); err != nil {
		return KanbanBoard{}, err
	}
	for i, c := range columns {
		cn, err := s.kanbanSeal("kanban_columns", "name", id, c)
		if err != nil {
			return KanbanBoard{}, err
		}
		if _, err := tx.Exec(`INSERT INTO kanban_columns (board_id, name, position) VALUES (?,?,?)`, id, cn, i); err != nil {
			return KanbanBoard{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return KanbanBoard{}, err
	}
	return s.GetKanbanBoard(id)
}

func (s *Store) UpdateKanbanBoard(id int64, name string, shared bool) error {
	sealed, err := s.kanbanSeal("kanban_boards", "name", id, name)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE kanban_boards SET name = ?, shared = ?, rev = rev + 1, updated_at = ? WHERE id = ?`,
		sealed, boolInt(shared), nowRFC3339(), id)
	return err
}

func (s *Store) DeleteKanbanBoard(id int64) error {
	_, err := s.db.Exec(`DELETE FROM kanban_boards WHERE id = ?`, id)
	return err
}

// KanbanContents is a board's columns and cards, in order.
func (s *Store) KanbanContents(boardID int64) ([]KanbanColumn, []KanbanCard, error) {
	cols := []KanbanColumn{}
	rows, err := s.db.Query(`SELECT id, name, position, wip_limit FROM kanban_columns WHERE board_id = ? ORDER BY position, id`, boardID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var c KanbanColumn
		if err := rows.Scan(&c.ID, &c.Name, &c.Position, &c.WIPLimit); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if c.Name, err = s.kanbanOpen("kanban_columns", "name", boardID, c.Name); err != nil {
			rows.Close()
			return nil, nil, err
		}
		cols = append(cols, c)
	}
	rows.Close()
	cards := []KanbanCard{}
	rows, err = s.db.Query(`SELECT k.id, k.column_id, k.title, k.description, k.labels, k.assignee_id, k.due, k.position,
		k.created_by, k.created_at, k.updated_at
		FROM kanban_cards k JOIN kanban_columns c ON c.id = k.column_id
		WHERE k.board_id = ? ORDER BY c.position, k.position, k.id`, boardID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := s.scanKanbanCard(rows, boardID)
		if err != nil {
			return nil, nil, err
		}
		cards = append(cards, c)
	}
	return cols, cards, rows.Err()
}

func (s *Store) scanKanbanCard(row interface{ Scan(...any) error }, boardID int64) (KanbanCard, error) {
	var c KanbanCard
	var labels string
	if err := row.Scan(&c.ID, &c.ColumnID, &c.Title, &c.Description, &labels, &c.AssigneeID, &c.Due, &c.Position,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c, errKanbanNotFound
		}
		return c, err
	}
	var err error
	if c.Title, err = s.kanbanOpen("kanban_cards", "title", boardID, c.Title); err != nil {
		return c, err
	}
	if c.Description, err = s.kanbanOpen("kanban_cards", "description", boardID, c.Description); err != nil {
		return c, err
	}
	c.Labels = []KanbanLabel{}
	json.Unmarshal([]byte(labels), &c.Labels)
	return c, nil
}

// columnBoard is the board a column belongs to.
func (s *Store) columnBoard(colID int64) (int64, error) {
	var b int64
	err := s.db.QueryRow(`SELECT board_id FROM kanban_columns WHERE id = ?`, colID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errKanbanNotFound
	}
	return b, err
}

// cardBoard is the board a card belongs to.
func (s *Store) cardBoard(cardID int64) (int64, error) {
	var b int64
	err := s.db.QueryRow(`SELECT board_id FROM kanban_cards WHERE id = ?`, cardID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errKanbanNotFound
	}
	return b, err
}

// placeAt reorders ids so that id sits at index (clamped), and writes positions
// 0..n-1 to table for every row in the list.
func placeAt(tx *sql.Tx, table string, ids []int64, id int64, index int) error {
	rest := make([]int64, 0, len(ids)+1)
	for _, x := range ids {
		if x != id {
			rest = append(rest, x)
		}
	}
	if index < 0 || index > len(rest) {
		index = len(rest)
	}
	order := append(append(append([]int64{}, rest[:index]...), id), rest[index:]...)
	for i, x := range order {
		if _, err := tx.Exec(`UPDATE `+table+` SET position = ? WHERE id = ?`, i, x); err != nil {
			return err
		}
	}
	return nil
}

func idsOf(tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) AddKanbanColumn(boardID int64, name string, index int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ids, err := idsOf(tx, `SELECT id FROM kanban_columns WHERE board_id = ? ORDER BY position, id`, boardID)
	if err != nil {
		return 0, err
	}
	if len(ids) >= kanbanColsMax {
		return 0, errors.New("a board has at most 30 columns")
	}
	sealed, err := s.kanbanSeal("kanban_columns", "name", boardID, name)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO kanban_columns (board_id, name, position) VALUES (?,?,?)`, boardID, sealed, len(ids))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if err := placeAt(tx, "kanban_columns", ids, id, index); err != nil {
		return 0, err
	}
	if err := bumpBoard(tx, boardID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) UpdateKanbanColumn(boardID, colID int64, name string, wip int) error {
	sealed, err := s.kanbanSeal("kanban_columns", "name", boardID, name)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE kanban_columns SET name = ?, wip_limit = ? WHERE id = ?`, sealed, wip, colID); err != nil {
		return err
	}
	if err := bumpBoard(tx, boardID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteKanbanColumn(boardID, colID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM kanban_cards WHERE column_id = ?`, colID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM kanban_columns WHERE id = ?`, colID); err != nil {
		return err
	}
	if err := bumpBoard(tx, boardID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MoveKanbanColumn(boardID, colID int64, index int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ids, err := idsOf(tx, `SELECT id FROM kanban_columns WHERE board_id = ? ORDER BY position, id`, boardID)
	if err != nil {
		return err
	}
	if err := placeAt(tx, "kanban_columns", ids, colID, index); err != nil {
		return err
	}
	if err := bumpBoard(tx, boardID); err != nil {
		return err
	}
	return tx.Commit()
}

// AddKanbanCard puts a new card in a column at index (-1: at the bottom).
func (s *Store) AddKanbanCard(boardID, colID int64, c KanbanCard, index int, by int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM kanban_cards WHERE board_id = ?`, boardID).Scan(&n)
	if n >= kanbanCardsMax {
		return 0, errors.New("a board holds at most 2,000 cards")
	}
	ids, err := idsOf(tx, `SELECT id FROM kanban_cards WHERE column_id = ? ORDER BY position, id`, colID)
	if err != nil {
		return 0, err
	}
	title, desc, labels, err := s.sealCard(boardID, c)
	if err != nil {
		return 0, err
	}
	now := nowRFC3339()
	res, err := tx.Exec(`INSERT INTO kanban_cards (board_id, column_id, title, description, labels, assignee_id, due, position,
		created_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		boardID, colID, title, desc, labels, c.AssigneeID, c.Due, len(ids), by, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if index >= 0 {
		if err := placeAt(tx, "kanban_cards", ids, id, index); err != nil {
			return 0, err
		}
	}
	if err := bumpBoard(tx, boardID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) sealCard(boardID int64, c KanbanCard) (title, desc, labels string, err error) {
	if title, err = s.kanbanSeal("kanban_cards", "title", boardID, c.Title); err != nil {
		return
	}
	if desc, err = s.kanbanSeal("kanban_cards", "description", boardID, c.Description); err != nil {
		return
	}
	if c.Labels == nil {
		c.Labels = []KanbanLabel{}
	}
	b, _ := json.Marshal(c.Labels)
	return title, desc, string(b), nil
}

func (s *Store) UpdateKanbanCard(boardID, cardID int64, c KanbanCard) error {
	title, desc, labels, err := s.sealCard(boardID, c)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE kanban_cards SET title = ?, description = ?, labels = ?, assignee_id = ?, due = ?, updated_at = ? WHERE id = ?`,
		title, desc, labels, c.AssigneeID, c.Due, nowRFC3339(), cardID); err != nil {
		return err
	}
	if err := bumpBoard(tx, boardID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteKanbanCard(boardID, cardID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM kanban_cards WHERE id = ?`, cardID); err != nil {
		return err
	}
	if err := bumpBoard(tx, boardID); err != nil {
		return err
	}
	return tx.Commit()
}

// MoveKanbanCard puts a card into a column (its own or another on the same board)
// at index: 0 is the top, past the end is the bottom.
func (s *Store) MoveKanbanCard(boardID, cardID, toCol int64, index int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var from int64
	if err := tx.QueryRow(`SELECT column_id FROM kanban_cards WHERE id = ?`, cardID).Scan(&from); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE kanban_cards SET column_id = ?, updated_at = ? WHERE id = ?`, toCol, nowRFC3339(), cardID); err != nil {
		return err
	}
	ids, err := idsOf(tx, `SELECT id FROM kanban_cards WHERE column_id = ? AND id != ? ORDER BY position, id`, toCol, cardID)
	if err != nil {
		return err
	}
	if err := placeAt(tx, "kanban_cards", ids, cardID, index); err != nil {
		return err
	}
	if from != toCol {
		left, err := idsOf(tx, `SELECT id FROM kanban_cards WHERE column_id = ? ORDER BY position, id`, from)
		if err != nil {
			return err
		}
		for i, x := range left {
			if _, err := tx.Exec(`UPDATE kanban_cards SET position = ? WHERE id = ?`, i, x); err != nil {
				return err
			}
		}
	}
	if err := bumpBoard(tx, boardID); err != nil {
		return err
	}
	return tx.Commit()
}

// ------------------------------------------------------------------ handlers

// kanbanBoardFor loads the board a request is about and checks the caller may use
// it: owner for anything, any signed-in user for a shared board's contents.
// ownerOnly is for renaming, sharing and deleting the board itself.
func (a *App) kanbanBoardFor(w http.ResponseWriter, r *http.Request, boardID int64, ownerOnly bool) (KanbanBoard, User, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return KanbanBoard{}, u, false
	}
	b, err := a.store.GetKanbanBoard(boardID)
	if err != nil || (b.OwnerID != u.ID && !b.Shared) {
		writeErr(w, http.StatusNotFound, "board not found")
		return KanbanBoard{}, u, false
	}
	if ownerOnly && b.OwnerID != u.ID {
		writeErr(w, http.StatusForbidden, "only the board's owner can do that")
		return KanbanBoard{}, u, false
	}
	return b, u, true
}

func kanbanPathID(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

func cleanText(s string, max int, what string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New(what + " cannot be empty")
	}
	if utf8.RuneCountInString(s) > max {
		return "", errors.New(what + " is too long")
	}
	return s, nil
}

func (a *App) handleKanbanBoards(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	list, err := a.store.ListKanbanBoards(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list boards")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) handleKanbanCreateBoard(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var in struct {
		Name    string   `json:"name"`
		Shared  bool     `json:"shared"`
		Columns []string `json:"columns"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name, err := cleanText(in.Name, kanbanNameMax, "a board's name")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cols := []string{}
	for _, c := range in.Columns {
		if c, err := cleanText(c, kanbanNameMax, "a column's name"); err == nil {
			cols = append(cols, c)
		}
	}
	if len(in.Columns) == 0 {
		cols = []string{"To do", "In progress", "Done"}
	}
	if len(cols) > kanbanColsMax {
		cols = cols[:kanbanColsMax]
	}
	b, err := a.store.CreateKanbanBoard(u.ID, name, in.Shared, cols)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create the board")
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (a *App) handleKanbanGetBoard(w http.ResponseWriter, r *http.Request) {
	b, _, ok := a.kanbanBoardFor(w, r, kanbanPathID(r, "id"), false)
	if !ok {
		return
	}
	// A poll with the rev it already has gets a one-liner when nothing changed.
	if since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && since == b.Rev {
		writeJSON(w, http.StatusOK, map[string]any{"unchanged": true, "rev": b.Rev})
		return
	}
	cols, cards, err := a.store.KanbanContents(b.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read the board")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"board": b, "columns": cols, "cards": cards})
}

func (a *App) handleKanbanUpdateBoard(w http.ResponseWriter, r *http.Request) {
	b, _, ok := a.kanbanBoardFor(w, r, kanbanPathID(r, "id"), true)
	if !ok {
		return
	}
	var in struct {
		Name   *string `json:"name"`
		Shared *bool   `json:"shared"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name, shared := b.Name, b.Shared
	if in.Name != nil {
		n, err := cleanText(*in.Name, kanbanNameMax, "a board's name")
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		name = n
	}
	if in.Shared != nil {
		shared = *in.Shared
	}
	if err := a.store.UpdateKanbanBoard(b.ID, name, shared); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the board")
		return
	}
	nb, _ := a.store.GetKanbanBoard(b.ID)
	writeJSON(w, http.StatusOK, nb)
}

func (a *App) handleKanbanDeleteBoard(w http.ResponseWriter, r *http.Request) {
	b, _, ok := a.kanbanBoardFor(w, r, kanbanPathID(r, "id"), true)
	if !ok {
		return
	}
	if err := a.store.DeleteKanbanBoard(b.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete the board")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": b.ID})
}

func (a *App) handleKanbanAddColumn(w http.ResponseWriter, r *http.Request) {
	b, _, ok := a.kanbanBoardFor(w, r, kanbanPathID(r, "id"), false)
	if !ok {
		return
	}
	var in struct {
		Name  string `json:"name"`
		Index *int   `json:"index"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name, err := cleanText(in.Name, kanbanNameMax, "a column's name")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	idx := -1
	if in.Index != nil {
		idx = *in.Index
	}
	id, err := a.store.AddKanbanColumn(b.ID, name, idx)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// kanbanColumnFor resolves a column and the board it is on, with the board's check.
func (a *App) kanbanColumnFor(w http.ResponseWriter, r *http.Request) (KanbanBoard, int64, bool) {
	cid := kanbanPathID(r, "cid")
	bid, err := a.store.columnBoard(cid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "column not found")
		return KanbanBoard{}, 0, false
	}
	b, _, ok := a.kanbanBoardFor(w, r, bid, false)
	return b, cid, ok
}

func (a *App) handleKanbanUpdateColumn(w http.ResponseWriter, r *http.Request) {
	b, cid, ok := a.kanbanColumnFor(w, r)
	if !ok {
		return
	}
	var in struct {
		Name     string `json:"name"`
		WIPLimit int    `json:"wipLimit"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name, err := cleanText(in.Name, kanbanNameMax, "a column's name")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.WIPLimit < 0 || in.WIPLimit > 999 {
		writeErr(w, http.StatusBadRequest, "a WIP limit is 0 (none) to 999")
		return
	}
	if err := a.store.UpdateKanbanColumn(b.ID, cid, name, in.WIPLimit); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the column")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": cid})
}

func (a *App) handleKanbanDeleteColumn(w http.ResponseWriter, r *http.Request) {
	b, cid, ok := a.kanbanColumnFor(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteKanbanColumn(b.ID, cid); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete the column")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": cid})
}

func (a *App) handleKanbanMoveColumn(w http.ResponseWriter, r *http.Request) {
	b, cid, ok := a.kanbanColumnFor(w, r)
	if !ok {
		return
	}
	var in struct {
		Index int `json:"index"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := a.store.MoveKanbanColumn(b.ID, cid, in.Index); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to move the column")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": cid})
}

// cardInput is what a card is made of, checked.
type cardInput struct {
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Labels      []KanbanLabel `json:"labels"`
	AssigneeID  int64         `json:"assigneeId"`
	Due         string        `json:"due"`
	Index       *int          `json:"index"`
}

func (a *App) checkCard(in cardInput) (KanbanCard, error) {
	title, err := cleanText(in.Title, kanbanTitleMax, "a card's title")
	if err != nil {
		return KanbanCard{}, err
	}
	if utf8.RuneCountInString(in.Description) > kanbanDescMax {
		return KanbanCard{}, errors.New("the description is too long")
	}
	if len(in.Labels) > kanbanLabelMax {
		return KanbanCard{}, errors.New("a card has at most 8 labels")
	}
	labels := []KanbanLabel{}
	for _, l := range in.Labels {
		l.Text = strings.TrimSpace(l.Text)
		if !kanbanLabelColors[l.Color] || utf8.RuneCountInString(l.Text) > kanbanLabelLen {
			return KanbanCard{}, errors.New("a label is a palette colour and at most 32 characters")
		}
		labels = append(labels, l)
	}
	due := strings.TrimSpace(in.Due)
	if due != "" {
		if len(due) != 10 || due[4] != '-' || due[7] != '-' {
			return KanbanCard{}, errors.New("a due date is YYYY-MM-DD")
		}
	}
	if in.AssigneeID != 0 {
		if u, err := a.store.GetUser(in.AssigneeID); err != nil || u.Status != StatusApproved {
			return KanbanCard{}, errors.New("no such person to assign")
		}
	}
	return KanbanCard{Title: title, Description: strings.TrimRight(in.Description, " \n\t"), Labels: labels, AssigneeID: in.AssigneeID, Due: due}, nil
}

func (a *App) handleKanbanAddCard(w http.ResponseWriter, r *http.Request) {
	b, cid, ok := a.kanbanColumnFor(w, r)
	if !ok {
		return
	}
	u, _ := a.currentUser(r)
	var in cardInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	c, err := a.checkCard(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	idx := -1
	if in.Index != nil {
		idx = *in.Index
	}
	id, err := a.store.AddKanbanCard(b.ID, cid, c, idx, u.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// kanbanCardFor resolves a card and its board, with the board's check.
func (a *App) kanbanCardFor(w http.ResponseWriter, r *http.Request) (KanbanBoard, int64, bool) {
	id := kanbanPathID(r, "kid")
	bid, err := a.store.cardBoard(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "card not found")
		return KanbanBoard{}, 0, false
	}
	b, _, ok := a.kanbanBoardFor(w, r, bid, false)
	return b, id, ok
}

func (a *App) handleKanbanUpdateCard(w http.ResponseWriter, r *http.Request) {
	b, id, ok := a.kanbanCardFor(w, r)
	if !ok {
		return
	}
	var in cardInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	c, err := a.checkCard(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.UpdateKanbanCard(b.ID, id, c); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the card")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (a *App) handleKanbanDeleteCard(w http.ResponseWriter, r *http.Request) {
	b, id, ok := a.kanbanCardFor(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteKanbanCard(b.ID, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete the card")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (a *App) handleKanbanMoveCard(w http.ResponseWriter, r *http.Request) {
	b, id, ok := a.kanbanCardFor(w, r)
	if !ok {
		return
	}
	var in struct {
		ColumnID int64 `json:"columnId"`
		Index    int   `json:"index"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if cb, err := a.store.columnBoard(in.ColumnID); err != nil || cb != b.ID {
		writeErr(w, http.StatusBadRequest, "a card moves to a column on its own board")
		return
	}
	if err := a.store.MoveKanbanCard(b.ID, id, in.ColumnID, in.Index); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to move the card")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleKanbanPeople is who a card can be assigned to: every approved account, by
// name and avatar (and nothing else about them).
func (a *App) handleKanbanPeople(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	users, err := a.store.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list people")
		return
	}
	out := []map[string]any{}
	for _, u := range users {
		if u.Status == StatusApproved {
			out = append(out, map[string]any{"id": u.ID, "name": u.displayName(), "username": u.Username, "avatar": u.Avatar})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
