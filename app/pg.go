package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Standalone PostgreSQL node (Type=="pg"). A single PostgreSQL instance (Percona
// Distribution for PostgreSQL) installed at deploy time on a systemd OS image
// (built by `make images`) — the node properties mirror the standalone Percona
// Server node (catalog OS/version/arch, superuser password, PMM, Intranet Squid
// proxy, Intranet-CA TLS, host-port export) plus an optional pgBackRest → SeaweedFS
// S3 backup, just like the Patroni cluster frame. Unlike the Patroni frame there is
// no Patroni/etcd and no replication: it is a plain read/write server bootstrapped
// directly from the packaged systemd unit. It exposes PostgreSQL on 5432 (publishable
// to the host).

// pgBackRestSeaweedIssues validates the SeaweedFS node backing pgBackRest (for a
// Patroni frame or standalone PostgreSQL node, identified by `who`): it must be in
// the design **and have S3 TLS enabled** — pgBackRest's S3 client requires HTTPS, so
// it cannot talk to a plain-HTTP SeaweedFS endpoint.
func pgBackRestSeaweedIssues(who, seaweedNodeID string, doc designDoc) []issue {
	if seaweedNodeID == "" {
		return []issue{{"error", who + " has pgBackRest enabled but no SeaweedFS node selected"}}
	}
	for _, n := range doc.Nodes {
		if n.ID == seaweedNodeID && n.Type == "seaweedfs" {
			if !n.TLS {
				return []issue{{"error", who + ": pgBackRest requires the SeaweedFS node " + n.Label + " to have S3 TLS enabled (pgBackRest's S3 client needs HTTPS)"}}
			}
			return nil
		}
	}
	return []issue{{"error", who + ": the selected pgBackRest SeaweedFS node is not in the design"}}
}

// pgConfig is the non-secret profile shown for a deployed standalone PostgreSQL node.
type pgConfig struct {
	Image         string `json:"image"`
	OS            string `json:"os"`
	Hostname      string `json:"hostname"`
	FQDN          string `json:"fqdn"`
	PGMajor       string `json:"pgMajor"`
	PGVersion     string `json:"pgVersion"`
	Role          string `json:"role"` // "standalone"
	UsePgBackRest bool   `json:"usePgBackRest"`
	BackupRepo    string `json:"backupRepo"` // e.g. "pgbackrest → SeaweedFS S3" when enabled
	GenerateCert  bool   `json:"generateCert"`
	UseProxy      bool   `json:"useProxy"`
	MonitoredBy   string `json:"monitoredBy"` // PMM node FQDN, if any
	Ports         []int  `json:"ports"`
	ExportPort    int    `json:"exportPort"` // published host port for 5432 (0 = none)
}

// pgServiceName / pgConfDir are OS-aware: on EL the packaged unit is
// postgresql-NN (config + data both under /var/lib/pgsql/NN/data); on Debian the
// PGDG-style unit is postgresql@NN-main with config under /etc/postgresql/NN/main.
func pgServiceName(os, major string) string {
	major = ppgMajorOf(major)
	if isDebianOS(os) {
		return "postgresql@" + major + "-main"
	}
	return "postgresql-" + major
}

func pgConfDir(os, major string) string {
	if isDebianOS(os) {
		return "/etc/postgresql/" + ppgMajorOf(major) + "/main"
	}
	return pgDataDir(os, major)
}

// provisionPG provisions a standalone PostgreSQL node: it records the deployment,
// creates the container, installs PostgreSQL (+ pgBackRest + pmm-client), initialises
// the data directory, configures listen/auth/archive/TLS, starts the service, sets
// the superuser password, optionally creates the pgBackRest stanza + initial backup,
// and registers PMM.
func (a *App) provisionPG(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)
	major := ppgMajorOf(n.PGMajor)
	image := pxcImage(n.OS, n.OSVersion, n.Arch)

	// Superuser credentials come from .env (re-read on every deploy).
	sec := pgSecrets{SuperUser: "postgres", SuperPassword: envOr("POSTGRES_PASSWORD", "postgres_password")}

	monitoredBy := ""
	if n.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == n.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(stackHostnames(doc)[m.ID], domain)
			}
		}
	}
	backupRepo := ""
	if n.UsePgBackRest {
		backupRepo = "pgbackrest → SeaweedFS S3"
	}

	cfg := pgConfig{
		Image: image, OS: n.OS, Hostname: host, FQDN: fqdn,
		PGMajor: major, PGVersion: n.PGVersion, Role: "standalone",
		UsePgBackRest: n.UsePgBackRest, BackupRepo: backupRepo,
		GenerateCert: n.GenerateCert, UseProxy: n.UseProxy, MonitoredBy: monitoredBy,
		Ports: []int{patroniPGPort},
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	go func() {
		ctx := context.Background()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Waiting for Intranet to be ready", 5)
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		// pgBackRest needs the SeaweedFS node up so its S3 config/secret are readable.
		var swCfg seaweedConfig
		var swSec seaweedSecrets
		if n.UsePgBackRest {
			pr.phase("Waiting for SeaweedFS (pgBackRest store)", 8)
			c, s, e := a.waitSeaweedRunning(ctx, st.ID, n.SeaweedFSNodeID, deployTimeout())
			if e != nil {
				pr.fail("%v", e)
				return
			}
			swCfg, swSec = c, s
		}

		// ---- create + start the container ----
		pr.phase("Creating container", 15)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		spec := ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		}
		if n.ExportEnabled {
			spec.PublishMap = []PortMap{{ContainerPort: patroniPGPort, HostPort: n.ExportHostPort}}
		}
		id, err := a.docker.ContainerCreate(ctx, spec)
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.docker.ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
		if n.ExportEnabled {
			if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", patroniPGPort)); e == nil {
				if p, e2 := strconv.Atoi(hp); e2 == nil {
					cfg.ExportPort = p
				}
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		pr.phase("Waiting for systemd", 25)
		if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}
		a.ensureDNFIPv4(ctx, id, n.OS, pr.logln)

		debian := isDebianOS(n.OS)
		if n.UseProxy {
			proxyScript := pkgProxyRHEL
			if debian {
				proxyScript = pkgProxyDebian
			}
			if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
				pr.fail("configure package proxy: %v", err)
				return
			}
			pr.logln("package egress via Intranet proxy")
		}

		// ---- install PostgreSQL (+ pgBackRest + pmm-client) ----
		pr.phase("Installing PostgreSQL", 35)
		pkgs := pgServerPackages(n.OS, major)
		if n.UsePgBackRest {
			pkgs = append(pkgs, "percona-pgbackrest")
		}
		instScript := patroniInstallRHEL
		if debian {
			instScript = patroniInstallDebian
		}
		env := []string{"PRODUCT=" + ppgProduct(major), "PKGS=" + strings.Join(pkgs, " ")}
		if n.UsePgBackRest && !debian {
			env = append(env, "WITH_EPEL=1", "EPELPKG="+epelPackage(n.OSVersion))
		}
		if err := a.runStep(ctx, id, instScript, env, pr.logln); err != nil {
			pr.fail("install PostgreSQL: %v", err)
			return
		}
		pr.logln("PostgreSQL " + major + " installed")
		a.ensureRsyslog(ctx, id, n.OS, pr.logln)

		// Install pmm-client only when this node is monitored by a PMM server;
		// unmonitored nodes skip it entirely.
		if n.PMMNodeID != "" {
			pr.phase("Installing PMM client", 45)
			pmmScript := pxcInstallPMMClientRHEL
			if debian {
				pmmScript = pxcInstallPMMClientDebian
			}
			if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
				pr.logln("pmm-client install skipped: " + err.Error())
			} else {
				pr.logln("pmm-client installed")
			}
		}

		dataDir := pgDataDir(n.OS, major)
		confDir := pgConfDir(n.OS, major)
		service := pgServiceName(n.OS, major)
		stanza := patroniStanza(n.Label)

		// ---- initialise the data directory ----
		pr.phase("Initialising data directory", 52)
		initEnv := []string{"MAJOR=" + major, "BINDIR=" + pgBinDir(n.OS, major), "DATADIR=" + dataDir}
		if debian {
			initEnv = append(initEnv, "DEBIAN=1")
		}
		if err := a.runStep(ctx, id, pgInitScript, initEnv, pr.logln); err != nil {
			pr.fail("initdb: %v", err)
			return
		}

		// ---- pgBackRest config (written before start so archive-push works) ----
		if n.UsePgBackRest {
			pr.phase("Writing pgBackRest config", 56)
			if err := a.runStep(ctx, id, patroniPgBackRestDirsScript, nil, pr.logln); err != nil {
				pr.fail("prepare pgbackrest dirs: %v", err)
				return
			}
			conf := patroniPgBackRestConf(n.Label, n.OS, major, swCfg, swSec)
			if err := a.docker.CopyFile(ctx, id, "/etc/pgbackrest", "pgbackrest.conf", 0o644, []byte(conf)); err != nil {
				pr.fail("write pgbackrest.conf: %v", err)
				return
			}
		}

		// ---- optional per-node TLS cert (Intranet CA) into the data dir ----
		if n.GenerateCert {
			pr.phase("Issuing certificate", 58)
			if err := a.pgApplyCert(ctx, id, intranetID, fqdn, dataDir, n.CertTTLValue, n.CertTTLUnit, pr.logln); err != nil {
				pr.fail("%v", err)
				return
			}
		}

		// ---- configure postgresql.conf + pg_hba.conf ----
		pr.phase("Configuring PostgreSQL", 62)
		confEnv := []string{"CONFDIR=" + confDir, "DATADIR=" + dataDir, "STANZA=" + stanza}
		if n.UsePgBackRest {
			confEnv = append(confEnv, "PGBACKREST=1")
		}
		if n.GenerateCert {
			confEnv = append(confEnv, "TLS=1")
		}
		if err := a.runStep(ctx, id, pgConfigureScript, confEnv, pr.logln); err != nil {
			pr.fail("configure PostgreSQL: %v", err)
			return
		}

		// ---- start the service ----
		pr.phase("Starting PostgreSQL", 70)
		if err := a.runStep(ctx, id, pgStartScript, []string{"SERVICE=" + service}, pr.logln); err != nil {
			pr.fail("start PostgreSQL: %v", err)
			return
		}
		a.reconcileStackDNS(ctx, st.ID)

		// ---- set the superuser password ----
		pr.phase("Setting superuser password", 78)
		if err := a.runStep(ctx, id, pgSetPasswordScript, []string{"SUPERPW=" + sec.SuperPassword}, pr.logln); err != nil {
			pr.fail("set superuser password: %v", err)
			return
		}

		// ---- pgBackRest stanza + initial full backup ----
		if n.UsePgBackRest {
			pr.phase("Creating pgBackRest stanza + initial backup", 86)
			if err := a.runStep(ctx, id, patroniBackupScript, []string{"STANZA=" + stanza}, pr.logln); err != nil {
				// Non-fatal: the server is up; surface the failure but keep running.
				pr.logln("initial pgBackRest backup failed: " + err.Error())
			} else {
				pr.logln("pgBackRest stanza created + initial full backup taken")
			}
		}

		// ---- PMM register (best-effort) ----
		if n.PMMNodeID != "" {
			pr.phase("Registering with PMM", 94)
			a.pgRegisterPMM(ctx, st, n, doc, sec, pr)
		}

		dep, _ := a.store.GetDeployment(st.ID, n.ID)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: dep.ContainerID, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d pg %s: provisioned (standalone PostgreSQL %s)", st.ID, n.Label, major)
	}()
}

