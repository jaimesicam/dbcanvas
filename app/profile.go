package main

import (
	"errors"
	"net/http"
	"strings"
	"unicode"
)

// profile.go — who a person is, beyond a username: a first and last name and an
// avatar.
//
// The names are a person's own, so they are sealed at rest like a password hash
// (encryption.go, users.first_name / users.last_name). The avatar is one of a fixed
// set of pictures (web/src/components/Avatar.jsx draws them), stored by id: nothing
// is uploaded, so there is nothing to scan, resize or serve.
//
// A profile is asked for when an account is created (setup, register). Accounts
// from before it have none until their owner fills it in; the app asks them once
// after they sign in, and the username stands in meanwhile.

// avatarIDs is every avatar there is. web/src/components/Avatar.jsx lists the same
// ids, in the same order; profile_test.go checks the two agree.
var avatarIDs = []string{
	"fox", "panda", "koala", "owl", "octopus", "whale", "lion", "tiger",
	"frog", "monkey", "penguin", "unicorn", "bee", "turtle", "butterfly", "wolf",
	"bear", "rabbit", "dino", "dragon", "cat", "dog", "rocket", "cactus",
	"clover", "rainbow", "robot", "alien",
}

func validAvatar(id string) bool {
	if id == "" {
		return true // none chosen: initials stand in
	}
	for _, a := range avatarIDs {
		if a == id {
			return true
		}
	}
	return false
}

const profileNameMax = 60

// Profile is the part of an account its owner describes.
type Profile struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Avatar    string `json:"avatar"`
}

// clean trims the names and checks every field. required makes both names
// mandatory, as they are for a new account.
func (p *Profile) clean(required bool) error {
	p.FirstName = strings.TrimSpace(p.FirstName)
	p.LastName = strings.TrimSpace(p.LastName)
	p.Avatar = strings.TrimSpace(p.Avatar)
	for _, n := range []string{p.FirstName, p.LastName} {
		if len([]rune(n)) > profileNameMax {
			return errors.New("a name is at most 60 characters")
		}
		for _, r := range n {
			if unicode.IsControl(r) {
				return errors.New("a name cannot contain control characters")
			}
		}
	}
	if required && (p.FirstName == "" || p.LastName == "") {
		return errors.New("first name and last name are required")
	}
	if !validAvatar(p.Avatar) {
		return errors.New("unknown avatar")
	}
	return nil
}

// displayName is how a person is shown: their full name when they gave one, else
// their username.
func (u User) displayName() string {
	if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
		return n
	}
	return u.Username
}

// SetUserProfile writes a user's names (sealed) and avatar.
func (s *Store) SetUserProfile(id int64, p Profile) error {
	first, err := s.sealVal(aadID("users", "first_name", id), p.FirstName)
	if err != nil {
		return err
	}
	last, err := s.sealVal(aadID("users", "last_name", id), p.LastName)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE users SET first_name = ?, last_name = ?, avatar = ? WHERE id = ?", first, last, p.Avatar, id)
	return err
}

// handleUpdateProfile is a signed-in user changing their own names and avatar.
func (a *App) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var p Profile
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := p.clean(true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.SetUserProfile(u.ID, p); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the profile")
		return
	}
	nu, err := a.store.GetUser(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read the profile back")
		return
	}
	writeJSON(w, http.StatusOK, nu)
}

// handleAvatars lists the avatar ids, for a client that draws its own.
func (a *App) handleAvatars(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"avatars": avatarIDs})
}
