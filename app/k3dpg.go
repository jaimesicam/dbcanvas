package main

import (
	"context"
	"fmt"
	"strings"
)

// k3dpg.go — the Percona Operator for PostgreSQL (PGO) on a K3D cluster.
//
// Same rails as PXC and PSMDB (fetch the tag's source into /root, bundle.yaml, secrets, a rewritten
// cr.yaml), but PostgreSQL's operator is the odd one of the three:
//
//   - **It ships no users secret.** The operator *generates* one secret per user
//     (<cluster>-pguser-<name>) — but it reuses the password of a secret that already exists and
//     derives the SCRAM verifier from it (internal/controller/postgrescluster/postgres.go). So the
//     way to give a cluster the .env password is to create those secrets *before* the CR, which is
//     exactly the ordering the other two operators need anyway.
//   - **Its anti-affinity is already soft** — a `preferred` podAntiAffinity, not PXC/PSMDB's hard
//     topology key — so a 1-node cluster schedules as shipped and there is nothing to neutralise.
//   - **pgBackRest speaks S3 over TLS only.** There is no plaintext S3 in pgBackRest, so a SeaweedFS
//     node with TLS off cannot be a backup repo at all; the cluster then keeps the operator's own
//     PVC repo, and the deploy log says why.
//   - The front end is **pgBouncer**, and the primary Postgres Service is exposed separately.
//
// cr.yaml ships almost everything commented out (expose, users, the S3 repo, pgBackRest's global
// options), so this transform mostly *inserts* rather than rewrites.

// pgOptions drives pgTransform.
type pgOptions struct {
	Name            string // metadata.name — the PostgreSQL cluster's name
	ExposePostgres  string // ClusterIP | NodePort | LoadBalancer ("" = leave it alone)
	ExposePGBouncer string //
	PMMHost         string // "" = leave PMM disabled
	S3              *crS3  // nil = keep the shipped PVC repo
}

// pgShippedName is the cluster name Percona's cr.yaml and secrets.yaml ship with.
const pgShippedName = "cluster1"

// pgUsers are the users DBCanvas asks the operator to create: the superuser, plus an application
// user (and a like-named database) that the operator would otherwise name after the cluster anyway.
// Both get the POSTGRES_PASSWORD from .env, like every other PostgreSQL DBCanvas deploys.
func pgUsers(cluster string) []string { return []string{"postgres", cluster} }

