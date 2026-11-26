package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"dbcanvas/internal/seal"
)

// doctor.go — GET /api/system/doctor: what you should know about this installation, as checks a
// person or a program can read. `dbcanvas doctor` prints it, with the data folder, networks and
// the service added (cmd_doctor.go).
//
// Each check is ok, warn or fail with a one-line detail and, when it is not ok, what to do.

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// exampleSecrets are the node account settings (nodepasswords.go) with the example value each
// had in DBCanvas's published configuration. dbcanvas generates its own on the first run; a
// value set under env: in config.yaml, or in the environment, that equals one of these is a
// password anybody can read in the repository.
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

// defaultSecrets lists the settings set to their published example value. Unset is not one: an
// unset setting gets its generated password.
func defaultSecrets() []string {
	var out []string
	for k, v := range exampleSecrets {
		if os.Getenv(k) == v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// versionsGeneratedAt reads the catalogue in use's versions_generated_at.
func versionsGeneratedAt() (time.Time, bool) { return catalogGeneratedAt(versionsFilePath()) }

func (a *App) doctorChecks(ctx context.Context) []doctorCheck {
	var out []doctorCheck
	add := func(name, status, detail, fix string) {
		out = append(out, doctorCheck{Name: name, Status: status, Detail: detail, Fix: fix})
	}

	// Docker.
	sock := dockerSocket()
	pingErr := a.engCtx(ctx).Ping(ctx)
	dockerOK := pingErr == nil
	switch {
	case dockerOK:
		add("docker", "ok", "answers on "+sock, "")
	case strings.Contains(strings.ToLower(pingErr.Error()), "permission denied"):
		add("docker", "fail", "permission denied on "+sock,
			"add your user to the docker group (sudo usermod -aG docker $USER), then log in again")
	default:
		add("docker", "fail", "does not answer on "+sock+": "+pingErr.Error(),
			"start Docker (Docker Desktop, Rancher Desktop, Colima, OrbStack or dockerd), or set dockerSocket in ~/.dbcanvas/config.yaml")
	}
	if dockerOK {
		host := firstNonEmpty(a.hostArch, hostArchName(a.docker.HostArch(ctx)))
		add("architecture", "ok", fmt.Sprintf("Docker runs on %s; nodes default to %s, Kubernetes nodes run on %s", firstNonEmpty(host, "an unknown architecture"), platformArch(), k8sArch()), "")
	}

	// Node images: pulled from the registry when a deploy needs them, so a missing one is a
	// note, not a fault — unless this release's images are not published.
	if dockerOK {
		imgs := onPlatform(loadImagesCatalog())
		var missing []string
		for _, img := range imgs {
			if ok, _ := a.engCtx(ctx).ImageExists(ctx, pxcImage(img.OS, img.OSVersion, img.Arch)); !ok {
				missing = append(missing, img.OS+" "+img.OSVersion)
			}
		}
		switch {
		case len(imgs) == 0:
			add("base images", "fail", "the version catalogue lists no base image", "dbcanvas versions update")
		case len(missing) > 0:
			add("base images", "ok", fmt.Sprintf("%d of %d here; the rest are pulled when a node needs them: %s", len(imgs)-len(missing), len(imgs), strings.Join(missing, ", ")), "")
		default:
			add("base images", "ok", fmt.Sprintf("all %d here", len(imgs)), "")
		}
		if ok, _ := a.engCtx(ctx).ImageExists(ctx, intranetImage("")); ok {
			add("intranet image", "ok", intranetImage(""), "")
		} else {
			add("intranet image", "ok", intranetImage("")+" is pulled on the first deploy", "")
		}
	}

	// Version catalogues, one per architecture.
	for _, arch := range catalogArches {
		path := versionsFilePathFor(arch)
		name := "catalogue (" + arch + ")"
		t, ok := catalogGeneratedAt(path)
		switch {
		case path == "":
			add(name, "warn", "none", "dbcanvas versions update")
		case !ok:
			add(name, "warn", describeCatalog(path)+" has no generation date", "dbcanvas versions update")
		case time.Since(t) > 60*24*time.Hour:
			add(name, "warn", fmt.Sprintf("generated %d days ago; newer releases are not offered", int(time.Since(t).Hours()/24)), "dbcanvas versions update")
		default:
			add(name, "ok", describeCatalog(path), "")
		}
	}

	// The data folder.
	if dir, err := dataDir(); err == nil {
		if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
			add("data folder", "fail", fmt.Sprintf("%s is open to other users (%s)", dir, fi.Mode().Perm()), "chmod 700 "+dir)
		} else if err == nil {
			add("data folder", "ok", dir, "")
		}
		var fs syscall.Statfs_t
		if syscall.Statfs(dir, &fs) == nil {
			free := int64(fs.Bavail) * int64(fs.Bsize)
			gb := float64(free) / (1 << 30)
			switch {
			case gb < 1:
				add("disk space", "fail", fmt.Sprintf("%.1f GB free where the data folder is", gb), "free some space: the database and its log live there")
			case gb < 5:
				add("disk space", "warn", fmt.Sprintf("%.1f GB free where the data folder is", gb), "")
			default:
				add("disk space", "ok", fmt.Sprintf("%.0f GB free where the data folder is", gb), "")
			}
		}
		keyPath := seal.KeyPath(filepath.Join(dir, "dbcanvas.db"))
		if _, err := os.Stat(keyPath); err == nil {
			add("encryption key", "ok", keyPath+" — back it up with the data folder", "")
		} else if _, err := os.Stat(filepath.Join(dir, "dbcanvas.db")); err == nil {
			add("encryption key", "fail", "no key at "+keyPath+", so the database cannot be read", "restore it from a backup of ~/.dbcanvas")
		}
	}

	// Node passwords: generated, unless config.yaml's env: (or the environment) sets one
	// to the example value DBCanvas once published.
	if bad := defaultSecrets(); len(bad) > 0 {
		add("node passwords", "warn", strings.Join(bad, ", ")+" set to a published example value",
			"remove them from env: in config.yaml to use the generated ones (dbcanvas passwords), then dbcanvas stop")
	} else {
		add("node passwords", "ok", "generated on the first run, or set in config.yaml (dbcanvas passwords)", "")
	}

	// Stack networks.
	if dockerOK {
		space, err := subnetSearchSpace()
		avoid, aerr := a.docker.stackSubnetAvoid(ctx)
		switch {
		case err != nil:
			add("stack networks", "fail", err.Error(), "fix networkRange in ~/.dbcanvas/config.yaml")
		case aerr != nil:
			add("stack networks", "warn", "could not list Docker's networks: "+aerr.Error(), "")
		default:
			if next, ok := pickSubnet(space, avoid, instanceName); ok {
				add("stack networks", "ok", "the next stack gets "+next.String()+" (dbcanvas network check shows why)", "")
			} else {
				add("stack networks", "fail", "no free /24 in "+cidrList(space), "remove unused Docker networks, or name more room in networkRange")
			}
		}
	}

	// What K3D clusters and the bundled features need.
	if k3dInstalled() {
		if p := k3dPinnedPath(); p != "" {
			if _, err := os.Stat(p); err == nil {
				add("k3d", "ok", p, "")
			} else {
				add("k3d", "ok", "k3d "+k3dVersion+" is downloaded when the first K3D cluster is deployed", "")
			}
		} else {
			add("k3d", "ok", "found", "")
		}
	} else {
		add("k3d", "warn", "no k3d for "+k3dPlatform()+", so K3D clusters cannot be deployed", "install k3d, or set K3D_BIN")
	}
	for _, b := range []struct{ name, env, what string }{
		{"Big Hole", "BIGHOLE_TARBALL", "the Big Hole option installs nothing"},
		{"node Dockerfiles", "DBCANVAS_IMAGES_DIR", "the Build button cannot build a missing image"},
		{"Pager Panic banks", "PAGERPANIC_BANKS", "Pager Panic has no questions"},
		{"licences", "DBCANVAS_LICENSE_DIR", "/licenses/ lists no texts"},
	} {
		if p := os.Getenv(b.env); p != "" {
			if _, err := os.Stat(p); err == nil {
				add(b.name, "ok", p, "")
				continue
			}
		}
		add(b.name, "warn", "not in this build: "+b.what, "build with make dbcanvas, which bundles it")
	}
	out = append(out, a.vmDoctorChecks()...)
	return out
}

// vmDoctorChecks reports on running nodes as virtual machines, which is optional: without
// Lima every node runs in a container as usual, so its absence is a note, never a failure.
func (a *App) vmDoctorChecks() []doctorCheck {
	lima, why := a.lima, a.limaWhy
	if lima == nil && why == "" {
		if dir, err := dataDir(); err == nil {
			lima, why = NewLima(limaHomeFor(dir))
		}
	}
	if lima == nil {
		return []doctorCheck{{Name: "vm nodes", Status: "ok",
			Detail: "optional, not available: " + firstNonEmpty(why, "limactl is not installed") + " — every node runs in a container; install Lima 2.0 or newer (https://lima-vm.io) to run nodes as VMs"}}
	}
	out := []doctorCheck{{Name: "vm nodes", Status: "ok", Detail: "limactl " + lima.version + "; VMs are kept in " + lima.home}}
	// The QEMU each architecture needs: on Linux every VM runs on it, on macOS only the other
	// architecture does. A missing one is a note: the architecture it serves is optional.
	for _, arch := range []string{runtime.GOARCH, map[string]string{"arm64": "amd64", "amd64": "arm64"}[runtime.GOARCH]} {
		if arch == "" || !limaNeedsQEMU(runtime.GOOS, runtime.GOARCH, arch) {
			continue
		}
		bin := limaQEMUBinary(arch)
		if p, err := qemuLookPath(bin); err == nil {
			out = append(out, doctorCheck{Name: "vm qemu " + arch, Status: "ok", Detail: arch + " VMs run on " + p})
		} else {
			out = append(out, doctorCheck{Name: "vm qemu " + arch, Status: "ok", Detail: "optional, not available: " + limaQEMUMissing(arch)})
		}
	}
	if runtime.GOOS == "linux" {
		if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
			f.Close()
		} else {
			out = append(out, doctorCheck{Name: "kvm", Status: "warn",
				Detail: "cannot use /dev/kvm (" + err.Error() + "): VMs of this machine's architecture would run under emulation, many times more slowly",
				Fix:    "add your user to the kvm group (sudo usermod -aG kvm $USER), then log in again; on a cloud machine, enable nested virtualization"})
		}
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
