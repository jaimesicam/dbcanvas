package main

import (
	"context"
	"fmt"
	"strings"
)

// aio_orch.go — Percona Orchestrator instances inside an All-in-One node.
//
// Orchestrator is the only feature that exists BOTH as a standalone canvas node
// (orchestrator.go) and as an All-in-One instance, which is why a MySQL
// instance's "Monitored by" picker resolves an `orchestratorRef` that may be
// either a node id or `inst:<instanceId>`.
//
// Four things the classic path can assume and this one cannot, each verified
// against the live-install notes in orchestrator.go:
//
//   - The RHEL package auto-loads /etc/orchestrator.conf.json with no flag
//     (orchestrator.go:44). With N instances that single path is useless, so the
//     unit passes an explicit `-config`.
//   - SQLite3DataFile must move into the instance's own datadir, or two
//     instances would share one backend database.
//   - ListenAddress moves off the :3000 default onto the instance's slot.
//   - `orchestrator-client` defaults to http://localhost:3000. Each instance's
//     env file therefore exports ORCHESTRATOR_API, or every discover call from a
//     shell would silently hit the wrong instance.
//
// Repo caveat, also from orchestrator.go: percona-orchestrator lives ONLY in the
// PDPS repo family, never PDPXC. In a PXC-flavored node the PDPS repo must be
// enabled for this install while the PXC install stays scoped away from it. That
// combination is unreachable today (the PXC family is not implemented), so this
// file simply enables PDPS; the interaction needs live verification when PXC lands.

// aioProvisionOrch installs percona-orchestrator once, then brings up every
// Orchestrator instance the node declares.
func (a *App) aioProvisionOrch(ctx context.Context, st Stack, n designNode, doc designDoc, id string, cfg aioConfig, fresh map[string]bool, sec pxcSecrets, pr *pxcProg, base, span int) error {
	pr.phase("Installing Orchestrator", base)
	instScript := orchestratorInstallRHEL
	if isDebianOS(n.OS) {
		instScript = orchestratorInstallDebian
	}
	env := []string{"REPO=" + orchestratorRepo(), "VER=" + n.AIOOrchVersion}
	if n.UseProxy {
		env = append(env, "PROXY=http://intranet."+envOr("DOMAIN", "example.net")+":3128")
	}
	if err := a.runStep(ctx, id, instScript, env, pr.logln); err != nil {
		return fmt.Errorf("install Orchestrator: %w", err)
	}
	pr.logln("percona-orchestrator installed")
	if err := a.runStep(ctx, id, aioMaskVendorUnits, []string{"UNITS=" + orchestratorUnit}, pr.logln); err != nil {
		return fmt.Errorf("mask vendor orchestrator unit: %w", err)
	}
	pr.logln("vendor orchestrator unit masked — instances own their ports")

	declBy := map[string]aioInstance{}
	for _, in := range n.AIOInstances {
		if in.Kind == "orchestrator" {
			declBy[aioSanitizeInst(in.Name)] = in
		}
	}
	for i, m := range aioMembersOfFamily(cfg, famOrch) {
		in, ok := declBy[m.Inst]
		if !ok {
			continue
		}
		if !fresh[m.Inst] {
			continue
		}
		pr.phase(fmt.Sprintf("Preparing Orchestrator %s", m.Inst), base+span/2+i)
		if err := a.aioOrchPrepare(ctx, id, in, m, sec, pr); err != nil {
			return err
		}
	}
	return nil
}

// aioOrchPrepare writes one instance's config, unit and env file, then starts it.
func (a *App) aioOrchPrepare(ctx context.Context, id string, in aioInstance, m aioInstanceRuntime, sec pxcSecrets, pr *pxcProg) error {
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
		return fmt.Errorf("%s: create directories: %w", m.Inst, err)
	}
	conf := aioOrchConfJSON(l, m, sec, strings.TrimSpace(in.AlertEmail))
	if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, "orchestrator.conf.json", 0o640, []byte(conf)); err != nil {
		return fmt.Errorf("%s: write orchestrator.conf.json: %w", m.Inst, err)
	}
	spec := aioUnitSpec{
		Description: fmt.Sprintf("dbcanvas All-in-One Orchestrator instance %s (port %d)", m.Inst, m.Ports.Client),
		// -config is mandatory here: without it the binary picks up
		// /etc/orchestrator.conf.json and every instance would share one config.
		ExecStart: fmt.Sprintf("/usr/local/orchestrator/orchestrator -config %s http", l.ConfPath),
		// Orchestrator loads its web templates from ./resources/templates, so it must
		// run from its install prefix. Without this every /web/ page returns 500
		// "templates/layout is undefined" while the API answers normally — which is a
		// confusing way to find out. The classic Orchestrator node's unit does the same.
		WorkingDirectory: "/usr/local/orchestrator",
		Type:             "simple",
		User:             "root",
		Group:            "root",
		TimeoutSec:       120,
		EnvFile: fmt.Sprintf("AIO_INST=%s\nAIO_KIND=%s\nAIO_PORT=%d\nAIO_CNF=%s\nORCHESTRATOR_API=http://127.0.0.1:%d/api\n",
			m.Inst, m.Kind, m.Ports.Client, l.ConfPath, m.Ports.Client),
	}
	if err := a.aioWriteUnit(ctx, id, l, spec); err != nil {
		return err
	}
	if err := a.runStep(ctx, id, aioStartUnitScript, []string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
		return fmt.Errorf("%s: start: %w", m.Inst, err)
	}
	pr.logln(fmt.Sprintf("%s running — web UI on port %d", m.Inst, m.Ports.Client))
	return nil
}

