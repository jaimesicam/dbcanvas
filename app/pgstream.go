package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pgstream.go — the PostgreSQL Streaming Replication cluster frame (Type=="pgstream").
//
// A group of Percona Distribution for PostgreSQL servers on the systemd OS images, replicating
// with PostgreSQL's own physical streaming replication and nothing on top of it: member 1 is
// initialised as the primary, every other member is a hot standby made with pg_basebackup and
// streaming from it. There is no failover manager — no Patroni, no repmgrd — so a primary that
// stops stays stopped until somebody promotes a standby, which is the point of the frame: it is
// the replication PostgreSQL ships, configured by hand the way the documentation describes.
//
// What the frame sets:
//   - a `replicator` role with REPLICATION, and a pg_hba rule for it;
//   - one physical replication slot per standby on the primary (pgNoSlots turns them off, and
//     wal_keep_size alone then holds WAL back for a standby that falls behind);
//   - each standby's primary_conninfo, with application_name = its hostname, so the primary's
//     pg_stat_replication and synchronous_standby_names can name it;
//   - synchronous replication when pgSyncStandbys > 0: synchronous_standby_names =
//     'ANY N ("host2", "host3", …)' and synchronous_commit = pgSyncCommit (on, remote_write or
//     remote_apply). It is applied once every standby streams, so the cluster never waits on a
//     standby that does not exist yet.
//
//   - optionally, pgBackRest to a SeaweedFS S3 node (usePgBackRest): installed and configured on
//     every member, archive_command set everywhere (only the primary archives; a standby promoted
//     later starts archiving by itself), the stanza and a first full backup taken on the primary.
//     Barman is not offered: the frame runs Percona's packages, and Barman is not one of them.
//
// A switchover (switchover.go) is the manual one: the primary is shut down cleanly, the
// candidate replays everything up to the old primary's shutdown checkpoint and is promoted, and
// the old primary and the other standbys are pointed at it. A rebuild takes a fresh pg_basebackup.

// pgClusterNoun names a PostgreSQL cluster frame type in a message.
func pgClusterNoun(frameType string) string {
	switch frameType {
	case "patroni":
		return "Patroni cluster"
	case "repmgr":
		return "repmgr cluster"
	case "pgstream":
		return "streaming replication cluster"
	case "spock":
		return "Spock cluster"
	}
	return "PostgreSQL cluster"
}

// pgStreamReplUser is the role the standbys stream as.
const pgStreamReplUser = "replicator"

// pgStreamConfig is the non-secret profile shown for a deployed member.
type pgStreamConfig struct {
	Cluster   string `json:"cluster"`
	Image     string `json:"image"`
	OS        string `json:"os"`
	Hostname  string `json:"hostname"`
	FQDN      string `json:"fqdn"`
	PGMajor   string `json:"pgMajor"`
	PGVersion string `json:"pgVersion"`
	// Role is the role at deploy time ("primary" | "standby"); a switchover rewrites it.
	Role string `json:"role"`
	// SourceHost is the FQDN a standby streams from ("" on the primary). The switchover code
	// keeps it current, the same field the MySQL replication members carry.
	SourceHost string `json:"sourceHost"`
	// SlotName is this member's physical replication slot on whichever member is primary
	// ("" when the cluster runs without slots). The application_name is the Hostname.
	SlotName string `json:"slotName,omitempty"`
	// The synchronous replication the cluster was deployed with: how many standbys must
	// confirm a commit (0 = asynchronous) and the synchronous_commit level they confirm at.
	SyncStandbys int    `json:"syncStandbys"`
	SyncCommit   string `json:"syncCommit,omitempty"`
	// Backups: "pgbackrest" or "". The rest is what the Backup tab's commands need — the same
	// fields, with the same meaning, as the standalone node's and the repmgr frame's.
	BackupEngine string `json:"backupEngine,omitempty"`
	BackupRepo   string `json:"backupRepo,omitempty"`
	BackupStanza string `json:"backupStanza,omitempty"`
	BackupBucket string `json:"backupBucket,omitempty"`
	Service      string `json:"service,omitempty"`
	DataDir      string `json:"dataDir,omitempty"`
	BinDir       string `json:"binDir,omitempty"`
	GenerateCert bool   `json:"generateCert"`
	UseProxy     bool   `json:"useProxy"`
	MonitoredBy  string `json:"monitoredBy"`
	QuerySource  string `json:"querySource,omitempty"`
	PGVector     string `json:"pgVector,omitempty"`
	Ports        []int  `json:"ports"`
	ExportPort   int    `json:"exportPort"`
	// Peers are the cluster's other members, so the panel can print commands naming them.
	Peers []pgStreamPeer `json:"peers,omitempty"`
}

// pgStreamPeer is one other member of the same cluster.
type pgStreamPeer struct {
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
	Role     string `json:"role"` // at deploy time
	SlotName string `json:"slotName,omitempty"`
}

// pgStreamSyncCommits are the synchronous_commit levels the frame offers ("" is "on").
var pgStreamSyncCommits = map[string]bool{"": true, "on": true, "remote_write": true, "remote_apply": true}

// pgStreamMembers is the frame's members in label order; member 0 is the initial primary.
func pgStreamMembers(frame designFrame, doc designDoc) []designNode {
	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "pgstream" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	return members
}

var pgSlotUnsafe = regexp.MustCompile(`[^a-z0-9_]`)

