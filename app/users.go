package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// pathID parses the {id} path value as an int64.
func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (a *App) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	// Each row carries its live sessions too (useradmin.go), so the table can say who
	// is around without a call per account.
	stats, _ := a.store.SessionStats()
	type row struct {
		User
		LastSeenAt string `json:"lastSeenAt,omitempty"`
		Sessions   int    `json:"sessions"`
	}
	out := make([]row, 0, len(users))
	for _, u := range users {
		st := stats[u.ID]
		out = append(out, row{User: u, LastSeenAt: st.LastSeen, Sessions: st.Count})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUserStatus returns a handler that transitions a user to the given status.
func (a *App) handleUserStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, _ := a.currentUser(r)
		id, err := pathID(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid user id")
			return
		}
		// You may not change your own status, except approving (a harmless no-op
		// for an already-approved admin).
		if id == me.ID && status != StatusApproved {
			writeErr(w, http.StatusBadRequest, "you cannot change your own status")
			return
		}
		if _, err := a.store.GetUser(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeErr(w, http.StatusNotFound, "user not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "failed to read user")
			return
		}
		u, err := a.store.SetStatus(id, status)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to update user")
			return
		}
		// Revoke active sessions AND API tokens when access is removed. Missing the
		// tokens here would mean "disable" closed the browser and left the API open,
		// which is the one failure mode worth being careful about.
		if status == StatusDisabled || status == StatusRejected {
			a.store.DeleteUserSessions(id)
			a.store.RevokeUserAPITokens(id)
		}
		writeJSON(w, http.StatusOK, u)
	}
}

// handleDeleteUser deletes an account. Its stacks are deleted with it, containers
// included, unless ?transferStacksTo=<user id> hands them to another account first.
func (a *App) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	me, _ := a.currentUser(r)
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if id == me.ID {
		writeErr(w, http.StatusBadRequest, "you cannot delete your own account")
		return
	}
	if _, ok := a.loadUser(w, id); !ok {
		return
	}
	var heir int64
	if v := r.URL.Query().Get("transferStacksTo"); v != "" {
		if heir, err = strconv.ParseInt(v, 10, 64); err != nil || heir == id {
			writeErr(w, http.StatusBadRequest, "transferStacksTo is the id of another account")
			return
		}
		if h, ok := a.loadUser(w, heir); !ok {
			return
		} else if h.Status != StatusApproved {
			writeErr(w, http.StatusBadRequest, "stacks can only go to an approved account")
			return
		}
	}
	stacks, err := a.store.ListStacks(id, false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list the account's stacks")
		return
	}
	// The stack rows go with the account (ON DELETE CASCADE), but the containers they
	// deployed do not: those are torn down here, as DELETE /api/stacks/{id} does.
	for _, st := range stacks {
		if heir != 0 {
			if err := a.store.SetStackOwner(st.ID, heir); err != nil {
				writeErr(w, http.StatusInternalServerError, "failed to transfer a stack")
				return
			}
			continue
		}
		a.teardownStack(st.ID)
		a.store.DeleteStack(st.ID)
		a.store.DeleteAppSetting(dnsCustomKey(st.ID))
	}
	if err := a.store.DeleteUser(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	out := map[string]any{"status": "deleted"}
	if heir != 0 {
		out["stacksTransferred"] = len(stacks)
	} else {
		out["stacksDeleted"] = len(stacks)
	}
	writeJSON(w, http.StatusOK, out)
}

// loadUser reads an account by id, writing the error response itself.
func (a *App) loadUser(w http.ResponseWriter, id int64) (User, bool) {
	u, err := a.store.GetUser(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "user not found")
		} else {
			writeErr(w, http.StatusInternalServerError, "failed to read user")
		}
		return User{}, false
	}
	return u, true
}

// handleGetUser is one account as GET /api/users lists it, plus how many stacks and
// active API tokens it has.
func (a *App) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid user id")
		return
	}
	u, ok := a.loadUser(w, id)
	if !ok {
		return
	}
	stats, _ := a.store.SessionStats()
	stacks, _ := a.store.ListStacks(id, false)
	tokens, _ := a.store.ListAPITokens(id)
	active := 0
	for _, t := range tokens {
		if t.state(time.Now()) == "active" {
			active++
		}
	}
	writeJSON(w, http.StatusOK, struct {
		User
		LastSeenAt string `json:"lastSeenAt,omitempty"`
		Sessions   int    `json:"sessions"`
		Stacks     int    `json:"stacks"`
		Tokens     int    `json:"activeTokens"`
	}{u, stats[id].LastSeen, stats[id].Count, len(stacks), active})
}

// handleUserStacks lists the stacks an account owns.
func (a *App) handleUserStacks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if _, ok := a.loadUser(w, id); !ok {
		return
	}
	stacks, err := a.store.ListStacks(id, false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list stacks")
		return
	}
	writeJSON(w, http.StatusOK, stacks)
}

// handleUserTokens lists an account's API tokens, never their secrets.
func (a *App) handleUserTokens(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if _, ok := a.loadUser(w, id); !ok {
		return
	}
	tokens, err := a.store.ListAPITokens(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleRevokeUserTokens revokes every active API token of an account and tells its
// owner. Sessions and the password are untouched.
func (a *App) handleRevokeUserTokens(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if _, ok := a.loadUser(w, id); !ok {
		return
	}
	if err := a.store.RevokeUserAPITokens(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to revoke tokens")
		return
	}
	a.notify(Notification{UserID: id, Scope: "user", Type: "token.revoked", Severity: "warning",
		Title: "API tokens revoked", Body: "An administrator revoked all of your API tokens."})
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked"})
}

// handleSetStackOwner hands a stack to another approved account (admin only). The
// deployed nodes keep running; only who may see and change the stack moves.
func (a *App) handleSetStackOwner(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		UserID int64 `json:"userId"`
	}
	if err := decode(r, &body); err != nil || body.UserID == 0 {
		writeErr(w, http.StatusBadRequest, "body is {\"userId\": <account id>}")
		return
	}
	u, ok := a.loadUser(w, body.UserID)
	if !ok {
		return
	}
	if u.Status != StatusApproved {
		writeErr(w, http.StatusBadRequest, "a stack can only go to an approved account")
		return
	}
	if err := a.store.SetStackOwner(st.ID, u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to change the owner")
		return
	}
	a.notify(Notification{UserID: u.ID, Scope: "user", Type: "stack.transferred", Severity: "info",
		Title: "Stack transferred to you", Body: "An administrator gave you the stack " + st.Name + "."})
	st.OwnerID = u.ID
	st.Design = nil
	writeJSON(w, http.StatusOK, st)
}
