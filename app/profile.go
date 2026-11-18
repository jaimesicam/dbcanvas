package main

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"unicode"
)

// profile.go — who a person is, beyond a username: a first and last name, an email
// address and an avatar.
//
// The names and the email are a person's own, so they are sealed at rest
// (encryption.go, users.first_name / last_name / email). The avatar is one of a fixed
// set of pictures (web/src/components/Avatar.jsx draws them), stored by id: nothing is
// uploaded, so there is nothing to scan or serve.
//
// The account starts with no profile; the app asks for one once, and the username
// stands in meanwhile. It is what guests see when you share a session.

// avatarIDs is every avatar there is. web/src/components/Avatar.jsx lists the same
// ids, in the same order; profile_test.go checks the two agree.
var avatarIDs = []string{
	"fox", "panda", "koala", "owl", "octopus", "whale", "lion", "tiger",
	"frog", "monkey", "penguin", "unicorn", "bee", "turtle", "butterfly", "wolf",
	"bear", "rabbit", "dino", "dragon", "cat", "dog", "rocket", "cactus",
	"clover", "rainbow", "robot", "alien", "otter", "hedgehog", "sloth", "flamingo",
	"parrot", "peacock", "hamster", "chick", "elephant", "giraffe", "zebra", "deer",
	"shark", "dolphin", "crab", "snail", "mushroom", "sunflower", "planet", "coffee",
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
	Email     string `json:"email"`
	Avatar    string `json:"avatar"`
}

const profileEmailMax = 254

// clean trims the fields and checks every one. The names and the email are required.
func (p *Profile) clean() error {
	p.FirstName = strings.TrimSpace(p.FirstName)
	p.LastName = strings.TrimSpace(p.LastName)
	p.Avatar = strings.TrimSpace(p.Avatar)
	p.Email = strings.TrimSpace(p.Email)
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
	if p.FirstName == "" || p.LastName == "" {
		return errors.New("first name and last name are required")
	}
	if p.Email == "" {
		return errors.New("an email address is required")
	}
	if len(p.Email) > profileEmailMax {
		return errors.New("that email address is too long")
	}
	if a, err := mail.ParseAddress(p.Email); err != nil || a.Address != p.Email {
		return errors.New("that is not a valid email address")
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

// SetUserProfile writes a user's names and email (sealed) and avatar.
func (s *Store) SetUserProfile(id int64, p Profile) error {
	first, err := s.sealVal(aadID("users", "first_name", id), p.FirstName)
	if err != nil {
		return err
	}
	last, err := s.sealVal(aadID("users", "last_name", id), p.LastName)
	if err != nil {
		return err
	}
	email, err := s.sealVal(aadID("users", "email", id), p.Email)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE users SET first_name = ?, last_name = ?, email = ?, avatar = ? WHERE id = ?",
		first, last, email, p.Avatar, id)
	return err
}

// handleUpdateProfile changes the names, email and avatar.
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
	if err := p.clean(); err != nil {
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
