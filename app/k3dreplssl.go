package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// k3dreplssl.go — SSL on a cross-cluster replication link between two PXC-operator clusters.
//
// The replica's channel takes three settings from its `configuration` (pxc_types.go,
// ReplicationChannelConfig), which the operator turns into one CHANGE REPLICATION SOURCE:
//
//	ssl: true            → SOURCE_SSL=1, SOURCE_SSL_CA=<ca>
//	sslSkipVerify: false → SOURCE_SSL_VERIFY_SERVER_CERT=1
//	ca: /etc/mysql/ssl-internal/ca.crt
//
// The `ca` is a path in the REPLICA's pod: its own `<cluster>-ssl-internal` Secret's ca.crt — the
// internal one, because the internal certificate is what the source serves on 3306 (see below), so
// its CA is the one that verifies it, even where the two Secrets carry the same CA. So SSL between
// two clusters only works when both clusters' certificates come from one CA — left to itself each
// operator makes a CA of its own, and the replica then refuses the source's certificate as signed
// by a stranger. That is true even with verification off: given a CA, the MySQL client still checks
// the chain, it only skips the host name.
//
// The one CA is the stack's Intranet CA. Every Kubernetes frame already waits for the Intranet (its
// CoreDNS forwards there), every other node already trusts it, and it is the same file for every
// frame — so two clusters deploying at the same time agree on it without having to coordinate.
//
// ------------------------------------------------------------------------------- which certificate
//
// mysqld serves the INTERNAL certificate on 3306, not the "external" one: the image's
// pxc-configure-pxc.sh points ssl-cert at /etc/mysql/ssl-internal whenever that secret is mounted.
// So the names a replica verifies against belong in `<cluster>-ssl-internal`. They go in both, as
// they would in a certificate made by hand, because ProxySQL serves the other one.
//
// With verification on, the source's certificate has to name what the replica dials. That is
// either a per-pod LoadBalancer address or the stack DNS name DBCanvas publishes for it
// (`<cluster>-pxc-0.<domain>`), so the certificate carries both: every address in the frame's
// MetalLB block as an IP SAN — the block is chosen before the operator is installed, the per-pod
// addresses are not, and the block is only eight — and the three pod names as DNS SANs. The
// replica dials the DNS names (k3dReplLink), which match whichever way the MySQL client compares.
//
// ------------------------------------------------------------------------------- two ways to make them
//
// The certificates exist before cr.yaml is applied, so the operator finds them and leaves them
// alone (reconcileSSL: both secrets present → nothing to create). How they are made follows the
// frame's cert-manager checkbox:
//
//   - cert-manager: the Intranet CA goes into the namespace as a Secret, a CA Issuer is made from
//     it, and DBCanvas applies the two Certificates itself — with ipAddresses, which the operator's
//     own `tls.SANs` cannot express (it appends SANs to dnsNames). `tls.issuerConf` then names the
//     same Issuer, so a certificate the operator ever has to make again comes from the same CA.
//   - no cert-manager: Percona's "Generate certificates manually" procedure — a CA, an external
//     and an internal server certificate, and the two Secrets — with the Intranet CA as the CA and
//     the certificates signed here rather than with cfssl.

const (
	// k3dReplSSLCA is the CA a replica verifies its source with: its own ssl-internal Secret's
	// ca.crt, where the operator mounts it. Not cr.yaml's commented example (/etc/mysql/ssl/ca.crt):
	// the source serves its INTERNAL certificate on 3306, so the internal CA is the one that matches
	// it — the same file today, since both Secrets come from the Intranet CA, but not by necessity.
	k3dReplSSLCA = "/etc/mysql/ssl-internal/ca.crt"
	// The operator's defaults for a channel's retry settings. It fills them in only when a channel
	// carries no `configuration` at all, so one that does — for SSL — has to state them too, or
	// they go to MySQL as 0.
	k3dReplRetryCount   = 3
	k3dReplConnectRetry = 60
	// k3dReplCAIssuer names both the Secret holding the Intranet CA and the cert-manager Issuer
	// made from it, in a cluster's namespace.
	k3dReplCAIssuer = "dbcanvas-intranet-ca"
	// k3dReplCertTTL is how long the certificates DBCanvas signs itself are valid. Manually made
	// certificates are not renewed by anything, so a lab's are made to outlast it.
	k3dReplCertTTL = 3 * 365 * 24 * time.Hour
)

// k3dReplTLS is what a frame needs for the replication links over SSL it takes part in.
type k3dReplTLS struct {
	On   bool // certificates from the Intranet CA, made before cr.yaml
	SANs bool // and they name the LoadBalancer addresses and DNS names a verifying replica dials
}