// pgStreamSlotName is a member's physical slot name: its hostname with everything a slot name
// cannot hold ([a-z0-9_] only, at most 63 bytes) turned into underscores.
func pgStreamSlotName(host string) string {
	s := pgSlotUnsafe.ReplaceAllString(strings.ToLower(host), "_")
	if s == "" {
		s = "standby"
	}
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// pgStreamSlot is the slot a member uses in this frame, or "" when slots are off.
func pgStreamSlot(frame designFrame, host string) string {
	if frame.PGNoSlots {
		return ""
	}
	return pgStreamSlotName(host)
}

// pgStreamSyncNames is synchronous_standby_names for a primary whose standbys are `standbys`
// (their hostnames, which are their application_names): "" for an asynchronous cluster, and
// otherwise a quorum of min(pgSyncStandbys, len(standbys)) among all of them. Quorum (ANY) rather
// than priority (FIRST), so any standby confirming counts and none of them is special.
func pgStreamSyncNames(frame designFrame, standbys []string) string {
	n := frame.PGSyncStandbys
	if n <= 0 || len(standbys) == 0 {
		return ""
	}
	if n > len(standbys) {
		n = len(standbys)
	}
	quoted := make([]string, 0, len(standbys))
	for _, h := range standbys {
		quoted = append(quoted, `"`+strings.ReplaceAll(h, `"`, ``)+`"`)
	}
	return fmt.Sprintf("ANY %d (%s)", n, strings.Join(quoted, ", "))
}

// pgStreamSyncCommit is synchronous_commit for the frame.
func pgStreamSyncCommit(frame designFrame) string {
	if frame.PGSyncCommit == "" {
		return "on"
	}
	return frame.PGSyncCommit
}

// pgStreamPeersOf is every member except `self`.
func pgStreamPeersOf(frame designFrame, members []designNode, hosts map[string]string, domain string, self int) []pgStreamPeer {
	var out []pgStreamPeer
	for i, n := range members {
		if i == self {
			continue
		}
		role := "standby"
		if i == 0 {
			role = "primary"
		}
		out = append(out, pgStreamPeer{Hostname: hosts[n.ID], FQDN: fqdnOf(hosts[n.ID], domain), Role: role, SlotName: pgStreamSlot(frame, hosts[n.ID])})
	}
	return out
}

// pgStreamMemberConfig is the deployment record of member i.
func pgStreamMemberConfig(frame designFrame, doc designDoc, members []designNode, i int, role, source string) pgStreamConfig {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	major := ppgMajorOf(frame.PGMajor)
	n := members[i]
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == frame.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[m.ID], domain)
			}
		}
	}
	pgVector := ""
	if frame.PGVector {
		pgVector = pgVectorSource(frame.Type, frame.OS, major)
	}
	slot := ""
	if role != "primary" {
		slot = pgStreamSlot(frame, hosts[n.ID])
	}
	engine, repo, stanza := "", "", ""
	if frame.UsePgBackRest {
		engine, repo, stanza = "pgbackrest", "pgBackRest → SeaweedFS S3", patroniStanza(frame.Label)
	}
	return pgStreamConfig{
		BackupEngine: engine, BackupRepo: repo, BackupStanza: stanza,
		Cluster: frame.Label, Image: pxcImage(frame.OS, frame.OSVersion, frame.Arch), OS: frame.OS,
		Hostname: hosts[n.ID], FQDN: fqdnOf(hosts[n.ID], domain),
		PGMajor: major, PGVersion: frame.PGVersion, Role: role, SourceHost: source, SlotName: slot,
		SyncStandbys: frame.PGSyncStandbys, SyncCommit: pgStreamSyncCommit(frame),
		Service: pgServiceName(frame.OS, major), DataDir: pgDataDir(frame.OS, major), BinDir: pgBinDir(frame.OS, major),
		GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
		QuerySource: pgQuerySourceFor(frame.Type, frame.PGQuerySource, frame.PMMNodeID),
		PGVector:    pgVector, Ports: []int{patroniPGPort},
		Peers: pgStreamPeersOf(frame, members, hosts, domain, i),
	}
}

// pgStreamFrameIssues validates one frame (everything but the image, which the caller checks
// once per image across the design). Export ports are added to exportReq for the shared check.
func pgStreamFrameIssues(f designFrame, doc designDoc, exportReq map[int][]string) []issue {
	var out []issue
	who := "Streaming replication cluster " + f.Label
	members := 0
	for _, n := range doc.Nodes {
		if n.FrameID != f.ID || n.Type != "pgstream" {
			continue
		}
		members++
		if n.ExportEnabled && n.ExportHostPort > 0 {
			exportReq[n.ExportHostPort] = append(exportReq[n.ExportHostPort], n.Label)
		}
	}
	switch {
	case members < 2:
		out = append(out, issue{Level: "error", Message: who + " needs at least 2 nodes: a primary and a standby"})
	case members > 9:
		out = append(out, issue{Level: "error", Message: who + " allows at most 9 nodes"})
	}
	if f.PGSyncStandbys < 0 {
		out = append(out, issue{Level: "error", Message: who + ": the number of synchronous standbys cannot be negative"})
	} else if members >= 2 && f.PGSyncStandbys > members-1 {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s waits for %d synchronous standbys but has only %d — every commit would hang", who, f.PGSyncStandbys, members-1)})
	} else if f.PGSyncStandbys > 0 && f.PGSyncStandbys == members-1 {
		out = append(out, issue{Level: "warning", Message: who + " waits for every standby to confirm a commit: writes stop while any one of them is down"})
	}
	if !pgStreamSyncCommits[f.PGSyncCommit] {
		out = append(out, issue{Level: "error", Message: who + `: unknown synchronous_commit level "` + f.PGSyncCommit + `" (on, remote_write or remote_apply)`})
	}
	out = append(out, pgHostAuthIssues(who, f.PGHostAuth, f.GenerateCert)...)
	out = append(out, pgQuerySourceIssues(who, "pgstream", f.PGQuerySource)...)
	out = append(out, pgBenchIssues(who, f.PGBenchScale)...)
	if f.UsePgBackRest {
		// pgBackRest's S3 client is HTTPS-only: the same rule, and the same check, as Patroni's.
		out = append(out, pgBackRestSeaweedIssues(who, f.SeaweedFSNodeID, doc)...)
		out = append(out, seaweedBucketIssues(who, f.SeaweedFSNodeID, f.SeaweedFSBucket, doc)...)
	}
	return out
}

// --- frame orchestration ---

