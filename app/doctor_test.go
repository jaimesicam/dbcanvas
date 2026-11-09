package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// exampleSecrets must be exactly .env.example's passwords, or the doctor misses one.
func TestExampleSecretsMatchEnvExample(t *testing.T) {
	b, err := os.ReadFile("../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^([A-Z_]*PASSWORD)=(.*)$`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		seen[m[1]] = true
		if exampleSecrets[m[1]] != m[2] {
			t.Errorf("%s: .env.example has %q, exampleSecrets %q", m[1], m[2], exampleSecrets[m[1]])
		}
	}
	for k := range exampleSecrets {
		if !seen[k] {
			t.Errorf("exampleSecrets has %s, which .env.example does not", k)
		}
	}
}

func TestDefaultSecrets(t *testing.T) {
	for k := range exampleSecrets {
		t.Setenv(k, "a-real-password-"+k)
	}
	if d := defaultSecrets(); len(d) != 0 {
		t.Fatalf("all set: %v", d)
	}
	t.Setenv("POSTGRES_PASSWORD", "postgres_password")
	t.Setenv("VNC_PASSWORD", "")
	if d := strings.Join(defaultSecrets(), ","); d != "POSTGRES_PASSWORD,VNC_PASSWORD" {
		t.Fatalf("got %s", d)
	}
}

func TestVersionsGeneratedAt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "versions.yaml")
	os.WriteFile(p, []byte("# x\nversions_generated_at: 2026-10-27T21:57:20Z\nimages:\n"), 0o644)
	t.Setenv("VERSIONS_FILE", p)
	if ts, ok := versionsGeneratedAt(); !ok || ts.Format("2006-01-02") != "2026-10-27" {
		t.Fatalf("%v %v", ts, ok)
	}
}

// scripts/registry.sh pulls the base images images/build.sh builds; the two matrices must agree.
func TestRegistryMatrixMatchesBuild(t *testing.T) {
	read := func(p string) []string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range regexp.MustCompile(`(?m)^\s+"([a-z]+:[0-9.]+)\|([a-z]+)\|([0-9.]+)`).FindAllStringSubmatch(string(b), -1) {
			out = append(out, m[1]+"|"+m[2]+"|"+m[3])
		}
		return out
	}
	build, reg := read("../images/build.sh"), read("../scripts/registry.sh")
	if len(build) == 0 || strings.Join(build, " ") != strings.Join(reg, " ") {
		t.Fatalf("build.sh %v\nregistry.sh %v", build, reg)
	}
}
