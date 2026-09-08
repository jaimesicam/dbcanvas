package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// password.go — changing your own password.
//
// Three decisions, and each one is the answer to a way this goes wrong:
//
//   - **The current password is always required.** Being signed in is not enough. A
//     session somebody else got hold of should not be able to change the password and
//     lock the owner out of their own account, which is precisely the move an attacker
//     makes with a stolen session.
//   - **An API token cannot do it** (NoToken on the route). Same asymmetry as token
//     creation: a leaked token must not be able to escalate itself into a full account
//     takeover. The password is the thing that guards the password.
//   - **Other sessions are signed out, the current one is kept.** The reason to change
//     a password is usually that somebody else might have it; leaving their browser
//     signed in would defeat the exercise. Signing out the tab you just used to do it
//     would be gratuitous.
//
// API tokens are deliberately *not* revoked by default. Rotating a password on a
// schedule should not silently break a CI job — but a password changed because it
// leaked is a different situation, so the caller can ask for it.
//
// An administrator resetting somebody *else's* password is a different problem with a
// different answer: `dbcanvas_reset_password` in the image (see cmd/), which needs no
// working login at all because the case it exists for is nobody having one.

type passwordChange struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
	// RevokeTokens additionally revokes every API token the account holds. Off by
	// default; the UI explains when to turn it on.
	RevokeTokens bool `json:"revokeTokens"`
}

// SetUserPassword replaces an account's password hash.
func (s *Store) SetUserPassword(id int64, hash string) error {
	res, err := s.db.Exec("UPDATE users SET password_hash = ? WHERE id = ?", hash, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteUserSessionsExcept signs out every session an account has apart from one.
// Passing "" signs out all of them, which is what DeleteUserSessions already does —
// this exists for the case where the caller is holding one of them.
func (s *Store) DeleteUserSessionsExcept(userID int64, keep string) error {
	if keep == "" {
		return s.DeleteUserSessions(userID)
	}
	_, err := s.db.Exec("DELETE FROM sessions WHERE user_id = ? AND token != ?", userID, keep)
	return err
}

func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var in passwordChange
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Re-read the stored hash rather than trusting the session: this is the check
	// the whole endpoint exists for.
	_, hash, err := a.store.CredByUsername(u.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read your account")
		return
	}
	if !checkPassword(hash, in.CurrentPassword) {
		// Deliberately not "wrong password" in the abstract — the caller is signed
		// in, so the only thing in question is what they typed here.
		writeErr(w, http.StatusForbidden, "that is not your current password")
		return
	}
	// The same floor as every other place a password is set (see credentials.validate),
	// because a rule that applies at sign-up and not at change is not a rule.
	if len(in.NewPassword) < minPasswordLen {
		writeErr(w, http.StatusBadRequest, "your new password must be at least 8 characters")
		return
	}
	if in.NewPassword == in.CurrentPassword {
		writeErr(w, http.StatusBadRequest, "that is already your password")
		return
	}
	// Leading or trailing whitespace in a password is almost always a paste
	// accident, and it is the kind that locks somebody out of their own account
	// tomorrow. Refuse rather than silently trim: trimming would mean the password
	// they think they set is not the one that works.
	if strings.TrimSpace(in.NewPassword) != in.NewPassword {
		writeErr(w, http.StatusBadRequest,
			"your new password starts or ends with a space — remove it, or that space becomes part of the password")
		return
	}

	newHash, err := hashPassword(in.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to hash the new password")
		return
	}
	if err := a.store.SetUserPassword(u.ID, newHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "your account no longer exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to save the new password")
		return
	}

	// Sign out everywhere else, keeping this session so the page the user is
	// looking at keeps working.
	keep := ""
	if c, err := r.Cookie(cookieName); err == nil {
		keep = c.Value
	}
	a.store.DeleteUserSessionsExcept(u.ID, keep)

	revoked := 0
	if in.RevokeTokens {
		if tokens, err := a.store.ListAPITokens(u.ID); err == nil {
			for _, t := range tokens {
				if t.State == "active" {
					revoked++
				}
			}
		}
		a.store.RevokeUserAPITokens(u.ID)
	}

	body := "Your password was changed and every other session was signed out."
	if in.RevokeTokens {
		body = "Your password was changed, every other session was signed out, and your API tokens were revoked."
	}
	// A notification the owner sees even if the change was not theirs — which is the
	// only warning they would get.
	a.notify(Notification{UserID: u.ID, Scope: "user", Type: "password.changed", Severity: "warning",
		Title: "Password changed", Body: body})

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"tokensRevoked":  revoked,
		"sessionsSigned": true,
	})
}