// pgApplyCert stages the Intranet CA into the node and signs a server cert + key
// into the PostgreSQL data dir (postgres-owned) with the given TTL.
func (a *App) pgApplyCert(ctx context.Context, containerID, intranetID, fqdn, dataDir string, ttlValue int, ttlUnit string, logln func(string)) error {
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	caCrt, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKey, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	if err := a.docker.PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{
		"dbca-ca.crt": {0o644, 0, caCrt},
		"dbca-ca.key": {0o644, 0, caKey},
	})); err != nil {
		return fmt.Errorf("stage CA: %w", err)
	}
	if ttlValue <= 0 {
		ttlValue, ttlUnit = 365, "days"
	}
	switch ttlUnit {
	case "minutes", "hours", "days":
	default:
		ttlUnit = "days"
	}
	env := []string{"FQDN=" + fqdn, "VALUE=" + strconv.Itoa(ttlValue), "UNIT=" + ttlUnit, "DIR=" + dataDir}
	if err := a.runStep(ctx, containerID, pgCertScript, env, logln); err != nil {
		return fmt.Errorf("generate certificate: %w", err)
	}
	logln("per-node certificate written to " + dataDir + " (postgres-owned)")
	return nil
}

// pgRegisterPMM registers the standalone node's PostgreSQL with the PMM server
// (best-effort), using the superuser over the local connection. Reuses the Patroni
// PMM register scripts.
func (a *App) pgRegisterPMM(ctx context.Context, st Stack, n designNode, doc designDoc, sec pgSecrets, pr *pxcProg) {
	pmmFQDN, pmmUser, pmmPass, ok := a.pmmServerFor(st, doc, n.PMMNodeID)
	if !ok {
		pr.logln("PMM registration skipped: PMM node not running")
		return
	}
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	script := patroniPMMRHEL
	if isDebianOS(n.OS) {
		script = patroniPMMDebian
	}
	env := []string{
		"PMM_FQDN=" + pmmFQDN, "PMM_USER=" + pmmUser, "PMM_PASS=" + pmmPass, "PMM_URL=" + pmmServerURL(pmmFQDN, pmmUser, pmmPass),
		// PMM connects as the dedicated 'pmm' role (created via local peer auth);
		// PMM_PW is that role's password.
		"PMM_PW=" + envOr("PMM_PASSWORD", "pmm_password"),
		"NODE=" + n.Label,
	}
	if _, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", script}, env); err != nil {
		pr.logln("PMM registration skipped: " + err.Error())
	} else {
		pr.logln("registered with PMM at " + pmmFQDN)
	}
}