// provisionPGStreamFrame brings up the cluster: every member's container and packages in
// parallel, then the primary, then each standby in turn, then synchronous replication, PMM.
func (a *App) provisionPGStreamFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	members := pgStreamMembers(frame, doc)
	if len(members) == 0 {
		return
	}
	sec := pgFamilySecrets()
	sec.ReplUser = pgStreamReplUser
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := ppgMajorOf(frame.PGMajor)
	primaryFQDN := fqdnOf(hosts[members[0].ID], domain)

	for i, n := range members {
		role, source := "standby", primaryFQDN
		if i == 0 {
			role, source = "primary", ""
		}
		cfgJSON, _ := json.Marshal(pgStreamMemberConfig(frame, doc, members, i, role, source))
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		for _, n := range members {
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			a.pxcNewProg(st.ID, n.ID).phase("Waiting for Intranet to be ready", 5)
		}
		_, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).fail("%v", err)
			}
			return
		}
		sw, err := a.pgStreamBackupStore(ctx, st, frame, members)
		if err != nil {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).fail("%v", err)
			}
			return
		}

		// ---- Phase 1 (parallel): container + packages ----
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if a.pgStreamPrepareNode(ctx, st, frame, n, image, intranetIP, domain, sec, sw) != nil {
					mu.Lock()
					failed = true
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		if failed {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)

		// ---- Phase 2: the primary ----
		primary := members[0]
		ppr := a.pxcNewProg(st.ID, primary.ID)
		ppr.phase("Initialising primary PostgreSQL", 55)
		if a.pgStreamSetupPrimary(ctx, st, frame, primary, major, sec, ppr) != nil {
			for _, n := range members[1:] {
				a.pxcNewProg(st.ID, n.ID).fail("the primary %s failed to initialise", primary.Label)
			}
			return
		}
		pdep, _ := a.store.GetDeployment(st.ID, primary.ID)

		// ---- Phase 3: each standby, one at a time (each base backup checkpoints the primary) ----
		var standbyHosts []string
		for _, n := range members[1:] {
			spr := a.pxcNewProg(st.ID, n.ID)
			spr.phase("Base backup from "+primary.Label, 60)
			if a.pgStreamSetupStandby(ctx, st, frame, n, major, primaryFQDN, pdep.ContainerID, sec, spr) == nil {
				standbyHosts = append(standbyHosts, hosts[n.ID])
			}
		}

		// ---- Phase 4: synchronous replication, now that the standbys stream ----
		if err := a.pgStreamApplySync(ctx, pdep.ContainerID, frame, standbyHosts, ppr.logln); err != nil {
			ppr.logln("synchronous replication not applied: " + err.Error())
		}

		// ---- Phase 5: pgbench data on the primary, then the first backup, PMM, finish ----
		if frame.PGBenchScale > 0 {
			ppr.phase(fmt.Sprintf("Loading pgbench data (scale %d)", frame.PGBenchScale), 88)
			if err := a.loadPGBench(ctx, pdep.ContainerID, pgBinDir(frame.OS, major), frame.PGBenchScale, ppr.logln); err != nil {
				ppr.logln(err.Error())
			}
		}
		if frame.UsePgBackRest {
			ppr.phase("Creating pgBackRest stanza + initial backup", 91)
			if err := a.runStep(ctx, pdep.ContainerID, patroniBackupScript, []string{"STANZA=" + patroniStanza(frame.Label)}, ppr.logln); err != nil {
				// Non-fatal, as on repmgr: the cluster replicates, and Backup now retries it.
				ppr.logln("initial pgBackRest backup failed: " + err.Error())
			} else {
				ppr.logln("pgBackRest stanza created + initial full backup taken")
			}
		}
		for _, n := range members {
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			if dep.State == DeployError {
				continue
			}
			pr := a.pxcNewProg(st.ID, n.ID)
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 95)
				a.patroniRegisterPMM(ctx, st, n, frame, doc, sec, pr)
			}
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: dep.ContainerID, State: DeployRunning, Config: dep.Config, Secrets: secJSON})
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d pgstream %s: provisioned (%d node(s), primary %s)", st.ID, frame.Label, len(members), primary.Label)
	}()
}

