package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// k3dissuer.go — which CA a K3D frame's cert-manager issues the database certificates from.
//
// With cert-manager installed and nothing else said, each Percona operator makes a self-signed CA
// of its own per cluster and issues from it. The frame can instead name the CA, and the operator
// is pointed at it through cr.yaml's `tls.issuerConf`:
//
//	intranet  a CA Issuer holding the Intranet CA — the stack's own root, which every node, pod and
//	          the VNC desktop already trust (the same Issuer replication over SSL uses).
//	openbao   a Vault Issuer signing through the OpenBao node's PKI mount, which holds the Intranet
//	          CA (openbaopki.go — and why not an intermediate), logging in with an AppRole of the
//	          cluster's own.
//
// Both chain to the Intranet CA, so ca.crt in every Secret is that one root either way, and two
// clusters replicating over SSL verify each other whichever each one picked.
//
// ------------------------------------------------------------------- which operators, which versions
//
// The PXC, PS and PSMDB operators all read `tls.issuerConf` and, given one, issue their leaf
// certificates from it instead of making a CA. Verified against each operator's source, because
// what they do with an issuer they did not make differs:
//
//	PXC    createSSLByCertManager uses the named issuer as it is, and skips its CA rotation
//	       entirely when issuerConf is set. Every release in the catalog (1.7.0+) has the field.
//	PS     ensureSSLByCertManager looks the issuer up as a namespaced Issuer and uses it as it is.
//	PSMDB  only from 1.23.0. Before it, ApplyIssuer createOrUpdate()s an Issuer under the
//	       issuerConf name with the operator's own CA spec — overwriting the one DBCanvas made.
//	       From 1.23.0 IsUserIssuer leaves an issuer the operator did not label as its own alone.
//
// cr.yaml ships the tls section live (PXC 1.15.0+) or commented out (older PXC, every PS and
// PSMDB), so crTLSSection inserts whichever is missing. A release whose cr.yaml does not mention
// issuerConf at all (PS 0.1.0) is told so in the log and left to self-sign.
//
// The Issuer is namespaced and lives in the cluster's namespace: PS only looks up kind Issuer, and
// one per cluster keeps one cluster's AppRole out of another's namespace.

const (
	k3dIssuerIntranet = "intranet"
	k3dIssuerOpenBao  = "openbao"
	// k3dOpenBaoIssuer names the Vault Issuer, and k3dOpenBaoAppRoleSecret the Secret holding its
	// AppRole secret-id.
	k3dOpenBaoIssuer        = "dbcanvas-openbao"
	k3dOpenBaoAppRoleSecret = "dbcanvas-openbao-approle"
	// psmdbUserIssuerMinVer is the first PSMDB operator that leaves a user's Issuer alone.
	psmdbUserIssuerMinVer = "1.23.0"
)

// k3dIssuerOperators are the operators that take `tls.issuerConf`.
var k3dIssuerOperators = map[string]bool{"pxc": true, "ps": true, "psmdb": true}

// k3dIssuerOf is the issuer a frame asks for: "" (the operator's own CA), "intranet" or "openbao".
// Only meaningful with cert-manager installed on an operator that reads issuerConf.
func k3dIssuerOf(f designFrame) string {
	if !f.K3DCertManager || !k3dIssuerOperators[strings.TrimSpace(f.K3DOperator)] {
		return ""
	}
	switch strings.TrimSpace(f.K3DCertIssuer) {
	case k3dIssuerIntranet:
		return k3dIssuerIntranet
	case k3dIssuerOpenBao:
		return k3dIssuerOpenBao
	}
	return ""
}

// k3dIssuerDescribe says in words where an Issuer's certificates come from.
func k3dIssuerDescribe(issuer string) string {
	switch issuer {
	case k3dOpenBaoIssuer:
		return "the Intranet CA, signing through OpenBao's PKI"
	case k3dReplCAIssuer:
		return "the Intranet CA"
	}
	return issuer
}

