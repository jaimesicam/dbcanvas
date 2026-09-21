package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// k3dvault.go — data-at-rest encryption for the PXC and PSMDB operators, keyed to the stack's
// OpenBao node.
//
// This is dbvault.go's problem again — the standalone Percona Server and PSMDB nodes already
// solve it — and k3dpg.go's answer to it: the cluster gets its OWN KV v2 mount and a token scoped
// to that mount, never the root token, and it verifies OpenBao with the one CA in the stack. What
// differs per operator is only how the credentials are delivered and which file the database reads
// them from.
//
//	PXC    a Secret with a `keyring_vault.conf` key, named by spec.vaultSecretName. The operator
//	       mounts it at /etc/mysql/vault-keyring-secret, and its entrypoint turns encryption on
//	       purely because that file exists. But WHAT IT DOES WITH THE FILE DEPENDS ON THE SERVER
//	       VERSION, and the two formats are not compatible:
//
//	         vault_secret="/etc/mysql/vault-keyring-secret/keyring_vault.conf"
//	         if [ -f "$vault_secret" ]; then
//	           if [[ $MYSQL_VERSION =~ ^(5\.7|8\.0)$ ]]; then          # the PLUGIN
//	             sed -i "/\[mysqld\]/a early-plugin-load=keyring_vault.so" $CFG
//	             sed -i "/\[mysqld\]/a keyring_vault_config=$vault_secret" $CFG
//	           fi
//	         ...
//	           if [[ $MYSQL_VERSION == '8.4' ]]; then                  # the COMPONENT
//	             echo -n '{ "components": "file://component_keyring_vault" }' >/var/lib/mysql/mysqld.my
//	             cp ${vault_secret} /var/lib/mysql/component_keyring_vault.cnf
//
//	       The 8.4 branch copies the Secret's file VERBATIM to the component's config, and the
//	       component parses it as JSON — so one Secret key carries two different formats. The
//	       plugin's `key = value` file on 8.4 fails as
//
//	         [ERROR] Component component_keyring_vault reported: 'Keyring configuration JSON
//	                 parse error: Invalid value. (0)'
//	         [ERROR] [InnoDB] Check keyring fail, please check the keyring is loaded.
//
//	       and the pod crash-loops before the cluster ever forms. This is the same split
//	       dbvault.go handles for a standalone Percona Server node (the plugin does not exist from
//	       8.4, the component does not exist before it), so the two renderers are reused whole and
//	       chosen the same way — by mysqlModernMajor. The version comes from the `spec.pxc.image`
//	       tag in the operator release's own cr.yaml, which is the only place that states which
//	       server this cluster will actually run.
//
//	PSMDB  two halves that must agree. `spec.secrets.vault` names a Secret mounted at
//	       /etc/mongodb-vault, and each replica set's `configuration` carries a mongod.conf
//	       `security.vault` block pointing at the files in it. The operator keys off BOTH: it
//	       mounts the Secret when encryption is enabled, and it omits its own
//	       `--enableEncryption --encryptionKeyFile` arguments only when the configuration has a
//	       `security.vault` section (MongoConfiguration.VaultEnabled). Write one without the
//	       other and you get a cluster that is either unencrypted or told to use two key sources.
//
// Both were verified against the operators' own source rather than their documentation — the PXC
// entrypoint above, and PSMDB's e2e-tests/data-at-rest-encryption, which is this exact feature's
// acceptance test.
//
// ------------------------------------------------------------------- what is deliberately absent
//
// PXC's cr.yaml documents a `percona.com/issue-vault-token` annotation, and it looks like the
// thing to set here. It is not: it makes the operator STOP reconciling ("wait for token issuing")
// until a human removes it, for people who create the vault secret after the cluster. DBCanvas
// creates the Secret before cr.yaml is applied, so the annotation would only hang the deploy.
//
// PSMDB's `spec.secrets.encryptionKey` is left as cr.yaml ships it. From operator 1.23.0 a CR with
// `secrets.vault` set does not generate that local key at all; below it the operator still creates
// a 32-byte Secret that nothing mounts. Harmless either way, and removing the line would make this
// transform disagree with every other cluster's cr.yaml for no gain.

