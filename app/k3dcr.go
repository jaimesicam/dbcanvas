package main

import (
	"fmt"
	"strings"
)

// k3dcr.go — the cr.yaml rewrite applied before the custom resource is created.
//
// Percona's cr.yaml is written for a real, multi-node cluster. On a 1–3 node k3d cluster it will
// never schedule as shipped, so three things are changed before it is applied — and the file in
// /root on the first node is rewritten to match, so what you read is what ran:
//
//   1. antiAffinityTopologyKey → "none". Shipped as "kubernetes.io/hostname", which spreads the 3
//      database pods across 3 *nodes*; a single-node cluster then leaves 2 pods Pending forever.
//   2. Every section's own `resources:` block is commented out. The requests (600m CPU / 1G per
//      pod, plus HAProxy and the rest) exceed a small k3d budget, and a pod that cannot be
//      admitted never starts. Commenting them out lets the pods take what they need.
//   3. `expose` gets the frame's Service type (ClusterIP / NodePort / LoadBalancer), in every
//      section that has one — cr.yaml ships them all commented out.
//
// Rule 2 has one exception, and it matters: a `resources:` inside a **persistentVolumeClaim** is the
// storage size, which is *required* — comment it out and the operator rejects the CR. So the
// transform tracks PVC blocks and leaves their resources alone (crPVC). Indentation alone is not
// enough to tell the two apart: a PXC section's resources sit at 4 spaces, a PSMDB replset's at 4
// but its arbiter/mongos/config-server's at 6, while PVC resources turn up at 8 and 12.
//
// The transform is line-based on purpose: DBCanvas has no YAML dependency (even versions.yaml is
// hand-parsed), and a round-trip through a YAML library would reflow the file and throw away the
// comments that make cr.yaml worth reading.

// crStorageName is the single backup storage a K3D cluster gets. cr.yaml's shipped `schedules:`
// (and pitr) reference a storage *by name*, so replacing the storages block means every active
// storageName must be repointed at this one — the operator refuses the whole CR otherwise
// ("storage fs-pvc doesn't exist"), and never creates the cluster.
const crStorageName = "seaweedfs"

// crS3 points the operator's backups at a SeaweedFS node's S3 endpoint.
type crS3 struct {
	Bucket      string
	Region      string
	EndpointURL string // http(s)://<fqdn>:8333
	Secret      string // the k8s secret holding AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
	// ForcePathStyle emits `forcePathStyle: true` (SeaweedFS does not do virtual-host bucket
	// addressing). The field only exists from PXC operator 1.20.0 — an older CRD rejects the entire
	// custom resource with a strict-decoding error ("unknown field ...forcePathStyle") and never
	// creates the cluster, so it is set from what the selected version's own cr.yaml knows about
	// (installPXCOperator). Backups work either way: xbcloud already uses path-style addressing
	// against a custom endpoint — verified on 1.19.1.
	ForcePathStyle bool
}

// crOptions drives crTransform.
type crOptions struct {
	Name string // metadata.name — the PXC cluster name
	// Proxy is the one that stays enabled: "haproxy" (cr.yaml's default) or "proxysql". They are
	// mutually exclusive — the operator runs one front end, not both.
	Proxy string
	// Expose is the Service type per section. They are independent: a cluster can keep its database
	// pods in-cluster (ClusterIP) while the proxy takes a LoadBalancer address.
	ExposePXC      string // ClusterIP | NodePort | LoadBalancer ("" = leave the section alone)
	ExposeHAProxy  string
	ExposeProxySQL string
	PMMHost        string // "" = leave PMM disabled
	S3             *crS3  // nil = leave the shipped (placeholder) storages alone
}

// crExposeFor is the Service type for a section, or "" when that section should be left as shipped.
func (o crOptions) crExposeFor(section string) string {
	switch section {
	case "pxc":
		return o.ExposePXC
	case "haproxy":
		return o.ExposeHAProxy
	case "proxysql":
		return o.ExposeProxySQL
	}
	return ""
}

// crProxyEnabled reports whether a section is the chosen front end. cr.yaml ships haproxy enabled
// and proxysql disabled; picking one flips both.
func (o crOptions) crProxyEnabled(section string) (enabled, isProxy bool) {
	if section != "haproxy" && section != "proxysql" {
		return false, false
	}
	proxy := o.Proxy
	if proxy == "" {
		proxy = "haproxy" // cr.yaml's own default
	}
	return section == proxy, true
}

