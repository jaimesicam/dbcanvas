package main

import (
	"context"
	"fmt"
	"strings"
)

// k3dps.go — the Percona Operator for MySQL (Percona Server based, "ps-operator") on a K3D cluster.
//
// It is the closest of the four to PXC — same secrets file (root, monitor, replication, …), same
// pmmservertoken, an S3 backup storage with the same shape — but its cr.yaml is a level deeper and
// carries a choice PXC does not have:
//
//   - **clusterType**: `group-replication` (MySQL Group Replication, the shipped default) or `async`
//     (classic replication managed by **Orchestrator**, which then has to be enabled — the operator
//     will not run async replication without it).
//   - the front end lives under `proxy:` — **HAProxy** (works with both cluster types) or
//     **MySQL Router** (group replication only; it speaks the GR protocol).
//   - so rules key on paths (spec.proxy.haproxy.enabled), not the section+indent that PXC's flat file
//     allows — the same yPath the MongoDB and PostgreSQL transforms use.
//
// Everything else is the shared machinery: anti-affinity → none, the CPU/memory requests commented
// out, the SeaweedFS storage substituted for the shipped placeholder, and a PMM service token patched
// into the cluster secret before cr.yaml.

// psOptions drives psTransform.
type psOptions struct {
	Name        string // metadata.name — the MySQL cluster's name
	ClusterType string // "group-replication" (default) | "async"
	Proxy       string // "haproxy" (default) | "router"
	ExposeMySQL string // the primary's Service: ClusterIP | NodePort | LoadBalancer
	ExposeProxy string // the chosen front end's Service
	PMMHost     string // "" = leave PMM disabled
	S3          *crS3  // nil = leave the shipped placeholder storage alone
}

// psShippedName is the cluster name Percona's cr.yaml and secrets.yaml ship with.
const psShippedName = "ps-cluster1"

// psClusterType normalizes the topology. Async replication is Orchestrator-managed; group
// replication is the operator's own default.
func psClusterType(want string) string {
	if strings.ToLower(strings.TrimSpace(want)) == "async" {
		return "async"
	}
	return "group-replication"
}

// psProxy normalizes the front end. MySQL Router only understands group replication, so an async
// cluster always gets HAProxy.
func psProxy(want, clusterType string) string {
	if strings.ToLower(strings.TrimSpace(want)) == "router" && clusterType == "group-replication" {
		return "router"
	}
	return "haproxy"
}