// k3dOpenBaoNodeOf is the OpenBao node a frame uses: the one it names, else the stack's (a
// per-stack singleton, so there is at most one to find).
func k3dOpenBaoNodeOf(f designFrame, doc designDoc) string {
	if f.OpenBaoNodeID != "" {
		return f.OpenBaoNodeID
	}
	for _, n := range doc.Nodes {
		if n.Type == "openbao" {
			return n.ID
		}
	}
	return ""
}

// k3dIssuerIssues validates a frame's issuer choice.
func k3dIssuerIssues(f designFrame, doc designDoc, opCat OperatorCatalog, deployed bool) []issue {
	want := strings.TrimSpace(f.K3DCertIssuer)
	if want == "" {
		return nil
	}
	lvl, tail := "error", ""
	if deployed {
		lvl, tail = "warning", " (this cluster is already running, so nothing here is re-applied — destroy the frame to change it)"
	}
	name, op := f.Label, strings.TrimSpace(f.K3DOperator)
	switch {
	case !f.K3DCertManager:
		return []issue{{Level: "warning", Message: "K3D cluster " + name + " names a certificate issuer but does not install cert-manager, " +
			"so the operator issues its own certificates — tick Install cert-manager, or clear the issuer"}}
	case !k3dIssuerOperators[op]:
		return []issue{{Level: "warning", Message: "K3D cluster " + name + " names a certificate issuer, which " +
			orDefault(k3dOperatorLabel(op), "no operator") + " does not read here — only the PXC, Percona Server and MongoDB operators take tls.issuerConf"}}
	}
	var out []issue
	if want == k3dIssuerOpenBao && k3dOpenBaoNodeOf(f, doc) == "" {
		out = append(out, issue{Level: lvl, Message: "K3D cluster " + name +
			" issues its certificates through OpenBao's PKI, but the stack has no OpenBao node. Add one" + tail})
	}
	if op == "psmdb" {
		ver, ok := opCat.resolveOperatorVersion("psmdb", f.K3DOperatorVer)
		if !ok || compareVersions(ver, psmdbUserIssuerMinVer) < 0 {
			out = append(out, issue{Level: lvl, Message: "K3D cluster " + name + " names a certificate issuer, which the Percona Operator for MongoDB " +
				"only leaves alone from " + psmdbUserIssuerMinVer + " — older releases overwrite an Issuer of that name with their own CA. " +
				"Choose " + psmdbUserIssuerMinVer + " or newer, or let the operator issue its own" + tail})
		}
	}
	return out
}

