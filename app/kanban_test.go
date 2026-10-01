package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type kanbanFixture struct {
	app      *App
	ann, bob User
	boardID  int64
	cols     []int64
	t        *testing.T
}

func (f *kanbanFixture) do(u User, method, path string, vals map[string]string, body string, h func(*App) http.HandlerFunc) *httptest.ResponseRecorder {
	f.t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range vals {
		r.SetPathValue(k, v)
	}
	h(f.app)(w, withPrincipal(r, principal{User: u}))
	return w
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

func newKanbanFixture(t *testing.T) *kanbanFixture {
	app := newTestApp(t)
	ann, _ := app.store.CreateUser("ann", "x", RoleUser, StatusApproved)
	bob, _ := app.store.CreateUser("bob", "x", RoleUser, StatusApproved)
	f := &kanbanFixture{app: app, ann: ann, bob: bob, t: t}
	w := f.do(ann, "POST", "/api/kanban/boards", nil, `{"name":"Release 1.0"}`, m((*App).handleKanbanCreateBoard))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var b KanbanBoard
	json.Unmarshal(w.Body.Bytes(), &b)
	f.boardID = b.ID
	cols, _, _ := app.store.KanbanContents(b.ID)
	for _, c := range cols {
		f.cols = append(f.cols, c.ID)
	}
	return f
}

func (f *kanbanFixture) addCard(col int64, title string) int64 {
	f.t.Helper()
	w := f.do(f.ann, "POST", "/", map[string]string{"cid": id(col)}, `{"title":"`+title+`"}`, m((*App).handleKanbanAddCard))
	if w.Code != http.StatusCreated {
		f.t.Fatalf("add card: %d %s", w.Code, w.Body)
	}
	var out struct{ ID int64 }
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.ID
}

// order is the titles of a column's cards, top to bottom.
func (f *kanbanFixture) order(col int64) string {
	_, cards, _ := f.app.store.KanbanContents(f.boardID)
	var out []string
	for _, c := range cards {
		if c.ColumnID == col {
			out = append(out, c.Title)
		}
	}
	return strings.Join(out, ",")
}

func TestABoardStartsWithThreeColumns(t *testing.T) {
	f := newKanbanFixture(t)
	cols, _, _ := f.app.store.KanbanContents(f.boardID)
	var names []string
	for _, c := range cols {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "To do,In progress,Done" {
		t.Errorf("columns: %v", names)
	}
}

func TestCardsKeepTheOrderTheyAreDroppedIn(t *testing.T) {
	f := newKanbanFixture(t)
	todo, doing := f.cols[0], f.cols[1]
	a, b, c := f.addCard(todo, "A"), f.addCard(todo, "B"), f.addCard(todo, "C")
	if f.order(todo) != "A,B,C" {
		t.Fatalf("added: %s", f.order(todo))
	}
	move := func(card, col int64, index int) {
		t.Helper()
		w := f.do(f.ann, "POST", "/", map[string]string{"kid": id(card)},
			`{"columnId":`+id(col)+`,"index":`+strconv.Itoa(index)+`}`, m((*App).handleKanbanMoveCard))
		if w.Code != http.StatusOK {
			t.Fatalf("move: %d %s", w.Code, w.Body)
		}
	}
	move(c, todo, 0) // C to the top
	if f.order(todo) != "C,A,B" {
		t.Errorf("C to the top: %s", f.order(todo))
	}
	move(c, todo, 2) // and back to the bottom
	if f.order(todo) != "A,B,C" {
		t.Errorf("C to the bottom: %s", f.order(todo))
	}
	move(b, doing, 0)
	move(a, doing, 1) // A under B in the other column
	if f.order(todo) != "C" || f.order(doing) != "B,A" {
		t.Errorf("across: todo %s, doing %s", f.order(todo), f.order(doing))
	}
	move(c, doing, 1) // C between B and A
	if f.order(doing) != "B,C,A" {
		t.Errorf("between: %s", f.order(doing))
	}
	move(a, doing, 99) // past the end is the bottom
	if f.order(doing) != "B,C,A" {
		t.Errorf("past the end: %s", f.order(doing))
	}
	// An insert at a place, as an undo of a delete does.
	w := f.do(f.ann, "POST", "/", map[string]string{"cid": id(doing)}, `{"title":"D","index":1}`, m((*App).handleKanbanAddCard))
	if w.Code != http.StatusCreated || f.order(doing) != "B,D,C,A" {
		t.Errorf("insert at 1: %d %s", w.Code, f.order(doing))
	}
}

func TestColumnsReorder(t *testing.T) {
	f := newKanbanFixture(t)
	w := f.do(f.ann, "POST", "/", map[string]string{"cid": id(f.cols[2])}, `{"index":0}`, m((*App).handleKanbanMoveColumn))
	cols, _, _ := f.app.store.KanbanContents(f.boardID)
	if w.Code != http.StatusOK || cols[0].Name != "Done" || cols[1].Name != "To do" {
		t.Errorf("move column: %d %+v", w.Code, cols)
	}
}

func TestPrivateBoardsArePrivate(t *testing.T) {
	f := newKanbanFixture(t)
	vals := map[string]string{"id": id(f.boardID)}
	if w := f.do(f.bob, "GET", "/", vals, "", m((*App).handleKanbanGetBoard)); w.Code != http.StatusNotFound {
		t.Errorf("bob read ann's private board: %d", w.Code)
	}
	if w := f.do(f.bob, "POST", "/", map[string]string{"cid": id(f.cols[0])}, `{"title":"x"}`, m((*App).handleKanbanAddCard)); w.Code != http.StatusNotFound {
		t.Errorf("bob wrote to ann's private board: %d", w.Code)
	}
	list, _ := f.app.store.ListKanbanBoards(f.bob.ID)
	if len(list) != 0 {
		t.Errorf("bob's list shows %d boards", len(list))
	}
	// Shared: bob works on it, but the board itself stays ann's.
	f.do(f.ann, "PUT", "/", vals, `{"shared":true}`, m((*App).handleKanbanUpdateBoard))
	if w := f.do(f.bob, "POST", "/", map[string]string{"cid": id(f.cols[0])}, `{"title":"from bob"}`, m((*App).handleKanbanAddCard)); w.Code != http.StatusCreated {
		t.Errorf("bob could not add to a shared board: %d", w.Code)
	}
	if w := f.do(f.bob, "DELETE", "/", vals, "", m((*App).handleKanbanDeleteBoard)); w.Code != http.StatusForbidden {
		t.Errorf("bob deleted ann's board: %d", w.Code)
	}
	if w := f.do(f.bob, "PUT", "/", vals, `{"name":"mine now"}`, m((*App).handleKanbanUpdateBoard)); w.Code != http.StatusForbidden {
		t.Errorf("bob renamed ann's board: %d", w.Code)
	}
}

func TestACardMovesOnlyWithinItsBoard(t *testing.T) {
	f := newKanbanFixture(t)
	card := f.addCard(f.cols[0], "A")
	other, _ := f.app.store.CreateKanbanBoard(f.ann.ID, "Other", false, []string{"X"})
	ocols, _, _ := f.app.store.KanbanContents(other.ID)
	w := f.do(f.ann, "POST", "/", map[string]string{"kid": id(card)}, `{"columnId":`+id(ocols[0].ID)+`,"index":0}`, m((*App).handleKanbanMoveCard))
	if w.Code != http.StatusBadRequest {
		t.Errorf("a card crossed boards: %d", w.Code)
	}
}

func TestKanbanTextIsSealed(t *testing.T) {
	f := newKanbanFixture(t)
	f.do(f.ann, "POST", "/", map[string]string{"cid": id(f.cols[0])},
		`{"title":"rotate the prod password","description":"it is hunter2","labels":[{"color":"red","text":"urgent"}],"due":"2026-10-31"}`,
		m((*App).handleKanbanAddCard))
	var title, desc, board, col string
	f.app.store.db.QueryRow(`SELECT title, description FROM kanban_cards`).Scan(&title, &desc)
	f.app.store.db.QueryRow(`SELECT name FROM kanban_boards`).Scan(&board)
	f.app.store.db.QueryRow(`SELECT name FROM kanban_columns LIMIT 1`).Scan(&col)
	for _, v := range []string{title, desc, board, col} {
		if !strings.HasPrefix(v, "v1:") {
			t.Errorf("stored in the clear: %q", v)
		}
	}
	_, cards, _ := f.app.store.KanbanContents(f.boardID)
	if len(cards) != 1 || cards[0].Description != "it is hunter2" || cards[0].Labels[0].Text != "urgent" || cards[0].Due != "2026-10-31" {
		t.Errorf("read back: %+v", cards)
	}
}

func TestCardInputIsChecked(t *testing.T) {
	f := newKanbanFixture(t)
	for _, body := range []string{
		`{"title":""}`,
		`{"title":"x","labels":[{"color":"chartreuse"}]}`,
		`{"title":"x","due":"tomorrow"}`,
		`{"title":"x","assigneeId":9999}`,
	} {
		if w := f.do(f.ann, "POST", "/", map[string]string{"cid": id(f.cols[0])}, body, m((*App).handleKanbanAddCard)); w.Code != http.StatusBadRequest {
			t.Errorf("%s accepted: %d", body, w.Code)
		}
	}
}

func TestAPollWithNothingNewIsCheap(t *testing.T) {
	f := newKanbanFixture(t)
	b, _ := f.app.store.GetKanbanBoard(f.boardID)
	vals := map[string]string{"id": id(f.boardID)}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/?since="+strconv.FormatInt(b.Rev, 10), nil)
	r.SetPathValue("id", vals["id"])
	f.app.handleKanbanGetBoard(w, withPrincipal(r, principal{User: f.ann}))
	if !strings.Contains(w.Body.String(), `"unchanged":true`) {
		t.Errorf("unchanged poll: %s", w.Body)
	}
	f.addCard(f.cols[0], "A")
	nb, _ := f.app.store.GetKanbanBoard(f.boardID)
	if nb.Rev <= b.Rev {
		t.Error("a new card did not move the rev")
	}
}

func TestCardsAndColumnsHaveColours(t *testing.T) {
	f := newKanbanFixture(t)
	w := f.do(f.ann, "POST", "/", map[string]string{"cid": id(f.cols[0])}, `{"title":"A","color":"teal"}`, m((*App).handleKanbanAddCard))
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if w := f.do(f.ann, "POST", "/", map[string]string{"cid": id(f.cols[0])}, `{"title":"B","color":"neon"}`, m((*App).handleKanbanAddCard)); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown colour was accepted: %d", w.Code)
	}
	if w := f.do(f.ann, "PUT", "/", map[string]string{"cid": id(f.cols[1])}, `{"color":"purple"}`, m((*App).handleKanbanUpdateColumn)); w.Code != http.StatusOK {
		t.Fatalf("recolour: %d %s", w.Code, w.Body)
	}
	cols, cards, _ := f.app.store.KanbanContents(f.boardID)
	if cards[0].Color != "teal" || cols[1].Color != "purple" || cols[1].Name != "In progress" {
		t.Errorf("colours: card %q, column %+v", cards[0].Color, cols[1])
	}
}
