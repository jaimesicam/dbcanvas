package main

import (
	"context"
	"fmt"
	"strings"
)

// k3dpsmdb.go — the Percona Operator for MongoDB (PSMDB) on a K3D cluster.
//
// Same strategy as the PXC operator (k3d.go, k3dcr.go): fetch the tag's source into /root on the
// first node, apply deploy/bundle.yaml, then secrets.yaml (renamed, with .env passwords), then a
// rewritten deploy/cr.yaml. What differs is the custom resource itself, so PSMDB gets its own
// transform rather than bending crTransform out of shape:
//
//   - cr.yaml is *nested*: replsets is a list, and its members (arbiter, hidden, nonvoting) and the
//     sharding block (configsvrReplSet, mongos) each carry their own affinity/resources/expose. So
//     rules are keyed on the **path** of a line (spec.sharding.mongos.expose.type) rather than on a
//     section plus an indent, which is all PXC's flat cr.yaml needs.
//   - the front end is not a proxy but a *router*: sharding on gives 3 mongos + 3 config servers on
//     top of the replica set (9 pods), so DBCanvas defaults it off and lets the frame turn it on.
//   - the shipped backup storages are entirely commented out, so the SeaweedFS storage is inserted
//     rather than substituted, and nothing references a storage by name (no repointing to do).
//   - the users secret carries MONGODB_*_USER/PASSWORD pairs, and PMM 3's token lives in the same
//     secret under PMM_SERVER_TOKEN (PMM 2's PMM_SERVER_API_KEY is dropped — the operator picks the
//     PMM 2 code path whenever it is present and no token is).

// psmdbOptions drives psmdbTransform.
type psmdbOptions struct {
	Name          string // metadata.name — the MongoDB cluster's name
	Sharding      bool   // true: config servers + mongos routers; false: a plain replica set
	ExposeReplset string // ClusterIP | NodePort | LoadBalancer ("" = leave the section alone)
	ExposeMongos  string //
	PMMHost       string // "" = leave PMM disabled
	S3            *crS3  // nil = no backup storage
}

