package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hostports.go — choosing the host ports DBCanvas publishes, and keeping track of them.
//
// Every TCP port a node publishes (a PMM UI, a database export, a VNC desktop, …) is
// bound on the Docker host, at CONTAINER_BIND_IP. Choosing one used to go wrong in
// two ways:
//
//   - A port DBCanvas picked itself was "free" by a bind test made inside the app's
//     own container, whose network namespace is not the host's. A port some host
//     process held looked free, and the container failed to start with "address
//     already in use" (a PMM node did exactly that).
//   - A port Docker picked (HostPort "") is re-picked on every start, so a node's URL
//     changed when it restarted, and nothing stopped Docker handing a stopped node's
//     pinned port to somebody else's new container.
//
// So DBCanvas now picks every published host port itself, pins it, and records it:
//
//  1. The test is made on the host. A short-lived probe container, the app's own image
//     run as `/dbcanvas -probe-ports` in the host network namespace, binds candidate
//     ports where Docker will bind them and reports which it could. (When the app
//     runs directly on the host, the same test runs in-process.)
//  2. A pinned port is checked again before every create. One DBCanvas chose (PortMap
//     Auto) is re-picked if it is taken, and the node reads back the new one; one a
//     user typed (an export port) is never moved — the create fails, naming who holds
//     it.
//  3. The host_ports table records which container owns which port, running or
//     stopped, so no other node is given it. A node keeps its ports across stop, start
//     and redeploy; they are released when its deployment or its stack is deleted, and
//     the table is rebuilt from Docker when the app starts.
//
// One lock serializes the whole choose → reserve step, so two users deploying at once
// cannot be handed the same port.

const hostPortsSchema = `
CREATE TABLE IF NOT EXISTS host_ports (
  port       INTEGER PRIMARY KEY,
  owner      TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_host_ports_owner ON host_ports(owner);`

// hostPortBook serializes allocation.
var hostPortBook sync.Mutex

// errPortTaken is a fixed (user-chosen) port another node or host process holds.
type errPortTaken struct {
	Port int
	By   string
}

func (e errPortTaken) Error() string {
	return fmt.Sprintf("host port %d is already in use by %s — choose another export port", e.Port, e.By)
}

// pickHostPorts settles the host ports for owner (a container name): want[i].HostPort
// 0 means choose one, otherwise keep it if it can be had. An Auto port that cannot is
// replaced; a fixed one is an errPortTaken. The result is reserved for owner, and
// owner's other reservations are released (its publish set is exactly this).
func (a *App) pickHostPorts(ctx context.Context, owner string, want []PortMap) ([]int, error) {
	hostPortBook.Lock()
	defer hostPortBook.Unlock()

	owners, err := a.store.HostPortOwners()
	if err != nil {
		return nil, err
	}
	out := make([]int, len(want))
	var check []int
	need := 0
	// owner's own reservations are tested on the host like any other port: a create
	// cannot happen while a container of the same name exists, so if one is held it is
	// held by something else — which is how a PMM node kept failing on 45499.
	for p, by := range owners {
		if by == owner {
			delete(owners, p)
		}
	}
	for _, m := range want {
		switch by, held := owners[m.HostPort]; {
		case m.HostPort <= 0:
			need++
		case held && !m.Auto:
			return nil, errPortTaken{m.HostPort, describeOwner(by)}
		case held:
			need++ // somebody else's now; choose again
		default:
			check = append(check, m.HostPort)
			if m.Auto {
				need++ // a spare, in case it turns out to be taken
			}
		}
	}
	exclude := make([]int, 0, len(owners))
	for p := range owners {
		exclude = append(exclude, p)
	}
	var res probeResult
	if len(check) > 0 || need > 0 {
		if res, err = a.probeHostPorts(ctx, probeRequest{IP: bindIP(), Check: check, Count: need, Exclude: exclude}); err != nil {
			return nil, fmt.Errorf("choose host ports: %w", err)
		}
	}
	free := map[int]bool{}
	for _, p := range res.Free {
		free[p] = true
	}
	picked := res.Picked
	take := func() (int, error) {
		if len(picked) == 0 {
			return 0, fmt.Errorf("no free host port found")
		}
		p := picked[0]
		picked = picked[1:]
		return p, nil
	}
	for i, m := range want {
		if out[i] != 0 {
			continue
		}
		by, held := owners[m.HostPort]
		switch {
		case m.HostPort > 0 && !held && free[m.HostPort]:
			out[i] = m.HostPort
		case m.HostPort > 0 && !held && !m.Auto:
			return nil, errPortTaken{m.HostPort, "another program on the Docker host"}
		default:
			p, err := take()
			if err != nil {
				return nil, err
			}
			if m.HostPort > 0 {
				why := "another program on the Docker host"
				if held {
					why = describeOwner(by)
				}
				log.Printf("host ports: %s: port %d is taken by %s; using %d", owner, m.HostPort, why, p)
			}
			out[i] = p
		}
	}
	if err := a.store.SetHostPorts(owner, out); err != nil {
		return nil, err
	}
	return out, nil
}