// handlePGBackup runs an on-demand pgBackRest full backup on a standalone
// PostgreSQL node (owner-scoped).
func (a *App) handlePGBackup(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return
	}
	var node designNode
	found := false
	for _, n := range doc.Nodes {
		if n.ID == nid && n.Type == "pg" {
			node, found = n, true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "PostgreSQL node not found")
		return
	}
	if !node.UsePgBackRest {
		writeErr(w, http.StatusBadRequest, "pgBackRest is not enabled for this node")
		return
	}
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "node is not running")
		return
	}
	ctx := r.Context()
	env := []string{"STANZA=" + patroniStanza(node.Label)}
	if res, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", patroniBackupNowScript}, env); err != nil {
		writeErr(w, http.StatusInternalServerError, "pgBackRest backup failed: "+err.Error())
		return
	} else if res.Code != 0 {
		writeErr(w, http.StatusInternalServerError, "pgBackRest backup failed: "+lastLines(res.Stderr+res.Stdout, 200))
		return
	}
	a.notifyStack(st.ID, "backup.done", "success", "Backup completed", node.Label+": pgBackRest backup finished.", nid)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ------------------------------------------------------------------ scripts

// pgInitScript initialises the PostgreSQL data directory once (guarded on
// PG_VERSION). On EL it runs initdb directly as the postgres user into the
// packaged-unit's data dir; on Debian it registers a cluster with pg_createcluster
// (which the postgresql@NN-main unit manages).
const pgInitScript = `set -e
if [ -n "$DEBIAN" ]; then
  if ! pg_lsclusters -h 2>/dev/null | awk '{print $1"/"$2}' | grep -qx "$MAJOR/main"; then
    pg_createcluster "$MAJOR" main -- -E UTF8 -k --auth-host=scram-sha-256 >/dev/null
  fi
  exit 0
fi
if [ ! -s "$DATADIR/PG_VERSION" ]; then
  install -d -m 700 -o postgres -g postgres "$DATADIR"
  runuser -u postgres -- "$BINDIR/initdb" -D "$DATADIR" -E UTF8 -k --auth-local=peer --auth-host=scram-sha-256 >/dev/null
fi`

