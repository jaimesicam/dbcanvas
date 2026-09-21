package main

import (
	"context"
	"fmt"
	"strconv"
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
	// The three cluster features the operator grew in 3.1.0. Every one of them is a spec section
	// that simply does not exist on an older CRD, so installPGOperator leaves all of them unset
	// below that release — see pgHasClusterFeatures.
	LogicalReplicas  int      // spec.logicalReplicas: how many read-only logical replicas; 0 = none
	LogicalStorageGB int      // each replica's dataVolumeClaimSpec size
	LogicalBootstrap string   // pgbackrest | pg_basebackup
	LogicalDatabases []string // which databases each replica subscribes to; empty = the CRD's "all"
	LogCollector     *bool    // spec.logcollector.enabled; nil = keep whatever cr.yaml ships
	TDE              *pgTDE   // spec.extensions.pg_tde; nil = no encryption at rest
}

// pgTDE is spec.extensions.pg_tde — pg_tde keyed to the stack's OpenBao node.
//
// There is no local-keyring mode to fall back on: the CRD carries a CEL rule ("vault is required
// for enabling pg_tde") that refuses the whole custom resource if pg_tde.enabled is set without a
// vault, so a cluster either gets a key store or it gets no encryption at all.
type pgTDE struct {
	WALEncryption bool   // also encrypt the write-ahead log
	VaultHost     string // OpenBao's API address — https://<fqdn>:8200
	MountPath     string // the KV v2 mount minted for this cluster (pg_tde's vault_mount_path)
	Secret        string // the Secret holding `token` (and `ca.crt` when OpenBao serves TLS)
	CAKey         string // "ca.crt", or "" when the OpenBao node runs plaintext
}

// pgFeatures310 is the operator release that introduced spec.logicalReplicas, spec.logcollector
// and spec.extensions.pg_tde. Nothing about them degrades gracefully on an older one: the CRD has
// no such field, so the API server rejects the *entire* cr.yaml with a strict-decoding error and
// the cluster is never created — which is why this gate is checked at validation (k3dPGFeatureIssues)
// as well as here.
const pgFeatures310 = "3.1.0"

// pgHasClusterFeatures reports whether an operator version understands the pgFeatures310 sections.
// An empty version is "unknown", not "newest": it means the catalog could not resolve what will
// actually be installed, and guessing wrong here costs the whole cr.yaml.
func pgHasClusterFeatures(operatorVer string) bool {
	v := strings.TrimSpace(operatorVer)
	return v != "" && compareVersions(v, pgFeatures310) >= 0
}

// ------------------------------------------------------------------ 3.1.0 frame knobs

// pgLogicalReplicas is how many logical replicas spec.logicalReplicas asks for. Capped at 3: each
// one is a StatefulSet pod with its own PVC on top of the instances, the repo host and pgBouncer.
func pgLogicalReplicas(f designFrame) int {
	if f.K3DPGLogicalReplicas <= 0 {
		return 0
	}
	return clampInt(f.K3DPGLogicalReplicas, 1, 3)
}

// pgLogicalStorageGB is each logical replica's dataVolumeClaimSpec size. The CRD requires the
// claim, so there is no "leave it to the operator" — 1 GiB is the same default the instances get.
func pgLogicalStorageGB(f designFrame) int {
	if f.K3DPGLogicalStorageGB > 0 {
		return clampInt(f.K3DPGLogicalStorageGB, 1, 512)
	}
	return 1
}

// pgLogicalBootstrap is how a logical replica is seeded before it starts streaming: from the
// pgBackRest repository (the CRD's own default) or with pg_basebackup straight off the primary.
func pgLogicalBootstrap(f designFrame) string {
	if strings.TrimSpace(f.K3DPGLogicalBootstrap) == "pg_basebackup" {
		return "pg_basebackup"
	}
	return "pgbackrest"
}