const (
	// pxcVaultMountPath is where the PXC operator mounts spec.vaultSecretName. The path is the
	// operator's, not ours (pkg/pxc/app/statefulset.VaultSecretMountPath), and `vault_ca` inside
	// the config has to point back into it.
	pxcVaultMountPath = "/etc/mysql/vault-keyring-secret"
	// pxcVaultConfKey is the Secret key the entrypoint looks for. Renaming it turns encryption
	// off silently — the file simply is not there.
	pxcVaultConfKey = "keyring_vault.conf"
	// psmdbVaultMountPath is where the PSMDB operator mounts spec.secrets.vault
	// (pkg/psmdb/config.VaultDir).
	psmdbVaultMountPath = "/etc/mongodb-vault"
	// psmdbVaultTokenKey is the Secret key mongod's `tokenFile` reads.
	psmdbVaultTokenKey = "token"
	// k3dVaultCAKey is the Secret key holding the Intranet CA, in both operators' secrets. It is
	// present only when OpenBao serves TLS: a key the Secret does not carry fails the pod.
	k3dVaultCAKey = "ca.crt"
)

// psmdbVaultMinVer is the first PSMDB operator release whose CRD has `spec.secrets.vault` (and
// whose reconciler consults MongoConfiguration.VaultEnabled). Below it the field does not exist,
// and the best case — the API server prunes it — is the worst outcome available: a cluster that
// comes up healthy, reports encryption on the canvas, and is not encrypted.
const psmdbVaultMinVer = "1.13.0"

// psmdbHasVault reports whether an operator version understands spec.secrets.vault. An empty
// version is "unknown", not "newest" — the same rule pgHasClusterFeatures follows, and for the
// same reason: guessing wrong here costs the encryption, quietly.
func psmdbHasVault(operatorVer string) bool {
	v := strings.TrimSpace(operatorVer)
	return v != "" && compareVersions(v, psmdbVaultMinVer) >= 0
}

// k3dVaultOperators are the operators this file can encrypt. PostgreSQL has its own route
// (spec.extensions.pg_tde — see k3dpg.go), and neither the PS operator nor the two community
// PostgreSQL ones have a vault integration at all.
var k3dVaultOperators = map[string]bool{"pxc": true, "psmdb": true}

// k3dVaultOn reports whether a frame asks for data-at-rest encryption on an operator that has it.
func k3dVaultOn(f designFrame) bool {
	return f.K3DVaultEncryption && k3dVaultOperators[strings.TrimSpace(f.K3DOperator)]
}

// k3dVaultMount is the cluster's own KV v2 mount on the OpenBao node, named the way dbvault.go
// names a standalone node's (mysql-<host> / mongodb-<host>) and pgTDEMount names a PostgreSQL
// cluster's. One mount per cluster is not tidiness: Percona is explicit that a secret_mount_point
// must belong to a single server, and two clusters sharing one would corrupt each other's keys.
func k3dVaultMount(operator, cluster string) string {
	if operator == "psmdb" {
		return "mongodb-" + cluster
	}
	return "mysql-" + cluster
}

// crPXCImageMajor is the Percona XtraDB Cluster series the operator release's cr.yaml pins —
// "8.4" from `image: percona/percona-xtradb-cluster:8.4.8-8.1`, "" when it cannot be read.
//
// The image line is the authority on which server the cluster will run, and nothing else in the
// deploy states it: the frame has no version picker for the database (only for the operator), and
// each operator release ships its own matched image. The same trick the backup code uses to ask
// the selected release's own cr.yaml what its schema accepts, for the same reason — the answer
// differs per release and the file is already in hand.
//
// Only the section's own `image:` counts. cr.yaml carries a dozen of them (the proxies, the log
// collector, PMM, the backup images), so this is anchored on `spec.pxc.image` rather than on the
// first image line it meets.
func crPXCImageMajor(src string) string {
	inPXC := false
	for _, ln := range strings.Split(src, "\n") {
		ind, commented, body := crLine(ln)
		if commented || body == "" {
			continue
		}
		if ind == 2 {
			inPXC = body == "pxc:"
			continue
		}
		if !inPXC || ind != 4 || !strings.HasPrefix(body, "image:") {
			continue
		}
		_, tag, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(body, "image:")), ":")
		if !ok {
			return ""
		}
		// 8.4.8-8.1 → 8.4. Two components, because that is what the operator's own entrypoint
		// matches on ($MYSQL_VERSION is `mysqld -V` cut to major.minor).
		parts := strings.SplitN(tag, ".", 3)
		if len(parts) < 2 {
			return ""
		}
		return parts[0] + "." + parts[1]
	}
	return ""
}

