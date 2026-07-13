package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// OpenBao node (Type=="openbao"; a per-stack singleton) — a HashiCorp-Vault-compatible secrets
// manager, used as the KMS for Percona data-at-rest encryption: Percona Server for MySQL reaches
// it with the keyring_vault component/plugin, Percona Server for MongoDB with its security.vault
// settings (dbvault.go). Both speak the Vault API, so the node exports the familiar VAULT_ADDR /
// VAULT_CACERT environment.
//
// Runs on the systemd Oracle Linux 9 image (`make images`) — OpenBao is packaged in EPEL, so
// the install is `dnf install oracle-epel-release-el9` then `dnf install openbao`, which gives
// the `bao` binary + the openbao.service unit reading /etc/openbao.d/openbao.hcl.
//
// TLS (the default) is served with a certificate signed by the Intranet CA. The server cert,
// its key and the CA cert all live in /etc/openbao.d/tls and are named in openbao.hcl, so the
// database nodes — which already trust the Intranet CA (catrust.go) — verify it.
//
// At the end of provisioning the node is *initialized and unsealed*: `bao operator init` runs
// with 5 key shares / threshold 3, the five unseal keys + the root token are stored as node
// secrets (shown in the node's properties), and three of the keys are replayed to unseal it.
// The KV mounts and Percona policies below are then created with the root token, so the node
// is immediately usable as a keyring for a PS/PSMDB node.

const (
	openbaoAPIPort     = 8200
	openbaoClusterPort = 8201
	openbaoConfDir     = "/etc/openbao.d"
	openbaoTLSDir      = openbaoConfDir + "/tls"
	openbaoDataDir     = "/opt/openbao/data"
	// TLS material, all under openbaoTLSDir per the node's contract.
	openbaoCertFile = openbaoTLSDir + "/server.crt"
	openbaoKeyFile  = openbaoTLSDir + "/server.key"
	openbaoCAFile   = openbaoTLSDir + "/ca.crt"
	// operator init parameters — the node's properties surface all five keys.
	openbaoKeyShares    = 5
	openbaoKeyThreshold = 3
)

// openbaoMounts are the KV secrets engines created at deploy, each with a matching policy
// (openbaoPolicy). Percona Server for MySQL's keyring_vault component speaks KV v1 or v2, so it
// gets one of each. Percona Server for MongoDB supports **only KV v2** — there is deliberately
// no mongodb-v1: a mount PSMDB can never authenticate against is a trap, not an option.
var openbaoMounts = []struct {
	Path    string // mount path, also the policy name
	KV      string // "kv" (v1) | "kv-v2"
	Engine  string // percona engine this is meant for (UI copy)
	Version string // "1" | "2"
}{
	{"mysql-v1", "kv", "Percona Server for MySQL", "1"},
	{"mysql-v2", "kv-v2", "Percona Server for MySQL", "2"},
	{"mongodb-v2", "kv-v2", "Percona Server for MongoDB", "2"},
}

// openbaoConfig is the non-secret profile shown for a deployed OpenBao node.
type openbaoConfig struct {
	Image     string `json:"image"`
	OS        string `json:"os"`
	Hostname  string `json:"hostname"`
	FQDN      string `json:"fqdn"`
	Addr      string `json:"addr"`      // VAULT_ADDR — https://<fqdn>:8200 (http when TLS is off)
	CACert    string `json:"caCert"`    // VAULT_CACERT — the Intranet CA copy in /etc/openbao.d/tls
	TLS       bool   `json:"tls"`       // serving HTTPS with an Intranet-CA cert
	ConfFile  string `json:"confFile"`  // /etc/openbao.d/openbao.hcl
	Initted   bool   `json:"initted"`   // `bao operator init` ran (keys are in the node's secrets)
	Sealed    bool   `json:"sealed"`    // seal state right after provisioning
	Mounts    []kvM  `json:"mounts"`    // KV mounts + their policies
	UseProxy  bool   `json:"useProxy"`  // package egress via the Intranet Squid proxy
	Ports     []int  `json:"ports"`     // 8200 (API), 8201 (cluster)
	PolicyDir string `json:"policyDir"` // /etc/openbao.d — where the .hcl policy files live
}

// kvM is one KV mount as surfaced to the UI.
type kvM struct {
	Path       string `json:"path"`       // mount path (= policy name)
	KV         string `json:"kv"`         // kv | kv-v2
	Version    string `json:"version"`    // 1 | 2
	Engine     string `json:"engine"`     // which Percona engine it is meant for
	PolicyFile string `json:"policyFile"` // /etc/openbao.d/policy-<path>.hcl
}