// psmdbTransform rewrites the operator's cr.yaml for a small k3d cluster. Like crTransform it is
// line-based and forgiving: a key it does not recognise is passed through untouched.
func psmdbTransform(src string, o psmdbOptions) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines)+30)

	pvc := newCRPVC()
	path := newYPath()
	commentTo := -1 // >=0: commenting out a resources block until a line dedents to this indent

	for _, ln := range lines {
		ind, commented, body := crLine(ln)
		pvc.update(ind, commented, body)

		if commentTo >= 0 && !commented && body != "" && ind <= commentTo {
			commentTo = -1
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
		// The cluster's name, and everything cr.yaml names after it. `secrets.users` and
		// `secrets.encryptionKey` are spelled out in the shipped file ("my-cluster-name-secrets"),
		// so they do not follow metadata.name on their own — and a users secret the operator cannot
		// find means it quietly generates its own random passwords instead.
		case p == "metadata.name" && o.Name != "":
			out = append(out, "  name: "+o.Name)

		case strings.HasPrefix(p, "spec.secrets.") && o.Name != "" && strings.Contains(body, psmdbShippedName):
			out = append(out, strings.Repeat(" ", ind)+strings.ReplaceAll(body, psmdbShippedName, o.Name))

		// A 1–3 node cluster cannot place one pod per node.
		case key == "antiAffinityTopologyKey":
			out = append(out, strings.Repeat(" ", ind)+`antiAffinityTopologyKey: "none"`)

		// Every CPU/memory request — but never the PVC's storage size.
		case body == "resources:" && !pvc.inside():
			commentTo = ind
			out = append(out, "#"+ln)

		// Sharding: config servers + mongos, or a plain replica set.
		case p == "spec.sharding.enabled":
			out = append(out, fmt.Sprintf("%senabled: %t", strings.Repeat(" ", ind), o.Sharding))

		// Expose. The replset block ships disabled; mongos has a type and no enabled key.
		case p == "spec.replsets.expose.enabled" && o.ExposeReplset != "":
			out = append(out, strings.Repeat(" ", ind)+"enabled: true")
		case p == "spec.replsets.expose.type" && o.ExposeReplset != "":
			out = append(out, strings.Repeat(" ", ind)+"type: "+o.ExposeReplset)
		case p == "spec.sharding.mongos.expose.type" && o.ExposeMongos != "":
			out = append(out, strings.Repeat(" ", ind)+"type: "+o.ExposeMongos)

		// PMM.
		case p == "spec.pmm.enabled" && o.PMMHost != "":
			out = append(out, strings.Repeat(" ", ind)+"enabled: true")
		case p == "spec.pmm.serverHost" && o.PMMHost != "":
			out = append(out, strings.Repeat(" ", ind)+"serverHost: "+o.PMMHost)

		// Backups: cr.yaml ships every storage commented out, so ours is inserted at the top of the
		// backup section (nothing references a storage by name, so there is nothing to repoint).
		case p == "spec.backup" && o.S3 != nil:
			out = append(out, ln)
			out = append(out, crIndent(psmdbSeaweedStorage(o.S3), 4)...)

		default:
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// psmdbShippedName is the cluster name Percona's cr.yaml and secrets.yaml ship with; every resource
// they name after it has to follow the frame's name instead.
const psmdbShippedName = "my-cluster-name"

// psmdbSeaweedStorage is the one backup storage a PSMDB cluster gets: the stack's SeaweedFS node.
//
// insecureSkipTLSVerify mirrors the PXC storage's verifyTLS: false — the backup pods trust only
// their image's CA bundle and nothing hands them the Intranet CA, and the traffic never leaves the
// stack network. forcePathStyle is required outright: SeaweedFS has no virtual-host bucket
// addressing, and PBM (unlike xbcloud) does not assume path style for a custom endpoint.
func psmdbSeaweedStorage(s *crS3) string {
	block := fmt.Sprintf(`storages:
  %s:
    main: true
    type: s3
    s3:
      bucket: %s
      credentialsSecret: %s
      region: %s
      endpointUrl: %s
      insecureSkipTLSVerify: true`, crStorageName, s.Bucket, s.Secret, s.Region, s.EndpointURL)
	if s.ForcePathStyle {
		block += "\n      forcePathStyle: true"
	}
	return block
}

// psmdbSecretsPasswords maps the users in the operator's deploy/secrets.yaml to DBCanvas's own
// credentials: MONGODB_ADMIN_PASSWORD from .env, like every other MongoDB DBCanvas deploys. The
// usernames the operator ships (userAdmin, clusterAdmin, …) are kept — they are the operator's, and
// renaming them would only make the cluster harder to reason about.
func psmdbSecretsPasswords() map[string]string {
	pw := envOr("MONGODB_ADMIN_PASSWORD", "admin_password")
	return map[string]string{
		"MONGODB_BACKUP_PASSWORD":          pw,
		"MONGODB_DATABASE_ADMIN_PASSWORD":  pw,
		"MONGODB_CLUSTER_ADMIN_PASSWORD":   pw,
		"MONGODB_CLUSTER_MONITOR_PASSWORD": pw,
		"MONGODB_USER_ADMIN_PASSWORD":      pw,
	}
}

// psmdbSecretsTransform rewrites deploy/secrets.yaml before it is applied: the secret is renamed to
// what the CR's `secrets.users` points at, the passwords come from .env, and the shipped
// PMM_SERVER_API_KEY is dropped.
//
// That last one is not cosmetic. The operator picks its PMM **2** code path whenever the secret
// carries PMM_SERVER_API_KEY and no PMM_SERVER_TOKEN — so leaving the shipped placeholder in place
// would point PMM 2 sidecars, authenticating with the literal string "apikey", at a PMM 3 server.
func psmdbSecretsTransform(src, clusterName string, pw map[string]string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	inMetadata, inStringData := false, false
	for _, ln := range lines {
		ind, commented, body := crLine(ln)
		if !commented && ind == 0 && body != "" {
			inMetadata = strings.HasPrefix(body, "metadata:")
			inStringData = strings.HasPrefix(body, "stringData:")
			out = append(out, ln)
			continue
		}
		if inMetadata && !commented && ind == 2 && strings.HasPrefix(body, "name:") {
			out = append(out, "  name: "+clusterName+"-secrets")
			continue
		}
		if inStringData && !commented && ind == 2 && strings.Contains(body, ":") {
			key := strings.TrimSpace(strings.SplitN(body, ":", 2)[0])
			if key == "PMM_SERVER_API_KEY" {
				continue // PMM 2's credential: its presence alone selects the PMM 2 sidecar
			}
			if v, ok := pw[key]; ok && v != "" {
				out = append(out, "  "+key+": "+v)
				continue
			}
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------- yPath

// yPath tracks the path of the line being read ("spec.sharding.mongos.expose.type"), which is what
// PSMDB's nested cr.yaml needs to be rewritten safely — `expose.type` alone appears under the
// replica set, the config servers and the routers, and they do not all mean the same thing.
//
// List items are transparent: `replsets:` is a list whose one entry is `- name: rs0`, and its keys
// read better as spec.replsets.* than as spec.replsets[0].*.
type yPath struct {
	keys    []string
	indents []int
}

func newYPath() *yPath { return &yPath{} }

// update pops the stack back to the line's indent and pushes the line's own key. It returns the
// line's key ("" for a comment, a blank line, a list item or a scalar).
func (y *yPath) update(ind int, commented bool, body string) string {
	if commented || body == "" {
		return ""
	}
	pop := func(deeperThan int) {
		for len(y.indents) > 0 && y.indents[len(y.indents)-1] >= deeperThan {
			y.indents = y.indents[:len(y.indents)-1]
			y.keys = y.keys[:len(y.keys)-1]
		}
	}
	// A sequence element ("- name: rs0") is transparent: it sits at its parent key's own indent,
	// so popping it the usual way would throw the parent (replsets) away and leave every key of the
	// replica set hanging off "spec.name".
	if strings.HasPrefix(body, "-") {
		pop(ind + 1)
		return ""
	}
	pop(ind)
	key, _, isKey := strings.Cut(body, ":")
	if !isKey || key == "" {
		return "" // a scalar inside a block literal, not a key
	}
	y.keys = append(y.keys, key)
	y.indents = append(y.indents, ind)
	return key
}

// String is the path of the line last passed to update.
func (y *yPath) String() string { return strings.Join(y.keys, ".") }

// ---------------------------------------------------------------- the PSMDB operator

// installPSMDBOperator installs the Percona Operator for MongoDB and creates a cluster on it. The
// order is the same as PXC's, and for the same reasons: bundle (CRDs + operator) → secrets (the
// operator reads them while creating the cluster) → cr.yaml.
func (a *App) installPSMDBOperator(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID string, cfg *k3dConfig, pr *pxcProg) error {
	tarball, err := a.k3dFetchOperator(ctx, serverID, k3dOperatorRepos["psmdb"], cfg, pr)
	if err != nil {
		return err
	}
	if err := a.k3dApplyBundle(ctx, serverID, "percona-server-mongodb-operator", cfg, pr); err != nil {
		return err
	}
	ns := cfg.Namespace

	// ---- secrets.yaml, BEFORE cr.yaml ----
	pr.phase("Applying secrets.yaml", 82)
	rawSecrets, err := tarFile(tarball, "deploy/secrets.yaml")
	if err != nil {
		return fmt.Errorf("read secrets.yaml from the operator source: %w", err)
	}
	newSecrets := psmdbSecretsTransform(string(rawSecrets), cfg.ClusterName, psmdbSecretsPasswords())
	if err := a.engCtx(ctx).CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "secrets.yaml", 0o600, []byte(newSecrets)); err != nil {
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
	opts := psmdbOptions{
		Name:          cfg.ClusterName,
		Sharding:      cfg.Sharding,
		ExposeReplset: cfg.ExposeReplset,
		ExposeMongos:  cfg.ExposeMongos,
	}
	if !cfg.Sharding {
		opts.ExposeMongos = "" // no routers to expose
	}
	if opts.S3 = a.k3dBackupSecret(ctx, st, frame, serverID, cfg, pr); opts.S3 != nil {
		// forcePathStyle is what makes SeaweedFS work, but an operator whose CRD predates the field
		// rejects the entire custom resource over it (strict decoding) — so the selected version's
		// own CRD is the authority. crd.yaml is part of the source we already downloaded.
		if crd, cerr := tarFile(tarball, "deploy/crd.yaml"); cerr == nil {
			opts.S3.ForcePathStyle = strings.Contains(string(crd), "forcePathStyle")
		}
		if !opts.S3.ForcePathStyle {
			pr.logln("operator " + cfg.OperatorVer + " has no forcePathStyle option — SeaweedFS needs path-style addressing, so backups may fail; use a newer operator")
		}
	}
	// PMM 3's pmm-client sidecars authenticate with a service token, which PSMDB reads from the
	// users secret under PMM_SERVER_TOKEN (that key is also what selects the PMM 3 sidecar).
	opts.PMMHost = a.k3dPMMToken(ctx, st, frame, doc, serverID, cfg.ClusterName+"-secrets", "PMM_SERVER_TOKEN", cfg, pr)

	newCR := psmdbTransform(string(raw), opts)
	if err := a.engCtx(ctx).CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "cr.yaml", 0o644, []byte(newCR)); err != nil {
		pr.logln("could not write the rewritten cr.yaml back to the source tree: " + err.Error())
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(newCR)); err != nil {
		return err
	}
	topology := "replica set (rs0)"
	if cfg.Sharding {
		topology = "sharded (rs0 + config servers + mongos)"
	}
	pr.logln("cr.yaml applied (affinity none, resources commented out, " + topology + ")")
	return nil
}
