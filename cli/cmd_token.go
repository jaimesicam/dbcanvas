package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cmd_token.go — managing tokens with a token.
//
// Listing and revoking work with a bearer token; creating does not, deliberately
// (see apitokens.go: POST /api/tokens is marked NoToken). So `token create` here
// goes through the same password handshake `login` does — asked for at the moment
// it is needed, rather than pretending a token can do something it cannot.

type token struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Username   string `json:"username"`
	Prefix     string `json:"prefix"`
	Scope      string `json:"scope"`
	CreatedAt  string `json:"createdAt"`
	ExpiresAt  string `json:"expiresAt"`
	LastUsedAt string `json:"lastUsedAt"`
	State      string `json:"state"`
}

func cmdToken(args []string) error {
	return sub("token", args, map[string]func([]string) error{
		"list":   tokenList,
		"create": tokenCreate,
		"revoke": tokenRevoke,
	}, []string{"list", "create", "revoke"})
}

func tokenList(args []string) error {
	fs := flagsFor("token list")
	all := fs.Bool("all", false, "every token on the instance, with its owner (administrators only)")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	path := "/api/tokens"
	if *all {
		path = "/api/admin/tokens"
	}
	var raw []byte
	if err := c.get(path, &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var res struct {
		Tokens  []token  `json:"tokens"`
		Scopes  []string `json:"scopes"`
		MaxDays int      `json:"maxDays"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if len(res.Tokens) == 0 {
		empty("tokens", "Create one with `dbcanvas token create --name \"my script\"`.")
		return nil
	}
	head := []string{"id", "name", "scope", "state", "expires", "last used"}
	if *all {
		head = []string{"id", "owner", "name", "scope", "state", "expires", "last used"}
	}
	t := newTable(head...)
	for _, tk := range res.Tokens {
		row := []string{strconv.FormatInt(tk.ID, 10)}
		if *all {
			owner := tk.Username
			if owner == "" {
				owner = "-"
			}
			row = append(row, owner)
		}
		exp := untilText(tk.ExpiresAt)
		if tk.ExpiresAt == "" {
			exp = "never"
		}
		row = append(row, truncate(tk.Name, 30), tk.Scope, tk.State, exp, shortDate(tk.LastUsedAt))
		t.add(row...)
	}
	t.print()
	if !*all && res.MaxDays > 0 {
		fmt.Printf("\nThis installation allows lifetimes up to %d days. Scopes you can create: %s.\n",
			res.MaxDays, strings.Join(res.Scopes, ", "))
	}
	return nil
}

func tokenCreate(args []string) error {
	fs := flagsFor("token create")
	name := fs.String("name", "", "what the token is for")
	scope := fs.String("scope", "write", "read, write or admin")
	days := fs.Int("expires", 90, "lifetime in days; 0 = never (administrators only)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "dbcanvas: --name is required — an unnamed token is one nobody dares revoke")
		return errUsage
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	p, _, err := cfg.resolve(g.profile, g.url, g.token)
	if err != nil {
		return err
	}

	// Creating a token needs a password, so ask for one rather than sending the
	// token we have and reporting the server's refusal.
	fmt.Fprintf(os.Stderr, "Creating a token requires your password (a token cannot create tokens).\n")
	user := p.User
	if user == "" {
		if user, err = prompt("Username: "); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "Username: %s\n", user)
	}
	password, err := promptSecret("Password: ")
	if err != nil {
		return err
	}

	c := newClient(p.URL, "")
	if err := c.withCookies(); err != nil {
		return err
	}
	if err := c.post("/api/auth/login", map[string]string{
		"username": strings.TrimSpace(user), "password": password}, nil); err != nil {
		return err
	}
	defer func() { c.post("/api/auth/logout", nil, nil) }()

	var created struct {
		Token  token  `json:"token"`
		Secret string `json:"secret"`
	}
	if err := c.post("/api/tokens", map[string]any{
		"name": *name, "scope": *scope, "days": *days}, &created); err != nil {
		return err
	}
	if g.json {
		return printJSON(created)
	}
	exp := "never expires"
	if created.Token.ExpiresAt != "" {
		exp = "expires " + shortDate(created.Token.ExpiresAt)
	}
	fmt.Printf("Created %q (id %d), %s scope, %s.\n\n%s\n\n",
		created.Token.Name, created.Token.ID, created.Token.Scope, exp, created.Secret)
	fmt.Println("Copy it now — the server keeps only a hash, so it cannot be shown again.")
	return nil
}

func tokenRevoke(args []string) error {
	fs := flagsFor("token revoke")
	admin := fs.Bool("admin", false, "revoke somebody else's token (administrators only)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a token id (see `dbcanvas token list`)"); err != nil {
		return err
	}
	id, err := strconv.ParseInt(arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not a token id — `dbcanvas token list` shows them", arg(0))
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/tokens/%d", id)
	if *admin {
		path = fmt.Sprintf("/api/admin/tokens/%d", id)
	}
	if err := c.delete(path, nil); err != nil {
		return err
	}
	fmt.Printf("Revoked token %d. It stops working immediately.\n", id)
	return nil
}