// openbaoSecrets holds what `bao operator init` printed. These are the only copy — OpenBao
// never shows them again — so they are stored with the deployment and rendered in the node's
// properties.
type openbaoSecrets struct {
	RootToken  string   `json:"rootToken"`
	UnsealKeys []string `json:"unsealKeys"` // 5 base64 shares; any 3 unseal
	Threshold  int      `json:"threshold"`
}

// openbaoAddr is the VAULT_ADDR clients (and the bao CLI on the node) use.
func openbaoAddr(fqdn string, tls bool) string {
	scheme := "http"
	if tls {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, fqdn, openbaoAPIPort)
}

// openbaoHCL renders /etc/openbao.d/openbao.hcl: a file-backed store and one TCP listener,
// serving TLS from the Intranet-CA material in /etc/openbao.d/tls (or plain HTTP when the
// node opted out of SSL). api_addr/cluster_addr are the node's FQDN so a client redirected by
// the API lands back on a name the stack's DNS resolves.
func openbaoHCL(fqdn string, tls bool) string {
	var b strings.Builder
	b.WriteString("# Managed by DBCanvas — OpenBao server configuration.\n")
	b.WriteString("ui = true\n\n")
	fmt.Fprintf(&b, "storage \"file\" {\n  path = %q\n}\n\n", openbaoDataDir)
	fmt.Fprintf(&b, "listener \"tcp\" {\n  address = \"0.0.0.0:%d\"\n", openbaoAPIPort)
	if tls {
		fmt.Fprintf(&b, "  tls_cert_file = %q\n", openbaoCertFile)
		fmt.Fprintf(&b, "  tls_key_file  = %q\n", openbaoKeyFile)
		fmt.Fprintf(&b, "  tls_client_ca_file = %q\n", openbaoCAFile)
		b.WriteString("  tls_min_version = \"tls12\"\n")
	} else {
		b.WriteString("  tls_disable = 1\n")
	}
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "api_addr = %q\n", openbaoAddr(fqdn, tls))
	fmt.Fprintf(&b, "cluster_addr = \"https://%s:%d\"\n", fqdn, openbaoClusterPort)
	b.WriteString("disable_mlock = true\n")
	return b.String()
}

// openbaoPolicy renders the policy for one KV mount.
//
// KV v1 is a flat tree, so one rule over <mount>/* is all the engine needs. KV v2 splits the
// data and metadata trees, and Percona Server for MongoDB additionally reads <mount>/config
// and the metadata (it checks the version count before rotating a master key, so it does not
// silently lose one) — see the Percona "Using Vault to store the master key" docs. The MySQL
// keyring_vault component both reads and writes keys, so it gets full capabilities on data and
// metadata; MongoDB follows Percona's documented policy exactly.
func openbaoPolicy(mount, kv, engine string) string {
	mysql := strings.Contains(engine, "MySQL")
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — KV %s mount %q (DBCanvas)\n", engine, map[string]string{"kv": "v1", "kv-v2": "v2"}[kv], mount)
	if kv == "kv" {
		fmt.Fprintf(&b, "path \"%s/*\" {\n  capabilities = [\"create\", \"read\", \"update\", \"delete\", \"list\"]\n}\n", mount)
		return b.String()
	}
	if mysql {
		fmt.Fprintf(&b, "path \"%s/data/*\" {\n  capabilities = [\"create\", \"read\", \"update\", \"delete\", \"list\"]\n}\n", mount)
		fmt.Fprintf(&b, "path \"%s/metadata/*\" {\n  capabilities = [\"create\", \"read\", \"update\", \"delete\", \"list\"]\n}\n", mount)
		fmt.Fprintf(&b, "path \"%s/config\" {\n  capabilities = [\"read\"]\n}\n", mount)
		return b.String()
	}
	fmt.Fprintf(&b, "path \"%s/data/*\" {\n  capabilities = [\"create\", \"read\", \"update\", \"delete\"]\n}\n", mount)
	fmt.Fprintf(&b, "path \"%s/metadata/*\" {\n  capabilities = [\"read\"]\n}\n", mount)
	fmt.Fprintf(&b, "path \"%s/config\" {\n  capabilities = [\"read\"]\n}\n", mount)
	return b.String()
}