// pgTransform rewrites the operator's cr.yaml for a small k3d cluster.
func pgTransform(src string, o pgOptions) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines)+40)

	pvc := newCRPVC()
	path := newYPath()
	commentTo := -1 // >=0: commenting out a resources block until a line dedents to this indent
	dropTo := -1    // >=0: dropping the shipped PVC repo, which the S3 repo replaces

	for _, ln := range lines {
		ind, commented, body := crLine(ln)
		pvc.update(ind, commented, body)

		if commentTo >= 0 && body != "" && ind <= commentTo {
			commentTo = -1
		}
		if dropTo >= 0 && body != "" && ind <= dropTo {
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

		path.update(ind, commented, body)
		p := path.String()

		if commented || body == "" {
			out = append(out, ln)
			continue
		}

		switch {
		case p == "metadata.name" && o.Name != "":
			out = append(out, "  name: "+o.Name)

		// spec: everything DBCanvas adds that cr.yaml ships commented out — the users (whose secrets
		// are pre-created with the .env password) and the primary Postgres Service.
		case p == "spec":
			out = append(out, ln)
			if o.Name != "" {
				out = append(out, crIndent(pgUsersBlock(o.Name), 2)...)
			}
			if o.ExposePostgres != "" {
				out = append(out, crIndent("expose:\n  type: "+o.ExposePostgres, 2)...)
			}

		// The connection pooler in front of the database.
		case p == "spec.proxy.pgBouncer" && o.ExposePGBouncer != "":
			out = append(out, ln)
			out = append(out, crIndent("expose:\n  type: "+o.ExposePGBouncer, 6)...)

		// Every CPU/memory request — but never a volume claim's size (crPVC knows PostgreSQL's
		// dataVolumeClaimSpec and volumeClaimSpec as well as the other two operators' PVCs).
		case body == "resources:" && !pvc.inside():
			commentTo = ind
			out = append(out, "#"+ln)

		// PMM. `secret:` names the secret the token is patched into, and it is spelled out in the
		// shipped file rather than derived from metadata.name.
		case p == "spec.pmm.enabled" && o.PMMHost != "":
			out = append(out, strings.Repeat(" ", ind)+"enabled: true")
		case p == "spec.pmm.serverHost" && o.PMMHost != "":
			out = append(out, strings.Repeat(" ", ind)+"serverHost: "+o.PMMHost)
		case p == "spec.pmm.secret" && o.Name != "":
			out = append(out, strings.Repeat(" ", ind)+"secret: "+o.Name+"-pmm-secret")

		// Backups. pgBackRest's S3 credentials live in a config file, not in the CR, so the repo
		// needs a `configuration:` pointing at the secret and a `global:` carrying the options that
		// have no CR field (path-style URIs, and skipping TLS verification of the stack's own CA).
		case p == "spec.backups.pgbackrest" && o.S3 != nil:
			out = append(out, ln)
			out = append(out, crIndent(pgBackRestGlobal(o.Name, o.S3), 6)...)

		// …and repo1 becomes that S3 repo instead of the shipped PVC.
		case p == "spec.backups.pgbackrest.repos.volume" && o.S3 != nil:
			out = append(out, crIndent(pgS3Repo(o.S3), ind)...)
			dropTo = ind // drop the volumeClaimSpec that follows

		default:
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// pgUsersBlock asks the operator for the superuser and an application user with a like-named
// database. Their secrets are pre-created (pgUserSecret), so these carry the .env password.
func pgUsersBlock(cluster string) string {
	return fmt.Sprintf(`users:
- name: postgres
- name: %s
  databases:
  - %s`, cluster, cluster)
}

// pgS3Repo points repo1 at the stack's SeaweedFS node. The endpoint keeps its port and drops its
// scheme — pgBackRest takes `host:port` and is always TLS.
func pgS3Repo(s *crS3) string {
	return fmt.Sprintf(`s3:
  bucket: %s
  endpoint: %s
  region: %s`, s.Bucket, pgEndpointHost(s.EndpointURL), s.Region)
}

// pgBackRestGlobal carries the repo options that have no field in the CR: the S3 credentials file,
// path-style URIs (SeaweedFS has no virtual-host bucket addressing), and no TLS verification — the
// backup pods trust only their image's CA bundle, and nothing hands them the Intranet CA that signed
// SeaweedFS's certificate. The traffic never leaves the stack network.
func pgBackRestGlobal(cluster string, s *crS3) string {
	return fmt.Sprintf(`configuration:
- secret:
    name: %s
global:
  repo1-path: /pgbackrest/%s/repo1
  repo1-s3-uri-style: path
  repo1-storage-verify-tls: "n"`, s.Secret, cluster)
}

// pgEndpointHost strips the scheme off a SeaweedFS endpoint: pgBackRest wants host[:port].
func pgEndpointHost(endpoint string) string {
	e := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	return strings.TrimSuffix(e, "/")
}

// pgSecretsTransform renames the secrets the operator ships (cluster1-pmm-secret,
// cluster1-extensions-secret) after the frame's cluster, and drops PMM 2's PMM_SERVER_KEY: the
// operator picks the PMM 2 sidecar whenever that key is set and a PMM 3 token is not.
func pgSecretsTransform(src, cluster string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ind, commented, body := crLine(ln)
		switch {
		case !commented && ind == 2 && strings.HasPrefix(body, "name: "+pgShippedName):
			out = append(out, "  "+strings.Replace(body, pgShippedName, cluster, 1))
		case !commented && ind == 2 && strings.HasPrefix(body, "PMM_SERVER_KEY:"):
			// dropped
		default:
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// pgUserSecret is a <cluster>-pguser-<user> secret holding the .env password. The operator reuses an
// existing secret's password and derives the SCRAM verifier from it, so creating these *before* the
// CR is what makes a PGO cluster's superuser password the POSTGRES_PASSWORD you already know —
// there is no deploy/secrets.yaml to rewrite, as there is for the other two operators.
func pgUserSecret(cluster, user, password string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s-pguser-%s
type: Opaque
stringData:
  user: %s
  password: %s
`, cluster, user, user, password)
}

// ---------------------------------------------------------------- the PG operator

func (a *App) installPGOperator(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID string, cfg *k3dConfig, pr *pxcProg) error {
	tarball, err := a.k3dFetchOperator(ctx, serverID, k3dOperatorRepos["pg"], cfg, pr)
	if err != nil {
		return err
	}
	if err := a.k3dApplyBundle(ctx, serverID, "percona-postgresql-operator", cfg, pr); err != nil {
		return err
	}
	ns := cfg.Namespace

	// ---- the secrets, BEFORE cr.yaml ----
	pr.phase("Applying secrets", 82)
	rawSecrets, err := tarFile(tarball, "deploy/secrets.yaml")
	if err != nil {
		return fmt.Errorf("read secrets.yaml from the operator source: %w", err)
	}
	newSecrets := pgSecretsTransform(string(rawSecrets), cfg.ClusterName)
	if err := a.docker.CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "secrets.yaml", 0o600, []byte(newSecrets)); err != nil {
		pr.logln("could not write secrets.yaml back to the source tree: " + err.Error())
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(newSecrets)); err != nil {
		return fmt.Errorf("apply secrets.yaml: %w", err)
	}
	// The user secrets the operator would otherwise fill with random passwords.
	pw := envOr("POSTGRES_PASSWORD", "postgres_password")
	for _, user := range pgUsers(cfg.ClusterName) {
		if err := a.kubectlApply(ctx, serverID, ns, []byte(pgUserSecret(cfg.ClusterName, user, pw))); err != nil {
			return fmt.Errorf("create the %s user secret: %w", user, err)
		}
	}
	pr.logln("user secrets created for " + strings.Join(pgUsers(cfg.ClusterName), ", ") + " (password from .env)")

	// ---- cr.yaml ----
	pr.phase("Applying cr.yaml", 88)
	raw, err := tarFile(tarball, "deploy/cr.yaml")
	if err != nil {
		return fmt.Errorf("read cr.yaml from the operator source: %w", err)
	}
	opts := pgOptions{
		Name:            cfg.ClusterName,
		ExposePostgres:  cfg.ExposePG,
		ExposePGBouncer: cfg.ExposePGBouncer,
	}

	// Backups. pgBackRest reads its S3 credentials from a config file rather than AWS_* env vars, so
	// this does not go through k3dBackupSecret — and it has no plaintext S3 at all, so a SeaweedFS
	// node with TLS off cannot be a repo: the cluster keeps the operator's PVC repo instead of
	// failing every backup.
	if frame.SeaweedFSNodeID != "" {
		sw, sec, serr := a.waitSeaweedRunning(ctx, st.ID, frame.SeaweedFSNodeID, deployTimeout())
		switch {
		case serr != nil:
			pr.logln("backups skipped: " + serr.Error())
		case !sw.TLS:
			pr.logln("backups → the PVC repo the operator ships: pgBackRest speaks S3 over TLS only, and " +
				sw.InternalEndpoint + " is plaintext — turn TLS on for the SeaweedFS node to back up to it")
			cfg.BackupRepo = "PVC (pgBackRest)"
		default:
			secret := cfg.ClusterName + "-pgbackrest-secrets"
			conf := fmt.Sprintf("[global]\nrepo1-s3-key=%s\nrepo1-s3-key-secret=%s\n",
				seaweedAccessKeyOf(sw, sec), sec.SecretKey)
			if _, err := a.kubectl(ctx, serverID, "-n", ns, "create", "secret", "generic", secret,
				"--from-literal=s3.conf="+conf); err != nil && !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("create the pgBackRest secret: %w", err)
			}
			opts.S3 = &crS3{Bucket: sw.Bucket, Region: sw.Region, EndpointURL: sw.InternalEndpoint, Secret: secret}
			pr.logln("backups → " + sw.InternalEndpoint + " (bucket " + sw.Bucket + ", pgBackRest repo1)")
		}
	}
	// PMM 3: the sidecar authenticates with a service token, from the cluster's own PMM secret.
	opts.PMMHost = a.k3dPMMToken(ctx, st, frame, doc, serverID, cfg.ClusterName+"-pmm-secret", "PMM_SERVER_TOKEN", cfg, pr)

	newCR := pgTransform(string(raw), opts)
	if err := a.docker.CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "cr.yaml", 0o644, []byte(newCR)); err != nil {
		pr.logln("could not write the rewritten cr.yaml back to the source tree: " + err.Error())
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(newCR)); err != nil {
		return err
	}
	pr.logln(fmt.Sprintf("cr.yaml applied (resources commented out, postgres %s / pgBouncer %s)",
		cfg.ExposePG, cfg.ExposePGBouncer))
	return nil
}