// pgLogicalDatabases is which databases the replicas subscribe to, parsed from the frame's
// comma-separated list.
//
// Empty is the interesting value and the default: an empty `databases` array is the CRD's own
// "every non-template database except postgres", resolved by the operator at bootstrap against
// whatever the cluster actually holds. That is the only sensible default for a lab, because the
// frame is designed before the application has created anything — and naming a database that
// does not exist yet does not degrade, it leaves the replica permanently unready.
//
// Duplicates are dropped rather than passed through: `databases` is a set as far as the operator
// is concerned, and a repeated name would only show up as a confusing diff against the applied CR.
func pgLogicalDatabases(f designFrame) []string {
	var out []string
	seen := map[string]bool{}
	for _, db := range strings.Split(f.K3DPGLogicalDatabases, ",") {
		db = strings.TrimSpace(db)
		if db == "" || seen[db] {
			continue
		}
		seen[db] = true
		out = append(out, db)
	}
	return out
}

// pgLogCollector is whether the fluent-bit sidecar + logrotate run — "persistent logging", which
// keeps PostgreSQL's server log as rotated files on the data volume instead of only in the pod's
// stdout, where a restart loses it.
//
// The frame spells this as a NEGATIVE (K3DPGNoLogCollector) on purpose: 3.1.0's cr.yaml ships
// logcollector enabled, so the zero value has to mean "on" or every design saved before this
// option would quietly turn logging off on its next deploy.
func pgLogCollector(f designFrame) bool { return !f.K3DPGNoLogCollector }

// pgTDEMount is the cluster's own KV v2 mount on the OpenBao node, named the way dbvault.go names
// the standalone engines' (mysql-<host> / mongodb-<host>). One mount per cluster, never shared:
// two clusters writing principal keys into one mount is how you lose both.
func pgTDEMount(cluster string) string { return "postgresql-" + cluster }

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
			// logicalReplicas and extensions ship entirely commented out, so like the users block
			// above they are inserted here rather than rewritten in place.
			if o.LogicalReplicas > 0 {
				out = append(out, crIndent(pgLogicalReplicasBlock(o.LogicalReplicas, o.LogicalStorageGB, o.LogicalBootstrap, o.LogicalDatabases), 2)...)
			}
			if o.TDE != nil {
				out = append(out, crIndent(pgTDEBlock(o.TDE), 2)...)
			}

		// The connection pooler in front of the database.
		case p == "spec.proxy.pgBouncer" && o.ExposePGBouncer != "":
			out = append(out, ln)
			out = append(out, crIndent("expose:\n  type: "+o.ExposePGBouncer, 6)...)

		// Persistent logging. This is the one 3.1.0 knob that rewrites a line instead of inserting
		// a block: cr.yaml ships spec.logcollector ACTIVE and enabled, so the only reason to touch
		// it is to turn the fluent-bit sidecar off.
		case p == "spec.logcollector.enabled" && o.LogCollector != nil:
			out = append(out, strings.Repeat(" ", ind)+"enabled: "+strconv.FormatBool(*o.LogCollector))

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