// crExposeBlocks are the expose keys each section owns. cr.yaml ships them all commented out, so
// the chosen Service type is inserted rather than uncommented (the commented examples carry
// cloud-specific keys we do not want).
var crExposeBlocks = map[string][]string{
	// pxc: per-pod services for the database itself.
	"pxc": {"expose:\n  enabled: true\n  type: %s"},
	// haproxy: the cluster's front door — primary + replica services.
	"haproxy": {
		"exposePrimary:\n  enabled: true\n  type: %s",
		"exposeReplicas:\n  enabled: true\n  type: %s",
	},
	// proxysql: the alternative front end.
	"proxysql": {"expose:\n  enabled: true\n  type: %s"},
}

// crTransform rewrites cr.yaml. It is intentionally forgiving: a key it does not recognise is
// passed through untouched, so a new operator release cannot break the deploy — at worst a knob is
// not applied, which the manager reports.
func crTransform(src string, o crOptions) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines)+40)

	inMetadata := false // the document's top-level metadata block
	section := ""       // the current 4-space section (pxc, haproxy, proxysql, pmm, backup, …)
	commentTo := -1     // >=0: commenting out a block until a line dedents to this indent
	dropTo := -1        // >=0: dropping lines (the shipped backup storages) until this indent
	inStorages := false
	pvc := newCRPVC()

	for _, ln := range lines {
		ind, commented, body := crLine(ln)
		pvc.update(ind, commented, body)

		// Close an open comment/drop range once the block dedents.
		if commentTo >= 0 && body != "" && ind <= commentTo {
			commentTo = -1
		}
		if dropTo >= 0 && body != "" && ind <= dropTo {
			dropTo = -1
		}
		if dropTo >= 0 {
			continue // inside the replaced storages block
		}
		if commentTo >= 0 {
			if commented || body == "" {
				out = append(out, ln) // already a comment (or blank) — leave it
			} else {
				out = append(out, "#"+ln)
			}
			continue
		}

		// metadata.name — what the operator names every resource it creates after.
		if !commented && ind == 0 && strings.HasPrefix(body, "metadata:") {
			inMetadata = true
			out = append(out, ln)
			continue
		}
		if !commented && ind == 0 && body != "" {
			inMetadata = false
		}
		if inMetadata && !commented && ind == 2 && strings.HasPrefix(body, "name:") && o.Name != "" {
			out = append(out, "  name: "+o.Name)
			continue
		}

		// Track the current spec section: an active 2-space key under `spec:`.
		if !commented && ind == 2 && strings.HasSuffix(body, ":") {
			section = strings.TrimSuffix(body, ":")
			out = append(out, ln)
			// cr.yaml ships every expose block commented out, so the chosen Service type is
			// *inserted* at the top of the section rather than uncommented (the commented
			// examples carry cloud-specific keys we do not want).
			if blocks, ok := crExposeBlocks[section]; ok {
				if expose := o.crExposeFor(section); expose != "" {
					for _, b := range blocks {
						out = append(out, crIndent(fmt.Sprintf(b, expose), 4)...)
					}
				}
			}
			continue
		}

		if !commented {
			switch {
			// 0. The front end: exactly one of haproxy/proxysql stays enabled.
			case ind == 4 && strings.HasPrefix(body, "enabled:") && func() bool { _, isProxy := o.crProxyEnabled(section); return isProxy }():
				enabled, _ := o.crProxyEnabled(section)
				out = append(out, fmt.Sprintf("%senabled: %t", strings.Repeat(" ", ind), enabled))
				continue

			// 1. Anti-affinity: a single node cannot satisfy "one pod per node".
			case strings.HasPrefix(body, "antiAffinityTopologyKey:"):
				out = append(out, strings.Repeat(" ", ind)+`antiAffinityTopologyKey: "none"`)
				continue

			// 2. A section's own resources — never the PVC's (its storage size is required).
			case body == "resources:" && !pvc.inside():
				commentTo = ind
				out = append(out, "#"+ln)
				continue

			// 3. PMM.
			case section == "pmm" && ind == 4 && o.PMMHost != "" && body == "enabled: false":
				out = append(out, strings.Repeat(" ", ind)+"enabled: true")
				continue
			case section == "pmm" && ind == 4 && o.PMMHost != "" && strings.HasPrefix(body, "serverHost:"):
				out = append(out, strings.Repeat(" ", ind)+"serverHost: "+o.PMMHost)
				continue

			// 4a. Backups: the shipped schedule (and pitr) name a storage that the replacement
			//     below removes. Repoint them, or the operator rejects the CR outright.
			case section == "backup" && o.S3 != nil && strings.HasPrefix(body, "storageName:"):
				out = append(out, strings.Repeat(" ", ind)+"storageName: "+crStorageName)
				continue

			// 4b. Backups: replace the shipped placeholder storages with the SeaweedFS one.
			case section == "backup" && ind == 4 && body == "storages:" && o.S3 != nil:
				out = append(out, ln)
				out = append(out, crIndent(crSeaweedStorage(o.S3), 6)...)
				dropTo = ind // drop the shipped storage entries that follow
				inStorages = true
				continue
			}
		}
		out = append(out, ln)
	}
	_ = inStorages
	return strings.Join(out, "\n")
}

