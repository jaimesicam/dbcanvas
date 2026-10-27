package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// openbaopki.go — a PKI secrets engine on the OpenBao node that signs as the Intranet CA.
//
// The mount is given the Intranet CA itself — certificate and key, through pki/config/ca — so what
// OpenBao signs is signed by the one root every node, pod and the VNC desktop already trust, and a
// cluster's Secrets carry tls.crt = the leaf and ca.crt = that root, exactly what a CA Issuer or a
// hand-made Secret would.
//
// ------------------------------------------------------------------ why not an intermediate
//
// An intermediate of the Intranet root (OpenBao generating the key, the root signing it) verifies
// everywhere a TLS library builds the chain. Percona XtraDB Cluster's SST does not build one:
// wsrep_sst_xtrabackup-v2 checks the node's own certificate with
//
//	openssl verify -verbose -CAfile "$ssl_ca" "$ssl_cert"     (verify_ca_matches_cert)
//
// and `openssl verify` checks only the FIRST certificate of the file it is given, against only the
// CA file. With an intermediate, cert-manager's Vault issuer writes tls.crt = leaf + intermediate
// and ca.crt = the root (pki.ParseSingleCertificateChain picks the self-signed root for ca.crt and
// leaves it out of the chain), so the intermediate is in neither place openssl looks:
//
//	error 20 at 0 depth lookup: unable to get local issuer certificate
//	[WSREP-SST] * The certifcate and CA (certificate authority) do not match.
//
// The first member bootstraps; every joiner crash-loops on its SST. Any PXC cluster whose
// cert-manager issues from a Vault intermediate does the same, so the mount holds the root instead
// and signs leaves directly.
//
// The price is that the root key is in OpenBao (and in its storage on the node). In a lab whose root
// already sits in a file on the Intranet node that is no new exposure.
//
// ------------------------------------------------------------------ who uses it
//
// A K3D frame whose cert-manager issuer is "openbao" (k3dissuer.go) gets an AppRole of its own
// whose policy may only sign through the role below, and a cert-manager Vault Issuer that logs in
// with it. AppRole rather than a token on purpose: the Vault issuer never renews a token, and
// OpenBao caps a token's life at 32 days, so a token-based issuer would stop issuing a month into a
// lab without anything on the canvas saying so. An AppRole secret-id can be minted with no expiry,
// and each login gets a fresh short token.
//
// The OpenBao node can also turn the engine on by itself at deploy (its "PKI" option), for use by
// hand. Either way the setup is idempotent: a mount that already has its CA is left alone.

const (
	openbaoPKIMount  = "pki"
	openbaoPKIRole   = "dbcanvas"
	openbaoPKIPolicy = "pki-dbcanvas"
	// openbaoPKIRolePSMDB signs for the MongoDB operator, with the subject it asks for. mongod
	// counts a peer as a cluster member only when the O/OU/DC of its certificate match its own,
	// and the operator requests O=PSMDB for both; a `sign` role keeps the request's CN but takes
	// the rest of the subject from the role, so the plain role's certificates arrive as bare
	// CN=<cluster> and every member's x509 cluster auth fails as an ordinary user
	// ("UserNotFound: Could not find user CN=<cluster> for db $external"). sign-verbatim would
	// keep O, but also the request's EKU — server auth only — so it is a role, not that endpoint.
	openbaoPKIRolePSMDB = "psmdb"
	// Leaves may ask for up to three years — the replication certificates do (k3dReplCertTTL).
	// The mount's own ceiling is the Intranet root's ten, and the issuer truncates rather than
	// refuses a leaf that would outlive the root.
	openbaoPKIMaxTTL  = "87600h"
	openbaoPKILeafMax = "26280h"
	// openbaoPKIBundle is where the CA certificate + key are staged for config/ca, and removed
	// from straight afterwards.
	openbaoPKIBundle = openbaoConfDir + "/pki-ca-bundle.pem"
)

// openbaoPKIInfo is what the node's panel shows about the engine.
type openbaoPKIInfo struct {
	Mount    string `json:"mount"`    // pki_int
	Role     string `json:"role"`     // dbcanvas
	Policy   string `json:"policy"`   // pki-dbcanvas
	SignPath string `json:"signPath"` // pki_int/sign/dbcanvas — what cert-manager's Vault issuer calls
	Subject  string `json:"subject"`  // the CA it signs as: the Intranet CA
}

// openbaoPKIMu serialises the setup per OpenBao container: two K3D frames deploying at once would
// otherwise both find no mount and both try to create it.
var openbaoPKIMu sync.Map // container id → *sync.Mutex

