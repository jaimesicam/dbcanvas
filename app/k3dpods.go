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
// The fourth part is not always a shell. On a database container it can be that
// database's own client — mysql, psql or mongosh, already logged in as an
// administrator, because all four Percona operators mount the cluster's users Secret
// into the pod and the script finds the credential there rather than being handed one
// (see the k3dPodClients block below).
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
// Clients are the database clients the console can start in this container instead of
// a shell (see k3dPodClients) — a menu hint, derived from the container's name, not a
// permission: the client scripts probe for themselves.
type k3dPodContainer struct {
	Name    string   `json:"name"`
	State   string   `json:"state"`
	Ready   bool     `json:"ready"`
	Init    bool     `json:"init,omitempty"`
	Clients []string `json:"clients,omitempty"`
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
			p.Containers = append(p.Containers, k3dPodContainer{Name: c.Name, State: state, Ready: ready, Clients: k3dPodClientsFor(c.Name)})
		}
		for _, c := range it.Spec.InitContainers {
			state, ready := stat(it.Status.InitContainerStatuses, c.Name)
			p.Containers = append(p.Containers, k3dPodContainer{Name: c.Name, State: state, Ready: ready, Init: true, Clients: k3dPodClientsFor(c.Name)})
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

// ------------------------------------------------------------- the database clients

// A pod console's last hop is not always a shell. On a database container the thing an
// operator actually wants is that database's own client, already logged in — and the
// credential for it is ALREADY INSIDE THE CONTAINER: every one of the four Percona
// operators mounts the cluster's users Secret into the pods it creates.
//
//   - PXC and PS: /etc/mysql/mysql-users-secret/root, and MYSQL_ROOT_PASSWORD in the
//     database container's environment. The same mount is on the haproxy and pxc-monit
//     sidecars, whose mysql client reaches the cluster through the proxy rather than
//     over the socket — a different path, and sometimes the broken one.
//   - PSMDB: /etc/users-secret/MONGODB_DATABASE_ADMIN_{USER,PASSWORD}.
//   - PG: nothing at all — the Postgres container runs AS the postgres user, and local
//     peer authentication lets psql in without a password.
//
// That the credential is already there is what makes this worth doing rather than
// merely possible: DBCanvas never reads the Secret for it, so the password does not
// cross the app, never lands in the k3s node's process list, and cannot reach a log.
// Each script below is handed to the pod's own /bin/sh, and finds the credential there.
// (The single exception is mongosh, which has no environment variable for a password,
// so one appears on an argv — inside the mongod container only, visible to the uid
// mongod already runs as.)
//
// A client is NOT restricted to the containers in Containers: that list is only what
// the menu offers a row on, and naming a client explicitly (?shell=mysql, or the CLI's
// --shell mysql) is allowed on any container. What stands in for that check is that
// every script PROBES BEFORE IT EXECS and explains itself when it cannot — see
// k3dPodClientFallback.
//
// Every probe reads `</dev/null` and forbids a password prompt (psql's `-w`). A probe
// runs with the caller's terminal on its stdin, so one that decides to ask for a
// password — `psql` does, against a server whose pg_hba wants one over TCP — would sit
// there holding a console that has printed nothing and cannot be typed into. It has to
// fail instead, and let the fallback say so.
type k3dPodClient struct {
	ID         string   // the ?shell= value, and the menu id
	Label      string   // the menu row
	Containers []string // the operators' container names this row is offered on
	Script     string   // what the pod's /bin/sh runs: find the credential, probe, exec
}

// k3dPodClientFallback ends a client script that could not start its client: it says
// why in one sentence, then opens a shell in that container anyway.
//
// Deliberately not an `exit 1`. This script is the terminal's PID 1, so exiting closes
// the window and takes the explanation with it — the operator gets a terminal that
// flashed and died, which is the exact failure the node console's own script was
// written to avoid. A shell keeps the sentence on screen, and a container where the
// database could not be reached is precisely where somebody now wants to look around.
//
// The reasons are literals in this file and hold no single quotes, which is what makes
// wrapping them in single quotes for `echo` sound.
func k3dPodClientFallback(reason string) string {
	return "echo '" + reason + "'; echo 'Opening a shell in this container instead.'; " + k3dPodShellScript("auto")
}

// k3dPodClients are the database clients the console can start, in menu order.
var k3dPodClients = []k3dPodClient{
	{
		ID:    "mysql",
		Label: "mysql",
		// pxc and mysql are the database itself (PXC and PS); haproxy and pxc-monit are
		// the two containers of a PXC proxy pod, and proxysql the other front end — all
		// three carry the client and the same secret mount, and answer on
		// 127.0.0.1:3306, so a console there tests the proxy path instead.
		Containers: []string{"pxc", "mysql", "haproxy", "pxc-monit", "proxysql"},
		Script: `if ! command -v mysql >/dev/null 2>&1; then
  ` + k3dPodClientFallback("This container has no mysql client.") + `
fi
if [ -r /etc/mysql/mysql-users-secret/root ]; then
  MYSQL_PWD=$(cat /etc/mysql/mysql-users-secret/root)
elif [ -n "$MYSQL_ROOT_PASSWORD" ]; then
  MYSQL_PWD=$MYSQL_ROOT_PASSWORD
else
  ` + k3dPodClientFallback("No root password in this container: /etc/mysql/mysql-users-secret/root is not readable and MYSQL_ROOT_PASSWORD is not set.") + `
fi
export MYSQL_PWD
if mysql -uroot -e 'SELECT 1' </dev/null >/dev/null 2>&1; then exec mysql -uroot; fi
if mysql -h 127.0.0.1 -uroot -e 'SELECT 1' </dev/null >/dev/null 2>&1; then exec mysql -h 127.0.0.1 -uroot; fi
unset MYSQL_PWD
` + k3dPodClientFallback("The mysql client is here and a root password with it, but nothing answered over the socket or on 127.0.0.1:3306."),
	},
	{
		ID:    "psql",
		Label: "psql",
		// database is the Percona PG operator's Postgres container, postgres is
		// CloudNativePG's. Both run as the postgres system user, which IS the
		// credential — peer authentication over the local socket needs no password, so
		// this script never looks for one.
		Containers: []string{"database", "postgres"},
		Script: `if ! command -v psql >/dev/null 2>&1; then
  ` + k3dPodClientFallback("This container has no psql client.") + `
fi
if psql -w -U postgres -c 'SELECT 1' </dev/null >/dev/null 2>&1; then exec psql -U postgres; fi
if psql -w -U postgres -h 127.0.0.1 -c 'SELECT 1' </dev/null >/dev/null 2>&1; then exec psql -U postgres -h 127.0.0.1; fi
` + k3dPodClientFallback("The psql client is here, but postgres answered neither over the local socket nor on 127.0.0.1:5432."),
	},
	{
		ID:    "mongosh",
		Label: "mongosh",
		// mongod is a replica set member or a config server; mongos is a router. The
		// users Secret is mounted on all of them.
		Containers: []string{"mongod", "mongos"},
		Script: `if ! command -v mongosh >/dev/null 2>&1; then
  ` + k3dPodClientFallback("This container has no mongosh client.") + `
fi
if [ -r /etc/users-secret/MONGODB_DATABASE_ADMIN_USER ]; then
  U=$(cat /etc/users-secret/MONGODB_DATABASE_ADMIN_USER); P=$(cat /etc/users-secret/MONGODB_DATABASE_ADMIN_PASSWORD)
else
  U=$MONGODB_DATABASE_ADMIN_USER; P=$MONGODB_DATABASE_ADMIN_PASSWORD
fi
if [ -n "$U" ] && mongosh --quiet --authenticationDatabase admin -u "$U" -p "$P" --eval 'db.version()' </dev/null >/dev/null 2>&1; then
  exec mongosh --authenticationDatabase admin -u "$U" -p "$P"
fi
if mongosh --quiet --eval 'db.version()' </dev/null >/dev/null 2>&1; then exec mongosh; fi
` + k3dPodClientFallback("mongosh is here, but it could not log in as the database admin and the server would not take an unauthenticated connection either."),
	},
}

// k3dPodClientsFor is the clients the menu offers on one container, by its name. Pure
// and name-based on purpose: the alternative is an exec per container just to build a
// context menu, and the container names of the four operators are a fixed, small set.
// A name nobody recognises simply gets the shells, which is what it had before.
func k3dPodClientsFor(container string) []string {
	var out []string
	for _, c := range k3dPodClients {
		if slices.Contains(c.Containers, container) {
			out = append(out, c.ID)
		}
	}
	return out
}

// k3dPodConsoleTargets is every value ?shell= accepts: the three shells, then the
// clients. One list so the error message names all of them.
func k3dPodConsoleTargets() []string {
	out := slices.Clone(k3dPodShells)
	for _, c := range k3dPodClients {
		out = append(out, c.ID)
	}
	return out
}

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

// k3dPodConsoleScript is what the pod's /bin/sh is given: a database client's script
// when the caller asked for one, and a shell otherwise.
func k3dPodConsoleScript(target string) string {
	for _, c := range k3dPodClients {
		if c.ID == target {
			return c.Script
		}
	}
	return k3dPodShellScript(target)
}

// k3dPodExecCmd is the argv the k3s server node runs to put the caller inside a pod's
// container: kubectl exec, with a TTY, into the container the menu named. Pure, so the
// shape of the command is testable.
func k3dPodExecCmd(ns, pod, container, shell string) []string {
	return []string{
		"kubectl", "-n", ns, "exec", "-i", "-t", pod, "-c", container,
		"--", "/bin/sh", "-c", k3dPodConsoleScript(shell),
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
	case !slices.Contains(k3dPodConsoleTargets(), shell):
		return "", "", "", "", false, fmt.Errorf("shell %q is not one of %s", shell, strings.Join(k3dPodConsoleTargets(), ", "))
	}
	return ns, pod, container, shell, true, nil
}