// crSeaweedStorage is the one backup storage a K3D cluster gets: the stack's SeaweedFS node.
//
// verifyTLS is false. A SeaweedFS node serves plain HTTP by default, and even with TLS on it uses
// an Intranet-CA certificate that the backup pods do not trust — their image ships its own CA
// bundle, and nothing hands them the stack CA. Verifying would fail the backup; the traffic never
// leaves the stack network. (Giving the pods the CA is a separate, larger piece of work.)
func crSeaweedStorage(s *crS3) string {
	block := fmt.Sprintf(`%s:
  type: s3
  verifyTLS: false
  s3:
    bucket: %s
    credentialsSecret: %s
    region: %s
    endpointUrl: %s`, crStorageName, s.Bucket, s.Secret, s.Region, s.EndpointURL)
	if s.ForcePathStyle {
		block += "\n    forcePathStyle: true"
	}
	return block
}

// crPVC tracks whether the current line is inside a `persistentVolumeClaim:` block — the one place a
// `resources:` must survive the rewrite, because it carries the volume's size and the operator
// requires it. Every other resources block is a CPU/memory request that will not fit a k3d budget.
type crPVC struct{ indent int }

func newCRPVC() *crPVC { return &crPVC{indent: -1} }

// update must be called for every line, in order, before the resources rule is applied to it.
func (p *crPVC) update(ind int, commented bool, body string) {
	if commented || body == "" {
		return
	}
	if p.indent >= 0 && ind <= p.indent {
		p.indent = -1 // the block dedented: we are out of it
	}
	if body == "persistentVolumeClaim:" {
		p.indent = ind
	}
}

func (p *crPVC) inside() bool { return p.indent >= 0 }

// crLine splits a cr.yaml line into its logical indent, whether it is commented, and its body.
// Comments in cr.yaml start at column 0 ("#      limits:"), so a commented line's logical indent is
// the indentation *after* the '#'.
func crLine(ln string) (indent int, commented bool, body string) {
	s := ln
	if strings.HasPrefix(s, "#") {
		commented = true
		s = s[1:]
	}
	trimmed := strings.TrimLeft(s, " ")
	return len(s) - len(trimmed), commented, strings.TrimRight(trimmed, " ")
}

// crIndent indents a multi-line block by n spaces.
func crIndent(block string, n int) []string {
	pad := strings.Repeat(" ", n)
	var out []string
	for _, ln := range strings.Split(block, "\n") {
		if ln == "" {
			out = append(out, "")
			continue
		}
		out = append(out, pad+ln)
	}
	return out
}

// ---------------------------------------------------------------- secrets.yaml

// k3dSecretsPasswords maps the users in the operator's deploy/secrets.yaml to DBCanvas's own
// credentials. Passwords in DBCanvas come from .env and are the single source of truth for every
// deployed database, so a PXC cluster running under the operator uses the same ones — its root
// password is the MYSQL_ROOT_PASSWORD you already know.
//
// `xtrabackup` and any key not listed keep the value the operator ships: they are internal users
// with no .env counterpart, and inventing one would be a password nobody could look up.
func k3dSecretsPasswords() map[string]string {
	sec := mysqlFamilySecrets()
	return map[string]string{
		"root":        sec.RootPassword,
		"monitor":     sec.MonitorPassword,
		"replication": sec.ReplPassword,
		"operator":    sec.AdminPassword,
		"proxyadmin":  envOr("PROXYSQL_ADMIN_PASSWORD", "admin_password"),
	}
}

// secretsTransform rewrites the operator's deploy/secrets.yaml before it is applied. Two things
// matter: the secret must be named `<cluster>-secrets` (what cr.yaml's secretsName defaults to —
// a mismatch and the operator quietly generates its own random passwords instead), and the
// passwords come from .env like every other database DBCanvas deploys.
//
// It must be applied BEFORE cr.yaml: the operator reads the secret while creating the cluster, and
// a secret that shows up afterwards does not change the users it already made.
func secretsTransform(src, clusterName string, pw map[string]string) string {
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
			if v, ok := pw[key]; ok && v != "" {
				out = append(out, "  "+key+": "+v)
				continue
			}
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
