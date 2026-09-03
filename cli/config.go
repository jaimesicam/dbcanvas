package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// config.go — where the CLI keeps its credential, and what it deliberately does not keep.
//
// The file holds a token per named installation, and never a password. That is the
// whole point of `login` doing the extra round trip through POST /api/tokens: the
// credential left on disk is scoped, expiring, listed in the UI and revocable from
// anywhere, none of which is true of a stored password.
//
// Mode 0600, and re-asserted on every write, because a config file that starts
// private and later becomes world-readable (a careless umask, a restored backup,
// a copied dotfiles repo) is a credential leak nobody notices.

const configPerm = 0o600

// Profile is one DBCanvas installation.
type Profile struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// User and Scope are cosmetic — `dbcanvas whoami` prints them without a round
	// trip, and they make the file legible to whoever opens it. Neither is trusted:
	// the server is the authority on what a token can do.
	User    string `json:"user,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Expires string `json:"expires,omitempty"`
}

// Config is the whole file.
type Config struct {
	Current  string             `json:"current"`
	Profiles map[string]Profile `json:"profiles"`
}

// configPath is ~/.config/dbcanvas/config.json, or the Windows equivalent.
// DBCANVAS_CONFIG overrides it, which is what the tests use.
func configPath() (string, error) {
	if p := os.Getenv("DBCANVAS_CONFIG"); p != "" {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		if dir := os.Getenv("AppData"); dir != "" {
			return filepath.Join(dir, "dbcanvas", "config.json"), nil
		}
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "dbcanvas", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate your home directory: %w", err)
	}
	return filepath.Join(home, ".config", "dbcanvas", "config.json"), nil
}

// loadConfig reads the config, returning an empty one when the file does not exist.
// A missing config is the normal state before `dbcanvas login`, not an error.
func loadConfig() (Config, error) {
	c := Config{Profiles: map[string]Profile{}}
	path, err := configPath()
	if err != nil {
		return c, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	return c, nil
}

// saveConfig writes the config at 0600, creating the directory if needed.
//
// It writes to a temp file in the same directory and renames, so an interrupted
// write cannot leave a truncated config — losing a token to a full disk would mean
// re-authenticating for no reason.
func saveConfig(c Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded
	if err := tmp.Chmod(configPerm); err != nil {
		tmp.Close()
		return fmt.Errorf("secure %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	// Re-assert the mode: a pre-existing file's permissions survive a rename onto
	// it on some systems, and this is a file holding a bearer token.
	return os.Chmod(path, configPerm)
}

// resolve works out which installation and credential this invocation should use.
//
// Precedence, narrowest first: explicit flags, then the environment, then the named
// profile, then the current profile. The environment matters most in practice — it
// is how CI passes a token without writing anything to disk, and it must not be
// silently overridden by a config file that happens to exist on the runner.
func (c Config) resolve(profileFlag, urlFlag, tokenFlag string) (Profile, string, error) {
	name := profileFlag
	if name == "" {
		name = c.Current
	}
	p := c.Profiles[name]

	if v := os.Getenv("DBCANVAS_URL"); v != "" {
		p.URL = v
	}
	if v := os.Getenv("DBCANVAS_TOKEN"); v != "" {
		p.Token = v
		p.User, p.Scope, p.Expires = "", "", "" // not ours to describe
	}
	if urlFlag != "" {
		p.URL = urlFlag
	}
	if tokenFlag != "" {
		p.Token = tokenFlag
		p.User, p.Scope, p.Expires = "", "", ""
	}
	p.URL = strings.TrimRight(p.URL, "/")
	if p.URL == "" {
		return p, name, errNotConfigured
	}
	return p, name, nil
}

var errNotConfigured = errors.New(
	"no DBCanvas installation configured — run `dbcanvas login --url http://localhost:8080`," +
		" or set DBCANVAS_URL and DBCANVAS_TOKEN")