// k3dProvisionIssuer makes the frame's Issuer in the cluster's namespace and waits for it to be
// Ready, returning its name ("" when the frame asked for none, or cert-manager is not there).
// Fatal on failure: the operator, told to use an Issuer that does not work, issues nothing at all
// and the cluster never forms — better said here than as pods waiting on a Secret.
func (a *App) k3dProvisionIssuer(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID string, cfg *k3dConfig, pr *pxcProg) (string, error) {
	want := k3dIssuerOf(frame)
	if want == "" {
		return "", nil
	}
	if cfg.CertManager == "" {
		pr.logln("certificate issuer " + want + " skipped: cert-manager is not installed, so the operator issues its own")
		return "", nil
	}
	pr.phase("Creating the certificate issuer", 80)
	ns := cfg.Namespace

	caCrt, caKey, err := a.k3dReadIntranetCA(ctx, st)
	if err != nil {
		return "", err
	}
	var name string
	switch want {
	case k3dIssuerIntranet:
		if err := a.k3dIntranetCAIssuer(ctx, serverID, ns, caCrt, caKey); err != nil {
			return "", err
		}
		name = k3dReplCAIssuer
		cfg.CertIssuer = "cert-manager CA Issuer " + name + " — the Intranet CA"

	case k3dIssuerOpenBao:
		baoCfg, rootToken, baoCID, err := a.waitOpenBaoReady(ctx, st.ID, k3dOpenBaoNodeOf(frame, doc), deployTimeout())
		if err != nil {
			return "", err
		}
		info, err := a.ensureOpenBaoPKI(ctx, st, baoCID, baoCfg, rootToken, pr.logln)
		if err != nil {
			return "", err
		}
		a.recordOpenBaoPKI(st.ID, k3dOpenBaoNodeOf(frame, doc), info)
		appRole := "cert-manager-" + hostLabel(cfg.Cluster)
		roleID, secretID, err := a.openbaoPKIAppRole(ctx, baoCID, baoCfg, rootToken, appRole)
		if err != nil {
			return "", err
		}
		caBundle := ""
		if baoCfg.TLS {
			caBundle = base64.StdEncoding.EncodeToString(caCrt)
		}
		signPath := openbaoPKISignPath(cfg.Operator)
		if err := a.kubectlApply(ctx, serverID, ns, []byte(k3dOpenBaoIssuerManifest(baoCfg.Addr, signPath, caBundle, roleID, secretID))); err != nil {
			return "", fmt.Errorf("create the Issuer %s: %w", k3dOpenBaoIssuer, err)
		}
		name = k3dOpenBaoIssuer
		cfg.CertIssuer = "cert-manager Vault Issuer " + name + " — OpenBao " + baoCfg.Addr + ", " + signPath +
			" (signing as the Intranet CA), AppRole " + appRole
		if !baoCfg.TLS {
			pr.logln("certificate issuer: OpenBao has SSL off, so cert-manager reaches it over plain HTTP")
		}
	}

	if _, err := a.kubectl(ctx, serverID, "-n", ns, "wait", "issuer/"+name, "--for=condition=Ready", "--timeout=120s"); err != nil {
		why, _ := a.kubectl(ctx, serverID, "-n", ns, "get", "issuer/"+name, "-o", "jsonpath={.status.conditions[0].message}")
		return "", fmt.Errorf("the Issuer %s never became ready: %s", name, orDefault(strings.TrimSpace(why), err.Error()))
	}
	pr.logln("certificate issuer: " + cfg.CertIssuer)
	return name, nil
}

// k3dReadIntranetCA reads the stack CA's certificate and key off the Intranet node.
func (a *App) k3dReadIntranetCA(ctx context.Context, st Stack) (crt, key []byte, err error) {
	intranetID := a.intranetContainerID(ctx, st)
	if intranetID == "" {
		return nil, nil, fmt.Errorf("no Intranet node — its CA is the one the certificates chain to")
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return nil, nil, err
	}
	crt, cerr := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	key, kerr := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if cerr != nil || kerr != nil || len(crt) == 0 || len(key) == 0 {
		return nil, nil, fmt.Errorf("read the Intranet CA: %v %v", cerr, kerr)
	}
	return crt, key, nil
}

// recordOpenBaoPKI notes on the OpenBao node's deployment that its PKI mount exists, so its panel
// shows it even when it was a K3D frame, not the node's own option, that created it.
func (a *App) recordOpenBaoPKI(stackID int64, nodeID string, info *openbaoPKIInfo) {
	dep, err := a.store.GetDeployment(stackID, nodeID)
	if err != nil {
		return
	}
	var cfg openbaoConfig
	if json.Unmarshal(dep.Config, &cfg) != nil || cfg.PKI != nil {
		return
	}
	cfg.PKI = info
	dep.Config, _ = json.Marshal(cfg)
	a.store.UpsertDeployment(dep)
}

// k3dOpenBaoIssuerManifest renders the AppRole Secret and the Vault Issuer. caBundle is the
// base64 Intranet CA OpenBao's listener is verified with ("" when it serves plain HTTP).
func k3dOpenBaoIssuerManifest(addr, signPath, caBundle, roleID, secretID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\ntype: Opaque\nstringData:\n  secretId: %q\n---\n",
		k3dOpenBaoAppRoleSecret, secretID)
	fmt.Fprintf(&b, "apiVersion: cert-manager.io/v1\nkind: Issuer\nmetadata:\n  name: %s\nspec:\n  vault:\n", k3dOpenBaoIssuer)
	fmt.Fprintf(&b, "    server: %s\n    path: %s\n", addr, signPath)
	if caBundle != "" {
		fmt.Fprintf(&b, "    caBundle: %s\n", caBundle)
	}
	fmt.Fprintf(&b, "    auth:\n      appRole:\n        path: approle\n        roleId: %q\n        secretRef:\n          name: %s\n          key: secretId\n",
		roleID, k3dOpenBaoAppRoleSecret)
	return b.String()
}

