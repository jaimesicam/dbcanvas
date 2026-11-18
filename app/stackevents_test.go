package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDeletedStackAnswersGone(t *testing.T) {
	app := newTestApp(t)
	ann, _ := app.store.CreateUser("ann")
	st, _ := app.store.CreateStack("lab", "4h", nil, []byte(`{}`))
	app.store.SetStackLabels(st.ID, map[string]string{"run": "7"})
	st, _ = app.store.GetStack(st.ID)

	ch := stackEvents.subscribe()
	defer stackEvents.unsubscribe(ch)
	if err := app.deleteStackRecord(st); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-ch:
		if e.Type != "deleted" || e.StackID != st.ID || e.Labels["run"] != "7" {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no deleted event")
	}

	get := func(u User) *httptest.ResponseRecorder {
		r := withPrincipal(httptest.NewRequest("GET", "/api/stacks/"+strconv.FormatInt(st.ID, 10), nil), principal{User: u})
		r.SetPathValue("id", strconv.FormatInt(st.ID, 10))
		w := httptest.NewRecorder()
		app.loadOwnedStack(w, r)
		return w
	}
	w := get(ann)
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "was deleted") || !strings.Contains(w.Body.String(), `"name":"lab"`) {
		t.Fatalf("deleted: %d %s", w.Code, w.Body)
	}
	// Old records go.
	app.store.db.Exec("UPDATE stack_tombstones SET deleted_at = '2020-01-01T00:00:00Z'")
	app.store.PurgeStackTombstones()
	if w := get(ann); w.Code != http.StatusNotFound {
		t.Fatalf("after the retention: %d", w.Code)
	}
}

func TestStackEventBusDropsForSlowReaders(t *testing.T) {
	ch := stackEvents.subscribe()
	defer stackEvents.unsubscribe(ch)
	for i := 0; i < 200; i++ { // more than the buffer: publish must not block
		stackEvents.publish(StackEvent{Type: "created", StackID: int64(i)})
	}
	if len(ch) != cap(ch) {
		t.Fatalf("buffered %d of %d", len(ch), cap(ch))
	}
}
