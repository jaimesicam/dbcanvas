package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
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
	writeJSON(w, http.StatusOK, users)
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
		// Revoke active sessions when access is removed.
		if status == StatusDisabled || status == StatusRejected {
			a.store.DeleteUserSessions(id)
		}
		writeJSON(w, http.StatusOK, u)
	}
}

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
	if err := a.store.DeleteUser(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}