// pgStreamPrepareNode creates the container and installs PostgreSQL (+ pmm-client,
// pg_stat_monitor, pgvector) and the replicator's .pgpass. Nothing is initialised yet.
func (a *App) pgStreamPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, image, intranetIP, domain string, sec pgSecrets, sw *pgStreamStore) error {
	pr := a.pxcNewProg(st.ID, n.ID)
	host := stackHostnames(buildDoc(st))[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	major := ppgMajorOf(frame.PGMajor)
	debian := isDebianOS(frame.OS)

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
		return pr.fail("create container: %v", err)
	}
	if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

	var cfg pgStreamConfig
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	json.Unmarshal(dep.Config, &cfg)
	if n.ExportEnabled {
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", patroniPGPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.ExportPort = p
			}
		}
	}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: dep.Secrets})

	pr.phase("Waiting for systemd", 25)
	if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
		return pr.fail("systemd did not start: %v", err)
	}
	a.trustIntranetCA(ctx, st, id, frame.OS, pr.logln)
	a.ensureDNFIPv4(ctx, id, frame.OS, pr.logln)
	if err := a.useRepository(ctx, st, id, frame.RepositoryNodeID, pr.logln); err != nil {
		return pr.fail("%v", err)
	}
	if frame.UseProxy {
		proxyScript := pkgProxyRHEL
		if debian {
			proxyScript = pkgProxyDebian
		}
		if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
			return pr.fail("configure package proxy: %v", err)
		}
		pr.logln("package egress via Intranet proxy")
	}

	pr.phase("Installing PostgreSQL", 35)
	instScript := patroniInstallRHEL
	if debian {
		instScript = patroniInstallDebian
	}
	pkgs := pgServerPackages(frame.OS, major)
	if frame.UsePgBackRest {
		pkgs = append(pkgs, "percona-pgbackrest")
	}
	env := []string{"PRODUCT=" + ppgProduct(major), "PKGS=" + strings.Join(pkgs, " "), "VER=" + frame.PGVersion}
	if frame.UsePgBackRest && !debian {
		// EPEL provides percona-pgbackrest's libssh2 on Oracle Linux.
		env = append(env, "WITH_EPEL=1", "EPELPKG="+epelPackage(frame.OSVersion))
	}
	if err := a.runStep(ctx, id, instScript, env, pr.logln); err != nil {
		return pr.fail("install PostgreSQL: %v", err)
	}
	pr.logln("Percona Distribution for PostgreSQL " + major + " installed")
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	if frame.PMMNodeID != "" {
		pr.phase("Installing PMM client", 42)
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
	// Every member, not only the primary: a standby is a copy of the primary's data directory,
	// which preloads the library and carries the catalog entries, but not the files a package
	// puts on disk — and any standby may be promoted.
	if pgQuerySourceFor(frame.Type, frame.PGQuerySource, frame.PMMNodeID) == pgQSMonitor {
		if err := a.installPGStatMonitor(ctx, id, frame.OS, major, pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}
	if frame.PGVector {
		pr.phase("Installing pgvector", 44)
		if err := a.installPGVector(ctx, id, frame.OS, major, "ppg", pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}

	// pgBackRest's configuration on every member: any of them may be primary one day, and the
	// archive_command every member carries names this stanza.
	if frame.UsePgBackRest && sw != nil {
		if err := a.runStep(ctx, id, patroniPgBackRestDirsScript, nil, pr.logln); err != nil {
			return pr.fail("prepare pgbackrest dirs: %v", err)
		}
		conf := patroniPgBackRestConf(frame.Label, frame.OS, major, sw.cfg, sw.sec, false)
		if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/pgbackrest", "pgbackrest.conf", 0o644, []byte(conf)); err != nil {
			return pr.fail("write pgbackrest.conf: %v", err)
		}
		pr.logln("pgbackrest.conf written (stanza " + patroniStanza(frame.Label) + ", repo " + sw.cfg.Bucket + "/pgbackrest)")
	}

	// The data directory exists (initdb on EL, pg_createcluster on Debian, which also lays down
	// /etc/postgresql/<major>/main). A standby's is emptied and refilled by pg_basebackup later;
	// on Debian its configuration directory stays, which is why the standby is configured too.
	pr.phase("Initialising data directory", 48)
	initEnv := []string{"MAJOR=" + major, "BINDIR=" + pgBinDir(frame.OS, major), "DATADIR=" + pgDataDir(frame.OS, major)}
	if debian {
		initEnv = append(initEnv, "DEBIAN=1")
	}
	if err := a.runStep(ctx, id, pgInitScript, initEnv, pr.logln); err != nil {
		return pr.fail("initdb: %v", err)
	}

	// The replicator's password, for every member: the standbys' primary_conninfo names this file
	// rather than carrying the password, and a demoted primary needs it as much as a standby does.
	home := pgHome(frame.OS)
	if err := a.engCtx(ctx).CopyFile(ctx, id, home, ".pgpass", 0o600, []byte(fmt.Sprintf("*:*:*:%s:%s\n", sec.ReplUser, sec.ReplPassword))); err != nil {
		return pr.fail("write .pgpass: %v", err)
	}
	if err := a.runStep(ctx, id, `chown postgres:postgres "$HOME/.pgpass"; chmod 600 "$HOME/.pgpass"`, []string{"HOME=" + home}, pr.logln); err != nil {
		return pr.fail("fix .pgpass ownership: %v", err)
	}
	return nil
}

// pgStreamConfEnv is the environment of pgStreamConfigureScript: the same on every member.
func pgStreamConfEnv(frame designFrame, confDir, dataDir string) []string {
	qs := pgQuerySourceFor(frame.Type, frame.PGQuerySource, frame.PMMNodeID)
	env := []string{"CONFDIR=" + confDir, "DATADIR=" + dataDir, "REPLUSER=" + pgStreamReplUser,
		"HBALINES=" + strings.Join(pgHostAuthLines(frame.PGHostAuth), "\n"),
		"PRELOAD=" + pgPreloadValue(nil, qs),
		"QSCONF=" + strings.Join(pgQueryConfLines(qs), "\n")}
	if frame.GenerateCert {
		env = append(env, "TLS=1")
	}
	if frame.UsePgBackRest {
		env = append(env, "ARCHIVE_CMD="+repmgrPgBackRestArchiveCommand(frame.Label))
	}
	return env
}

// pgStreamStore is the resolved SeaweedFS store a cluster backs up to.
type pgStreamStore struct {
	cfg seaweedConfig
	sec seaweedSecrets
}

// pgStreamBackupStore waits for the frame's SeaweedFS bucket when it backs up, records where the
// repository is in every member's config, and returns it; nil when the frame does not back up.
func (a *App) pgStreamBackupStore(ctx context.Context, st Stack, frame designFrame, members []designNode) (*pgStreamStore, error) {
	if !frame.UsePgBackRest {
		return nil, nil
	}
	for _, n := range members {
		a.pxcNewProg(st.ID, n.ID).phase("Waiting for SeaweedFS (pgBackRest repository)", 8)
	}
	c, s, err := a.waitSeaweedBucket(ctx, st.ID, frame.SeaweedFSNodeID, frame.SeaweedFSBucket, deployTimeout())
	if err != nil {
		return nil, err
	}
	for _, n := range members {
		a.persistConfigKeys(st, n.ID, map[string]any{
			"backupBucket": c.Bucket,
			"backupRepo":   fmt.Sprintf("pgBackRest → SeaweedFS S3 (%s/pgbackrest)", c.Bucket),
		})
	}
	return &pgStreamStore{cfg: c, sec: s}, nil
}

// pgStreamSetupPrimary initialises, configures and starts the primary, and creates the
// replicator role and the chosen extensions.
func (a *App) pgStreamSetupPrimary(ctx context.Context, st Stack, frame designFrame, n designNode, major string, sec pgSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	dataDir := pgDataDir(frame.OS, major)
	domain := envOr("DOMAIN", "example.net")
	fqdn := fqdnOf(stackHostnames(buildDoc(st))[n.ID], domain)
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 56)
		if err := a.pgApplyCert(ctx, id, a.intranetContainerID(ctx, st), fqdn, dataDir, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}
	if err := a.runStep(ctx, id, pgStreamConfigureScript, pgStreamConfEnv(frame, pgConfDir(frame.OS, major), dataDir), pr.logln); err != nil {
		return pr.fail("configure primary: %v", err)
	}
	pr.phase("Starting primary PostgreSQL", 58)
	if err := a.runStep(ctx, id, pgStartScript, []string{"SERVICE=" + pgServiceName(frame.OS, major)}, pr.logln); err != nil {
		return pr.fail("start PostgreSQL: %v", err)
	}
	if err := a.runStep(ctx, id, pgSetPasswordScript, []string{"SUPERPW=" + sec.SuperPassword}, pr.logln); err != nil {
		return pr.fail("set superuser password: %v", err)
	}
	if err := a.runStep(ctx, id, pgStreamCreateRoleScript, []string{"REPLUSER=" + sec.ReplUser, "REPLPW=" + sec.ReplPassword}, pr.logln); err != nil {
		return pr.fail("create the replication role: %v", err)
	}
	// The stanza now rather than with the first backup: archive_command runs from the moment the
	// primary starts, and every segment it tries before the stanza exists fails and is retried —
	// harmless, but it fills pg_stat_archiver.failed_count and the log with errors that mean nothing.
	if frame.UsePgBackRest {
		if err := a.runStep(ctx, id, `runuser -u postgres -- pgbackrest --stanza="$STANZA" stanza-create`, []string{"STANZA=" + patroniStanza(frame.Label)}, pr.logln); err != nil {
			pr.logln("pgBackRest stanza-create failed (retried with the first backup): " + err.Error())
		}
	}
	// Before any standby is copied, so they arrive with the extensions already there.
	if err := a.enablePGQueryExtension(ctx, id, pgQuerySourceFor(frame.Type, frame.PGQuerySource, frame.PMMNodeID), "", pr.logln); err != nil {
		return pr.fail("%v", err)
	}
	if frame.PGVector {
		if err := a.enablePGVector(ctx, id, "", pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}
	pr.logln("primary running; role " + sec.ReplUser + " can stream from it")
	return nil
}

// pgStreamSetupStandby makes member n a standby of the primary at primaryFQDN: its slot on the
// primary, a base backup, its own certificate, its standby settings, and a start. It returns
// once the standby reports a streaming WAL receiver.
func (a *App) pgStreamSetupStandby(ctx context.Context, st Stack, frame designFrame, n designNode, major, primaryFQDN, primaryCID string, sec pgSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	host := stackHostnames(buildDoc(st))[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	domain := envOr("DOMAIN", "example.net")
	dataDir := pgDataDir(frame.OS, major)
	slot := pgStreamSlot(frame, host)
	if slot != "" {
		if err := a.runStep(ctx, primaryCID, pgStreamCreateSlotScript, []string{"SLOT=" + slot}, pr.logln); err != nil {
			return pr.fail("create replication slot %s on the primary: %v", slot, err)
		}
		pr.logln("replication slot " + slot + " on the primary")
	}
	if err := a.runStep(ctx, id, pgStreamBaseBackupScript, pgStreamStandbyEnv(frame, major, host, primaryFQDN, slot, sec), pr.logln); err != nil {
		return pr.fail("pg_basebackup: %v", err)
	}
	pr.logln("base backup copied from " + primaryFQDN)
	if frame.GenerateCert {
		if err := a.pgApplyCert(ctx, id, a.intranetContainerID(ctx, st), fqdnOf(host, domain), dataDir, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}
	// Debian keeps postgresql.conf and pg_hba.conf in /etc/postgresql, which a base backup does
	// not copy: the standby has the package defaults until it is given the same settings.
	if isDebianOS(frame.OS) {
		if err := a.runStep(ctx, id, pgStreamConfigureScript, pgStreamConfEnv(frame, pgConfDir(frame.OS, major), dataDir), pr.logln); err != nil {
			return pr.fail("configure standby: %v", err)
		}
	}
	pr.phase("Starting standby PostgreSQL", 70)
	if err := a.runStep(ctx, id, pgStartScript, []string{"SERVICE=" + pgServiceName(frame.OS, major)}, pr.logln); err != nil {
		return pr.fail("start PostgreSQL: %v", err)
	}
	if err := a.runStep(ctx, id, pgStreamWaitStreamingScript, nil, pr.logln); err != nil {
		return pr.fail("%v", err)
	}
	pr.logln("streaming from " + primaryFQDN)
	return nil
}

// pgStreamStandbyEnv is the environment of the base backup and follow scripts.
func pgStreamStandbyEnv(frame designFrame, major, host, primaryFQDN, slot string, sec pgSecrets) []string {
	return []string{
		"SERVICE=" + pgServiceName(frame.OS, major), "BINDIR=" + pgBinDir(frame.OS, major),
		"DATADIR=" + pgDataDir(frame.OS, major), "PRIMARY=" + primaryFQDN, "REPLUSER=" + firstNonEmpty(sec.ReplUser, pgStreamReplUser),
		"APPNAME=" + host, "SLOT=" + slot, "PASSFILE=" + pgHome(frame.OS) + "/.pgpass",
	}
}

// pgStreamApplySync sets synchronous replication on the primary for these standbys, or turns it
// off when the frame is asynchronous. A reload is enough: neither setting needs a restart.
func (a *App) pgStreamApplySync(ctx context.Context, primaryCID string, frame designFrame, standbys []string, logln func(string)) error {
	names := pgStreamSyncNames(frame, standbys)
	env := []string{"NAMES=" + names, "COMMIT=" + pgStreamSyncCommit(frame)}
	if err := a.runStep(ctx, primaryCID, pgStreamSyncScript, env, logln); err != nil {
		return err
	}
	if names != "" {
		logln("synchronous_standby_names = '" + names + "', synchronous_commit = " + pgStreamSyncCommit(frame))
	}
	return nil
}

// pgStreamPrimaryContainer is the container of the member that is not in recovery, or "".
func (a *App) pgStreamPrimaryContainer(ctx context.Context, st Stack, frame designFrame, doc designDoc) string {
	for _, n := range pgStreamMembers(frame, doc) {
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			continue
		}
		if res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"bash", "-c", repmgrIsPrimaryScript}, nil); err == nil && strings.TrimSpace(res.Stdout) == "primary" {
			return dep.ContainerID
		}
	}
	return ""
}

