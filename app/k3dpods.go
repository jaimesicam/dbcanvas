package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// k3dpods.go — the pod console: a shell inside a container of a pod, opened from the
// k3s server node's right-click menu on the canvas.
//
// The node console (terminal.go) execs into a *container of the stack*. Inside a
// Kubernetes cluster the thing an operator actually wants a shell in is one layer
// further down — a container of a pod the operator created — and the address of that
// shell is four parts: namespace, pod, container, and which shell to run. So the menu
// asks for all four, and this file provides both halves of it: the inventory the menu
// is built from (handleK3DPods), and the argv that terminal.go execs once a leaf is
// picked (k3dPodExecCmd).
//
// Everything runs *inside the k3s server node*, through its own kubectl and its own
// admin kubeconfig, exactly like every other Kubernetes call in this app (see k3d.go's
// header). Nothing here needs a Kubernetes client, a kubeconfig on the app host, or a
// port published off the cluster.
//
// The inventory is deliberately NOT cached. A pod list is the most short-lived thing in
// this app — the operator deletes and recreates pods on its own schedule, names carry a
// generated suffix, and a menu built from a minute-old list offers shells into
// containers that no longer exist. Every open of the submenu asks the cluster again.

// k3dPodMaxOutput caps what a `kubectl get pods` is allowed to be. kubectl hides
// managedFields from `get` output by default, so a cluster's worth of pods is a few
// hundred KB; this is only here so a pathological cluster cannot make the app buffer
// something enormous on the way to a context menu.
const k3dPodMaxOutput = 8 << 20

// k3dPodContainer is one container of a pod, and whether a shell can be opened in it.
// State is the container's own state ("running", "waiting", "terminated", or "" when
// the kubelet has not reported one yet) — the menu greys out everything but running,
// because `kubectl exec` into a container that is not running fails with a message
// nobody reads at the bottom of a terminal that then closes.
type k3dPodContainer struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Ready bool   `json:"ready"`
	Init  bool   `json:"init,omitempty"`
}

// k3dPod is one pod, flattened to what a menu needs: where it is, whether it is up, and
// the containers you can get into.
type k3dPod struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Phase      string            `json:"phase"`
	Containers []k3dPodContainer `json:"containers"`
}