// aioOrchConfJSON renders one instance's orchestrator.conf.json: its own listen
// port, its own SQLite backend file, and the shared cluster-wide topology user
// every dbcanvas MySQL node already creates.
//
// DefaultInstancePort is deliberately NOT the classic 3306: inside an All-in-One
// node every MySQL instance is on its own slot port, and discovery addresses
// carry an explicit port anyway.
func aioOrchConfJSON(l instLayout, m aioInstanceRuntime, sec pxcSecrets, alertEmail string) string {
	hooks := "[]"
	if alertEmail != "" {
		hooks = `["bash /usr/local/bin/dbcanvas-orch-alert.sh '{failureType}' '{failureCluster}' '{failedHost}' '{failedPort}'"]`
	}
	return fmt.Sprintf(`{
  "ListenAddress": ":%d",
  "BackendDB": "sqlite",
  "SQLite3DataFile": %q,
  "MySQLTopologyUser": %q,
  "MySQLTopologyPassword": %q,
  "MySQLTopologySSLSkipVerify": true,
  "DiscoverByShowSlaveHosts": true,
  "DetectClusterAliasQuery": %q,
  "InstancePollSeconds": 5,
  "AuthenticationMethod": "",
  "OnFailureDetectionProcesses": %s
}
`, m.Ports.Client, l.DataDir+"/orchestrator.sqlite3",
		sec.OrchestratorUser, sec.OrchestratorPassword, aioOrchClusterAliasQuery, hooks)
}

// aioOrchClusterAliasQuery gives each cluster the name the user typed instead of
// the master's host:port.
//
// Orchestrator names a cluster after its master's key unless an alias is detected,
// so two All-in-One replication clusters show up as "aio-01:13000" and
// "aio-01:13030" — accurate, and unrecognisable as the clusters you drew. Every
// instance's report_host is "<instance>.<domain>" and a cluster member is
// "<cluster>-n<N>", so stripping the domain and the member suffix recovers the
// declaring instance's name.
//
// The suffix is removed with a regex anchored at the end rather than
// SUBSTRING_INDEX(x, '-n', 1), which would truncate any cluster whose own name
// contains "-n" ("my-node-cluster" → "my"). REGEXP_REPLACE is available on both
// MariaDB 10.0+ and MySQL 8.0+. A standalone instance has no -nN suffix and is left
// alone; a server with no report_host yields NULL, which Orchestrator reads as
// "no alias" and falls back to its default naming.
const aioOrchClusterAliasQuery = `SELECT REGEXP_REPLACE(SUBSTRING_INDEX(@@report_host, '.', 1), '-n[0-9]+$', '')`

// aioOrchDiscover points an Orchestrator instance at the MySQL instances that
// selected it, so a freshly deployed node shows its topology without the user
// having to discover each host by hand. Best-effort: a discovery failure leaves
// the instance running and is reported in the log, matching how the classic
// node treats it.
func (a *App) aioOrchDiscover(ctx context.Context, id string, n designNode, cfg aioConfig, pr *pxcProg) {
	// instance id → the orchestrator instance name it named.
	watchers := map[string][]string{}
	for _, in := range n.AIOInstances {
		ref, isLocal := strings.CutPrefix(in.OrchestratorRef, "inst:")
		if !isLocal || aioFamilyOf(in.Kind) != famMySQL {
			continue
		}
		if target := aioInstanceByID(n.AIOInstances, ref); target != nil {
			watchers[aioSanitizeInst(target.Name)] = append(watchers[aioSanitizeInst(target.Name)], aioSanitizeInst(in.Name))
		}
	}
	if len(watchers) == 0 {
		return
	}
	for orchName, groups := range watchers {
		var orch aioInstanceRuntime
		for _, m := range cfg.Instances {
			if m.Inst == orchName {
				orch = m
			}
		}
		if orch.Inst == "" {
			continue
		}
		var targets []string
		for _, g := range groups {
			for _, m := range cfg.Instances {
				if m.Family == famMySQL && (m.Group == g || m.Inst == g) {
					targets = append(targets, fmt.Sprintf("127.0.0.1:%d", m.Ports.Client))
				}
			}
		}
		if len(targets) == 0 {
			continue
		}
		if err := a.runStep(ctx, id, aioOrchDiscoverScript, []string{
			fmt.Sprintf("API=http://127.0.0.1:%d/api", orch.Ports.Client),
			"TARGETS=" + strings.Join(targets, " "),
		}, pr.logln); err != nil {
			pr.logln(fmt.Sprintf("%s: topology discovery incomplete: %v", orch.Inst, err))
			continue
		}
		pr.logln(fmt.Sprintf("%s discovered %d MySQL instance(s)", orch.Inst, len(targets)))
	}
}

// aioOrchDiscoverScript asks one Orchestrator instance to discover each target.
// ORCHESTRATOR_API is exported explicitly because orchestrator-client otherwise
// defaults to http://localhost:3000 — the wrong instance, or nothing at all.
const aioOrchDiscoverScript = `set -e
export ORCHESTRATOR_API="$API"
for i in $(seq 1 30); do
  curl -sf "$API/status" >/dev/null 2>&1 && break
  sleep 2
done
rc=0
for t in $TARGETS; do
  H=${t%:*}; P=${t##*:}
  orchestrator-client -c discover -i "$H:$P" >/dev/null 2>&1 || rc=1
done
exit $rc`