// psTransform rewrites the operator's cr.yaml for a small k3d cluster.
func psTransform(src string, o psOptions) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines)+40)

	pvc := newCRPVC()
	path := newYPath()
	commentTo := -1 // >=0: commenting out a resources block until a line dedents to this indent
	dropTo := -1    // >=0: dropping the shipped placeholder storages

	clusterType := psClusterType(o.ClusterType)
	proxy := psProxy(o.Proxy, clusterType)

	for _, ln := range lines {
		ind, commented, body := crLine(ln)
		pvc.update(ind, commented, body)

		if commentTo >= 0 && !commented && body != "" && ind <= commentTo {
			commentTo = -1
		}
		if dropTo >= 0 && !commented && body != "" && ind <= dropTo {
			dropTo = -1
		}
		if dropTo >= 0 {
			continue
		}
		if commentTo >= 0 {
			if commented || body == "" {
				out = append(out, ln)
			} else {
				out = append(out, "#"+ln)
			}
			continue
		}

		key := path.update(ind, commented, body)
		p := path.String()

		if commented || body == "" {
			out = append(out, ln)
			continue
		}

		switch {
		case p == "metadata.name" && o.Name != "":
			out = append(out, "  name: "+o.Name)

		// secretsName / sslSecretName are spelled out in the shipped file, so they do not follow
		// metadata.name on their own — and a users secret the operator cannot find means it generates
		// its own random passwords instead of the ones from .env.
		case (p == "spec.secretsName" || p == "spec.sslSecretName") && o.Name != "":
			out = append(out, strings.Repeat(" ", ind)+strings.ReplaceAll(body, psShippedName, o.Name))

		// Group replication, or async replication under Orchestrator (which then must run).
		case p == "spec.mysql.clusterType":
			out = append(out, strings.Repeat(" ", ind)+"clusterType: "+clusterType)
		case p == "spec.orchestrator.enabled":
			out = append(out, fmt.Sprintf("%senabled: %t", strings.Repeat(" ", ind), clusterType == "async"))

		// The front end: exactly one of HAProxy / MySQL Router.
		case p == "spec.proxy.haproxy.enabled":
			out = append(out, fmt.Sprintf("%senabled: %t", strings.Repeat(" ", ind), proxy == "haproxy"))
		case p == "spec.proxy.router.enabled":
			out = append(out, fmt.Sprintf("%senabled: %t", strings.Repeat(" ", ind), proxy == "router"))

		// A 1–3 node cluster cannot place one pod per node.
		case key == "antiAffinityTopologyKey":
			out = append(out, strings.Repeat(" ", ind)+`antiAffinityTopologyKey: "none"`)

		// Every CPU/memory request — but never the PVC's storage size.
		case body == "resources:" && !pvc.inside():
			commentTo = ind
			out = append(out, "#"+ln)

		// Expose. exposePrimary ships enabled with its type commented out; the proxies' whole expose
		// block is commented, so the chosen Service type is inserted into the section that is on.
		case p == "spec.mysql.exposePrimary" && o.ExposeMySQL != "":
			out = append(out, ln)
			out = append(out, crIndent("type: "+o.ExposeMySQL, ind+2)...)
		case p == "spec.proxy."+proxy && o.ExposeProxy != "":
			out = append(out, ln)
			out = append(out, crIndent("expose:\n  type: "+o.ExposeProxy, ind+2)...)

		// PMM.
		case p == "spec.pmm.enabled" && o.PMMHost != "":
			out = append(out, strings.Repeat(" ", ind)+"enabled: true")
		case p == "spec.pmm.serverHost" && o.PMMHost != "":
			out = append(out, strings.Repeat(" ", ind)+"serverHost: "+o.PMMHost)

		// Backups: the shipped placeholder storage (s3-us-west, "S3-BACKUP-BUCKET-NAME-HERE") is
		// replaced by the stack's SeaweedFS node.
		case p == "spec.backup.storages" && o.S3 != nil:
			out = append(out, ln)
			out = append(out, crIndent(crSeaweedStorage(o.S3), ind+2)...)
			dropTo = ind

		default:
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------- the PS operator

func (a *App) installPSOperator(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID string, cfg *k3dConfig, pr *pxcProg) error {
	tarball, err := a.k3dFetchOperator(ctx, serverID, k3dOperatorRepos["ps"], cfg, pr)
	if err != nil {
		return err
	}
	if err := a.k3dApplyBundle(ctx, serverID, "percona-server-mysql-operator", cfg, pr); err != nil {
		return err
	}
	ns := cfg.Namespace

	// ---- secrets.yaml, BEFORE cr.yaml ----
	// The same users as PXC (root, monitor, replication, operator …) plus a few of its own
	// (orchestrator, heartbeat, clusterset), which have no .env counterpart and keep what they ship.
	pr.phase("Applying secrets.yaml", 82)
	rawSecrets, err := tarFile(tarball, "deploy/secrets.yaml")
	if err != nil {
		return fmt.Errorf("read secrets.yaml from the operator source: %w", err)
	}
	newSecrets := secretsTransform(string(rawSecrets), cfg.ClusterName, k3dSecretsPasswords())
	if err := a.docker.CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "secrets.yaml", 0o600, []byte(newSecrets)); err != nil {
		pr.logln("could not write secrets.yaml back to the source tree: " + err.Error())
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(newSecrets)); err != nil {
		return fmt.Errorf("apply secrets.yaml: %w", err)
	}
	pr.logln("secrets.yaml applied as " + cfg.ClusterName + "-secrets (passwords from .env)")

	// ---- cr.yaml ----
	pr.phase("Applying cr.yaml", 88)
	raw, err := tarFile(tarball, "deploy/cr.yaml")
	if err != nil {
		return fmt.Errorf("read cr.yaml from the operator source: %w", err)
	}
	opts := psOptions{
		Name:        cfg.ClusterName,
		ClusterType: cfg.ClusterType,
		Proxy:       cfg.Proxy,
		ExposeMySQL: cfg.ExposeMySQL,
		ExposeProxy: cfg.ExposeProxy,
	}
	if opts.S3 = a.k3dBackupSecret(ctx, st, frame, serverID, cfg, pr); opts.S3 != nil {
		// The ps-operator's S3 schema has no forcePathStyle at all (not even in 1.2.0), and an
		// unknown field makes the API server reject the whole custom resource. xbcloud addresses
		// path-style against a custom endpoint anyway — the same as PXC before 1.20.0.
		opts.S3.ForcePathStyle = strings.Contains(string(raw), "forcePathStyle")
	}
	opts.PMMHost = a.k3dPMMToken(ctx, st, frame, doc, serverID, cfg.ClusterName+"-secrets", "pmmservertoken", cfg, pr)

	newCR := psTransform(string(raw), opts)
	if err := a.docker.CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "cr.yaml", 0o644, []byte(newCR)); err != nil {
		pr.logln("could not write the rewritten cr.yaml back to the source tree: " + err.Error())
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(newCR)); err != nil {
		return err
	}
	pr.logln(fmt.Sprintf("cr.yaml applied (%s, %s front end, mysql %s / proxy %s)",
		psClusterType(cfg.ClusterType), psProxy(cfg.Proxy, psClusterType(cfg.ClusterType)),
		cfg.ExposeMySQL, cfg.ExposeProxy))
	return nil
}