// pgLogicalReplicasBlock renders spec.logicalReplicas: n read-only replicas fed by logical
// replication from the primary, each with its own PVC and its own Service.
//
// `databases` is emitted for every replica, empty unless the frame named some. An EMPTY ARRAY is
// not the same as leaving the key out: `[]` is the CRD's own "every non-template database except
// postgres", resolved at bootstrap, and it is what the operator's own cr.yaml documents. It is
// also written in flow style deliberately — a block sequence with no items is not YAML.
func pgLogicalReplicasBlock(n, storageGB int, bootstrap string, databases []string) string {
	var b strings.Builder
	b.WriteString("logicalReplicas:\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "- name: replica%d\n", i)
		if len(databases) == 0 {
			b.WriteString("  databases: []\n")
		} else {
			b.WriteString("  databases:\n")
			for _, db := range databases {
				fmt.Fprintf(&b, "  - %s\n", db)
			}
		}
		b.WriteString("  bootstrapMethod: " + bootstrap + "\n")
		b.WriteString("  dataVolumeClaimSpec:\n")
		b.WriteString("    accessModes:\n")
		b.WriteString("    - ReadWriteOnce\n")
		fmt.Fprintf(&b, "    resources:\n      requests:\n        storage: %dGi\n", storageGB)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// pgTDEBlock renders spec.extensions.pg_tde against the stack's OpenBao node.
//
// mountPath is the KV *mount*, not a path inside it: the operator hands it to
// pg_tde_add_global_key_provider_vault_v2 as vault_mount_path and the extension builds
// /v1/<mount>/data/... itself. caSecret is omitted when OpenBao serves plain HTTP — there is
// nothing to verify, and pointing at a key that the Secret does not carry fails the pod.
func pgTDEBlock(t *pgTDE) string {
	var b strings.Builder
	b.WriteString("extensions:\n  pg_tde:\n    enabled: true\n")
	if t.WALEncryption {
		b.WriteString("    walEncryption: true\n")
	}
	b.WriteString("    vault:\n")
	fmt.Fprintf(&b, "      host: %s\n", t.VaultHost)
	fmt.Fprintf(&b, "      mountPath: %s\n", t.MountPath)
	fmt.Fprintf(&b, "      tokenSecret:\n        name: %s\n        key: token\n", t.Secret)
	if t.CAKey != "" {
		fmt.Fprintf(&b, "      caSecret:\n        name: %s\n        key: %s\n", t.Secret, t.CAKey)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// pgTDEVaultSecret is the Secret pg_tde reads its OpenBao credentials from: the scoped token, and
// the Intranet CA when OpenBao serves TLS. Written as a manifest rather than through
// `kubectl create secret --from-literal` because the CA is a multi-line PEM.
func pgTDEVaultSecret(name, token, caPEM string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\ntype: Opaque\nstringData:\n  token: %s\n", name, token)
	if caPEM != "" {
		b.WriteString("  ca.crt: |\n")
		for _, ln := range strings.Split(strings.TrimRight(caPEM, "\n"), "\n") {
			b.WriteString("    " + ln + "\n")
		}
	}
	return b.String()
}

// pgBackRestRepo is the pgBackRest repository the operator's cr.yaml ships and DBCanvas keeps —
// the one a PerconaPGBackup names in `repoName`, and the one whose objects land under
// /pgbackrest/<cluster>/repo1 in the bucket (pgBackRestGlobal's repo1-path).
const pgBackRestRepo = "repo1"

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

// pgProvisionTDE gives the cluster a key store on the stack's OpenBao node and returns the
// spec.extensions.pg_tde configuration that points at it.
//
// The shape is dbvault.go's, because it is the same problem the standalone Percona Server and
// PSMDB nodes already solved: the cluster gets its OWN KV v2 mount and a token scoped to it (never
// the root token — the operator's own e2e test uses root, DBCanvas does not), and it verifies
// OpenBao with the one CA in the stack. What is different is only how the credentials are
// delivered: a Secret in the operator's namespace instead of a file on a node.
//
// Reachability comes free: k3s' CoreDNS forwards the stack's domain to the Intranet DNS
// (corednsCustomConfigMap), so a pod resolves the OpenBao node by the same FQDN every other node
// uses, over the stack network.
func (a *App) pgProvisionTDE(ctx context.Context, st Stack, frame designFrame, serverID string, cfg *k3dConfig, pr *pxcProg) (*pgTDE, error) {
	pr.phase("Provisioning pg_tde key store", 85)
	baoCfg, rootToken, baoCID, err := a.waitOpenBaoReady(ctx, st.ID, frame.OpenBaoNodeID, deployTimeout())
	if err != nil {
		return nil, err
	}
	mount := pgTDEMount(cfg.ClusterName)
	token, err := a.provisionVaultMount(ctx, baoCID, baoCfg, rootToken, mount, "kv-v2", "Percona Operator for PostgreSQL", pr.logln)
	if err != nil {
		return nil, err
	}

	// The CA, only when there is TLS to verify. An OpenBao node with SSL off serves plain HTTP,
	// pg_tde has nothing to check, and a caSecret key that the Secret does not carry stops the
	// instance pods from starting at all.
	caPEM, caKey := "", ""
	if baoCfg.TLS {
		if caPEM, err = a.k3dIntranetCA(ctx, st, baoCfg.Addr); err != nil {
			return nil, err
		}
		caKey = k3dVaultCAKey
	} else {
		pr.logln("pg_tde → " + baoCfg.Addr + " over plain HTTP: the OpenBao node has SSL off, so the principal key crosses the stack network unencrypted")
	}

	secret := cfg.ClusterName + "-pgtde-vault"
	if err := a.kubectlApply(ctx, serverID, cfg.Namespace, []byte(pgTDEVaultSecret(secret, token, caPEM))); err != nil {
		return nil, fmt.Errorf("create the pg_tde vault secret: %w", err)
	}
	return &pgTDE{
		WALEncryption: frame.K3DPGTDEWal,
		VaultHost:     baoCfg.Addr,
		MountPath:     mount,
		Secret:        secret,
		CAKey:         caKey,
	}, nil
}

func (a *App) installPGOperator(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID string, cfg *k3dConfig, pr *pxcProg) error {
	tarball, err := a.k3dFetchOperator(ctx, serverID, k3dOperatorRepos["pg"], cfg, pr)
	if err != nil {
		return err
	}
	if err := a.k3dApplyBundle(ctx, serverID, "percona-postgresql-operator", cfg, pr); err != nil {
		return err
	}
	// The debugger goes on BEFORE the custom resource, so a breakpoint set while the deploy is
	// still running catches the cluster's very first reconcile. Never fatal — see
	// k3dInstallDebugger.
	if k3dDebugOn(frame) {
		a.k3dInstallDebugger(ctx, st, frame, "percona-postgresql-operator", tarball, serverID, cfg, pr)
	}
	ns := cfg.Namespace

	// ---- the secrets, BEFORE cr.yaml ----
	pr.phase("Applying secrets", 82)
	rawSecrets, err := tarFile(tarball, "deploy/secrets.yaml")
	if err != nil {
		return fmt.Errorf("read secrets.yaml from the operator source: %w", err)
	}
	newSecrets := pgSecretsTransform(string(rawSecrets), cfg.ClusterName)
	if err := a.engCtx(ctx).CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "secrets.yaml", 0o600, []byte(newSecrets)); err != nil {
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
		sw, sec, serr := a.waitSeaweedBucket(ctx, st.ID, frame.SeaweedFSNodeID, frame.SeaweedFSBucket, deployTimeout())
		switch {
		case serr != nil:
			pr.logln("backups skipped: " + serr.Error())
		case !sw.TLS:
			pr.logln("backups → the PVC repo the operator ships: pgBackRest speaks S3 over TLS only, and " +
				sw.InternalEndpoint + " is plaintext — turn TLS on for the SeaweedFS node to back up to it")
			cfg.BackupRepo = "PVC (pgBackRest)"
			cfg.BackupStorage = pgBackRestRepo
		default:
			secret := cfg.ClusterName + "-pgbackrest-secrets"
			conf := fmt.Sprintf("[global]\nrepo1-s3-key=%s\nrepo1-s3-key-secret=%s\n",
				seaweedAccessKeyOf(sw, sec), sec.SecretKey)
			if _, err := a.kubectl(ctx, serverID, "-n", ns, "create", "secret", "generic", secret,
				"--from-literal=s3.conf="+conf); err != nil && !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("create the pgBackRest secret: %w", err)
			}
			opts.S3 = &crS3{Bucket: sw.Bucket, Region: sw.Region, EndpointURL: sw.InternalEndpoint, Secret: secret}
			cfg.BackupRepo = "SeaweedFS S3 (" + sw.Bucket + ")"
			cfg.BackupBucket, cfg.BackupEndpoint, cfg.BackupRegion = sw.Bucket, sw.InternalEndpoint, sw.Region
			cfg.BackupStorage = pgBackRestRepo
			// A SECOND credentials secret, in the shape every other operator's already has:
			// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY. pgBackRest wants its keys as an ini file
			// under `s3.conf` and reads nothing else, so the secret above cannot be handed to a
			// pod as `envFrom` — which is exactly what the bucket toolbox does (k3dbucket.go).
			// Writing both at deploy time is what lets the Backups tab treat all four operators
			// identically instead of learning to parse an ini file out of a Secret.
			awsSecret := cfg.ClusterName + "-backup-s3"
			if _, err := a.kubectl(ctx, serverID, "-n", ns, "create", "secret", "generic", awsSecret,
				"--from-literal=AWS_ACCESS_KEY_ID="+seaweedAccessKeyOf(sw, sec),
				"--from-literal=AWS_SECRET_ACCESS_KEY="+sec.SecretKey); err != nil &&
				!strings.Contains(err.Error(), "already exists") {
				pr.logln("bucket toolbox credentials skipped: " + err.Error())
			} else {
				cfg.BackupSecret = awsSecret
			}
			pr.logln("backups → " + sw.InternalEndpoint + " (bucket " + sw.Bucket + ", pgBackRest repo1)")
		}
	}
	// PMM 3: the sidecar authenticates with a service token, from the cluster's own PMM secret.
	opts.PMMHost = a.k3dPMMToken(ctx, st, frame, doc, serverID, cfg.ClusterName+"-pmm-secret", "PMM_SERVER_TOKEN", cfg, pr)

	// ---- the 3.1.0 cluster features ----
	//
	// Guarded by the operator version rather than by the checkbox alone: below 3.1.0 these spec
	// sections do not exist, and writing one would have the API server reject the whole cr.yaml
	// instead of ignoring the field. Validation says so before a deploy ever starts
	// (k3dPGFeatureIssues); this is the belt to that braces, for a design that reached here anyway.
	if pgHasClusterFeatures(cfg.OperatorVer) {
		if n := pgLogicalReplicas(frame); n > 0 {
			opts.LogicalReplicas = n
			opts.LogicalStorageGB = pgLogicalStorageGB(frame)
			opts.LogicalBootstrap = pgLogicalBootstrap(frame)
			opts.LogicalDatabases = pgLogicalDatabases(frame)
			cfg.PGLogicalReplicas = n
			dbs := "every non-template database except postgres"
			if len(opts.LogicalDatabases) > 0 {
				dbs = strings.Join(opts.LogicalDatabases, ", ")
			}
			pr.logln(fmt.Sprintf("logical replicas: %d × %d GiB, seeded by %s, subscribing to %s",
				n, opts.LogicalStorageGB, opts.LogicalBootstrap, dbs))
		}
		on := pgLogCollector(frame)
		opts.LogCollector = &on
		cfg.PGLogCollector = on
		if !on {
			pr.logln("persistent logging off: no fluent-bit sidecar, so the server log lives only in the pod's stdout")
		}
		if frame.K3DPGTDE {
			tde, terr := a.pgProvisionTDE(ctx, st, frame, serverID, cfg, pr)
			if terr != nil {
				// Fatal, unlike backups or PMM. Those degrade to a cluster with one feature
				// missing; this one degrades to a cluster that says it is encrypted and is not,
				// and pg_tde cannot be turned on afterwards without re-creating the data.
				return fmt.Errorf("transparent data encryption: %w", terr)
			}
			opts.TDE = tde
			cfg.PGTDE = "OpenBao " + tde.VaultHost + " (KV v2 mount " + tde.MountPath + ")"
			if tde.WALEncryption {
				cfg.PGTDE += " + WAL"
			}
			pr.logln("pg_tde keyed to " + tde.VaultHost + ", mount " + tde.MountPath)
		}
	}

	newCR := pgTransform(string(raw), opts)
	if err := a.engCtx(ctx).CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "cr.yaml", 0o644, []byte(newCR)); err != nil {
		pr.logln("could not write the rewritten cr.yaml back to the source tree: " + err.Error())
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(newCR)); err != nil {
		return err
	}
	pr.logln(fmt.Sprintf("cr.yaml applied (resources commented out, postgres %s / pgBouncer %s)",
		cfg.ExposePG, cfg.ExposePGBouncer))
	return nil
}