// --- join, rebuild, switchover ---

// joinPGStreamFrame adds fresh standbys to a running cluster: each is built like a first-deploy
// member, copied from whichever member is primary now, and added to synchronous_standby_names.
func (a *App) joinPGStreamFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	hosts := stackHostnames(doc)
	domain := envOr("DOMAIN", "example.net")
	sec := pgFamilySecrets()
	sec.ReplUser = pgStreamReplUser
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := ppgMajorOf(frame.PGMajor)
	members := pgStreamMembers(frame, doc)
	for _, n := range fresh {
		idx := 0
		for k, m := range members {
			if m.ID == n.ID {
				idx = k
			}
		}
		cfgJSON, _ := json.Marshal(pgStreamMemberConfig(frame, doc, members, idx, "standby", ""))
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range fresh {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		_, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			for _, n := range fresh {
				progs[n.ID].fail("%v", err)
			}
			return
		}
		sw, err := a.pgStreamBackupStore(ctx, st, frame, fresh)
		if err != nil {
			for _, n := range fresh {
				progs[n.ID].fail("%v", err)
			}
			return
		}
		var built []designNode
		for _, n := range fresh {
			if a.pgStreamPrepareNode(ctx, st, frame, n, image, intranetIP, domain, sec, sw) == nil {
				built = append(built, n)
			}
		}
		a.reconcileStackDNS(ctx, st.ID)
		joined := false
		for _, n := range built {
			pr := progs[n.ID]
			markNodeAction(st.ID, n.ID)
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, primary, err := a.joinPrimary(pctx, st, frame, doc)
			cancel()
			if err != nil {
				pr.fail("%v", err)
				continue
			}
			pr.phase("Base backup from "+primary.Node.Label, 60)
			if a.pgStreamSetupStandby(ctx, st, frame, n, major, primary.FQDN, primary.Dep.ContainerID, sec, pr) != nil {
				continue
			}
			a.persistConfigKeys(st, n.ID, map[string]any{"sourceHost": primary.FQDN})
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 95)
				a.patroniRegisterPMM(ctx, st, n, frame, doc, sec, pr)
			}
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
			markNodeAction(st.ID, n.ID)
			pr.logln("joined " + frame.Label + " as a standby of " + primary.Node.Label)
			joined = true
		}
		if joined && frame.PGSyncStandbys > 0 {
			a.pgStreamResync(ctx, st, frame, doc, hosts)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d pgstream %s: %d member(s) joined", st.ID, frame.Label, len(fresh))
	}()
}

