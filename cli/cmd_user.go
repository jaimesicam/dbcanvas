package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cmd_user.go — `dbcanvas user list | create`, for administrators.
//
// Listing works with the stored token (an admin-scoped one). Creating does not, and
// that is the server's rule, not this file's: POST /api/users is marked NoToken
// (app/useradmin.go), because a leaked admin token must not be able to mint accounts —
// least of all an admin account for itself. So `user create` does what `token create`
// and `password` do: asks for the administrator's password at the moment it is needed,
// signs in for the one request, and signs out.
//
// The new account gets either a temporary password, which the owner has to replace at
// their first sign-in (`dbcanvas login` walks them through it), or with --invite a
// one-time link, valid for 7 days, to choose their own in a browser.

type userRow struct {
	ID                 int64  `json:"id"`
	Username           string `json:"username"`
	Role               string `json:"role"`
	Status             string `json:"status"`
	FirstName          string `json:"firstName"`
	LastName           string `json:"lastName"`
	Email              string `json:"email"`
	MustChangePassword bool   `json:"mustChangePassword"`
	InvitePending      bool   `json:"invitePending"`
	LastLoginAt        string `json:"lastLoginAt"`
	Sessions           int    `json:"sessions"`
}

func cmdUser(args []string) error {
	return sub("user", args, map[string]func([]string) error{
		"list":   userList,
		"create": userCreate,
	}, []string{"list", "create"})
}

func userList(args []string) error {
	fs := flagsFor("user list")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/users", &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var users []userRow
	if err := json.Unmarshal(raw, &users); err != nil {
		return err
	}
	if len(users) == 0 {
		empty("accounts", "Create one with `dbcanvas user create <username> --first … --last … --email …`.")
		return nil
	}
	t := newTable("id", "username", "name", "email", "role", "status", "last sign-in", "sessions")
	for _, u := range users {
		status := u.Status
		switch {
		case u.InvitePending:
			status += " (invited)"
		case u.MustChangePassword:
			status += " (must change password)"
		}
		last := shortDate(u.LastLoginAt)
		if u.LastLoginAt == "" {
			last = "-"
		}
		name := strings.TrimSpace(u.FirstName + " " + u.LastName)
		if name == "" {
			name = "-"
		}
		t.add(strconv.FormatInt(u.ID, 10), u.Username, truncate(name, 28), truncate(u.Email, 32),
			u.Role, status, last, strconv.Itoa(u.Sessions))
	}
	t.print()
	return nil
}

func userCreate(args []string) error {
	fs := flagsFor("user create")
	first := fs.String("first", "", "first name (required)")
	last := fs.String("last", "", "last name (required)")
	email := fs.String("email", "", "email address (required)")
	role := fs.String("role", "user", "user or admin")
	invite := fs.Bool("invite", false,
		"no temporary password: print a one-time link (valid 7 days) for the owner to choose one in a browser")
	admin := fs.String("admin", "",
		"the administrator signing in to do this; only needed when the profile does not name one")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a username for the new account"); err != nil {
		return err
	}
	username := strings.TrimSpace(arg(0))
	// The server checks all of this too; checking here first means the administrator
	// is not asked for two passwords only to be told a name was missing.
	var missing []string
	for _, f := range []struct{ flag, val string }{{"--first", *first}, {"--last", *last}, {"--email", *email}} {
		if strings.TrimSpace(f.val) == "" {
			missing = append(missing, f.flag)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "dbcanvas: %s required — every account carries a name and an email address\n",
			strings.Join(missing, ", "))
		return errUsage
	}
	if *role != "user" && *role != "admin" {
		return fmt.Errorf(`--role must be "user" or "admin", not %q`, *role)
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	p, _, err := cfg.resolve(g.profile, g.url, g.token)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Creating an account requires an administrator's password (a token cannot create accounts).\n")
	who := *admin
	if who == "" {
		who = p.User
	}
	if who == "" {
		if who, err = prompt("Administrator username: "); err != nil {
			return err
		}
	}
	adminPW, err := promptSecret(fmt.Sprintf("Password for %s: ", who))
	if err != nil {
		return err
	}
	temp := ""
	if !*invite {
		if temp, err = promptNewPassword("Temporary password for "+username, "the password"); err != nil {
			return err
		}
	}

	c := newClient(p.URL, "")
	if err := c.withCookies(); err != nil {
		return err
	}
	var me struct {
		Role               string `json:"role"`
		MustChangePassword bool   `json:"mustChangePassword"`
	}
	if err := c.post("/api/auth/login", map[string]string{
		"username": strings.TrimSpace(who), "password": adminPW}, &me); err != nil {
		return err
	}
	defer func() { c.post("/api/auth/logout", nil, nil) }()
	// Both of these would be refused by the server anyway, with a less useful message.
	if me.MustChangePassword {
		return fmt.Errorf("%s has to choose their own password before doing anything else — "+
			"run `dbcanvas login --user %s` first", who, who)
	}
	if me.Role != "admin" {
		return fmt.Errorf("%s is not an administrator — only an administrator can create accounts", who)
	}

	var created struct {
		User      userRow `json:"user"`
		URL       string  `json:"url"`
		ExpiresAt string  `json:"expiresAt"`
	}
	if err := c.post("/api/users", map[string]any{
		"username": username, "role": *role, "password": temp,
		"firstName": strings.TrimSpace(*first), "lastName": strings.TrimSpace(*last),
		"email": strings.TrimSpace(*email),
	}, &created); err != nil {
		return err
	}
	if g.json {
		return printJSON(created)
	}
	fmt.Printf("Created %s (id %d, %s).\n", created.User.Username, created.User.ID, created.User.Role)
	if created.URL != "" {
		fmt.Printf("\nSend them this one-time link to choose a password (expires %s):\n\n  %s\n\n",
			shortDate(created.ExpiresAt), created.URL)
		fmt.Printf("Once they have, they can sign in from the CLI with:\n\n")
	} else {
		fmt.Printf("\nThey must replace the temporary password at their first sign-in, which this does:\n\n")
	}
	fmt.Printf("  dbcanvas login --url %s --user %s --profile %s\n", p.URL, created.User.Username, created.User.Username)
	return nil
}

// promptNewPassword asks for a password twice — a typo in a value nothing echoes is
// otherwise discovered at the next sign-in — and applies the server's rules first, in
// its words, so the round trip is not spent on something it would refuse. subject
// names the password in those messages ("your new password", "the password").
func promptNewPassword(label, subject string) (string, error) {
	next, err := promptSecret(label + ": ")
	if err != nil {
		return "", err
	}
	again, err := promptSecret(label + " again: ")
	if err != nil {
		return "", err
	}
	if next != again {
		return "", fmt.Errorf("the two passwords do not match")
	}
	if len(next) < 8 {
		return "", fmt.Errorf("%s must be at least 8 characters", subject)
	}
	if strings.TrimSpace(next) != next {
		return "", fmt.Errorf("%s starts or ends with a space — remove it, "+
			"or that space becomes part of the password", subject)
	}
	return next, nil
}