// openbaoPKISignPath is the endpoint cert-manager signs through for an operator.
func openbaoPKISignPath(operator string) string {
	if operator == "psmdb" {
		return openbaoPKIMount + "/sign/" + openbaoPKIRolePSMDB
	}
	return openbaoPKIMount + "/sign/" + openbaoPKIRole
}

// openbaoPKIPolicyHCL lets a holder sign certificate requests through the roles, and read the
// CA. Not `issue`: the role takes any key type, which only `sign` accepts, and a client that
// sends its own CSR keeps its private key to itself.
func openbaoPKIPolicyHCL() string {
	return fmt.Sprintf(`# cert-manager's Vault issuer — sign through the roles of PKI mount %[1]q (DBCanvas)
path "%[1]s/sign/%[2]s" {
  capabilities = ["create", "update"]
}
path "%[1]s/sign/%[3]s" {
  capabilities = ["create", "update"]
}
path "%[1]s/cert/ca" {
  capabilities = ["read"]
}
`, openbaoPKIMount, openbaoPKIRole, openbaoPKIRolePSMDB)
}

// openbaoPKIPrepScript enables and tunes the mount, then prints READY when it already has its CA,
// NEED_CA when the CA still has to be imported.
const openbaoPKIPrepScript = `set -e
export BAO_TOKEN="$ROOT_TOKEN"
bao secrets list -format=json | grep -q "\"$MOUNT/\"" || bao secrets enable -path="$MOUNT" pki >/dev/null
bao secrets tune -max-lease-ttl="$MAXTTL" "$MOUNT" >/dev/null
if bao read -field=certificate "$MOUNT/cert/ca" 2>/dev/null | grep -q "BEGIN CERTIFICATE"; then echo READY; else echo NEED_CA; fi`

// openbaoPKIImportScript gives the mount the Intranet CA, then removes the staged bundle — the
// key now lives in OpenBao's storage and nowhere else on this node.
const openbaoPKIImportScript = `set -e
export BAO_TOKEN="$ROOT_TOKEN"
trap 'rm -f "$BUNDLE"' EXIT
bao write "$MOUNT/config/ca" pem_bundle=@"$BUNDLE" >/dev/null
echo "Intranet CA imported into $MOUNT"`

// openbaoPKIRoleScript is the part that is (re)applied every time: the URLs, the issuer's
// truncate-rather-than-refuse behaviour, the role and the policy. It prints the CA's subject.
//
// The role is deliberately permissive — it is a lab. allow_any_name because the operators ask for
// bare in-cluster names (cluster1-pxc, *.cluster1-pxc) that no allowed_domains list would cover;
// enforce_hostnames off for the same names; IP SANs because a replication source names its
// LoadBalancer addresses; client_flag because Galera presents the same certificate as a client.
const openbaoPKIRoleScript = `set -e
export BAO_TOKEN="$ROOT_TOKEN"
bao write "$MOUNT/config/urls" issuing_certificates="$ADDR/v1/$MOUNT/ca" crl_distribution_points="$ADDR/v1/$MOUNT/crl" >/dev/null
bao write "$MOUNT/issuer/default" leaf_not_after_behavior=truncate >/dev/null 2>&1 || true
for r in "$ROLE" "$ROLE_PSMDB"; do
  org=""; [ "$r" = "$ROLE_PSMDB" ] && org=PSMDB
  bao write "$MOUNT/roles/$r" organization="$org" allow_any_name=true enforce_hostnames=false allow_ip_sans=true \
    allow_wildcard_certificates=true server_flag=true client_flag=true key_type=any require_cn=false \
    ttl=2160h max_ttl="$LEAFMAX" >/dev/null
done
bao policy write "$POLICY" "$POLICY_FILE" >/dev/null
bao read -field=certificate "$MOUNT/cert/ca" | openssl x509 -noout -subject 2>/dev/null || true`