// pgStreamResync re-applies synchronous_standby_names on the current primary, naming every
// other member that has a container — after a join or a switchover changed who those are.
func (a *App) pgStreamResync(ctx context.Context, st Stack, frame designFrame, doc designDoc, hosts map[string]string) {
	pcid := a.pgStreamPrimaryContainer(ctx, st, frame, doc)
	if pcid == "" {
		return
	}
	var standbys []string
	for _, n := range pgStreamMembers(frame, doc) {
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" || dep.ContainerID == pcid {
			continue
		}
		standbys = append(standbys, hosts[n.ID])
	}
	a.pgStreamApplySync(ctx, pcid, frame, standbys, func(string) {})
}

// rebuildPGStream takes a new base backup of the primary into the target.
func (a *App) rebuildPGStream(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob) error {
	var cfg pgStreamConfig
	json.Unmarshal(p.target.Dep.Config, &cfg)
	var sec pgSecrets
	json.Unmarshal(p.target.Dep.Secrets, &sec)
	frame := p.t.Frame
	major := firstNonEmpty(cfg.PGMajor, ppgMajorOf(frame.PGMajor))
	host := firstNonEmpty(cfg.Hostname, p.target.Host)
	slot := pgStreamSlot(frame, host)
	if slot != "" {
		if _, err := a.swExec(ctx, st, p.primary, pgStreamCreateSlotScript, []string{"SLOT=" + slot}); err != nil {
			return fmt.Errorf("create replication slot %s on %s: %v", slot, p.primary.Node.Label, err)
		}
	}
	j.add("%s: PostgreSQL stopped; base backup from %s", p.target.Node.Label, p.primary.FQDN)
	if _, err := a.swExec(ctx, st, p.target, pgStreamBaseBackupScript, pgStreamStandbyEnv(frame, major, host, p.primary.FQDN, slot, sec)); err != nil {
		return fmt.Errorf("pg_basebackup: %v", err)
	}
	j.add("base backup copied")
	if frame.GenerateCert {
		if err := a.pgApplyCert(ctx, p.target.Dep.ContainerID, a.intranetContainerID(ctx, st), p.target.FQDN, pgDataDir(frame.OS, major),
			frame.CertTTLValue, frame.CertTTLUnit, func(string) {}); err != nil {
			return fmt.Errorf("re-issue the certificate: %v", err)
		}
		j.add("TLS certificate re-issued")
	}
	if _, err := a.swExec(ctx, st, p.target, pgStartScript, []string{"SERVICE=" + pgServiceName(frame.OS, major)}); err != nil {
		return fmt.Errorf("start PostgreSQL: %v", err)
	}
	a.persistConfigKeys(st, p.target.Node.ID, map[string]any{"sourceHost": p.primary.FQDN, "role": "standby"})
	return a.waitHealthy(ctx, st, p, j, 10*time.Minute, func(r *liveRole) bool {
		return r.Role == "replica" && len(r.Problems) == 0
	})
}

// switchPGStream moves the primary by hand, the way the PostgreSQL documentation describes a
// planned switchover: stop the primary cleanly (its walsenders send everything, the shutdown
// checkpoint included, before it exits), let the candidate replay all of it, promote the
// candidate, then point the old primary and every other standby at it. Because the old primary
// stopped cleanly and the candidate has its last record, the old primary follows the new
// timeline without pg_rewind; if it cannot (it diverged anyway), pg_rewind is tried once.
func (a *App) switchPGStream(ctx context.Context, st Stack, t swTopo, primary, cand swMember, others []swMember, log *swLog) error {
	frame := t.Frame
	var pcfg pgStreamConfig
	json.Unmarshal(primary.Dep.Config, &pcfg)
	var sec pgSecrets
	json.Unmarshal(primary.Dep.Secrets, &sec)
	major := firstNonEmpty(pcfg.PGMajor, ppgMajorOf(frame.PGMajor))
	service := pgServiceName(frame.OS, major)

	// Slots for every member that will stream from the candidate, created on it while it is
	// still a standby (physical slots can be), so none of them misses WAL after the promotion.
	for _, m := range append([]swMember{primary}, others...) {
		if slot := pgStreamSlot(frame, m.Host); slot != "" {
			if _, err := a.swExec(ctx, st, cand, pgStreamCreateSlotScript, []string{"SLOT=" + slot}); err != nil {
				return fmt.Errorf("create replication slot %s on %s: %v", slot, cand.Node.Label, err)
			}
		}
	}
	if _, err := a.swExec(ctx, st, primary, `runuser -u postgres -- psql -qAtc 'CHECKPOINT' >/dev/null && systemctl stop "$SERVICE"`, []string{"SERVICE=" + service}); err != nil {
		return fmt.Errorf("stop the primary %s: %v", primary.Node.Label, err)
	}
	// Where the old primary ended: its shutdown checkpoint. The candidate has every committed
	// write only once it has replayed past that record — comparing it with what it happened to
	// receive would promote a standby whose stream had stopped, and lose what it never got.
	end, err := a.swExec(ctx, st, primary, pgStreamShutdownLSNScript, []string{"BINDIR=" + pgBinDir(frame.OS, major), "DATADIR=" + pgDataDir(frame.OS, major)})
	if err != nil {
		a.swExec(ctx, st, primary, pgStartScript, []string{"SERVICE=" + service})
		return fmt.Errorf("read where %s stopped (it was started again): %v", primary.Node.Label, err)
	}
	log.add("%s: PostgreSQL stopped cleanly at %s", primary.Node.Label, end)
	if out, err := a.swExec(ctx, st, cand, pgStreamCatchUpScript, []string{"TARGET=" + end}); err != nil {
		a.swExec(ctx, st, primary, pgStartScript, []string{"SERVICE=" + service})
		return fmt.Errorf("%s did not catch up (the old primary was started again): %v", cand.Node.Label, err)
	} else {
		log.add("%s: %s", cand.Node.Label, out)
	}
	if _, err := a.swExec(ctx, st, cand, `runuser -u postgres -- psql -v ON_ERROR_STOP=1 -qAtc 'SELECT pg_promote(true, 60)' | grep -qx t`, nil); err != nil {
		a.swExec(ctx, st, primary, pgStartScript, []string{"SERVICE=" + service})
		return fmt.Errorf("pg_promote on %s failed (the old primary was started again): %v", cand.Node.Label, err)
	}
	log.add("%s promoted", cand.Node.Label)
	var failed []string

	// The old primary becomes a standby of the candidate.
	env := pgStreamStandbyEnv(frame, major, primary.Host, cand.FQDN, pgStreamSlot(frame, primary.Host), sec)
	env = append(env, "SUPERPW="+sec.SuperPassword)
	if out, err := a.swExec(ctx, st, primary, pgStreamDemoteScript, env); err != nil {
		failed = append(failed, fmt.Sprintf("%s could not follow it — rebuild it (%v)", primary.Node.Label, err))
	} else {
		log.add("%s: %s", primary.Node.Label, lastLines(out, 200))
	}
	// The others re-point their primary_conninfo; a reload is enough on PostgreSQL 13+.
	for _, m := range others {
		e := pgStreamStandbyEnv(frame, major, m.Host, cand.FQDN, pgStreamSlot(frame, m.Host), sec)
		if _, err := a.swExec(ctx, st, m, pgStreamRepointScript, e); err != nil {
			failed = append(failed, fmt.Sprintf("%s was not re-pointed at it — rebuild it (%v)", m.Node.Label, err))
		} else {
			log.add("%s now streams from %s", m.Node.Label, cand.Node.Label)
		}
	}
	// Synchronous replication follows the primary: the new primary names everyone else, and the
	// old one, now a standby, must not keep a list that would mean something if it were promoted.
	var standbys []string
	for _, m := range append([]swMember{primary}, others...) {
		standbys = append(standbys, m.Host)
	}
	if _, err := a.swExec(ctx, st, cand, pgStreamSyncScript, []string{"NAMES=" + pgStreamSyncNames(frame, standbys), "COMMIT=" + pgStreamSyncCommit(frame)}); err != nil {
		failed = append(failed, fmt.Sprintf("synchronous replication was not set on it (%v)", err))
	}
	if err := a.waitPrimary(ctx, st, t, cand, log); err != nil {
		return err
	}
	if len(failed) > 0 {
		return &partialSwitch{fmt.Sprintf("%s is the new primary, but: %s", cand.Node.Label, strings.Join(failed, "; "))}
	}
	return nil
}