// k3dPodList is the `kubectl get pods -A -o json` shape, narrowed to the fields the
// console needs. Decoding into a narrow struct rather than map[string]any is what keeps
// this readable: unknown fields are dropped by encoding/json, which is most of the
// document.
type k3dPodList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Containers     []struct{ Name string } `json:"containers"`
			InitContainers []struct{ Name string } `json:"initContainers"`
		} `json:"spec"`
		Status struct {
			Phase                 string             `json:"phase"`
			ContainerStatuses     []k3dContainerStat `json:"containerStatuses"`
			InitContainerStatuses []k3dContainerStat `json:"initContainerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type k3dContainerStat struct {
	Name  string         `json:"name"`
	Ready bool           `json:"ready"`
	State map[string]any `json:"state"`
}

// k3dContainerState reduces a container's state object — a map with exactly one of
// running/waiting/terminated in it — to that one key.
func k3dContainerState(state map[string]any) string {
	for _, k := range []string{"running", "waiting", "terminated"} {
		if _, ok := state[k]; ok {
			return k
		}
	}
	return ""
}

// parseK3DPods turns `kubectl get pods -A -o json` into the console's inventory,
// sorted by namespace then pod then the pod's own container order (which is the order
// they are declared in, and the order somebody reading the manifest expects). Init
// containers come after the regular ones, tagged: they are usually not running, but a
// pod stuck in Init is exactly when you want to look inside one.
//
// Pure, so the parsing is testable without a cluster.
func parseK3DPods(out []byte) ([]k3dPod, error) {
	var doc k3dPodList
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("kubectl returned something that is not a pod list: %w", err)
	}
	pods := make([]k3dPod, 0, len(doc.Items))
	for _, it := range doc.Items {
		p := k3dPod{Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, Phase: it.Status.Phase}
		stat := func(list []k3dContainerStat, name string) (string, bool) {
			for _, s := range list {
				if s.Name == name {
					return k3dContainerState(s.State), s.Ready
				}
			}
			return "", false
		}
		for _, c := range it.Spec.Containers {
			state, ready := stat(it.Status.ContainerStatuses, c.Name)
			p.Containers = append(p.Containers, k3dPodContainer{Name: c.Name, State: state, Ready: ready})
		}
		for _, c := range it.Spec.InitContainers {
			state, ready := stat(it.Status.InitContainerStatuses, c.Name)
			p.Containers = append(p.Containers, k3dPodContainer{Name: c.Name, State: state, Ready: ready, Init: true})
		}
		pods = append(pods, p)
	}
	sort.SliceStable(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
	return pods, nil
}

// k3dServerDep resolves the node in the URL to a *running k3s server node*, writing the
// HTTP error itself when it is anything else. The server node is the one with the admin
// kubeconfig on it; an agent has no kubectl worth running, which is why the pod console
// is offered on the server and nowhere else.
func (a *App) k3dServerDep(w http.ResponseWriter, r *http.Request) (Deployment, bool) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return Deployment{}, false
	}
	var cfg k3dConfig
	if json.Unmarshal(dep.Config, &cfg) != nil || cfg.Role != "server" {
		writeErr(w, http.StatusConflict, "this is not a Kubernetes server node")
		return Deployment{}, false
	}
	return dep, true
}

// handleK3DPods lists every pod in the cluster, for the pod console menu.
func (a *App) handleK3DPods(w http.ResponseWriter, r *http.Request) {
	dep, ok := a.k3dServerDep(w, r)
	if !ok {
		return
	}
	pods, err := a.k3dPods(r.Context(), dep.ContainerID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list pods: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pods": pods})
}

// k3dPods asks one k3s node's kubectl for every pod in the cluster.
func (a *App) k3dPods(ctx context.Context, serverID string) ([]k3dPod, error) {
	out, err := a.kubectl(ctx, serverID, "get", "pods", "--all-namespaces", "-o", "json")
	if err != nil {
		return nil, err
	}
	if len(out) > k3dPodMaxOutput {
		return nil, fmt.Errorf("the pod list is larger than %d bytes", k3dPodMaxOutput)
	}
	return parseK3DPods([]byte(out))
}

// ------------------------------------------------------------------ the console itself

// k3dPodShells are the shells the menu offers, in menu order. "auto" is first and is
// what the node console already does: try bash, fall back to sh — right for nearly
// every database image. The explicit two are for the images where it is not: a
// container whose bash exists but is broken, or where /bin/sh is the one you meant.
var k3dPodShells = []string{"auto", "bash", "sh"}

// A namespace or a pod name is an RFC 1123 subdomain; a container name is an RFC 1123
// label. Both are checked here even though the argv is passed to Docker as a list and
// never through a shell — the names arrive in a URL from a browser, and a closed set of
// characters is a cheaper guarantee than reasoning about every layer they cross.
var (
	k3dDNSSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	k3dDNSLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// k3dPodShellScript is what runs as PID 1 of the exec inside the *pod's* container.
// -i so the shell prints a prompt (busybox sh does not without it); `command -v` before
// the exec, never a bare `exec bash`, because a failed exec exits the shell and the
// terminal dies blank — the same reasoning as the node console's own script.
func k3dPodShellScript(shell string) string {
	switch shell {
	case "bash":
		return "exec bash -i"
	case "sh":
		return "exec /bin/sh -i"
	default:
		return "if command -v bash >/dev/null 2>&1; then exec bash -i; else exec /bin/sh -i; fi"
	}
}

// k3dPodExecCmd is the argv the k3s server node runs to put the caller inside a pod's
// container: kubectl exec, with a TTY, into the container the menu named. Pure, so the
// shape of the command is testable.
func k3dPodExecCmd(ns, pod, container, shell string) []string {
	return []string{
		"kubectl", "-n", ns, "exec", "-i", "-t", pod, "-c", container,
		"--", "/bin/sh", "-c", k3dPodShellScript(shell),
	}
}

// k3dPodConsoleRequest reads the four parts of a pod console off a terminal request.
// Returns ok=false with no error when the request is a plain node console (no pod
// named), which is the common case — terminal.go serves that unchanged.
func k3dPodConsoleRequest(r *http.Request) (ns, pod, container, shell string, ok bool, err error) {
	q := r.URL.Query()
	pod = strings.TrimSpace(q.Get("pod"))
	if pod == "" {
		return "", "", "", "", false, nil
	}
	ns = strings.TrimSpace(q.Get("namespace"))
	container = strings.TrimSpace(q.Get("container"))
	shell = strings.TrimSpace(q.Get("shell"))
	if shell == "" {
		shell = "auto"
	}
	switch {
	case !k3dDNSSubdomain.MatchString(ns):
		return "", "", "", "", false, fmt.Errorf("namespace %q is not a Kubernetes name", ns)
	case !k3dDNSSubdomain.MatchString(pod):
		return "", "", "", "", false, fmt.Errorf("pod %q is not a Kubernetes name", pod)
	case !k3dDNSLabel.MatchString(container):
		return "", "", "", "", false, fmt.Errorf("container %q is not a Kubernetes name", container)
	case !slices.Contains(k3dPodShells, shell):
		return "", "", "", "", false, fmt.Errorf("shell %q is not one of %s", shell, strings.Join(k3dPodShells, ", "))
	}
	return ns, pod, container, shell, true, nil
}