// k3dVaultEngineLabel is what the OpenBao policy is annotated with, so a look at the node's mounts
// says which database each one belongs to.
func k3dVaultEngineLabel(operator string) string {
	if operator == "psmdb" {
		return "Percona Operator for MongoDB"
	}
	return "Percona Operator for MySQL (PXC)"
}

// k3dVault is what a provisioned key store hands back to the cr.yaml transform: the Secret the
// operator will mount, and the mount the keys live in.
type k3dVault struct {
	Secret    string // the Kubernetes Secret name (<cluster>-vault)
	Mount     string // the KV v2 mount on the OpenBao node
	VaultHost string // OpenBao's URL, for the panel
	VaultFQDN string // OpenBao's hostname — PSMDB's mongod.conf wants a name, not a URL
	TLS       bool   // OpenBao serves HTTPS, so there is a CA in the Secret
	// Method is how the database reads the keyring, for the panel and the deploy log:
	// "component_keyring_vault" / "keyring_vault plugin" on PXC, "security.vault" on PSMDB.
	// Worth recording because the two MySQL ones are chosen from the server version rather than
	// from anything the user set, and they are the difference between a cluster and a crash loop.
	Method string
	// Manifest is the Secret as it was applied, kept so the copy written back to /root is the
	// one the cluster actually reads rather than a second rendering of it.
	Manifest string
}

// k3dProvisionVault mints the cluster's mount and token on the OpenBao node, renders the
// operator's credentials Secret and applies it to the namespace. It returns what the cr.yaml
// transform needs to point the cluster at it.
//
// Reachability comes free, the same way it does for pg_tde: k3s' CoreDNS forwards the stack's
// domain to the Intranet DNS, so a pod resolves the OpenBao node by the same FQDN every other
// node in the stack uses.
//
// serverMajor is the database series the cluster will run ("8.4", "8.0", …), read from cr.yaml by
// the caller. It decides nothing for PSMDB and everything for PXC — see the file header.
func (a *App) k3dProvisionVault(ctx context.Context, st Stack, frame designFrame, serverID, serverMajor string, cfg *k3dConfig, pr *pxcProg) (*k3dVault, error) {
	operator := strings.TrimSpace(cfg.Operator)
	pr.phase("Provisioning the encryption key store", 85)

	baoCfg, rootToken, baoCID, err := a.waitOpenBaoReady(ctx, st.ID, frame.OpenBaoNodeID, deployTimeout())
	if err != nil {
		return nil, err
	}
	mount := k3dVaultMount(operator, cfg.ClusterName)
	token, err := a.provisionVaultMount(ctx, baoCID, baoCfg, rootToken, mount, "kv-v2", k3dVaultEngineLabel(operator), pr.logln)
	if err != nil {
		return nil, err
	}

	caPEM := ""
	if baoCfg.TLS {
		if caPEM, err = a.k3dIntranetCA(ctx, st, baoCfg.Addr); err != nil {
			return nil, err
		}
	} else {
		pr.logln("encryption keys → " + baoCfg.Addr + " over plain HTTP: the OpenBao node has SSL off, " +
			"so the master key crosses the stack network unencrypted")
	}

	v := &k3dVault{
		Secret:    cfg.ClusterName + "-vault",
		Mount:     mount,
		VaultHost: baoCfg.Addr,
		VaultFQDN: baoCfg.FQDN,
		TLS:       baoCfg.TLS,
	}
	switch operator {
	case "psmdb":
		v.Method = "security.vault"
		v.Manifest = k3dVaultSecret(v.Secret, map[string]string{psmdbVaultTokenKey: token}, caPEM)
	default:
		caFile := ""
		if baoCfg.TLS {
			// The path the CA will have INSIDE the pod. For 8.4 the operator copies this file
			// into the datadir, but the CA stays in the mounted Secret, so the absolute path
			// under the mount point is right for both formats.
			caFile = pxcVaultMountPath + "/" + k3dVaultCAKey
		}
		// Component or plugin, byte for byte the file a standalone node of the same series gets.
		// Writing the wrong one is not a degraded cluster, it is a crash loop — see the header.
		conf, method := mysqlKeyringComponentConf(baoCfg.Addr, mount, token, caFile), "component_keyring_vault"
		if !mysqlModernMajor(serverMajor) {
			conf, method = mysqlKeyringPluginConf(baoCfg.Addr, mount, token, caFile, "2"), "keyring_vault plugin"
		}
		v.Method = method
		pr.logln("keyring: " + method + " (Percona XtraDB Cluster " + orDefault(serverMajor, "unknown series") + ")")
		v.Manifest = k3dVaultSecret(v.Secret, map[string]string{pxcVaultConfKey: conf}, caPEM)
	}
	if err := a.kubectlApply(ctx, serverID, cfg.Namespace, []byte(v.Manifest)); err != nil {
		return nil, fmt.Errorf("create the vault credentials secret: %w", err)
	}
	return v, nil
}