// pgStreamRebuildMethod is the method line of a rebuild plan.
const pgStreamRebuildMethod = "pg_basebackup: PostgreSQL on the replica is stopped, its data directory emptied and copied from the primary through its replication slot, then started as a standby again."

// ------------------------------------------------------------------ scripts

// pgStreamConfigureScript appends the replication settings, TLS and preloads to postgresql.conf
// and the replication rule to pg_hba.conf, once (a marker guards both). On EL a standby's copies
// come from the primary with the base backup and already carry them.
const pgStreamConfigureScript = `set -e
CONF="$CONFDIR/postgresql.conf"
HBA="$CONFDIR/pg_hba.conf"
[ -f "$CONF" ] || { echo "postgresql.conf not found at $CONF"; exit 1; }
if ! grep -q "dbcanvas-pgstream" "$CONF"; then
  {
    echo ""
    echo "# --- dbcanvas-pgstream ---"
    echo "listen_addresses = '*'"
    echo "port = 5432"
    echo "password_encryption = scram-sha-256"
    echo "wal_level = replica"
    echo "max_wal_senders = 16"
    echo "max_replication_slots = 16"
    echo "wal_keep_size = 512MB"
    echo "hot_standby = on"
    # pg_rewind needs this (or data checksums) to rewind a primary that diverged.
    echo "wal_log_hints = on"
  } >> "$CONF"
  # Every member: only the primary archives (a standby with archive_mode = on does not), and a
  # standby promoted later starts on its own.
  if [ -n "$ARCHIVE_CMD" ]; then
    echo "archive_mode = on" >> "$CONF"
    echo "archive_command = '$ARCHIVE_CMD'" >> "$CONF"
  fi
  if [ -n "$TLS" ]; then
    {
      echo "ssl = on"
      echo "ssl_cert_file = '$DATADIR/server.crt'"
      echo "ssl_key_file = '$DATADIR/server.key'"
      echo "ssl_ca_file = '$DATADIR/ca.crt'"
    } >> "$CONF"
  fi
  if [ -n "$PRELOAD" ]; then
    sed -i "/# dbcanvas-preload/d" "$CONF"
    echo "shared_preload_libraries = '$PRELOAD' # dbcanvas-preload" >> "$CONF"
    [ -z "$QSCONF" ] || printf '%s\n' "$QSCONF" >> "$CONF"
  fi
fi
grep -q "dbcanvas-pgstream" "$HBA" 2>/dev/null || {
  {
    echo "# dbcanvas-pgstream"
    echo "local replication $REPLUSER peer"
    echo "host replication $REPLUSER 0.0.0.0/0 scram-sha-256"
    echo "host replication $REPLUSER ::/0 scram-sha-256"
    printf '%s\n' "$HBALINES"
  } >> "$HBA"
}
chown -R postgres:postgres "$CONFDIR" 2>/dev/null || true`

// pgStreamCreateRoleScript creates (or re-passwords) the replication role.
const pgStreamCreateRoleScript = `set -e
if runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='$REPLUSER'" | grep -q 1; then
  printf '%s\n' "ALTER ROLE \"$REPLUSER\" WITH LOGIN REPLICATION PASSWORD :'pw';" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v pw="$REPLPW" >/dev/null
else
  printf '%s\n' "CREATE ROLE \"$REPLUSER\" WITH LOGIN REPLICATION PASSWORD :'pw';" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v pw="$REPLPW" >/dev/null
fi`

// pgStreamCreateSlotScript creates a physical slot unless it exists. Works on a primary and,
// for a switchover, on a standby about to be promoted.
const pgStreamCreateSlotScript = `set -e
printf '%s\n' "SELECT pg_create_physical_replication_slot(:'slot') WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = :'slot');" \
  | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -qAt -v slot="$SLOT" >/dev/null`

// pgStreamStandbyConfScript is shared by the base backup and the demotion: standby.signal, and
// primary_conninfo / primary_slot_name in postgresql.auto.conf (replacing whatever the copy
// carried, and dropping a synchronous_standby_names a primary had there).
const pgStreamStandbyConfScript = `
AUTO="$DATADIR/postgresql.auto.conf"
touch "$AUTO"
sed -i -E '/^(primary_conninfo|primary_slot_name|synchronous_standby_names)[[:space:]]*=/d' "$AUTO"
echo "primary_conninfo = 'host=$PRIMARY port=5432 user=$REPLUSER passfile=$PASSFILE application_name=$APPNAME'" >> "$AUTO"
[ -z "$SLOT" ] || echo "primary_slot_name = '$SLOT'" >> "$AUTO"
touch "$DATADIR/standby.signal"
chown postgres:postgres "$AUTO" "$DATADIR/standby.signal"
`

