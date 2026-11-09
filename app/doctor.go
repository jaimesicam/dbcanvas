package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dbcanvas/internal/seal"
)

// doctor.go — GET /api/system/doctor: what an administrator should know about this installation,
// as checks a person or a program can read. `dbcanvas-cli doctor` prints it; `make doctor` checks
// the host side (Docker, disk, .env) without needing DBCanvas to be running.
//
// Each check is ok, warn or fail with a one-line detail and, when it is not ok, what to do.

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// exampleSecrets are the passwords .env.example ships with — the values a fresh `make env` no
// longer keeps (scripts/gen-secrets.sh). A setting still equal to one is a password anybody can
// read in the repository.
var exampleSecrets = map[string]string{
	"APP_PASSWORD": "app_password", "REPL_PASSWORD": "repl_password", "MONITOR_PASSWORD": "monitor_password",
	"CLUSTER_PASSWORD": "cluster_password", "CLUSTERCHECK_PASSWORD": "cluster_password",
	"ORCHESTRATOR_PASSWORD": "orchestrator_password", "MYSQL_ROOT_PASSWORD": "root_password",
	"MYSQL_ADMIN_PASSWORD": "admin_password", "POSTGRES_PASSWORD": "postgres_password",
	"VALKEY_PASSWORD": "valkey_password", "PROXYSQL_ADMIN_PASSWORD": "admin_password",
	"MONGODB_ADMIN_PASSWORD": "admin_password", "MCLUSTERADMIN_PASSWORD": "madmin_password",
	"MCLUSTERADMIN_RO_PASSWORD": "madmin_ro_password", "PMM_ADMIN_PASSWORD": "admin_password",
	"GRAFANA_PASSWORD": "grafana_password", "EVEREST_PASSWORD": "everest_password",
	"KEYCLOAK_PASSWORD": "keycloak_password", "KEYCLOAK_USER_PASSWORD": "keycloak_user_password",
	"VNC_PASSWORD": "vnc_password", "PMM_PASSWORD": "pmm_password", "SAMBA_PASSWORD": "SambaPassword2026",
}