// openbaoProfile renders /etc/profile.d/openbao.sh. VAULT_* is what the Percona engines and the
// Vault-compatible tooling read; BAO_* is the native name the `bao` CLI prefers. Setting both
// means `bao status` works over TLS on the node with no flags.
func openbaoProfile(addr string, tls bool) string {
	var b strings.Builder
	b.WriteString("# Managed by DBCanvas — OpenBao client environment.\n")
	fmt.Fprintf(&b, "export VAULT_ADDR=%q\n", addr)
	fmt.Fprintf(&b, "export BAO_ADDR=%q\n", addr)
	if tls {
		fmt.Fprintf(&b, "export VAULT_CACERT=%q\n", openbaoCAFile)
		fmt.Fprintf(&b, "export BAO_CACERT=%q\n", openbaoCAFile)
	}
	return b.String()
}

// openbaoInstallScript installs OpenBao from EPEL. On Oracle Linux the EPEL release package is
// oracle-epel-release-el9 (epelPackage), which is in the ol9_developer_EPEL repo; the generic
// `epel-release` name only exists on RHEL/CentOS, so both are tried. Idempotent.
const openbaoInstallScript = `set -e
command -v bao >/dev/null 2>&1 && { echo "openbao already installed"; bao version; exit 0; }
dnf install -y "$EPELPKG" >/dev/null 2>&1 || dnf install -y epel-release >/dev/null 2>&1 || true
dnf install -y openbao >/dev/null
command -v bao >/dev/null 2>&1 || { echo "openbao not installed (is EPEL reachable?)"; exit 1; }
bao version`

// openbaoPrepScript creates the openbao user + the config/TLS/data directories. It must run
// before the config and certificates are staged: CopyFile untars into an existing directory,
// so /etc/openbao.d/tls has to exist first (the package only creates /etc/openbao.d).
const openbaoPrepScript = `set -e
id openbao >/dev/null 2>&1 || useradd --system --home-dir /opt/openbao --shell /sbin/nologin openbao
install -d -o openbao -g openbao -m 0750 /opt/openbao ` + openbaoDataDir + ` ` + openbaoTLSDir + `
install -d -o openbao -g openbao -m 0755 ` + openbaoConfDir

// openbaoServiceScript locks down the staged files and starts openbao.service. The EPEL package
// ships the unit (reading /etc/openbao.d/openbao.hcl as the openbao user); if a future package
// drops it, a minimal equivalent is written so the node still comes up.
const openbaoServiceScript = `set -e
chown -R openbao:openbao ` + openbaoTLSDir + `
chmod 0750 ` + openbaoTLSDir + `
# The key stays private to the openbao user; the cert + CA are world-readable (clients on this
# node read the CA to verify the listener). Absent when the node opted out of TLS.
if [ -f ` + openbaoKeyFile + ` ]; then chmod 0640 ` + openbaoKeyFile + `; fi
if [ -f ` + openbaoCertFile + ` ]; then chmod 0644 ` + openbaoCertFile + `; fi
if [ -f ` + openbaoCAFile + ` ]; then chmod 0644 ` + openbaoCAFile + `; fi
chown openbao:openbao ` + openbaoConfDir + `/openbao.hcl
chmod 0640 ` + openbaoConfDir + `/openbao.hcl
if [ ! -f /usr/lib/systemd/system/openbao.service ] && [ ! -f /etc/systemd/system/openbao.service ]; then
  cat > /etc/systemd/system/openbao.service <<'UNIT'
[Unit]
Description=OpenBao
After=network-online.target
Wants=network-online.target

[Service]
User=openbao
Group=openbao
ExecStart=/usr/bin/bao server -config=` + openbaoConfDir + `/openbao.hcl
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
LimitNOFILE=65536
AmbientCapabilities=CAP_IPC_LOCK

[Install]
WantedBy=multi-user.target
UNIT
fi
systemctl daemon-reload
systemctl enable --now openbao >/dev/null 2>&1 || systemctl restart openbao
# Listening-but-uninitialized/sealed counts as up: until operator init runs, every status call
# reports "not yet initialized" (a non-zero exit) even though the listener is healthy.
for i in $(seq 1 30); do
  bao status >/dev/null 2>&1 && exit 0
  case "$(bao status 2>&1)" in *"not yet initialized"*|*[Ss]ealed*) exit 0;; esac
  sleep 2
done
echo "openbao did not answer on $VAULT_ADDR:"; systemctl status openbao --no-pager -l 2>&1 | tail -20; exit 1`