// pgStreamBaseBackupScript stops PostgreSQL, empties the data directory and copies the primary's
// into it with pg_basebackup (WAL streamed alongside, through the slot when there is one).
const pgStreamBaseBackupScript = `set -eo pipefail
systemctl stop "$SERVICE" 2>/dev/null || true
install -d -m 700 -o postgres -g postgres "$DATADIR"
find "$DATADIR" -mindepth 1 -delete
SLOTARG=""
[ -z "$SLOT" ] || SLOTARG="--slot=$SLOT"
runuser -u postgres -- env PGPASSFILE="$PASSFILE" "$BINDIR/pg_basebackup" -h "$PRIMARY" -p 5432 -U "$REPLUSER" \
  -D "$DATADIR" -X stream --checkpoint=fast $SLOTARG -w 2>&1 | tail -20
` + pgStreamStandbyConfScript

// pgStreamWaitStreamingScript waits for the WAL receiver to report streaming.
const pgStreamWaitStreamingScript = `for i in $(seq 1 60); do
  S=$(runuser -u postgres -- psql -tAc "SELECT status FROM pg_stat_wal_receiver" 2>/dev/null || true)
  [ "$S" = "streaming" ] && exit 0
  sleep 2
done
echo "the standby started but is not streaming (WAL receiver: ${S:-none}) — see the PostgreSQL log"
exit 1`

// pgStreamSyncScript sets (or, with NAMES empty, resets) synchronous_standby_names and
// synchronous_commit with ALTER SYSTEM, and reloads.
const pgStreamSyncScript = `set -e
if [ -n "$NAMES" ]; then
  printf '%s\n' "ALTER SYSTEM SET synchronous_standby_names = :'names';" "ALTER SYSTEM SET synchronous_commit = :'commit';" \
    | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -qAt -v names="$NAMES" -v commit="$COMMIT" >/dev/null
else
  runuser -u postgres -- psql -v ON_ERROR_STOP=1 -qAt -c "ALTER SYSTEM RESET synchronous_standby_names" -c "ALTER SYSTEM RESET synchronous_commit" >/dev/null
fi
runuser -u postgres -- psql -qAtc "SELECT pg_reload_conf()" >/dev/null`

// pgStreamShutdownLSNScript prints where a stopped server's last (shutdown) checkpoint record
// starts, and refuses unless the server really did shut down cleanly.
const pgStreamShutdownLSNScript = `set -e
OUT=$(LC_ALL=C runuser -u postgres -- "$BINDIR/pg_controldata" -D "$DATADIR")
printf '%s\n' "$OUT" | grep -q '^Database cluster state: *shut down$' || { printf '%s\n' "$OUT" | grep '^Database cluster state'; echo "not shut down cleanly"; exit 1; }
printf '%s\n' "$OUT" | awk -F': *' '/^Latest checkpoint location/ {print $2}'`

// pgStreamCatchUpScript waits, on a standby whose primary has stopped, until it has replayed the
// old primary's shutdown checkpoint ($TARGET) — the switchover's guarantee that no committed
// write is left behind. The replay position is the end of the last record replayed, so it is
// past the checkpoint's start once that record is in.
const pgStreamCatchUpScript = `for i in $(seq 1 60); do
  R=$(runuser -u postgres -- psql -tAc "SELECT pg_last_wal_replay_lsn() > '$TARGET'::pg_lsn" 2>/dev/null || true)
  if [ "$R" = "t" ]; then
    echo "replayed past $TARGET, the old primary's shutdown checkpoint"
    exit 0
  fi
  sleep 1
done
echo "replay did not reach $TARGET, where the old primary stopped, within 60s (at $(runuser -u postgres -- psql -tAc 'SELECT pg_last_wal_replay_lsn()' 2>/dev/null))"
exit 1`

// pgStreamDemoteScript turns a cleanly stopped primary into a standby of $PRIMARY. When it does
// not stream within a minute — its timeline diverged — it is stopped, rewound with pg_rewind
// from the new primary, and started again.
const pgStreamDemoteScript = `set -e
` + pgStreamStandbyConfScript + `
systemctl start "$SERVICE"
for i in $(seq 1 30); do
  S=$(runuser -u postgres -- psql -tAc "SELECT status FROM pg_stat_wal_receiver" 2>/dev/null || true)
  if [ "$S" = "streaming" ]; then
    runuser -u postgres -- psql -qAtc "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE NOT active AND slot_type = 'physical'" >/dev/null 2>&1 || true
    echo "streaming from $PRIMARY"
    exit 0
  fi
  sleep 2
done
echo "not streaming after 60s; rewinding with pg_rewind"
systemctl stop "$SERVICE"
runuser -u postgres -- env PGPASSWORD="$SUPERPW" "$BINDIR/pg_rewind" -D "$DATADIR" \
  --source-server="host=$PRIMARY port=5432 user=postgres dbname=postgres" 2>&1 | tail -5
` + pgStreamStandbyConfScript + `
systemctl start "$SERVICE"
for i in $(seq 1 30); do
  S=$(runuser -u postgres -- psql -tAc "SELECT status FROM pg_stat_wal_receiver" 2>/dev/null || true)
  [ "$S" = "streaming" ] && { echo "rewound; streaming from $PRIMARY"; exit 0; }
  sleep 2
done
echo "still not streaming after pg_rewind"
exit 1`

// pgStreamRepointScript points a running standby at a new primary (primary_conninfo and
// primary_slot_name are reloadable from PostgreSQL 13).
const pgStreamRepointScript = `set -e
printf '%s\n' "ALTER SYSTEM SET primary_conninfo = :'ci';" \
  | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -qAt -v ci="host=$PRIMARY port=5432 user=$REPLUSER passfile=$PASSFILE application_name=$APPNAME" >/dev/null
if [ -n "$SLOT" ]; then
  printf '%s\n' "ALTER SYSTEM SET primary_slot_name = :'slot';" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -qAt -v slot="$SLOT" >/dev/null
fi
runuser -u postgres -- psql -qAtc "SELECT pg_reload_conf()" >/dev/null
for i in $(seq 1 30); do
  H=$(runuser -u postgres -- psql -tAc "SELECT sender_host FROM pg_stat_wal_receiver WHERE status = 'streaming'" 2>/dev/null || true)
  [ "$H" = "$PRIMARY" ] && exit 0
  sleep 2
done
echo "not streaming from $PRIMARY after 60s"
exit 1`
