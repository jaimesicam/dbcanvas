package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// cmd_auth.go — login, logout, whoami.
//
// `login` is three requests, in this order, and the order is the design:
//
//	POST /api/auth/login    →  a session cookie, held in memory
//	POST /api/tokens        →  an API token, which is what gets stored
//	POST /api/auth/logout   →  the session is discarded immediately
//
// The extra round trip buys a much better credential to leave on a laptop. A stored
// password is unscoped, unexpiring, invisible to the person it belongs to, and can
// create anything; the token that replaces it is scoped, expires, appears in the
// UI's token list, and — because POST /api/tokens refuses bearer auth — cannot mint
// a longer-lived replacement for itself if it leaks.

func cmdLogin(args []string) error {
	fs := flagsFor("login")
	url := fs.String("url", "", "DBCanvas base URL (e.g. http://localhost:8080)")
	user := fs.String("user", "", "username (prompted for if omitted)")
	profile := fs.String("profile", "", "name for this installation in the config (default \"default\")")
	scope := fs.String("scope", "write", "token scope: read, write or admin")
	expires := fs.Int("expires", 90, "token lifetime in days; 0 = never (administrators only)")
	name := fs.String("name", "", "token name (default \"dbcanvas-cli on <hostname>\")")
	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// --url wins; otherwise reuse whatever this profile already pointed at, so
	// re-authenticating an expired token is just `dbcanvas login`.
	baseURL := *url
	if baseURL == "" && *profile != "" {
		baseURL = cfg.Profiles[*profile].URL
	}
	if baseURL == "" {
		baseURL = cfg.Profiles[cfg.Current].URL
	}
	if baseURL == "" {
		baseURL = os.Getenv("DBCANVAS_URL")
	}
	if baseURL == "" {
		return fmt.Errorf("which installation? pass --url http://localhost:8080")
	}

	username := *user
	if username == "" {
		if username, err = prompt("Username: "); err != nil {
			return err
		}
	}
	password, err := promptSecret("Password: ")
	if err != nil {
		return err
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return fmt.Errorf("a username and password are both required")
	}

	c := newClient(baseURL, "")
	if err := c.withCookies(); err != nil {
		return err
	}

	// 1. Password sign-in. The cookie lives in this process and nowhere else.
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := c.post("/api/auth/login", map[string]string{
		"username": strings.TrimSpace(username), "password": password,
	}, &me); err != nil {
		return err
	}
	// From here on the session must be cleaned up whatever happens, including on
	// the error paths below — leaving a live session behind would undo the point.
	defer func() { c.post("/api/auth/logout", nil, nil) }()

	tokenName := *name
	if tokenName == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "this machine"
		}
		tokenName = "dbcanvas-cli on " + host
	}

	// 2. Exchange it for a token. This is the request that requires a password
	// sign-in; a token cannot make this call, by design.
	var created struct {
		Token struct {
			Name      string `json:"name"`
			Prefix    string `json:"prefix"`
			Scope     string `json:"scope"`
			ExpiresAt string `json:"expiresAt"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	if err := c.post("/api/tokens", map[string]any{
		"name": tokenName, "scope": *scope, "days": *expires,
	}, &created); err != nil {
		return err
	}
	if created.Secret == "" {
		return fmt.Errorf("the server created a token but returned no secret")
	}

	// 3. Store the token — never the password.
	pname := *profile
	if pname == "" {
		pname = "default"
	}
	exp := created.Token.ExpiresAt
	cfg.Profiles[pname] = Profile{
		URL: strings.TrimRight(baseURL, "/"), Token: created.Secret,
		User: me.Username, Scope: created.Token.Scope, Expires: exp,
	}
	cfg.Current = pname
	if err := saveConfig(cfg); err != nil {
		return err
	}

	path, _ := configPath()
	expText := "never expires"
	if exp != "" {
		expText = "expires " + shortDate(exp)
	}
	fmt.Printf("Created token %q (%s…), %s scope, %s.\n",
		created.Token.Name, created.Token.Prefix, created.Token.Scope, expText)
	fmt.Printf("Saved to %s as profile %q.\n", path, pname)
	return nil
}

func cmdLogout(args []string) error {
	fs := flagsFor("logout")
	if err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	name := g.profile
	if name == "" {
		name = cfg.Current
	}
	p, ok := cfg.Profiles[name]
	if !ok || p.Token == "" {
		fmt.Println("Not signed in.")
		return nil
	}

	// Revoke server-side as well as locally. Deleting the local copy of a live
	// credential is not logging out — it is losing track of it.
	revoked := false
	c := newClient(p.URL, p.Token)
	var mine struct {
		Tokens []struct {
			ID     int64  `json:"id"`
			Prefix string `json:"prefix"`
			State  string `json:"state"`
		} `json:"tokens"`
	}
	if err := c.get("/api/tokens", &mine); err == nil {
		for _, tk := range mine.Tokens {
			if tk.State == "active" && strings.HasPrefix(p.Token, tk.Prefix) {
				if c.delete(fmt.Sprintf("/api/tokens/%d", tk.ID), nil) == nil {
					revoked = true
				}
			}
		}
	}

	delete(cfg.Profiles, name)
	if cfg.Current == name {
		cfg.Current = ""
		for other := range cfg.Profiles {
			cfg.Current = other
			break
		}
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	if revoked {
		fmt.Printf("Revoked the token and removed profile %q.\n", name)
	} else {
		// Say so plainly: the local copy is gone but something may still be live.
		fmt.Printf("Removed profile %q locally. The token could not be revoked on the server — "+
			"revoke it from the API page if it is still active.\n", name)
	}
	return nil
}

func cmdWhoami(args []string) error {
	fs := flagsFor("whoami")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var me struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Status   string `json:"status"`
	}
	if err := c.get("/api/me", &me); err != nil {
		return err
	}
	if g.json {
		return printJSON(me)
	}

	cfg, _ := loadConfig()
	name := g.profile
	if name == "" {
		name = cfg.Current
	}
	p := cfg.Profiles[name]

	fmt.Printf("%-12s %s\n", "account", me.Username)
	fmt.Printf("%-12s %s\n", "role", me.Role)
	fmt.Printf("%-12s %s\n", "server", c.BaseURL)
	if name != "" {
		fmt.Printf("%-12s %s\n", "profile", name)
	}
	// The token's own details, when it came from the config. A token from the
	// environment is not ours to describe, so nothing is printed for it.
	if p.Scope != "" {
		fmt.Printf("%-12s %s\n", "token scope", p.Scope)
	}
	if p.Expires != "" {
		fmt.Printf("%-12s %s\n", "token expiry", shortDate(p.Expires))
	} else if p.Token != "" {
		fmt.Printf("%-12s %s\n", "token expiry", "never")
	}
	var st struct {
		Version string `json:"version"`
	}
	if c.get("/api/setup/status", &st) == nil && st.Version != "" {
		fmt.Printf("%-12s %s\n", "version", st.Version)
	}
	return nil
}

// ------------------------------------------------------------- prompts

// stdinReader returns a buffered reader over the current os.Stdin, created once per
// underlying file.
//
// One shared reader, not one per prompt, and that is not tidiness: bufio reads
// ahead, so a fresh reader per prompt swallows the lines the *next* prompt needs.
// With three prompts in a row that means `printf 'old\nnew\nnew\n' | dbcanvas
// password` fails on the second one with EOF, having already consumed the answer.
// Keyed on the file so a test that swaps os.Stdin gets a reader over the new pipe.
var (
	stdinFile *os.File
	stdinBuf  *bufio.Reader
)

func stdinReader() *bufio.Reader {
	if stdinBuf == nil || stdinFile != os.Stdin {
		stdinFile = os.Stdin
		stdinBuf = bufio.NewReader(os.Stdin)
	}
	return stdinBuf
}

func prompt(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	line, err := stdinReader().ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("could not read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// promptSecret reads a password without echoing it.
//
// When stdin is not a terminal it falls back to reading a line, which is what makes
// `echo $PW | dbcanvas login --user x` work in a script. That is a deliberate
// convenience and a documented one — but CI should use DBCANVAS_TOKEN and not have a
// password to pipe in the first place.
func promptSecret(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := stdinReader().ReadString('\n')
		// A last line with no newline is still a password; only a genuinely empty
		// read is an error.
		if err != nil && line == "" {
			return "", fmt.Errorf("could not read a password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, label)
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("could not read the password: %w", err)
	}
	return string(raw), nil
}
