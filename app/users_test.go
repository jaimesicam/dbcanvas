package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// adminCall runs an admin handler with {id} set.
func adminCall(app *App, admin User, h func(*App, http.ResponseWriter, *http.Request), method, target, body string, id int64) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := withPrincipal(httptest.NewRequest(method, target, strings.NewReader(body)), principal{User: admin})
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	h(app, w, r)
	return w
}

func TestUserStacksTokensAndTransfer(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	ada, _ := app.store.CreateUser("ada", "x", RoleUser, StatusApproved)
	bob, _ := app.store.CreateUser("bob", "x", RoleUser, StatusApproved)
	pending, _ := app.store.CreateUser("pat", "x", RoleUser, StatusPending)
	s1, _ := app.store.CreateStack("one", ada.ID, "8h", nil, []byte(defaultDesign))
	app.store.CreateStack("two", ada.ID, "8h", nil, []byte(defaultDesign))
	app.store.CreateAPIToken(ada.ID, "ci", "hash1", "dbc_aaaa", "read", nil)

	w := adminCall(app, admin, (*App).handleGetUser, "GET", "/api/users/x", "", ada.ID)
	var got struct {
		Username     string
		Stacks       int
		ActiveTokens int `json:"activeTokens"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got.Username != "ada" || got.Stacks != 2 || got.ActiveTokens != 1 {
		t.Errorf("get user: %d %s", w.Code, w.Body)
	}
	if w := adminCall(app, admin, (*App).handleGetUser, "GET", "/api/users/x", "", 9999); w.Code != http.StatusNotFound {
		t.Errorf("missing user: %d", w.Code)
	}
	if w := adminCall(app, admin, (*App).handleUserStacks, "GET", "/x", "", ada.ID); !strings.Contains(w.Body.String(), `"two"`) {
		t.Errorf("user stacks: %s", w.Body)
	}

	// Revoking an account's tokens.
	if w := adminCall(app, admin, (*App).handleRevokeUserTokens, "DELETE", "/x", "", ada.ID); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	toks, _ := app.store.ListAPITokens(ada.ID)
	if len(toks) != 1 || toks[0].RevokedAt == nil {
		t.Errorf("token not revoked: %+v", toks)
	}

	// One stack handed over; only to an approved account.
	if w := adminCall(app, admin, (*App).handleSetStackOwner, "POST", "/x", `{"userId":`+strconv.FormatInt(pending.ID, 10)+`}`, s1.ID); w.Code != http.StatusBadRequest {
		t.Errorf("transfer to a pending account: %d", w.Code)
	}
	if w := adminCall(app, admin, (*App).handleSetStackOwner, "POST", "/x", `{"userId":`+strconv.FormatInt(bob.ID, 10)+`}`, s1.ID); w.Code != http.StatusOK {
		t.Fatalf("transfer: %d %s", w.Code, w.Body)
	}
	if st, _ := app.store.GetStack(s1.ID); st.OwnerID != bob.ID {
		t.Errorf("owner is %d", st.OwnerID)
	}

	// Deleting ada hands the remaining stack to bob instead of deleting it.
	if w := adminCall(app, admin, (*App).handleDeleteUser, "DELETE", "/x?transferStacksTo="+strconv.FormatInt(ada.ID, 10), "", ada.ID); w.Code != http.StatusBadRequest {
		t.Errorf("transfer to the account being deleted: %d", w.Code)
	}
	w = adminCall(app, admin, (*App).handleDeleteUser, "DELETE", "/x?transferStacksTo="+strconv.FormatInt(bob.ID, 10), "", ada.ID)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"stacksTransferred":1`) {
		t.Fatalf("delete with transfer: %d %s", w.Code, w.Body)
	}
	if st, _ := app.store.ListStacks(bob.ID, false); len(st) != 2 {
		t.Errorf("bob has %d stacks", len(st))
	}
	// Without a transfer, the stacks go with the account.
	w = adminCall(app, admin, (*App).handleDeleteUser, "DELETE", "/x", "", bob.ID)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"stacksDeleted":2`) {
		t.Errorf("delete: %d %s", w.Code, w.Body)
	}
}
