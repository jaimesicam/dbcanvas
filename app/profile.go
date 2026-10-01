package main

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"unicode"

	"dbcanvas/internal/seal"
)

// profile.go — who a person is, beyond a username: a first and last name, an email
// address and an avatar.
//
// The names and the email are a person's own, so they are sealed at rest like a
// password hash (encryption.go, users.first_name / last_name / email). One email is
// one account: users.email_hash, a hash of the address in lower case, is unique.
// The avatar is one of a fixed set of pictures (web/src/components/Avatar.jsx draws
// them), stored by id: nothing is uploaded, so there is nothing to scan or serve.
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
	Email     string `json:"email"`
	Avatar    string `json:"avatar"`
}

const profileEmailMax = 254

// ErrEmailTaken is an email address another account already has.
var ErrEmailTaken = errors.New("that email address belongs to another account")

func emailHash(email string) string {
	if email == "" {
		return ""
	}
	return seal.HashSecret("dbcanvas-user-email:" + strings.ToLower(email))
}

// clean trims the fields and checks every one. required makes the names and the
// email mandatory, as they are for a new account.
func (p *Profile) clean(required bool) error {
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
	if required && (p.FirstName == "" || p.LastName == "") {
		return errors.New("first name and last name are required")
	}
	if required && p.Email == "" {
		return errors.New("an email address is required")
	}
	if p.Email != "" {
		if len(p.Email) > profileEmailMax {
			return errors.New("that email address is too long")
		}
		if a, err := mail.ParseAddress(p.Email); err != nil || a.Address != p.Email {
			return errors.New("that is not a valid email address")
		}
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

// EmailInUse reports whether an account other than exceptID has this email.
func (s *Store) EmailInUse(email string, exceptID int64) (bool, error) {
	if email == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email_hash = ? AND id != ?`, emailHash(email), exceptID).Scan(&n)
	return n > 0, err
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
	_, err = s.db.Exec("UPDATE users SET first_name = ?, last_name = ?, email = ?, email_hash = ?, avatar = ? WHERE id = ?",
		first, last, email, emailHash(p.Email), p.Avatar, id)
	if isUniqueViolation(err) {
		return ErrEmailTaken
	}
	return err
}

// handleUpdateProfile is a signed-in user changing their own names, email and avatar.
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
		if errors.Is(err, ErrEmailTaken) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
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
