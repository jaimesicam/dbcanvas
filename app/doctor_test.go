package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
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

// scripts/registry.sh pushes and pulls the demo applications images/apps.sh builds; the lists must agree.
func TestRegistryAppsMatchBuild(t *testing.T) {
	re := regexp.MustCompile(`(?m)^APPS=\(([^)]*)\)`)
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		m := re.FindStringSubmatch(string(b))
		if m == nil {
			t.Fatalf("%s has no APPS=(...) list", p)
		}
		return strings.Join(strings.Fields(m[1]), " ")
	}
	if build, reg := read("../images/apps.sh"), read("../scripts/registry.sh"); build != reg {
		t.Fatalf("apps.sh (%s)\nregistry.sh (%s)", build, reg)
	}
}

// The Kubeadm node runs only natively, so both architectures are pushed and a pull takes only the
// installation's own.
func TestRegistryKubeadmBothArchitectures(t *testing.T) {
	list := func(env string, args ...string) []string {
		cmd := exec.Command("bash", append([]string{"../scripts/registry.sh", "list"}, args...)...)
		cmd.Env = append(os.Environ(), "K8S_PLATFORM="+env, "DOCKER_PLATFORM=linux/amd64")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("registry.sh list %v: %v\n%s", args, err, out)
		}
		var k []string
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, "dbcanvas-kubeadm:") {
				k = append(k, l)
			}
		}
		return k
	}
	push := list("linux/arm64", "push")
	if len(push) != 2 || !strings.Contains(push[0], "-amd64-") || !strings.Contains(push[1], "-arm64-") {
		t.Errorf("push: %v", push)
	}
	for _, p := range []string{"linux/amd64", "linux/arm64"} {
		pull := list(p)
		if len(pull) != 1 || !strings.Contains(pull[0], "-"+strings.TrimPrefix(p, "linux/")+"-") {
			t.Errorf("pull with K8S_PLATFORM=%s: %v", p, pull)
		}
	}
}

// Administrators are told, on every page, which .env passwords are still the example ones; other
// accounts are not told anything about the installation's secrets.
func TestSystemSettingsNameDefaultSecretsToAdminsOnly(t *testing.T) {
	app := newTestApp(t)
	for k := range exampleSecrets {
		t.Setenv(k, "changed-"+k)
	}
	t.Setenv("APP_PASSWORD", exampleSecrets["APP_PASSWORD"])
	admin, _ := app.store.CreateUser("boss", "x", RoleAdmin, StatusApproved)
	user, _ := app.store.CreateUser("plain", "x", RoleUser, StatusApproved)
	get := func(u User) SystemSettings {
		r, _ := signedIn(t, app, u, "")
		w := httptest.NewRecorder()
		app.handleGetSystemSettings(w, r)
		var s SystemSettings
		json.Unmarshal(w.Body.Bytes(), &s)
		return s
	}
	if got := get(admin).DefaultSecrets; len(got) != 1 || got[0] != "APP_PASSWORD" {
		t.Errorf("admin: %v", got)
	}
	if got := get(user).DefaultSecrets; got != nil {
		t.Errorf("a regular account was told %v", got)
	}
}