// k3dReplTLSFor works out a frame's certificates from the links it is on. The SSL choice is made on
// the replica's frame, but it decides the source's certificates too: the replica verifies them.
func k3dReplTLSFor(doc designDoc, frame designFrame) k3dReplTLS {
	var t k3dReplTLS
	for _, l := range k3dReplLinks(doc) {
		if !l.Dst.K3DReplSSL || (l.Src.ID != frame.ID && l.Dst.ID != frame.ID) {
			continue
		}
		t.On = true
		if l.Src.ID == frame.ID && !l.Dst.K3DReplSSLSkipVerify {
			t.SANs = true
		}
	}
	return t
}

// k3dReplVerifies reports whether a link's replica checks the source's certificate against the host
// it dials — which is what makes it dial DNS names the certificate carries.
func k3dReplVerifies(l k3dReplLink) bool { return l.Dst.K3DReplSSL && !l.Dst.K3DReplSSLSkipVerify }

// k3dReplPodHost is the stack DNS name published for one of a source cluster's database pods.
func k3dReplPodHost(cluster string, i int) string { return fmt.Sprintf("%s-pxc-%d", cluster, i) }

// k3dReplSSLDescribe is the replica's channel in words, for the log and the panel.
func k3dReplSSLDescribe(l k3dReplLink) string {
	switch {
	case !l.Dst.K3DReplSSL:
		return ""
	case l.Dst.K3DReplSSLSkipVerify:
		return "SSL, certificate not verified (sslSkipVerify)"
	default:
		return "SSL, source certificate verified against the Intranet CA and its host name"
	}
}

// k3dReplChannelConfig is the replica channel's `configuration` for a link over SSL, nil without.
func k3dReplChannelConfig(l k3dReplLink) map[string]any {
	if !l.Dst.K3DReplSSL {
		return nil
	}
	return map[string]any{
		"sourceRetryCount":   k3dReplRetryCount,
		"sourceConnectRetry": k3dReplConnectRetry,
		"ssl":                true,
		"sslSkipVerify":      l.Dst.K3DReplSSLSkipVerify,
		"ca":                 k3dReplSSLCA,
	}
}

// k3dReplPoolIPs lists every address in a MetalLB range ("a.b.c.d-a.b.c.e").
func k3dReplPoolIPs(pool string) []net.IP {
	lo, hi, ok := parseRange(pool)
	if !ok || hi < lo || hi-lo > 255 {
		return nil
	}
	var out []net.IP
	for v := lo; v <= hi; v++ {
		out = append(out, u32ToIP(v))
	}
	return out
}

// k3dReplCertSpec is one of the two certificates: its Secret, common name and names.
type k3dReplCertSpec struct {
	Secret string
	CN     string
	DNS    []string
	IPs    []net.IP
}

// k3dReplCertSpecs are a cluster's two certificates. The host lists are the manual procedure's
// joined with what the operator itself puts in them (tls.go, createSSLManualy), so nothing that
// reaches the cluster by an in-cluster name loses a match by DBCanvas having made the certificate;
// then the replication names when the replica verifies.
func k3dReplCertSpecs(cluster, ns, domain, pool string, sans bool) (external, internal k3dReplCertSpec, extraDNS []string) {
	external = k3dReplCertSpec{Secret: cluster + "-ssl", CN: cluster + "-pxc", DNS: []string{
		cluster + "-pxc", "*." + cluster + "-pxc",
		cluster + "-proxysql", "*." + cluster + "-proxysql", "*." + cluster + "-proxysql-unready",
	}}
	internal = k3dReplCertSpec{Secret: cluster + "-ssl-internal", CN: cluster + "-pxc-internal", DNS: []string{
		cluster + "-pxc", "*." + cluster + "-pxc",
		cluster + "-haproxy-replicas." + ns + ".svc.cluster.local", cluster + "-haproxy-replicas." + ns, cluster + "-haproxy-replicas",
		cluster + "-haproxy." + ns + ".svc.cluster.local", cluster + "-haproxy." + ns, cluster + "-haproxy",
	}}
	if sans {
		for i := 0; i < k3dReplPerPodServices; i++ {
			extraDNS = append(extraDNS, fqdnOf(k3dReplPodHost(cluster, i), domain))
		}
		ips := k3dReplPoolIPs(pool)
		for _, c := range []*k3dReplCertSpec{&external, &internal} {
			c.DNS = append(c.DNS, extraDNS...)
			c.IPs = ips
		}
	}
	return external, internal, extraDNS
}