// k3dCRIssuer points a rewritten cr.yaml at the frame's Issuer and adds host names to its
// certificates, each only when the operator release's own cr.yaml (raw) knows the field — one that
// does not would have its CRD reject the whole resource. sans is empty for MongoDB, which names
// its hosts through split horizons instead (psmdbTransform).
func k3dCRIssuer(cr, raw, issuer string, sans []string, cfg *k3dConfig, logln func(string), extra ...string) string {
	if issuer != "" && !strings.Contains(raw, "issuerConf") {
		logln("certificate issuer NOT used: operator " + cfg.OperatorVer + "'s cr.yaml has no tls.issuerConf, so it issues its own certificates")
		cfg.CertIssuer, issuer = "", ""
	}
	if len(sans) > 0 {
		if strings.Contains(raw, "SANs") {
			cfg.TLSHostsInCert = true
		} else {
			logln("host names NOT added to the certificates: operator " + cfg.OperatorVer + "'s cr.yaml has no tls.SANs — they are still published in DNS")
			sans = nil
		}
	}
	return crTLSSection(cr, issuer, sans, extra...)
}

// crTLSSection writes `spec.tls.issuerConf` (a namespaced Issuer) and `spec.tls.SANs` into a
// cr.yaml. Into the live section when there is one — leaving alone a key it already carries live,
// which crTransform's replication path may have written — or else into one created above the
// shipped commented-out example (or straight under `spec:`), with the extra lines (e.g. PSMDB's
// `mode: preferTLS`) that only a section being created needs.
func crTLSSection(src, issuer string, sans []string, extra ...string) string {
	if issuer == "" && len(sans) == 0 {
		return src
	}
	keys := func(hasIssuer, hasSANs bool) []string {
		var out []string
		if len(sans) > 0 && !hasSANs {
			out = append(out, "    SANs:")
			for _, n := range sans {
				out = append(out, "    - "+n)
			}
		}
		if issuer != "" && !hasIssuer {
			out = append(out, "    issuerConf:", "      name: "+issuer, "      kind: Issuer", "      group: cert-manager.io")
		}
		return out
	}
	lines := strings.Split(src, "\n")

	// A live `tls:` under spec: insert after it what it does not already carry.
	for i, ln := range lines {
		if ln != "  tls:" {
			continue
		}
		hasIssuer, hasSANs := false, false
		for _, next := range lines[i+1:] {
			ind, commented, body := crLine(next)
			if body == "" || commented {
				continue
			}
			if ind <= 2 {
				break
			}
			hasIssuer = hasIssuer || (ind == 4 && body == "issuerConf:")
			hasSANs = hasSANs || (ind == 4 && body == "SANs:")
		}
		out := append(append(append([]string{}, lines[:i+1]...), keys(hasIssuer, hasSANs)...), lines[i+1:]...)
		return strings.Join(out, "\n")
	}

	block := []string{"  tls:"}
	for _, e := range extra {
		block = append(block, "    "+e)
	}
	block = append(block, keys(false, false)...)
	at := -1
	for i, ln := range lines {
		if strings.TrimRight(ln, " ") == "#  tls:" {
			at = i
			break
		}
	}
	if at < 0 {
		for i, ln := range lines {
			if ln == "spec:" {
				at = i + 1
				break
			}
		}
	}
	if at < 0 {
		return src
	}
	out := append(append(append([]string{}, lines[:at]...), block...), lines[at:]...)
	return strings.Join(out, "\n")
}
