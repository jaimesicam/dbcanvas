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

// exampleSecrets is every node password setting, generated on first run (nodepasswords.go): the
// configuration reference must list each, and list no other.
func TestNodePasswordsAreDocumented(t *testing.T) {
	b, err := os.ReadFile("../docs/CONFIGURATION.md")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?m)^\\| `([A-Z_]*PASSWORD)` \\|")
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		seen[m[1]] = true
		if _, ok := exampleSecrets[m[1]]; !ok {
			t.Errorf("docs/CONFIGURATION.md lists %s, which is not a node password setting", m[1])
		}
	}
	for k := range exampleSecrets {
		if !seen[k] {
			t.Errorf("%s is not in docs/CONFIGURATION.md", k)
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
	t.Setenv("VNC_PASSWORD", "") // unset: it gets a generated one, so it is not the example
	if d := strings.Join(defaultSecrets(), ","); d != "POSTGRES_PASSWORD" {
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

// A run pushes and pulls one Kubeadm image, for the run's Kubernetes platform: K8S_PLATFORM when
// it is given, else a DOCKER_PLATFORM given on the command line (see run_k8s_platform). A release
// publishes both architectures as two runs, like every other image.
func TestRegistryKubeadmOneArchitecturePerRun(t *testing.T) {
	list := func(env []string, args ...string) []string {
		cmd := exec.Command("bash", append([]string{"../scripts/registry.sh", "list"}, args...)...)
		cmd.Env = append(os.Environ(), env...)
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
	for _, c := range []struct {
		env  []string
		arch string
	}{
		{[]string{"DOCKER_PLATFORM=linux/amd64", "K8S_PLATFORM=linux/arm64"}, "arm64"},
		{[]string{"DOCKER_PLATFORM=linux/arm64", "K8S_PLATFORM=linux/amd64"}, "amd64"},
		{[]string{"DOCKER_PLATFORM=linux/arm64", "K8S_PLATFORM=", "K3D_PLATFORM="}, "arm64"},
		{[]string{"DOCKER_PLATFORM=linux/amd64", "K8S_PLATFORM=", "K3D_PLATFORM="}, "amd64"},
	} {
		for _, mode := range [][]string{{"push"}, nil} {
			got := list(c.env, mode...)
			if len(got) != 1 || !strings.Contains(got[0], "-"+c.arch+"-") {
				t.Errorf("%v %v: %v, want the %s image only", c.env, mode, got, c.arch)
			}
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

// With Docker unreachable the doctor says so and how to fix it, and still checks the rest; a
// build without the bundled files says which feature that leaves out.
func TestDoctorWithoutDocker(t *testing.T) {
	t.Setenv("DBCANVAS_DATA", t.TempDir())
	t.Setenv("DOCKER_SOCK", filepath.Join(t.TempDir(), "absent.sock"))
	for _, e := range []string{"BIGHOLE_TARBALL", "DBCANVAS_IMAGES_DIR", "PAGERPANIC_BANKS", "DBCANVAS_LICENSE_DIR"} {
		t.Setenv(e, "")
		os.Unsetenv(e)
	}
	a := &App{docker: NewDocker(os.Getenv("DOCKER_SOCK"))}
	got := map[string]doctorCheck{}
	for _, c := range a.doctorChecks(httptest.NewRequest("GET", "/", nil).Context()) {
		got[c.Name] = c
	}
	if d := got["docker"]; d.Status != "fail" || !strings.Contains(d.Detail, "absent.sock") || d.Fix == "" {
		t.Errorf("docker: %+v", d)
	}
	if b := got["Big Hole"]; b.Status != "warn" || !strings.Contains(b.Detail, "installs nothing") {
		t.Errorf("Big Hole: %+v", b)
	}
	if _, ok := got["data folder"]; !ok {
		t.Error("the data folder was not checked")
	}
}