// pgConfigureScript enables remote access (listen_addresses, scram auth from any
// host) and, when requested, WAL archiving to pgBackRest and TLS. Settings are
// appended last so they win over the packaged defaults.
const pgConfigureScript = `set -e
CONF="$CONFDIR/postgresql.conf"
HBA="$CONFDIR/pg_hba.conf"
[ -f "$CONF" ] || { echo "postgresql.conf not found at $CONF"; exit 1; }
{
  echo ""
  echo "# --- dbcanvas ---"
  echo "listen_addresses = '*'"
  echo "port = 5432"
  echo "password_encryption = scram-sha-256"
} >> "$CONF"
if [ -n "$PGBACKREST" ]; then
  {
    echo "wal_level = replica"
    echo "archive_mode = on"
    echo "archive_command = 'pgbackrest --stanza=$STANZA archive-push %p'"
    echo "max_wal_senders = 3"
  } >> "$CONF"
fi
if [ -n "$TLS" ]; then
  {
    echo "ssl = on"
    echo "ssl_cert_file = '$DATADIR/server.crt'"
    echo "ssl_key_file = '$DATADIR/server.key'"
    echo "ssl_ca_file = '$DATADIR/ca.crt'"
  } >> "$CONF"
fi
grep -q "dbcanvas-remote" "$HBA" 2>/dev/null || {
  {
    echo "# dbcanvas-remote"
    echo "host all all 0.0.0.0/0 scram-sha-256"
  } >> "$HBA"
}
chown -R postgres:postgres "$CONFDIR" 2>/dev/null || true`

