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
		return []issue{{Level: "error", Message: who + " has pgBackRest enabled but no SeaweedFS node selected"}}
	}
	for _, n := range doc.Nodes {
		if n.ID == seaweedNodeID && n.Type == "seaweedfs" {
			if !n.TLS {
				return []issue{{Level: "error", Message: who + ": pgBackRest requires the SeaweedFS node " + n.Label + " to have S3 TLS enabled (pgBackRest's S3 client needs HTTPS)"}}
			}
			return nil
		}
	}
	return []issue{{Level: "error", Message: who + ": the selected pgBackRest SeaweedFS node is not in the design"}}
}

// ------------------------------------------------- client authentication (pg_hba)
//
// Every PostgreSQL node and cluster frame here used to authenticate remote clients
// exactly one way — `host all all 0.0.0.0/0 scram-sha-256`, written into four
// different provisioners. PGHostAuth makes that a list instead, because a server can
// legitimately offer more than one method at once and a lab is where you would want
// to show it: a client arriving over TLS with a certificate authenticated by it, and
// one arriving without falling back to a password.
//
// The list is ordered and the order is the whole subtlety. pg_hba is first-match-wins
// and does **not** fall through on failure: the first rule whose connection type,
// database, user and address match is the only one that gets to decide, and if
// authentication fails there the connection is refused rather than retried against
// the next line. So `cert` has to come before a password method to be reachable at
// all — a `host all all 0.0.0.0/0 scram-sha-256` above it matches every connection,
// SSL or not, and everything below it is dead config. pgHostAuthIssues says so.

// pgHostAuthDefault is what every design that predates this field keeps getting.
const pgHostAuthDefault = "scram-sha-256"

// pgHostAuthMethods parses the list, in order, with the empty value meaning the
// default. Duplicates are collapsed; anything unrecognised is returned as-is so
// pgHostAuthIssues can name it rather than silently dropping it.
func pgHostAuthMethods(spec string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = pgHostAuthDefault
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range strings.Split(spec, ",") {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		out = []string{pgHostAuthDefault}
	}
	return out
}

// pgHostAuthKnown is the set the provisioners will write. Deliberately short: these
// are the methods that mean something for a remote client on a lab network, and one
// nobody can reach (ldap, pam, gss) would be a picker entry that never works.
var pgHostAuthKnown = map[string]bool{
	"scram-sha-256": true, "md5": true, "cert": true, "trust": true, "reject": true,
}

// pgHostAuthNeedsTLS reports whether a method can only be served over TLS, and so
// requires the node to have been deployed with a certificate.
func pgHostAuthNeedsTLS(method string) bool { return method == "cert" }

// pgHostAuthLines renders the pg_hba rules for remote clients, in order.
//
// "cert" is written as **hostssl**, which is not decoration: the method needs a
// client certificate, so a rule that also matched plaintext connections could only
// ever refuse them. Every other method is `host`, which matches both — which is
// exactly why one of them below a hostssl rule still catches the clients that did
// not present a certificate, and why one above it catches everything.
func pgHostAuthLines(spec string) []string {
	var out []string
	for _, m := range pgHostAuthMethods(spec) {
		if !pgHostAuthKnown[m] {
			continue
		}
		if pgHostAuthNeedsTLS(m) {
			// `cert` implies clientcert=verify-full in PostgreSQL, so spelling it out
			// would add nothing but a second thing to keep true.
			out = append(out, "hostssl all all 0.0.0.0/0 "+m)
			continue
		}
		out = append(out, "host all all 0.0.0.0/0 "+m)
	}
	if len(out) == 0 {
		out = []string{"host all all 0.0.0.0/0 " + pgHostAuthDefault}
	}
	return out
}

// pgHostAuthRequiresClientCert reports whether a client arriving over TLS will be
// judged by the certificate rule — i.e. `cert` is listed and nothing that matches
// every connection comes above it.
//
// This is the question a *pooler* has to ask about its backend, and it is not the
// same as "does this node accept certificates". PgBouncer connects over TLS whenever
// the server offers it, so a backend whose first rule is `hostssl … cert` demands a
// certificate from the pool itself, and refuses it with "connection requires a valid
// client certificate" — a failure on the server leg that says nothing about how the
// pool's own client authenticated. See pgBouncerIssues.
func pgHostAuthRequiresClientCert(spec string) bool {
	for _, m := range pgHostAuthMethods(spec) {
		if !pgHostAuthKnown[m] {
			continue
		}
		if pgHostAuthNeedsTLS(m) {
			return true
		}
		return false // a plain host rule above it matches TLS connections too
	}
	return false
}