// k3dReplProvisionTLS makes the cluster's certificates from the Intranet CA, before cr.yaml. It
// returns what cr.yaml's `spec.tls` should then say (cert-manager only). Fatal on failure: an SSL
// link between clusters with two different CAs does not connect, and the place to find that out is
// here, not as an IO thread error at the end of a forty-minute deploy.
//
// issuer is the cert-manager Issuer the frame already set up for all its certificates
// (k3dissuer.go: the Intranet CA, directly or signing through OpenBao), "" when it has none. When it
// is set the replication certificates come from it too; both chain to the Intranet CA, so a
// replica verifies the source whichever of the two each cluster uses.
func (a *App) k3dReplProvisionTLS(ctx context.Context, st Stack, frame designFrame, doc designDoc,
	serverID string, cfg *k3dConfig, t k3dReplTLS, frameIssuer string, pr *pxcProg) (issuer string, sans []string, err error) {

	pr.phase("Issuing replication certificates", 84)
	intranetID := a.intranetContainerID(ctx, st)
	if intranetID == "" {
		return "", nil, fmt.Errorf("no Intranet node — its CA is the one both clusters share")
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return "", nil, err
	}
	caCrt, cerr := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	caKey, kerr := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if cerr != nil || kerr != nil || len(caCrt) == 0 || len(caKey) == 0 {
		return "", nil, fmt.Errorf("read the Intranet CA: %v %v", cerr, kerr)
	}
	if t.SANs && cfg.MetalLBRange == "" {
		// Without the block there are no addresses to name, and the replica dials by DNS name
		// anyway — so this is said, not fatal.
		pr.logln("replication certificates: no MetalLB range was recorded, so they name the DNS hosts only")
	}
	domain := envOr("DOMAIN", "example.net")
	external, internal, extraDNS := k3dReplCertSpecs(cfg.ClusterName, cfg.Namespace, domain, cfg.MetalLBRange, t.SANs)
	recorded := append([]string{}, extraDNS...)
	if t.SANs && cfg.MetalLBRange != "" {
		recorded = append(recorded, cfg.MetalLBRange)
	}

	if cfg.CertManager != "" {
		issuer, what := frameIssuer, k3dIssuerDescribe(frameIssuer)
		if issuer == "" {
			if err := a.k3dIntranetCAIssuer(ctx, serverID, cfg.Namespace, caCrt, caKey); err != nil {
				return "", nil, err
			}
			issuer, what = k3dReplCAIssuer, "the Intranet CA"
		}
		if err := a.k3dApplyCertificates(ctx, serverID, cfg.Namespace, issuer, external, internal); err != nil {
			return "", nil, err
		}
		cfg.TLSCerts = "cert-manager, Issuer " + issuer + " (" + what + ")"
		cfg.TLSSANs = recorded
		pr.logln("replication certificates: " + external.Secret + " and " + internal.Secret +
			" issued by cert-manager from " + what + " (Issuer " + issuer + ")" + k3dReplSANsNote(recorded))
		return issuer, extraDNS, nil
	}
	if frame.K3DCertManager {
		pr.logln("replication certificates: cert-manager did not install, so they are made by hand instead")
	}
	for _, c := range []k3dReplCertSpec{external, internal} {
		crt, key, err := issueTLSCert(caCrt, caKey, c.CN, c.DNS, c.IPs, k3dReplCertTTL,
			[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth})
		if err != nil {
			return "", nil, fmt.Errorf("sign %s: %w", c.Secret, err)
		}
		if err := a.kubectlApply(ctx, serverID, cfg.Namespace, k3dReplTLSSecret(c.Secret, caCrt, crt, key)); err != nil {
			return "", nil, fmt.Errorf("create %s: %w", c.Secret, err)
		}
	}
	cfg.TLSCerts = "made by DBCanvas from the Intranet CA (manual certificates)"
	cfg.TLSSANs = recorded
	pr.logln("replication certificates: " + external.Secret + " and " + internal.Secret +
		" signed by the Intranet CA, as in the operator's manual-certificate procedure" + k3dReplSANsNote(recorded))
	return "", nil, nil
}

func k3dReplSANsNote(sans []string) string {
	if len(sans) == 0 {
		return ""
	}
	return " — also naming " + strings.Join(sans, ", ")
}

// k3dReplTLSSecret is a kubernetes.io/tls Secret in the shape the operator mounts: ca.crt, tls.crt
// and tls.key.
func k3dReplTLSSecret(name string, ca, crt, key []byte) []byte {
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
		"metadata": map[string]any{"name": name},
		"data": map[string]string{
			"ca.crt":  base64.StdEncoding.EncodeToString(ca),
			"tls.crt": base64.StdEncoding.EncodeToString(crt),
			"tls.key": base64.StdEncoding.EncodeToString(key),
		},
	})
	return b
}