// openbaoInitScript initializes the server and prints the raw JSON of `bao operator init`
// (unseal_keys_b64 + root_token) on stdout for the caller to parse. If the node is already
// initialized (a redeploy over surviving data) it prints {"already_initialized":true} instead,
// and the caller falls back to the keys it stored the first time.
const openbaoInitScript = `set -e
if bao status -format=json 2>/dev/null | grep -q '"initialized": *true'; then
  echo '{"already_initialized":true}'
  exit 0
fi
bao operator init -key-shares=$SHARES -key-threshold=$THRESHOLD -format=json`

// openbaoUnsealScript unseals with the threshold keys passed as KEY1..KEY3 (a sealed server
// rejects every other API call). No-op when the server is already unsealed.
const openbaoUnsealScript = `set -e
bao status -format=json 2>/dev/null | grep -q '"sealed": *false' && { echo "already unsealed"; exit 0; }
for k in "$KEY1" "$KEY2" "$KEY3"; do
  [ -n "$k" ] || continue
  bao operator unseal "$k" >/dev/null
done
bao status -format=json | grep -q '"sealed": *false' || { echo "still sealed"; bao status; exit 1; }
echo "unsealed"`

// openbaoMountScript enables one KV mount and loads its policy file. Idempotent: an existing
// mount/policy is left alone (a redeploy must not wipe keys a database is still using).
const openbaoMountScript = `set -e
export BAO_TOKEN="$ROOT_TOKEN"
bao secrets list -format=json 2>/dev/null | grep -q "\"$MOUNT/\"" || bao secrets enable -path="$MOUNT" "$KV" >/dev/null
bao policy write "$MOUNT" "$POLICY_FILE" >/dev/null
echo "mount $MOUNT ($KV) + policy $MOUNT"`

// openbaoStatusScript prints the seal state as JSON. `bao status` exits non-zero when the
// server is sealed, which is a normal state here, so the exit code is swallowed.
const openbaoStatusScript = `bao status -format=json 2>/dev/null || true`

// baoClientEnv is the exec environment the bao CLI needs on the node: a non-login shell does
// not read /etc/profile.d, so VAULT_ADDR/VAULT_CACERT are passed explicitly.
func baoClientEnv(cfg openbaoConfig) []string {
	env := []string{"VAULT_ADDR=" + cfg.Addr, "BAO_ADDR=" + cfg.Addr}
	if cfg.TLS {
		env = append(env, "VAULT_CACERT="+openbaoCAFile, "BAO_CACERT="+openbaoCAFile)
	}
	return env
}