// defaultSecrets lists the settings still at their example value (unset counts: the code falls
// back to the same value).
func defaultSecrets() []string {
	var out []string
	for k, v := range exampleSecrets {
		if cur := os.Getenv(k); cur == "" || cur == v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// versionsGeneratedAt reads versions.yaml's versions_generated_at.
func versionsGeneratedAt() (time.Time, bool) {
	f, err := os.Open(versionsFilePath())
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for i := 0; sc.Scan() && i < 40; i++ {
		if v, ok := strings.CutPrefix(sc.Text(), "versions_generated_at:"); ok {
			t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
			return t, err == nil
		}
	}
	return time.Time{}, false
}

func (a *App) doctorChecks(ctx context.Context) []doctorCheck {
	var out []doctorCheck
	add := func(name, status, detail, fix string) {
		out = append(out, doctorCheck{Name: name, Status: status, Detail: detail, Fix: fix})
	}

	// Docker.
	dockerOK := a.engCtx(ctx).Ping(ctx) == nil
	if dockerOK {
		add("docker", "ok", "the Docker daemon answers", "")
	} else {
		add("docker", "fail", "the Docker daemon does not answer", "check that the socket is mounted (DOCKER_SOCK) and the daemon is running")
	}

	// Node images.
	if dockerOK {
		imgs := loadImagesCatalog()
		var missing []string
		for _, img := range imgs {
			if ok, _ := a.engCtx(ctx).ImageExists(ctx, pxcImage(img.OS, img.OSVersion, img.Arch)); !ok {
				missing = append(missing, img.OS+" "+img.OSVersion)
			}
		}
		switch {
		case len(imgs) == 0:
			add("base images", "fail", "images.yaml lists no base image", "run `make images`, or `make pull-images` to pull them")
		case len(missing) > 0:
			add("base images", "warn", fmt.Sprintf("%d of %d missing for this release: %s", len(missing), len(imgs), strings.Join(missing, ", ")),
				"run `make images` or `make pull-images` (after an upgrade, `make adopt-images` retags what was built)")
		default:
			add("base images", "ok", fmt.Sprintf("%d present", len(imgs)), "")
		}
		if ok, _ := a.engCtx(ctx).ImageExists(ctx, intranetImage("")); ok {
			add("intranet image", "ok", intranetImage(""), "")
		} else {
			add("intranet image", "fail", intranetImage("")+" is missing — every stack needs it", "run `make intranet-image`, or the Build button at Validate")
		}
	}

	// Version catalogue.
	if t, ok := versionsGeneratedAt(); !ok {
		add("version catalogue", "warn", "versions.yaml has no generation date", "run `make versions`")
	} else if age := time.Since(t); age > 60*24*time.Hour {
		add("version catalogue", "warn", fmt.Sprintf("probed %d days ago; newer releases are not offered", int(age.Hours()/24)), "run `make versions` (ONLY=percona|upstream|haproxy for one group)")
	} else {
		add("version catalogue", "ok", "probed "+t.Format("2006-01-02"), "")
	}

	// Secrets.
	if d := defaultSecrets(); len(d) > 0 {
		add("default passwords", "warn", fmt.Sprintf("%d still at their .env.example value: %s", len(d), strings.Join(d, ", ")),
			"set them in .env (scripts/gen-secrets.sh .env does it for you), then `make restart`; stacks deployed before keep their old passwords")
	} else {
		add("default passwords", "ok", "none at their .env.example value", "")
	}
	keyPath := seal.KeyPath(envOr("DB_PATH", "dbcanvas.db"))
	if _, err := os.Stat(keyPath); err == nil {
		add("encryption key", "ok", keyPath+" — back it up apart from the database", "")
	} else {
		add("encryption key", "fail", "no key at "+keyPath, "the database cannot be read without it; see docs/CONFIGURATION.md")
	}

	// Who can get in.
	if exposed() {
		add("exposure", "warn", "published on "+publishHost()+": reachable beyond this machine, and an account can deploy privileged containers",
			"prefer APP_HOST=127.0.0.1 with an SSH tunnel (docs/SECURITY.md)")
	} else {
		add("exposure", "ok", "published on "+publishHost()+" only", "")
	}
	if a.guestLoginEnabled() {
		st := "warn"
		if !exposed() {
			st = "ok"
		}
		add("guest sign-in", st, fmt.Sprintf("on; idle guest accounts removed after %d day(s)", a.guestIdleDays()), "set limits for Guests under Security group limits")
	}
	if a.openRegistration() {
		add("open registration", "warn", "anyone who registers is approved and signed in at once", "turn it off unless everyone who can reach the page should have an account")
	}

	// What it hands out.
	if ents, err := os.ReadDir(cliDir()); err == nil && len(ents) > 0 {
		add("CLI downloads", "ok", fmt.Sprintf("%d file(s) in %s", len(ents), cliDir()), "")
	} else {
		add("CLI downloads", "warn", "no dbcanvas-cli binaries to download", "they ship in the app image; a development run uses `make cli`")
	}
	if _, err := os.Stat(bigHoleTarball()); err == nil {
		add("Big Hole", "ok", "files present ("+filepath.Base(bigHoleTarball())+")", "")
	} else {
		add("Big Hole", "warn", "no Big Hole files — the option installs nothing", "they ship in the app image (app/Dockerfile, stage bighole)")
	}
	return out
}

// handleDoctor is GET /api/system/doctor.
func (a *App) handleDoctor(w http.ResponseWriter, r *http.Request) {
	checks := a.doctorChecks(r.Context())
	worst := "ok"
	for _, c := range checks {
		if c.Status == "fail" || (c.Status == "warn" && worst == "ok") {
			worst = c.Status
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": worst, "version": appVersion, "checks": checks})
}