// ensureOpenBaoPKI sets up the PKI mount on a running, unsealed OpenBao node. Safe to call again.
func (a *App) ensureOpenBaoPKI(ctx context.Context, st Stack, baoCID string, cfg openbaoConfig, rootToken string, logln func(string)) (*openbaoPKIInfo, error) {
	mu, _ := openbaoPKIMu.LoadOrStore(baoCID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	env := append(baoClientEnv(cfg), "ROOT_TOKEN="+rootToken, "MOUNT="+openbaoPKIMount, "MAXTTL="+openbaoPKIMaxTTL)
	out, err := a.execScript(ctx, baoCID, openbaoPKIPrepScript, env)
	if err != nil {
		return nil, fmt.Errorf("prepare PKI mount %s: %w", openbaoPKIMount, err)
	}
	if strings.TrimSpace(out) != "READY" {
		intranetID := a.intranetContainerID(ctx, st)
		if intranetID == "" {
			return nil, fmt.Errorf("no Intranet node to take the CA from")
		}
		if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
			return nil, err
		}
		caCrt, cerr := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
		caKey, kerr := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
		if cerr != nil || kerr != nil || len(caCrt) == 0 || len(caKey) == 0 {
			return nil, fmt.Errorf("read the Intranet CA: %v %v", cerr, kerr)
		}
		bundle := append(append(append([]byte{}, caKey...), '\n'), caCrt...)
		if err := a.engCtx(ctx).CopyFile(ctx, baoCID, openbaoConfDir, "pki-ca-bundle.pem", 0o600, bundle); err != nil {
			return nil, fmt.Errorf("stage the CA bundle: %w", err)
		}
		if _, err := a.execScript(ctx, baoCID, openbaoPKIImportScript, append(env, "BUNDLE="+openbaoPKIBundle)); err != nil {
			return nil, fmt.Errorf("import the Intranet CA: %w", err)
		}
		logln("PKI: " + openbaoPKIMount + " signs as the Intranet CA (imported, not an intermediate — see openbaopki.go)")
	}

	policyFile := fmt.Sprintf("%s/policy-%s.hcl", openbaoConfDir, openbaoPKIPolicy)
	if err := a.engCtx(ctx).CopyFile(ctx, baoCID, openbaoConfDir, "policy-"+openbaoPKIPolicy+".hcl", 0o644, []byte(openbaoPKIPolicyHCL())); err != nil {
		return nil, fmt.Errorf("write the PKI policy: %w", err)
	}
	out, err = a.execScript(ctx, baoCID, openbaoPKIRoleScript, append(env,
		"ADDR="+cfg.Addr, "ROLE="+openbaoPKIRole, "ROLE_PSMDB="+openbaoPKIRolePSMDB, "LEAFMAX="+openbaoPKILeafMax,
		"POLICY="+openbaoPKIPolicy, "POLICY_FILE="+policyFile))
	if err != nil {
		return nil, fmt.Errorf("configure PKI role %s: %w", openbaoPKIRole, err)
	}
	info := &openbaoPKIInfo{Mount: openbaoPKIMount, Role: openbaoPKIRole, Policy: openbaoPKIPolicy,
		SignPath: openbaoPKIMount + "/sign/" + openbaoPKIRole, Subject: "the Intranet CA"}
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "subject="); ok {
			info.Subject = strings.TrimSpace(v) + " (the Intranet CA)"
		}
	}
	return info, nil
}

// openbaoAppRoleScript enables AppRole (once), makes a role bound to the PKI policy, and prints its
// role-id and a fresh secret-id. secret_id_ttl=0: the secret-id does not expire — see the header.
const openbaoAppRoleScript = `set -e
export BAO_TOKEN="$ROOT_TOKEN"
bao auth list -format=json | grep -q '"approle/"' || bao auth enable approle >/dev/null
bao write "auth/approle/role/$APPROLE" token_policies="$POLICY" token_ttl=20m token_max_ttl=1h \
  secret_id_ttl=0 secret_id_num_uses=0 >/dev/null
echo "ROLE_ID=$(bao read -field=role_id "auth/approle/role/$APPROLE/role-id")"
echo "SECRET_ID=$(bao write -f -field=secret_id "auth/approle/role/$APPROLE/secret-id")"`

// openbaoPKIAppRole returns the role-id and a new secret-id of an AppRole that may sign through
// the PKI role and nothing else.
func (a *App) openbaoPKIAppRole(ctx context.Context, baoCID string, cfg openbaoConfig, rootToken, name string) (roleID, secretID string, err error) {
	out, err := a.execScript(ctx, baoCID, openbaoAppRoleScript, append(baoClientEnv(cfg),
		"ROOT_TOKEN="+rootToken, "APPROLE="+name, "POLICY="+openbaoPKIPolicy))
	if err != nil {
		return "", "", fmt.Errorf("AppRole %s: %w", name, err)
	}
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "ROLE_ID="); ok {
			roleID = v
		} else if v, ok := strings.CutPrefix(strings.TrimSpace(l), "SECRET_ID="); ok {
			secretID = v
		}
	}
	if roleID == "" || secretID == "" {
		return "", "", fmt.Errorf("AppRole %s: OpenBao returned no role-id or secret-id", name)
	}
	return roleID, secretID, nil
}