// k3dIntranetCAIssuer puts the Intranet CA into the namespace as a Secret and makes a CA Issuer of
// it (k3dReplCAIssuer). Applying it again changes nothing.
func (a *App) k3dIntranetCAIssuer(ctx context.Context, serverID, ns string, caCrt, caKey []byte) error {
	if err := a.kubectlApply(ctx, serverID, ns, k3dReplTLSSecret(k3dReplCAIssuer, caCrt, caCrt, caKey)); err != nil {
		return fmt.Errorf("create the CA secret %s: %w", k3dReplCAIssuer, err)
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(k3dReplIssuerManifest())); err != nil {
		return fmt.Errorf("create the Issuer %s: %w", k3dReplCAIssuer, err)
	}
	return nil
}

// k3dApplyCertificates has cert-manager issue certificates from an Issuer, then waits for each.
func (a *App) k3dApplyCertificates(ctx context.Context, serverID, ns, issuer string, certs ...k3dReplCertSpec) error {
	var docs []string
	for _, c := range certs {
		docs = append(docs, k3dReplCertificateManifest(c, issuer))
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(strings.Join(docs, "---\n"))); err != nil {
		return fmt.Errorf("create the Certificates: %w", err)
	}
	for _, c := range certs {
		if _, err := a.kubectl(ctx, serverID, "-n", ns, "wait", "certificate/"+c.Secret,
			"--for=condition=Ready", "--timeout=180s"); err != nil {
			return fmt.Errorf("certificate %s never became ready: %w", c.Secret, err)
		}
	}
	return nil
}

func k3dReplIssuerManifest() string {
	return `apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: ` + k3dReplCAIssuer + `
spec:
  ca:
    secretName: ` + k3dReplCAIssuer + `
`
}

// k3dReplCertificateManifest is one Certificate, named as the operator names its own (`<cluster>-ssl`,
// `<cluster>-ssl-internal`) so that the cluster reads as cert-manager-managed the usual way.
func k3dReplCertificateManifest(c k3dReplCertSpec, issuer string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: cert-manager.io/v1\nkind: Certificate\nmetadata:\n  name: %s\nspec:\n", c.Secret)
	fmt.Fprintf(&b, "  secretName: %s\n  commonName: %s\n  duration: %s\n", c.Secret, c.CN, k3dReplCertTTL)
	b.WriteString("  usages:\n  - digital signature\n  - key encipherment\n  - server auth\n  - client auth\n")
	b.WriteString("  dnsNames:\n")
	for _, d := range c.DNS {
		fmt.Fprintf(&b, "  - %q\n", d)
	}
	if len(c.IPs) > 0 {
		b.WriteString("  ipAddresses:\n")
		for _, ip := range c.IPs {
			fmt.Fprintf(&b, "  - %s\n", ip)
		}
	}
	fmt.Fprintf(&b, "  issuerRef:\n    name: %s\n    kind: Issuer\n    group: cert-manager.io\n", issuer)
	return b.String()
}

// k3dReplPublishHosts records a source's per-pod addresses under their DNS names and republishes the
// stack zone, returning the names to dial in address order.
func (a *App) k3dReplPublishHosts(ctx context.Context, st Stack, doc designDoc, src designFrame, cluster string, byPod map[string]string) []string {
	hosts := map[string]string{}
	for pod, ip := range byPod {
		hosts[pod] = ip
	}
	a.k3dReplSaveConfig(st, doc, src, func(c *k3dConfig) { c.ReplHosts = hosts })
	a.reconcileStackDNS(ctx, st.ID)
	domain := envOr("DOMAIN", "example.net")
	var names []string
	for pod := range hosts {
		names = append(names, fqdnOf(pod, domain))
	}
	sort.Strings(names)
	return names
}

// k3dPublishedHosts is every name a K3D member's config asks the stack DNS to publish: a
// replication source's per-pod addresses and the cluster's host names (k3dtlshosts.go).
func k3dPublishedHosts(cfg k3dConfig) map[string]string {
	out := map[string]string{}
	for h, ip := range cfg.TLSHosts {
		out[h] = ip
	}
	for h, ip := range cfg.ReplHosts {
		out[h] = ip
	}
	return out
}

// k3dReplDNSRecords are the stack DNS records for the replication sources' per-pod addresses, read
// off the recorded config of every K3D member (each carries the same map, so names are deduplicated).
func k3dReplDNSRecords(deps []Deployment, typeByID map[string]string) []dnsRecord {
	seen := map[string]bool{}
	var out []dnsRecord
	for _, d := range deps {
		if typeByID[d.NodeID] != "k3d" {
			continue
		}
		var cfg k3dConfig
		if json.Unmarshal(d.Config, &cfg) != nil {
			continue
		}
		for host, ip := range k3dPublishedHosts(cfg) {
			if seen[host] || net.ParseIP(ip) == nil {
				continue
			}
			seen[host] = true
			out = append(out, dnsRecord{host: host, ip: ip})
		}
	}
	return out
}