// k3dIntranetCA reads the stack CA off the Intranet node — the one certificate authority in a
// stack, and the only thing a pod can verify an OpenBao node's TLS with. `addr` is only used to
// say which endpoint the CA was wanted for.
func (a *App) k3dIntranetCA(ctx context.Context, st Stack, addr string) (string, error) {
	intranetID := a.intranetContainerID(ctx, st)
	if intranetID == "" {
		return "", fmt.Errorf("OpenBao at %s serves TLS but the stack has no Intranet to take the CA from", addr)
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return "", fmt.Errorf("wait for the Intranet CA: %w", err)
	}
	ca, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil || len(ca) == 0 {
		return "", fmt.Errorf("read the Intranet CA: %w", err)
	}
	return string(ca), nil
}

// k3dVaultSecret renders the credentials Secret. Written as a manifest rather than through
// `kubectl create secret --from-literal` because both the keyring config and the CA are
// multi-line, and stringData keeps them readable in the file that lands in /root.
//
// Keys are emitted in a fixed order (the caller's map is small and the CA last) so a redeploy
// produces the same bytes and the manifest diffs cleanly.
func k3dVaultSecret(name string, data map[string]string, caPEM string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\ntype: Opaque\nstringData:\n", name)
	for _, k := range []string{pxcVaultConfKey, psmdbVaultTokenKey} {
		v, ok := data[k]
		if !ok {
			continue
		}
		if strings.Contains(strings.TrimRight(v, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s: |-\n", k)
			for _, ln := range strings.Split(strings.TrimRight(v, "\n"), "\n") {
				b.WriteString("    " + ln + "\n")
			}
			continue
		}
		fmt.Fprintf(&b, "  %s: %s\n", k, strings.TrimRight(v, "\n"))
	}
	if caPEM != "" {
		b.WriteString("  " + k3dVaultCAKey + ": |\n")
		for _, ln := range strings.Split(strings.TrimRight(caPEM, "\n"), "\n") {
			b.WriteString("    " + ln + "\n")
		}
	}
	return b.String()
}

// psmdbVaultConfiguration renders the mongod.conf fragment for one replica set: the
// `security.vault` block that makes mongod fetch its master key from OpenBao instead of reading a
// local key file.
//
// `secret` is a path INSIDE the mount and each replica set gets its own — `<mount>/data/<cluster>-rs0`
// and `<mount>/data/<cluster>-cfg`. That is what Percona's own e2e test does, and it matters: the
// replica set and the config servers are separate WiredTiger deployments with separate keys, and
// pointing both at one path has the second to start overwrite the first's key.
//
// `/data/` is KV v2's storage prefix. A KV v1 mount would have the name directly under the mount,
// which is why the mount is always created as v2 here rather than following the engine's
// preference the way vaultMountFor does for a standalone node.
func psmdbVaultConfiguration(v *k3dVault, replset string) string {
	var b strings.Builder
	b.WriteString("configuration: |\n")
	b.WriteString("  security:\n")
	b.WriteString("    enableEncryption: true\n")
	b.WriteString("    vault:\n")
	fmt.Fprintf(&b, "      serverName: %s\n", v.VaultFQDN)
	fmt.Fprintf(&b, "      port: %d\n", openbaoAPIPort)
	fmt.Fprintf(&b, "      secret: %s/data/%s\n", v.Mount, replset)
	fmt.Fprintf(&b, "      tokenFile: %s/%s\n", psmdbVaultMountPath, psmdbVaultTokenKey)
	if v.TLS {
		fmt.Fprintf(&b, "      serverCAFile: %s/%s\n", psmdbVaultMountPath, k3dVaultCAKey)
	} else {
		// Exactly what it says it is. The operator's own e2e test sets it too, and a lab OpenBao
		// with SSL off gives mongod nothing to verify.
		b.WriteString("      disableTLSForTesting: true\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// ---------------------------------------------------------------- validation

// k3dVaultIssues validates a frame's data-at-rest encryption. Like k3dPGFeatureIssues it lives
// outside k3dFrameIssues because it needs the design: the key store is an OpenBao node on the
// canvas, and without one there is nothing to encrypt against.
//
// `deployed` decides error vs warning, for the reason k3dPGFeatureIssues documents at length: a
// running frame is not deployed again, so an error about what its cr.yaml would say would block a
// whole stack over a file nothing is going to re-apply.
func k3dVaultIssues(f designFrame, doc designDoc, opCat OperatorCatalog, deployed bool) []issue {
	if f.Type != "k3d" || !f.K3DVaultEncryption {
		return nil
	}
	lvl, tail := "error", ""
	if deployed {
		lvl, tail = "warning", " (this cluster is already running, so nothing here is re-applied — destroy the frame to change it)"
	}
	name := f.Label
	op := strings.TrimSpace(f.K3DOperator)

	// Wrong operator: the setting is simply not read. Worth saying rather than silently dropping
	// — a frame switched from PXC to CloudNativePG keeps the checkbox, and silence would look
	// like it still applies. PostgreSQL is called out by name because it *does* have encryption,
	// just under its own option.
	if !k3dVaultOperators[op] {
		msg := "K3D cluster " + name + " has data-at-rest encryption on, which this deploy cannot do: " +
			"it runs " + orDefault(k3dOperatorLabel(op), "no operator") + ", and only the PXC and MongoDB operators " +
			"key their encryption to a vault here"
		if op == "pg" {
			msg += ". The Percona Operator for PostgreSQL has its own — turn on transparent data encryption (pg_tde) instead"
		}
		return []issue{{Level: "warning", Message: msg}}
	}

	var out []issue
	// The key store itself.
	hasBao := false
	for _, n := range doc.Nodes {
		if n.Type == "openbao" && (f.OpenBaoNodeID == "" || n.ID == f.OpenBaoNodeID) {
			hasBao = true
			break
		}
	}
	if !hasBao {
		out = append(out, issue{Level: lvl, Message: "K3D cluster " + name +
			" has data-at-rest encryption on but no OpenBao node to keep the master key in. " +
			"Add an OpenBao node and select it" + tail})
	}

	// PSMDB's CRD gate. PXC has had vaultSecretName since 1.7.0, which is the oldest version the
	// catalog offers, so there is nothing to check on that side.
	if op == "psmdb" {
		ver, ok := opCat.resolveOperatorVersion("psmdb", f.K3DOperatorVer)
		switch {
		case !ok:
			out = append(out, issue{Level: lvl, Message: "K3D cluster " + name +
				" asks for data-at-rest encryption, which needs a known operator version to check against " +
				psmdbVaultMinVer + " — pick one from the list, or run `make versions`" + tail})
		case !psmdbHasVault(ver):
			out = append(out, issue{Level: lvl, Message: "K3D cluster " + name +
				" asks for data-at-rest encryption, which the Percona Operator for MongoDB only has from " +
				psmdbVaultMinVer + " — this frame pins " + ver + ", whose CRD has no `secrets.vault` field. " +
				"The cluster would come up unencrypted while the panel said otherwise. Choose " + psmdbVaultMinVer +
				" or newer, or turn encryption off" + tail})
		}
	}
	return out
}
