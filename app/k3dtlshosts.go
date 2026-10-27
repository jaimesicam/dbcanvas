package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// k3dtlshosts.go — host names for a K3D cluster's endpoints, in its certificates and in the stack DNS.
//
// A client that verifies the server's identity (--ssl-mode=VERIFY_IDENTITY, sslmode=verify-full,
// tlsAllowInvalidHostnames=false) needs two things to agree: the name it dials has to resolve to the
// endpoint, and the certificate the endpoint serves has to carry that name. The operators put only
// in-cluster names in their certificates (cluster1-haproxy, *.cluster1-pxc, …), which nothing
// outside Kubernetes resolves. So the frame's "Host names" option does both halves:
//
//	certificates  at design time, before cr.yaml is applied — `tls.SANs` for PXC and Percona Server
//	              (the operator appends them to every certificate it asks cert-manager for), and for
//	              MongoDB a replica set split horizon per pod, whose names the operator adds to the
//	              certificate from 1.22.0 (GetCertificateSans) and which mongod also advertises to a
//	              client that connected by one of them.
//	DNS           once the cluster is ready — every LoadBalancer Service the cluster owns is published
//	              as <service>.<domain>, an A record and its PTR (the MetalLB block is carved out of
//	              the stack network, so the reverse lands in the stack's own reverse zone).
//
// The certificate names are the candidates a cluster MAY expose — every Service the operator would
// create with a LoadBalancer for any expose setting — rather than the ones it will: a name nothing
// serves under costs nothing in a certificate, and predicting the operator's Service set exactly
// for every release is a guess this does not need to make. DNS is the other way round: only what
// actually has an address is published, so a name resolves only when it reaches something. The
// panel lists the published names and says which of them the certificates carry.
//
// MongoDB's sharded routers are the one gap: the operator has no field that adds a name to the
// mongos certificate, so <cluster>-mongos is published in DNS and said not to be in the
// certificate.

// psmdbHorizonMinVer is the first PSMDB operator that puts split-horizon names in its certificates.
const psmdbHorizonMinVer = "1.22.0"

// k3dTLSHostOperators are the operators this file can name.
var k3dTLSHostOperators = map[string]bool{"pxc": true, "ps": true, "psmdb": true}

// k3dTLSHostsOn reports whether a frame asks for host names (cert-manager on, an operator that
// takes them).
func k3dTLSHostsOn(f designFrame) bool {
	return f.K3DTLSHosts && f.K3DCertManager && k3dTLSHostOperators[strings.TrimSpace(f.K3DOperator)]
}

// k3dTLSHostCandidates are the Services a cluster may expose, by name. Per-pod names assume the
// three members cr.yaml ships with — DBCanvas does not change a cluster's size.
func k3dTLSHostCandidates(operator, cluster string, sharding bool) []string {
	pods := func(stem string) []string {
		var out []string
		for i := 0; i < 3; i++ {
			out = append(out, fmt.Sprintf("%s-%s-%d", cluster, stem, i))
		}
		return out
	}
	switch operator {
	case "pxc":
		return append([]string{cluster + "-haproxy", cluster + "-haproxy-replicas", cluster + "-proxysql"}, pods("pxc")...)
	case "ps":
		return append([]string{cluster + "-mysql-primary", cluster + "-haproxy", cluster + "-router"}, pods("mysql")...)
	case "psmdb":
		if sharding {
			return nil // see the header: no field names the mongos certificate
		}
		return pods("rs0")
	}
	return nil
}

// k3dTLSHostSANs are the candidates as fully qualified names.
func k3dTLSHostSANs(operator, cluster, domain string, sharding bool) []string {
	var out []string
	for _, h := range k3dTLSHostCandidates(operator, cluster, sharding) {
		out = append(out, fqdnOf(h, domain))
	}
	return out
}

// psmdbSplitHorizons renders a replica set's `splitHorizons:` — one `external` name per pod.
func psmdbSplitHorizons(cluster, domain string) string {
	var b strings.Builder
	b.WriteString("splitHorizons:")
	for _, pod := range k3dTLSHostCandidates("psmdb", cluster, false) {
		fmt.Fprintf(&b, "\n  %s:\n    external: %s", pod, fqdnOf(pod, domain))
	}
	return b.String()
}