// guardPublish is Docker.ContainerCreate's hook: it turns every publish of spec into
// a pinned, reserved host port.
func (a *App) guardPublish(ctx context.Context, spec *ContainerSpec) error {
	var maps []PortMap
	for _, p := range append([]int{spec.PublishPort}, spec.PublishPorts...) {
		if p > 0 {
			maps = append(maps, PortMap{ContainerPort: p})
		}
	}
	maps = append(maps, spec.PublishMap...)
	if len(maps) == 0 {
		return nil
	}
	ports, err := a.pickHostPorts(ctx, spec.Name, maps)
	if err != nil {
		return err
	}
	for i := range maps {
		maps[i].HostPort = ports[i]
	}
	spec.PublishPort, spec.PublishPorts, spec.PublishMap = 0, nil, maps
	return nil
}

// describeOwner names a reservation's owner for a person.
func describeOwner(owner string) string {
	name, extra, _ := strings.Cut(owner, "#")
	rest := strings.TrimPrefix(name, instancePrefix())
	if id, node, ok := strings.Cut(rest, "-"); ok && rest != name {
		if _, err := strconv.ParseInt(id, 10, 64); err == nil {
			s := "node " + node + " of stack " + id
			if extra != "" {
				s += " (" + extra + ")"
			}
			return s
		}
	}
	return owner
}

func bindIP() string { return envOr("CONTAINER_BIND_IP", "127.0.0.1") }

// --- the probe ---

type probeRequest struct {
	IP      string `json:"ip"`
	Check   []int  `json:"check,omitempty"`   // report whether each can be bound
	Count   int    `json:"count,omitempty"`   // and pick this many more that can
	Exclude []int  `json:"exclude,omitempty"` // never pick these
}

type probeResult struct {
	Free   []int `json:"free"`
	Picked []int `json:"picked"`
}

// probePorts runs in the namespace whose ports matter. It holds every listener it
// opened until it has answered, so it never picks one port twice or picks a port it
// was asked to check.
func probePorts(req probeRequest) (probeResult, error) {
	var res probeResult
	var held []net.Listener
	defer func() {
		for _, l := range held {
			l.Close()
		}
	}()
	listen := func(p int) bool {
		l, err := net.Listen("tcp", net.JoinHostPort(req.IP, strconv.Itoa(p)))
		if err != nil {
			return false
		}
		held = append(held, l)
		return true
	}
	skip := map[int]bool{}
	for _, p := range req.Exclude {
		skip[p] = true
	}
	for _, p := range req.Check {
		skip[p] = true
		if listen(p) {
			res.Free = append(res.Free, p)
		}
	}
	lo, hi := localPortRange()
	for tries := 0; len(res.Picked) < req.Count && tries < 2000; tries++ {
		var b [4]byte
		rand.Read(b[:])
		p := lo + int(binary.BigEndian.Uint32(b[:])%uint32(hi-lo+1))
		if skip[p] {
			continue
		}
		skip[p] = true
		if listen(p) {
			res.Picked = append(res.Picked, p)
		}
	}
	if len(res.Picked) < req.Count {
		return res, fmt.Errorf("found %d of %d free ports", len(res.Picked), req.Count)
	}
	return res, nil
}

// localPortRange is the kernel's ephemeral range, which is where Docker picks its own
// host ports from too.
func localPortRange() (int, int) {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err == nil {
		if f := strings.Fields(string(b)); len(f) == 2 {
			lo, e1 := strconv.Atoi(f[0])
			hi, e2 := strconv.Atoi(f[1])
			if e1 == nil && e2 == nil && lo > 1024 && hi > lo {
				return lo, hi
			}
		}
	}
	return 32768, 60999
}