// handleOpenBaoStatus reports the node's live seal state (it seals itself on every restart).
func (a *App) handleOpenBaoStatus(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	var cfg openbaoConfig
	json.Unmarshal(dep.Config, &cfg)
	out, err := a.execScript(r.Context(), dep.ContainerID, openbaoStatusScript, baoClientEnv(cfg))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "openbao status: "+err.Error())
		return
	}
	var st struct {
		Initialized bool `json:"initialized"`
		Sealed      bool `json:"sealed"`
		N           int  `json:"n"`
		T           int  `json:"t"`
		Progress    int  `json:"progress"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &st) != nil {
		writeErr(w, http.StatusInternalServerError, "openbao is not answering on "+cfg.Addr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized": st.Initialized, "sealed": st.Sealed,
		"keyShares": st.N, "threshold": st.T, "progress": st.Progress,
	})
}

// handleOpenBaoUnseal unseals the node with the stored keys. OpenBao seals itself whenever the
// process restarts, and the operator would otherwise have to paste threshold keys back in by
// hand — DBCanvas already holds them (they are shown in the node's properties), so this replays
// them. It never reveals a key: only the resulting seal state comes back.
func (a *App) handleOpenBaoUnseal(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	var cfg openbaoConfig
	json.Unmarshal(dep.Config, &cfg)
	var sec openbaoSecrets
	json.Unmarshal(dep.Secrets, &sec)
	if len(sec.UnsealKeys) == 0 {
		writeErr(w, http.StatusConflict, "no unseal keys are stored for this node")
		return
	}
	env := baoClientEnv(cfg)
	for i := 0; i < openbaoKeyThreshold && i < len(sec.UnsealKeys); i++ {
		env = append(env, fmt.Sprintf("KEY%d=%s", i+1, sec.UnsealKeys[i]))
	}
	if _, err := a.execScript(r.Context(), dep.ContainerID, openbaoUnsealScript, env); err != nil {
		writeErr(w, http.StatusInternalServerError, "unseal: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sealed": false})
}

// provisionOpenBao records the deployment then brings up an OpenBao node: install from EPEL,
// stage the Intranet-CA TLS material + config, start the service, initialize (5 keys / 3 to
// unseal), unseal, and create the Percona KV mounts + policies.
func (a *App) provisionOpenBao(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)
	image := pxcImage(n.OS, n.OSVersion, n.Arch)
	tls := n.GenerateCert
	addr := openbaoAddr(fqdn, tls)

	mounts := make([]kvM, 0, len(openbaoMounts))
	for _, m := range openbaoMounts {
		mounts = append(mounts, kvM{
			Path: m.Path, KV: m.KV, Version: m.Version, Engine: m.Engine,
			PolicyFile: fmt.Sprintf("%s/policy-%s.hcl", openbaoConfDir, m.Path),
		})
	}
	cfg := openbaoConfig{
		Image: image, OS: n.OS, Hostname: host, FQDN: fqdn,
		Addr: addr, TLS: tls, ConfFile: openbaoConfDir + "/openbao.hcl",
		Mounts: mounts, UseProxy: n.UseProxy, PolicyDir: openbaoConfDir,
		Ports: []int{openbaoAPIPort, openbaoClusterPort},
	}
	if tls {
		cfg.CACert = openbaoCAFile
	}

	// Reuse the init material across redeploys: OpenBao prints the unseal keys once, so if the
	// node was already initialized these are the only copy that can ever unseal it again.
	var sec openbaoSecrets
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		json.Unmarshal(dep.Secrets, &sec)
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID)
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

		// TLS material: a server cert for the node's FQDN signed by the Intranet CA, plus the
		// CA cert itself — VAULT_CACERT points at it, and it is what every client verifies with.
		var tlsCert, tlsKey, caCrt []byte
		if tls {
			pr.phase("Issuing TLS certificate", 12)
			if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
				pr.fail("%v", err)
				return
			}
			crt, cerr := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
			key, kerr := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
			if cerr != nil || kerr != nil || len(crt) == 0 || len(key) == 0 {
				pr.fail("read Intranet CA: %v %v", cerr, kerr)
				return
			}
			caCrt = crt
			c, k, serr := signTLSCert(crt, key, fqdn, []string{fqdn, host}, certTTL(n.CertTTLValue, n.CertTTLUnit))
			if serr != nil {
				pr.fail("sign certificate: %v", serr)
				return
			}
			tlsCert, tlsKey = c, k
		}

		pr.phase("Creating container", 18)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		id, err := a.docker.ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		})
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.docker.ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		pr.phase("Waiting for systemd", 25)
		if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, n.OS, pr.logln)
		a.ensureDNFIPv4(ctx, id, n.OS, pr.logln)
		if n.UseProxy {
			if err := a.runStep(ctx, id, pkgProxyRHEL, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
				pr.fail("configure package proxy: %v", err)
				return
			}
			pr.logln("package egress via Intranet proxy")
		}

		pr.phase("Installing OpenBao (EPEL)", 40)
		if err := a.runStep(ctx, id, openbaoInstallScript, []string{"EPELPKG=" + epelPackage(n.OSVersion)}, pr.logln); err != nil {
			pr.fail("install openbao: %v", err)
			return
		}

		// Config + TLS + the client environment. Staged before the service starts so the very
		// first boot already serves TLS on the FQDN clients will use.
		pr.phase("Configuring OpenBao", 55)
		if err := a.runStep(ctx, id, openbaoPrepScript, nil, pr.logln); err != nil {
			pr.fail("prepare openbao directories: %v", err)
			return
		}
		if tls {
			for _, f := range []struct {
				name string
				mode int64
				data []byte
			}{
				{"server.crt", 0o644, tlsCert},
				{"server.key", 0o640, tlsKey},
				{"ca.crt", 0o644, caCrt},
			} {
				if err := a.docker.CopyFile(ctx, id, openbaoTLSDir, f.name, f.mode, f.data); err != nil {
					pr.fail("write %s: %v", f.name, err)
					return
				}
			}
			pr.logln("TLS material in " + openbaoTLSDir + " (Intranet CA)")
		}
		if err := a.docker.CopyFile(ctx, id, openbaoConfDir, "openbao.hcl", 0o640, []byte(openbaoHCL(fqdn, tls))); err != nil {
			pr.fail("write openbao.hcl: %v", err)
			return
		}
		if err := a.docker.CopyFile(ctx, id, "/etc/profile.d", "openbao.sh", 0o644, []byte(openbaoProfile(addr, tls))); err != nil {
			pr.fail("write openbao.sh: %v", err)
			return
		}
		envLine := "VAULT_ADDR=" + addr
		if tls {
			envLine += " · VAULT_CACERT=" + openbaoCAFile
		}
		pr.logln(envLine)
		// The Percona policies, one file per KV mount, kept next to the server config.
		for _, m := range openbaoMounts {
			f := fmt.Sprintf("policy-%s.hcl", m.Path)
			if err := a.docker.CopyFile(ctx, id, openbaoConfDir, f, 0o644, []byte(openbaoPolicy(m.Path, m.KV, m.Engine))); err != nil {
				pr.fail("write %s: %v", f, err)
				return
			}
		}
		pr.logln("policies staged in " + openbaoConfDir)

		pr.phase("Starting OpenBao", 65)
		// The scripts below run `bao` as a client, which needs VAULT_ADDR/VAULT_CACERT in the
		// exec environment (a non-login shell does not read /etc/profile.d).
		baoEnv := []string{"VAULT_ADDR=" + addr, "BAO_ADDR=" + addr}
		if tls {
			baoEnv = append(baoEnv, "VAULT_CACERT="+openbaoCAFile, "BAO_CACERT="+openbaoCAFile)
		}
		if err := a.runStep(ctx, id, openbaoServiceScript, baoEnv, pr.logln); err != nil {
			pr.fail("start openbao: %v", err)
			return
		}

		// ---- initialize + unseal ----
		pr.phase("Initializing (operator init)", 78)
		out, err := a.execScript(ctx, id, openbaoInitScript,
			append(baoEnv, fmt.Sprintf("SHARES=%d", openbaoKeyShares), fmt.Sprintf("THRESHOLD=%d", openbaoKeyThreshold)))
		if err != nil {
			pr.fail("operator init: %v", err)
			return
		}
		var init struct {
			AlreadyInitialized bool     `json:"already_initialized"`
			UnsealKeysB64      []string `json:"unseal_keys_b64"`
			RootToken          string   `json:"root_token"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &init); err != nil {
			pr.fail("parse operator init output: %v", err)
			return
		}
		switch {
		case init.AlreadyInitialized:
			// Data survived a redeploy. Only the keys stored on the first deploy can unseal it.
			if len(sec.UnsealKeys) == 0 {
				pr.fail("openbao is already initialized but no unseal keys are stored for this node — destroy and redeploy it")
				return
			}
			pr.logln("already initialized — unsealing with the stored keys")
		default:
			sec = openbaoSecrets{RootToken: init.RootToken, UnsealKeys: init.UnsealKeysB64, Threshold: openbaoKeyThreshold}
			secJSON, _ = json.Marshal(sec)
			cfg.Initted = true
			cfgJSON, _ = json.Marshal(cfg)
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
			pr.logln(fmt.Sprintf("initialized: %d unseal keys (threshold %d) + root token stored in the node's properties", len(sec.UnsealKeys), openbaoKeyThreshold))
		}

		pr.phase("Unsealing", 85)
		unsealEnv := append([]string{}, baoEnv...)
		for i := 0; i < openbaoKeyThreshold && i < len(sec.UnsealKeys); i++ {
			unsealEnv = append(unsealEnv, fmt.Sprintf("KEY%d=%s", i+1, sec.UnsealKeys[i]))
		}
		if err := a.runStep(ctx, id, openbaoUnsealScript, unsealEnv, pr.logln); err != nil {
			pr.fail("unseal: %v", err)
			return
		}
		cfg.Initted, cfg.Sealed = true, false

		// ---- KV mounts + Percona policies (root token) ----
		pr.phase("Creating KV mounts + Percona policies", 92)
		for _, m := range openbaoMounts {
			env := append([]string{}, baoEnv...)
			env = append(env,
				"ROOT_TOKEN="+sec.RootToken, "MOUNT="+m.Path, "KV="+m.KV,
				"POLICY_FILE="+fmt.Sprintf("%s/policy-%s.hcl", openbaoConfDir, m.Path))
			if err := a.runStep(ctx, id, openbaoMountScript, env, pr.logln); err != nil {
				// A keyring the operator can still create by hand is not worth failing the node.
				pr.logln("mount " + m.Path + " skipped: " + err.Error())
			}
		}

		cfgJSON, _ = json.Marshal(cfg)
		secJSON, _ = json.Marshal(sec)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d openbao %s: provisioned (%s, unsealed)", st.ID, n.Label, addr)
	}()
}
