package main

import (
	"fmt"
	"os"
	"strings"
)

// cmd_password.go — `dbcanvas password`.
//
// The endpoint is cookie-only (see app/password.go), so this cannot simply send the
// stored token: it signs in with the current password, changes it, and signs out.
// That is not a workaround, it is the feature — a token being unable to change the
// password is what stops a leaked token becoming an account takeover, and the CLI
// should not look like it has a way around that.
//
// The new password is asked for twice, because a typo in a value nothing echoes is
// otherwise discovered at the next sign-in.

func cmdPassword(args []string) error {
	fs := flagsFor("password")
	user := fs.String("user", "",
		"which account; only needed when the profile does not name one (a token from the environment, or after --revoke-tokens removed it)")
	revoke := fs.Bool("revoke-tokens", false,
		"also revoke every API token on your account (including this CLI's)")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	p, name, err := cfg.resolve(g.profile, g.url, g.token)
	if err != nil {
		return err
	}

	who := *user
	if who == "" {
		who = p.User
	}
	if who == "" {
		// A token from the environment carries no username, and neither does a
		// profile whose token was just revoked, so ask.
		if who, err = prompt("Username: "); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "Changing the password for %s on %s.\n", who, p.URL)
	}

	current, err := promptSecret("Current password: ")
	if err != nil {
		return err
	}
	next, err := promptSecret("New password: ")
	if err != nil {
		return err
	}
	again, err := promptSecret("New password again: ")
	if err != nil {
		return err
	}
	if next != again {
		return fmt.Errorf("the two new passwords do not match")
	}
	// Checked here so the round trip is not spent on something the server would
	// refuse anyway, with the same wording it would have used.
	if len(next) < 8 {
		return fmt.Errorf("your new password must be at least 8 characters")
	}
	if strings.TrimSpace(next) != next {
		return fmt.Errorf("your new password starts or ends with a space — remove it, " +
			"or that space becomes part of the password")
	}

	c := newClient(p.URL, "")
	if err := c.withCookies(); err != nil {
		return err
	}
	if err := c.post("/api/auth/login", map[string]string{
		"username": strings.TrimSpace(who), "password": current,
	}, nil); err != nil {
		return err
	}
	defer func() { c.post("/api/auth/logout", nil, nil) }()

	var res struct {
		TokensRevoked int `json:"tokensRevoked"`
	}
	if err := c.post("/api/me/password", map[string]any{
		"currentPassword": current, "newPassword": next, "revokeTokens": *revoke,
	}, &res); err != nil {
		return err
	}

	fmt.Println("Password changed. Every other session was signed out.")
	if *revoke {
		fmt.Printf("Revoked %d API token(s), including this one — run `dbcanvas login` to get a new one.\n",
			res.TokensRevoked)
		// The stored token is dead, so leaving it in the config would only produce a
		// confusing 401 later. Drop it and say so.
		if name != "" {
			delete(cfg.Profiles, name)
			if cfg.Current == name {
				cfg.Current = ""
				for other := range cfg.Profiles {
					cfg.Current = other
					break
				}
			}
			if err := saveConfig(cfg); err == nil {
				fmt.Printf("Removed the now-dead token from profile %q.\n", name)
			}
		}
	} else {
		fmt.Println("Your API tokens still work. Pass --revoke-tokens if you wanted them gone too.")
	}
	return nil
}