// pgStartScript enables + (re)starts the PostgreSQL systemd unit and verifies it.
const pgStartScript = `set -e
systemctl enable "$SERVICE" >/dev/null 2>&1 || true
systemctl reset-failed "$SERVICE" 2>/dev/null || true
systemctl restart "$SERVICE"
sleep 2
systemctl is-active --quiet "$SERVICE" || { echo "postgresql failed to start:"; journalctl -u "$SERVICE" --no-pager 2>/dev/null | tail -20; exit 1; }`

// pgSetPasswordScript sets the postgres superuser password. It connects over the
// local socket as the postgres OS user (peer auth) and quotes the password safely
// via a psql variable (:'pw'). The SQL is fed on **stdin** (not -c): psql only
// expands :'var' for stdin/file input, never for a -c command string.
const pgSetPasswordScript = `set -e
printf '%s\n' "ALTER USER postgres PASSWORD :'pw';" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v pw="$SUPERPW"`

// pgCertScript signs a server cert + key from the staged Intranet CA into the
// PostgreSQL data dir ($DIR, postgres-owned) with the given TTL.
const pgCertScript = `set -e
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
CA=/tmp/dbca-ca.crt; CAKEY=/tmp/dbca-ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "CA material missing"; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl not installed in this image"; exit 1; }
mkdir -p "$DIR"
cp -f "$CA" "$DIR/ca.crt"
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/server.key" -out /tmp/s.csr -subj "/O=DBCanvas/CN=$FQDN" >/dev/null
openssl x509 -req -in /tmp/s.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/server.crt" -not_after "$END" >/dev/null
chown postgres:postgres "$DIR/ca.crt" "$DIR/server.crt" "$DIR/server.key"
chmod 600 "$DIR/server.key"
chmod 644 "$DIR/ca.crt" "$DIR/server.crt"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/s.csr /tmp/dbca-ca.srl`
