package main

// k8sexpose.go — giving an operator's database an address the load tools can dial.
//
// A Service of type ClusterIP has no address outside its cluster, and that is the
// operator default. The Database Explorer and the Data Generator work around it by
// running a client inside a pod, but the Query Runner and the Benchmark cannot: they
// open many connections and time them, and a process per statement would measure the
// process. So this is how such a cluster gets a real address.
//
// What it does NOT do is touch the operator's Service. Patching a resource the
// operator owns invites it to patch back — reconciliation is the operator's whole
// job — and a DBCanvas that fights an operator over a Service is a DBCanvas that
// breaks clusters. Instead it creates a *second* Service beside it, with the same
// selector and the same port, of a type that gets an address. Deleting it puts the
// cluster back exactly as it was, and the operator never notices either.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// k8sExposeSuffix marks the Services this feature creates, so they can be recognised,
// listed and removed again — and so one is never mistaken for the operator's own.
const k8sExposeSuffix = "-dbcanvas"

// k8sExposeLabel is set on every Service created here. It is what makes "remove it
// again" safe: nothing is deleted that DBCanvas did not create.
const k8sExposeLabel = "dbcanvas.io/exposed-for-tools"

// k8sExposeSourceAnnotation records which Service this one mirrors. It is a separate
// constant rather than k8sExposeLabel + "/source" because a Kubernetes key has at most
// one slash — the prefix — so appending a second path segment makes it invalid.
const k8sExposeSourceAnnotation = "dbcanvas.io/exposed-source"

// k8sExposeResult is what happened, in terms a user can act on.
type k8sExposeResult struct {
	Service string `json:"service"`
	Type    string `json:"type"`
	Addr    string `json:"addr,omitempty"`
	Port    int    `json:"port,omitempty"`
	Message string `json:"message"`
}

// k8sExposeEndpoint creates the companion Service for one endpoint.
//
// LoadBalancer first, because a MetalLB address is on the stack's own subnet and needs
// no port bookkeeping. MetalLB's pool is finite (eight addresses per cluster), so when
// there is none left the Service is converted to a NodePort rather than left pending —
// a NodePort is reachable at any k3s node's address and always available.
func (a *App) k8sExposeEndpoint(ctx context.Context, e k8sEndpoint) (k8sExposeResult, error) {
	if len(e.Selector) == 0 {
		return k8sExposeResult{}, fmt.Errorf(
			"%s has no pod selector of its own — its endpoints are managed by the operator "+
				"(this is how the PostgreSQL operators publish a primary, so that a failover moves it). "+
				"Expose a tier that does have one, such as the pooler or the replicas, or set this "+
				"cluster's Service type in the frame's settings and redeploy", e.Service)
	}
	name := e.Service + k8sExposeSuffix
	if err := a.kubectlApplyNS(withEngine(ctx, a.docker), e.ServerID, e.Namespace, k8sExposeManifest(name, e, "LoadBalancer")); err != nil {
		return k8sExposeResult{}, fmt.Errorf("could not create the Service: %v", err)
	}
	k8sInvalidateFrame(e.StackID, e.FrameID)

	// Give MetalLB a moment to serve an address; fall back rather than leave a
	// Service that will be pending forever.
	if addr := a.k8sWaitServiceAddr(ctx, e, name, 15*time.Second); addr != "" {
		return k8sExposeResult{
			Service: name, Type: "LoadBalancer", Addr: addr, Port: e.Port,
			Message: fmt.Sprintf("%s is now reachable at %s:%d", e.Label, addr, e.Port),
		}, nil
	}
	if err := a.kubectlApplyNS(withEngine(ctx, a.docker), e.ServerID, e.Namespace, k8sExposeManifest(name, e, "NodePort")); err != nil {
		return k8sExposeResult{}, fmt.Errorf("no LoadBalancer address was available and the NodePort fallback failed: %v", err)
	}
	k8sInvalidateFrame(e.StackID, e.FrameID)
	return k8sExposeResult{
		Service: name, Type: "NodePort",
		Message: "MetalLB had no address free, so a NodePort was created instead — " +
			"it is reachable at the cluster's node address",
	}, nil
}

// k8sUnexposeEndpoint removes the companion Service. It deletes by the label this
// feature sets, so a Service DBCanvas did not create cannot be removed by it even if
// somebody names one.
func (a *App) k8sUnexposeEndpoint(ctx context.Context, e k8sEndpoint) error {
	name := strings.TrimSuffix(e.Service, k8sExposeSuffix) + k8sExposeSuffix
	ctx = withEngine(ctx, a.docker)
	out, err := a.kubectl(ctx, e.ServerID, "-n", e.Namespace, "get", "svc", name,
		"-o", "jsonpath={.metadata.labels."+strings.ReplaceAll(k8sExposeLabel, ".", `\.`)+"}")
	if err != nil {
		return fmt.Errorf("no such Service in this cluster")
	}
	if strings.TrimSpace(out) != "true" {
		return fmt.Errorf("%s was not created by DBCanvas, so it will not be removed by it", name)
	}
	if _, err := a.kubectl(ctx, e.ServerID, "-n", e.Namespace, "delete", "svc", name); err != nil {
		return fmt.Errorf("could not remove the Service: %v", err)
	}
	k8sInvalidateFrame(e.StackID, e.FrameID)
	return nil
}

// k8sExposeManifest is the companion Service: the same pods, the same port, a type
// that has an address.
func k8sExposeManifest(name string, e k8sEndpoint, svcType string) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Service\nmetadata:\n  name: " + name + "\n")
	b.WriteString("  namespace: " + e.Namespace + "\n  labels:\n")
	b.WriteString("    " + k8sExposeLabel + ": \"true\"\n")
	b.WriteString("  annotations:\n")
	b.WriteString("    " + k8sExposeSourceAnnotation + ": " + e.Service + "\n")
	b.WriteString("spec:\n  type: " + svcType + "\n  selector:\n")
	for k, v := range e.Selector {
		b.WriteString("    " + k + ": " + jsonQuote(v) + "\n")
	}
	b.WriteString("  ports:\n")
	fmt.Fprintf(&b, "    - name: %s\n      port: %d\n      targetPort: %d\n      protocol: TCP\n",
		e.Engine, e.Port, e.TargetPort)
	return b.String()
}

// jsonQuote quotes a YAML scalar. Label values are Kubernetes-constrained already, so
// this is belt and braces rather than escaping anything interesting.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// kubectlApplyNS applies a manifest into one namespace.
func (a *App) kubectlApplyNS(ctx context.Context, serverID, ns, manifest string) error {
	res, err := a.engCtx(ctx).ExecInput(ctx, serverID, "",
		[]string{"kubectl", "-n", ns, "apply", "-f", "-"},
		[]string{"KUBECONFIG=" + k3dKubeconfig}, []byte(manifest))
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return nil
}

// k8sWaitServiceAddr polls a Service for the address its LoadBalancer implementation
// assigned. An empty return is not an error: MetalLB's pool may be exhausted, which is
// exactly the case the caller falls back for.
func (a *App) k8sWaitServiceAddr(ctx context.Context, e k8sEndpoint, name string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := a.kubectl(withEngine(ctx, a.docker), e.ServerID, "-n", e.Namespace,
			"get", "svc", name, "-o", "jsonpath={.status.loadBalancer.ingress[0].ip}")
		if err == nil {
			if ip := strings.TrimSpace(out); ip != "" {
				return ip
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(time.Second):
		}
	}
	return ""
}
