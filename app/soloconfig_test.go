package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSoloConfig(t *testing.T) {
	dir := t.TempDir()
	// First run: the template is written, and it sets nothing.
	if err := applySoloConfig(dir); err != nil {
		t.Fatal(err)
	}
	if c, err := readSoloConfig(dir); err != nil || len(c.vars()) != 0 {
		t.Fatalf("template: %v %v", c.vars(), err)
	}

	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, soloConfigName), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("domain: lab.example.org\nport: 7999\nenv:\n  EXPERIMENTAL: \"1\"\n  MYSQL_ROOT_PASSWORD: s3cret\n")
	t.Setenv("DOMAIN", "env.example.org") // the environment wins
	for _, k := range []string{"APP_PORT", "EXPERIMENTAL", "MYSQL_ROOT_PASSWORD"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := applySoloConfig(dir); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DOMAIN") != "env.example.org" || os.Getenv("APP_PORT") != "7999" ||
		os.Getenv("EXPERIMENTAL") != "1" || os.Getenv("MYSQL_ROOT_PASSWORD") != "s3cret" {
		t.Fatalf("applied: DOMAIN=%q APP_PORT=%q EXPERIMENTAL=%q", os.Getenv("DOMAIN"), os.Getenv("APP_PORT"), os.Getenv("EXPERIMENTAL"))
	}

	for in, want := range map[string]string{
		"domian: x\n":             "field domian not found",
		"env:\n  lower_case: x\n": "not a setting's variable name",
		"port: seventy\n":         "cannot unmarshal",
	} {
		write(in)
		if _, err := readSoloConfig(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}

func TestDeploymentTimeoutTakesMinutesOrADuration(t *testing.T) {
	for v, want := range map[string]time.Duration{"": time.Hour, "90": 90 * time.Minute, "45m": 45 * time.Minute, "2h": 2 * time.Hour, "junk": time.Hour} {
		t.Setenv("DEPLOYMENT_TIMEOUT", v)
		if got := deployTimeout(); got != want {
			t.Errorf("DEPLOYMENT_TIMEOUT=%q: %v, want %v", v, got, want)
		}
	}
}
