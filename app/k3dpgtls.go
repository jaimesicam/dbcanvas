package main

import (
	"context"
	"fmt"
	"strings"
)

// k3dpgtls.go — host names in a Percona Operator for PostgreSQL cluster's certificates.
//
// The PXC and PS operators append extra names to every certificate they ask cert-manager for
// (tls.SANs). The PG operator has no such field: it names its certificates after the in-cluster
// Services and nothing else. What it does take is whole certificates —
//
//	spec.secrets.customTLSSecret             the PostgreSQL server certificate (/pgconf/tls)
//	spec.secrets.customReplicationTLSSecret  the replication client certificate (CN _crunchyrepl);
//	                                         the operator requires both or neither, with one ca.crt
//	spec.proxy.pgBouncer.customTLSSecret     pgBouncer's certificate towards its clients
//
// — so with Host names on, DBCanvas has cert-manager issue those three from the frame's Issuer
// before cr.yaml is applied, carrying the in-cluster names the operator would have used and the
// published <service>.<domain> ones. Everything a client or the other members verify — the
// server, replication and pgBouncer certificates — then chains to the Intranet CA.
//
// Given a customTLSSecret, operator 3.1.0 leaves cert-manager out of the rest: the in-cluster
// Patroni REST and pgBackRest certificates are signed by its own CA (<cluster>-ca), not by the
// Issuer. Without Host names, tls.issuerConf alone has every certificate in the cluster issued
// from the Issuer, those two included.
//
// That is also why this needs the Intranet CA or OpenBao issuer: the operator's own CA is created
// with the cluster, after these certificates have to exist.

// pgCustomTLS names the Secrets cr.yaml points at.
type pgCustomTLS struct {
	Server, Replication, PGBouncer string
}

// pgCustomTLSNames are the Secret (and Certificate) names for a cluster.
func pgCustomTLSNames(cluster string) pgCustomTLS {
	return pgCustomTLS{
		Server:      cluster + "-dbcanvas-tls",
		Replication: cluster + "-dbcanvas-repl-tls",
		PGBouncer:   cluster + "-dbcanvas-pgbouncer-tls",
	}
}

// pgServiceDNSNames are a Service's in-cluster names, as the operator lists them
// (naming.ServiceDNSNames): bare, namespaced, .svc and fully qualified.
func pgServiceDNSNames(svc, ns string) []string {
	return []string{svc, svc + "." + ns, svc + "." + ns + ".svc", svc + "." + ns + ".svc.cluster.local"}
}

// pgCustomTLSManifest renders the three cert-manager Certificates. issuer is the namespaced Issuer
// the frame made (k3dProvisionIssuer).
func pgCustomTLSManifest(cluster, ns, domain, issuer string) string {
	n := pgCustomTLSNames(cluster)
	var server []string
	for _, svc := range []string{cluster + "-primary", cluster + "-replicas", cluster + "-ha"} {
		server = append(server, pgServiceDNSNames(svc, ns)...)
	}
	server = append(server, fqdnOf(cluster+"-ha", domain), fqdnOf(cluster+"-replicas", domain))
	bouncer := append(pgServiceDNSNames(cluster+"-pgbouncer", ns), fqdnOf(cluster+"-pgbouncer", domain))

	cert := func(name, cn string, dns []string, usages ...string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "apiVersion: cert-manager.io/v1\nkind: Certificate\nmetadata:\n  name: %s\n  namespace: %s\n", name, ns)
		b.WriteString("  labels:\n    app.kubernetes.io/managed-by: dbcanvas\n")
		fmt.Fprintf(&b, "spec:\n  secretName: %s\n  commonName: %s\n  dnsNames:\n", name, cn)
		for _, d := range dns {
			fmt.Fprintf(&b, "  - %s\n", d)
		}
		fmt.Fprintf(&b, "  issuerRef:\n    name: %s\n    kind: Issuer\n    group: cert-manager.io\n", issuer)
		b.WriteString("  privateKey:\n    algorithm: ECDSA\n    size: 256\n  usages:\n")
		for _, u := range usages {
			fmt.Fprintf(&b, "  - %s\n", u)
		}
		return b.String()
	}
	return strings.Join([]string{
		cert(n.Server, cluster+"-postgres", server, "server auth", "client auth", "digital signature", "key encipherment"),
		// The replication user's name is the CN, and the operator puts it in dnsNames too.
		cert(n.Replication, "_crunchyrepl", []string{"_crunchyrepl"}, "client auth", "digital signature", "key encipherment"),
		cert(n.PGBouncer, cluster+"-pgbouncer", bouncer, "server auth", "digital signature", "key encipherment"),
	}, "---\n")
}

// pgCustomTLSBlock renders spec.secrets (two spaces in, under spec).
func pgCustomTLSBlock(t *pgCustomTLS) string {
	return fmt.Sprintf("secrets:\n  customTLSSecret:\n    name: %s\n  customReplicationTLSSecret:\n    name: %s", t.Server, t.Replication)
}

// k3dPGCustomTLS issues the three certificates and waits for them, returning what cr.yaml points
// at. Fatal on failure, like the Issuer: cr.yaml naming a Secret that never appears leaves every
// pod waiting on a volume.
func (a *App) k3dPGCustomTLS(ctx context.Context, serverID string, cfg *k3dConfig, issuer string, pr *pxcProg) (*pgCustomTLS, error) {
	pr.phase("Issuing certificates with the host names", 87)
	ns, cluster := cfg.Namespace, cfg.ClusterName
	if err := a.kubectlApply(ctx, serverID, ns, []byte(pgCustomTLSManifest(cluster, ns, envOr("DOMAIN", "example.net"), issuer))); err != nil {
		return nil, fmt.Errorf("create the certificates: %w", err)
	}
	n := pgCustomTLSNames(cluster)
	for _, name := range []string{n.Server, n.Replication, n.PGBouncer} {
		if _, err := a.kubectl(ctx, serverID, "-n", ns, "wait", "certificate/"+name, "--for=condition=Ready", "--timeout=120s"); err != nil {
			why, _ := a.kubectl(ctx, serverID, "-n", ns, "get", "certificate/"+name, "-o", "jsonpath={.status.conditions[0].message}")
			return nil, fmt.Errorf("the certificate %s was never issued: %s", name, orDefault(strings.TrimSpace(why), err.Error()))
		}
	}
	pr.logln("certificates issued from " + k3dIssuerDescribe(issuer) + " with the host names: " +
		n.Server + " (PostgreSQL), " + n.Replication + " (replication), " + n.PGBouncer + " (pgBouncer)")
	return &n, nil
}