// probePortsMain is `/dbcanvas -probe-ports '<json request>'`: the probe container's
// whole job. It prints the result as JSON.
func probePortsMain(arg string) int {
	var req probeRequest
	if err := json.Unmarshal([]byte(arg), &req); err != nil {
		fmt.Fprintln(os.Stderr, "probe-ports:", err)
		return 2
	}
	res, err := probePorts(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe-ports:", err)
		return 1
	}
	json.NewEncoder(os.Stdout).Encode(res)
	return 0
}

// probeHostPorts answers req for the Docker host: in a probe container when the app
// itself runs in one, in-process when it runs on the host.
func (a *App) probeHostPorts(ctx context.Context, req probeRequest) (probeResult, error) {
	if a.docker == nil {
		return probePorts(req)
	}
	img, ok := a.docker.selfImage(ctx)
	if !ok {
		return probePorts(req)
	}
	arg, _ := json.Marshal(req)
	out, err := a.docker.runHostProbe(ctx, img, []string{"-probe-ports", string(arg)})
	if err != nil {
		return probeResult{}, err
	}
	var res probeResult
	if err := json.Unmarshal(out, &res); err != nil {
		return probeResult{}, fmt.Errorf("probe output: %v: %q", err, strings.TrimSpace(string(out)))
	}
	return res, nil
}

// --- store ---

// HostPortOwners maps every reserved port to its owner.
func (s *Store) HostPortOwners() (map[int]string, error) {
	rows, err := s.db.Query("SELECT port, owner FROM host_ports")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var p int
		var o string
		if err := rows.Scan(&p, &o); err != nil {
			return nil, err
		}
		out[p] = o
	}
	return out, rows.Err()
}

// SetHostPorts makes ports exactly owner's reservations.
func (s *Store) SetHostPorts(owner string, ports []int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM host_ports WHERE owner = ?", owner); err != nil {
		return err
	}
	now := nowRFC3339()
	for _, p := range ports {
		if _, err := tx.Exec("INSERT OR REPLACE INTO host_ports (port, owner, created_at) VALUES (?,?,?)", p, owner, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// releaseNodePorts drops a node's reservations: its container's, and anything it
// reserved beside it ("<container>#<what>", e.g. an Everest load balancer).
func releaseNodePorts(ex interface {
	Exec(string, ...any) (sql.Result, error)
}, stackID int64, nodeID string) {
	name := containerName(stackID, nodeID)
	ex.Exec("DELETE FROM host_ports WHERE owner = ? OR substr(owner, 1, ?) = ?", name, len(name)+1, name+"#")
}

// releaseStackPorts drops every reservation of a stack's containers.
func releaseStackPorts(ex interface {
	Exec(string, ...any) (sql.Result, error)
}, stackID int64) {
	p := stackContainerPrefix(stackID)
	ex.Exec("DELETE FROM host_ports WHERE substr(owner, 1, ?) = ?", len(p), p)
}

// reseedHostPorts rebuilds the table from Docker: every container of this
// installation, running or stopped, keeps the host ports it is pinned to. Reservations
// for a container that no longer exists go. Run at start-up, before anything deploys.
func (a *App) reseedHostPorts(ctx context.Context) {
	if a.docker == nil {
		return
	}
	pins, err := a.docker.pinnedHostPorts(ctx, instancePrefix())
	if err != nil {
		log.Printf("host ports: could not read the containers' published ports: %v", err)
		return
	}
	hostPortBook.Lock()
	defer hostPortBook.Unlock()
	owners, _ := a.store.HostPortOwners()
	tx, err := a.store.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	// Reservations whose owner has a container are rebuilt from it; the others (an
	// Everest load balancer, say, which is not a container of ours) are kept while
	// their stack exists.
	tx.Exec("DELETE FROM host_ports")
	now := nowRFC3339()
	var names []string
	for name := range pins {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, p := range pins[name] {
			tx.Exec("INSERT OR REPLACE INTO host_ports (port, owner, created_at) VALUES (?,?,?)", p, name, now)
		}
	}
	for p, o := range owners {
		if _, ok := pins[o]; ok || !strings.Contains(o, "#") {
			continue
		}
		tx.Exec("INSERT OR IGNORE INTO host_ports (port, owner, created_at) VALUES (?,?,?)", p, o, now)
	}
	tx.Commit()
	n := 0
	for _, ps := range pins {
		n += len(ps)
	}
	log.Printf("host ports: %d pinned port(s) across %d container(s)", n, len(pins))
}

// probeTimeout bounds one probe container's life.
const probeTimeout = 60 * time.Second