// k3dTLSHostIssues validates the option.
func k3dTLSHostIssues(f designFrame, doc designDoc, opCat OperatorCatalog, deployed bool) []issue {
	if !f.K3DTLSHosts {
		return nil
	}
	name, op := f.Label, strings.TrimSpace(f.K3DOperator)
	switch {
	case !f.K3DCertManager:
		return []issue{{Level: "warning", Message: "K3D cluster " + name + " asks for host names in its certificates, which is a cert-manager setting — tick Install cert-manager, or clear it"}}
	case !k3dTLSHostOperators[op]:
		return []issue{{Level: "warning", Message: "K3D cluster " + name + " asks for host names in its certificates, which only the PXC, Percona Server and MongoDB operators take here"}}
	}
	lvl, tail := "error", ""
	if deployed {
		lvl, tail = "warning", " (this cluster is already running, so nothing here is re-applied — destroy the frame to change it)"
	}
	var out []issue
	// Two clusters of one name would publish the same host names.
	for _, o := range doc.Frames {
		if o.ID != f.ID && o.Type == "k3d" && k3dTLSHostsOn(o) && k3dCRName(o) == k3dCRName(f) {
			out = append(out, issue{Level: lvl, Message: "K3D clusters " + name + " and " + o.Label + " would both publish host names as " +
				k3dCRName(f) + "-…; rename one of them" + tail})
			break
		}
	}
	if op == "psmdb" {
		ver, ok := opCat.resolveOperatorVersion("psmdb", f.K3DOperatorVer)
		switch {
		case f.K3DSharding:
			out = append(out, issue{Level: "warning", Message: "K3D cluster " + name + " is sharded: its mongos routers are published in DNS, but the " +
				"MongoDB operator has no setting that adds a name to their certificate, so a client verifying the host name will refuse them"})
		case !ok || compareVersions(ver, psmdbHorizonMinVer) < 0:
			out = append(out, issue{Level: "warning", Message: "K3D cluster " + name + " asks for host names in its certificates, which the MongoDB operator " +
				"only adds (as split-horizon names) from " + psmdbHorizonMinVer + " — the names are published in DNS, but not in the certificate"})
		case !strings.EqualFold(f.K3DExposeReplset, "loadbalancer"):
			out = append(out, issue{Level: "warning", Message: "K3D cluster " + name + " asks for host names, but its replica set is not exposed on a " +
				"LoadBalancer, so there is no address to publish them at — set Replica set exposure to LoadBalancer"})
		}
	}
	return out
}

// k3dLBAddrsFromServices picks the cluster's LoadBalancer Services that have an address out of
// `kubectl get svc -o json`, keyed by Service name. Only the cluster's own: the name is prefixed with
// it, which every Service each of the three operators creates is.
func k3dLBAddrsFromServices(js, cluster string) map[string]string {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Type string `json:"type"`
			} `json:"spec"`
			Status struct {
				LoadBalancer struct {
					Ingress []struct {
						IP string `json:"ip"`
					} `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		} `json:"items"`
	}
	out := map[string]string{}
	if json.Unmarshal([]byte(js), &list) != nil {
		return out
	}
	for _, s := range list.Items {
		if s.Spec.Type != "LoadBalancer" || !strings.HasPrefix(s.Metadata.Name, cluster+"-") || len(s.Status.LoadBalancer.Ingress) == 0 {
			continue
		}
		if ip := s.Status.LoadBalancer.Ingress[0].IP; ip != "" {
			out[s.Metadata.Name] = ip
		}
	}
	return out
}

// k3dTLSHost is one published name, for the panel.
type k3dTLSHost struct {
	Host   string `json:"host"`   // fully qualified
	IP     string `json:"ip"`     // the LoadBalancer address
	InCert bool   `json:"inCert"` // the cluster's certificates carry it
}

// k3dPublishTLSHosts publishes the ready cluster's LoadBalancer Services in the stack DNS. Called
// after the cluster reports ready, when the operator has created every Service it is going to.
// MetalLB assigns within seconds, so the short retry covers only a Service that appeared last.
func (a *App) k3dPublishTLSHosts(ctx context.Context, st Stack, doc designDoc, frame designFrame, serverID string, cfg k3dConfig, logln func(string)) {
	if !k3dTLSHostsOn(frame) {
		return
	}
	var addrs map[string]string
	for deadline := time.Now().Add(2 * time.Minute); ; {
		out, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "get", "svc", "-o", "json")
		if err == nil {
			addrs = k3dLBAddrsFromServices(out, cfg.ClusterName)
		}
		if len(addrs) > 0 || time.Now().After(deadline) || !k3dSleep(ctx, 10*time.Second) {
			break
		}
	}
	if len(addrs) == 0 {
		logln("host names: the cluster has no LoadBalancer Service with an address, so nothing was published — expose the proxy or the database pods on a LoadBalancer")
		return
	}
	domain := envOr("DOMAIN", "example.net")
	inCert := map[string]bool{}
	for _, h := range k3dTLSHostCandidates(cfg.Operator, cfg.ClusterName, cfg.Sharding) {
		inCert[h] = cfg.TLSHostsInCert
	}
	var published []k3dTLSHost
	for svc, ip := range addrs {
		published = append(published, k3dTLSHost{Host: fqdnOf(svc, domain), IP: ip, InCert: inCert[svc]})
	}
	sort.Slice(published, func(i, j int) bool { return published[i].Host < published[j].Host })
	a.k3dReplSaveConfig(st, doc, frame, func(c *k3dConfig) { c.TLSHosts = addrs; c.TLSHostList = published })
	a.reconcileStackDNS(ctx, st.ID)
	var names []string
	for _, h := range published {
		names = append(names, h.Host+" → "+h.IP)
	}
	logln("host names published in the Intranet DNS (A + PTR): " + strings.Join(names, ", "))
}