// pgHostAuthIssues validates one node or frame's list. `who` names it.
func pgHostAuthIssues(who, spec string, tls bool) []issue {
	var out []issue
	methods := pgHostAuthMethods(spec)
	for _, m := range methods {
		if !pgHostAuthKnown[m] {
			out = append(out, issue{Level: "error", Message: who + ": " + m +
				" is not a client authentication method DBCanvas writes (scram-sha-256, md5, cert, trust, reject)"})
			continue
		}
		if pgHostAuthNeedsTLS(m) && !tls {
			out = append(out, issue{Level: "error", Message: who +
				": certificate authentication needs PostgreSQL to be listening for TLS — tick \"Generate certificate from Intranet CA\", or drop cert from the authentication methods"})
		}
		if m == "trust" {
			out = append(out, issue{Level: "warn", Message: who +
				": trust accepts any remote client with no credential at all — fine for a lab, never past one"})
		}
	}
	// The consequence of putting cert first, said once and plainly: it is not "this
	// server also accepts certificates", it is "every client that speaks TLS to this
	// server must present one". A client that cannot has to connect without TLS to
	// reach the password rule below it.
	if pgHostAuthRequiresClientCert(spec) {
		out = append(out, issue{Level: "warn", Message: who +
			": with cert first, every client that connects over TLS must present a certificate whose CN is its role — one that cannot has to connect without TLS to reach the rule below"})
	}
	// A plain `host` rule matches every connection, SSL or not, so nothing after it
	// is ever consulted. Saying which rule is dead is more use than saying the list
	// is wrong.
	for i, m := range methods {
		if !pgHostAuthKnown[m] || pgHostAuthNeedsTLS(m) {
			continue
		}
		if i < len(methods)-1 {
			out = append(out, issue{Level: "warn", Message: who + ": " + m +
				" matches every connection, so " + strings.Join(methods[i+1:], ", ") +
				" after it can never be reached — pg_hba is first-match-wins and does not fall through. Put cert first."})
		}
		break
	}
	return out
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
	// What the Backup tab's commands need to be runnable as they stand: the stanza every
	// pgbackrest command takes, and the bucket the repository actually lives in (the node
	// picks one of the SeaweedFS node's, so it is not always that node's default).
	BackupStanza string `json:"backupStanza,omitempty"`
	BackupBucket string `json:"backupBucket,omitempty"`
	// The systemd unit and data directory this node's PostgreSQL uses. Both are
	// OS/major-dependent (postgresql-18 vs postgresql@18-main), and a restore needs to
	// name them exactly — so they are recorded rather than guessed in the panel.
	Service      string `json:"service,omitempty"`
	DataDir      string `json:"dataDir,omitempty"`
	GenerateCert bool   `json:"generateCert"`
	UseProxy     bool   `json:"useProxy"`
	// Data-at-rest encryption (pg_tde) keyed to an OpenBao node. Same field and same shape as
	// the Percona Server and PSMDB nodes carry, so the panels describe it the same way; empty
	// when the node is not encrypted. See pgtde.go.
	Vault       vaultInfo `json:"vault"`
	MonitoredBy string    `json:"monitoredBy"` // PMM node FQDN, if any
	Ports       []int     `json:"ports"`
	ExportPort  int       `json:"exportPort"` // published host port for 5432 (0 = none)
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
	backupRepo, backupStanza := "", ""
	if n.UsePgBackRest {
		backupRepo = "pgbackrest → SeaweedFS S3"
		backupStanza = patroniStanza(n.Label)
	}

	cfg := pgConfig{
		Image: image, OS: n.OS, Hostname: host, FQDN: fqdn,
		PGMajor: major, PGVersion: n.PGVersion, Role: "standalone",
		UsePgBackRest: n.UsePgBackRest, BackupRepo: backupRepo, BackupStanza: backupStanza,
		Service: pgServiceName(n.OS, major), DataDir: pgDataDir(n.OS, major),
		GenerateCert: n.GenerateCert, UseProxy: n.UseProxy, MonitoredBy: monitoredBy,
		Ports: []int{patroniPGPort},
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
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
			c, s, e := a.waitSeaweedBucket(ctx, st.ID, n.SeaweedFSNodeID, n.SeaweedFSBucket, deployTimeout())
			if e != nil {
				pr.fail("%v", e)
				return
			}
			swCfg, swSec = c, s
			// The bucket is the node's own choice among the store's; say which one, now
			// that it is resolved, so the panel names the repository it can be found in.
			a.persistConfigKeys(st, n.ID, map[string]any{
				"backupBucket": swCfg.Bucket,
				"backupRepo":   fmt.Sprintf("pgbackrest → SeaweedFS S3 (%s/pgbackrest)", swCfg.Bucket),
			})
		}

		// ---- create + start the container ----
		pr.phase("Creating container", 15)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		spec := ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		}
		applyVMSize(&spec, n.limits())
		if n.ExportEnabled {
			spec.PublishMap = []PortMap{{ContainerPort: patroniPGPort, HostPort: n.ExportHostPort}}
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
		if n.ExportEnabled {
			if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", patroniPGPort)); e == nil {
				if p, e2 := strconv.Atoi(hp); e2 == nil {
					cfg.ExportPort = p
				}
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		pr.phase("Waiting for systemd", 25)
		if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, n.OS, pr.logln)
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
		env := []string{"PRODUCT=" + ppgProduct(major), "PKGS=" + strings.Join(pkgs, " "), "VER=" + n.PGVersion}
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
			if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/pgbackrest", "pgbackrest.conf", 0o644, []byte(conf)); err != nil {
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
		confEnv := []string{"CONFDIR=" + confDir, "DATADIR=" + dataDir, "STANZA=" + stanza,
			"HBALINES=" + strings.Join(pgHostAuthLines(n.PGHostAuth), "\n")}
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

		// ---- data-at-rest encryption (pg_tde → OpenBao) ----
		// After the server is up, not before it starts: unlike MySQL's keyring and MongoDB's
		// security.vault, a pg_tde key provider is registered by calling a SQL function. Before
		// the pgBackRest stanza so the initial full backup is taken of an encrypted cluster.
		if n.EnableVault {
			info, verr := a.applyPGVault(ctx, st, n, id, host, confDir, service, pr)
			if verr != nil {
				// Fatal, like the Percona Server node's keyring. A node that reports encryption
				// it does not have is worse than a node that failed to deploy, and pg_tde cannot
				// be added to data that was already written unencrypted without rewriting it.
				pr.fail("configure pg_tde: %v", verr)
				return
			}
			cfg.Vault = info
			cfgJSON, _ = json.Marshal(cfg)
			a.persistConfigKey(st, n.ID, "vault", info)
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
		if n.LdapAuth || n.KerberosAuth {
			if err := a.applyDirectoryAuth(ctx, st, n, doc, dep.ContainerID, "pg", "", pr); err != nil {
				pr.logln("directory authentication skipped: " + err.Error())
			}
		}
		if n.EnableOIDC {
			if err := a.applyPGOIDC(ctx, st, n, doc, dep.ContainerID, service, pr); err != nil {
				pr.logln("Keycloak OIDC skipped: " + err.Error())
			}
		}
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d pg %s: provisioned (standalone PostgreSQL %s)", st.ID, n.Label, major)
	}()
}

// pgApplyCert stages the Intranet CA into the node and signs a server cert + key
// into the PostgreSQL data dir (postgres-owned) with the given TTL.
func (a *App) pgApplyCert(ctx context.Context, containerID, intranetID, fqdn, dataDir string, ttlValue int, ttlUnit string, logln func(string)) error {
	if logln == nil {
		logln = func(string) {}
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	caCrt, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKey, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	if err := a.engCtx(ctx).PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{
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
	if _, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"bash", "-c", script}, env); err != nil {
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
	a.stampEngine(r, st, nid)
	ctx := r.Context()
	env := []string{"STANZA=" + patroniStanza(node.Label)}
	if res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"bash", "-c", patroniBackupNowScript}, env); err != nil {
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
    printf '%s\n' "$HBALINES"
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
` + serverCertExtScript + `openssl x509 -req -in /tmp/s.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/server.crt" -extfile /tmp/dbca-san.ext -not_after "$END" >/dev/null
chown postgres:postgres "$DIR/ca.crt" "$DIR/server.crt" "$DIR/server.key"
chmod 600 "$DIR/server.key"
chmod 644 "$DIR/ca.crt" "$DIR/server.crt"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/s.csr /tmp/dbca-san.ext /tmp/dbca-ca.srl`
