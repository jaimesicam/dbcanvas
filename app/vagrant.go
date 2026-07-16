package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// vagrant.go — the Vagrant + VirtualBox provisioning backend.
//
// It implements the Engine interface (engine.go) by driving the `vagrant` and
// `VBoxManage` CLIs plus ssh, so each stack "node" is a real VirtualBox VM instead
// of a Docker container. A node maps to one single-machine Vagrantfile in its own
// directory under `root`; the VM's name — which is also the ContainerSpec.Name the
// rest of the app already computes with containerName() — is used as the opaque
// "container id" everywhere the Engine returns/accepts one.
//
// Requirements: the DBCanvas process must run on a host that has `vagrant`,
// `VBoxManage` and `ssh` on PATH (i.e. not inside the distroless container). When
// they are absent NewVagrant returns nil and the app simply has no vagrant backend.
//
// Networking: each stack network (networkName(stackID)) is backed by a VirtualBox
// host-only /24. VMs get a static IP on it (private_network); DNS/hostname
// resolution between nodes is provided by the Intranet VM exactly as in Docker mode
// (the generic provisioners point each node's resolv.conf at the Intranet and the
// Intranet's DNS zone is reconciled with each node's ContainerIP). Ports a node
// publishes become VirtualBox forwarded_port rules.

// Vagrant is an Engine backed by Vagrant + VirtualBox.
type Vagrant struct {
	root   string // working root, one subdir per VM and the shared network/port state
	vagant string // path to the `vagrant` binary
	vbox   string // path to `VBoxManage`
	ssh    string // path to `ssh`

	mu    sync.Mutex // guards the on-disk state files (networks, ports)
	terms sync.Map   // execID -> *vagrantTerm, for ResizeExec of live consoles
}

// NewVagrant returns a Vagrant engine if the host has the required tooling, else
// nil (the app then runs Docker-only). Honors DBCANVAS_VAGRANT_ROOT for the work
// dir, defaulting to ~/.dbcanvas/vagrant.
func NewVagrant() *Vagrant {
	vg, err1 := exec.LookPath("vagrant")
	vb, err2 := exec.LookPath("VBoxManage")
	sh, err3 := exec.LookPath("ssh")
	if err1 != nil || err2 != nil || err3 != nil {
		return nil
	}
	root := os.Getenv("DBCANVAS_VAGRANT_ROOT")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".dbcanvas", "vagrant")
	}
	if os.MkdirAll(filepath.Join(root, "vms"), 0o755) != nil {
		return nil
	}
	return &Vagrant{root: root, vagant: vg, vbox: vb, ssh: sh}
}

// --- box catalog -----------------------------------------------------------

// vagrantBoxSpec is the box backing an OS/version: a Vagrant box Name plus an optional
// box_url metadata JSON. Oracle publishes its boxes via a box_url (there is no
// `oraclelinux` registry namespace — `vagrant box add oraclelinux/9` 404s), so those
// carry a URL; the HashiCorp-registry `cloud-image/*` boxes resolve by Name alone.
type vagrantBoxSpec struct {
	Name string
	URL  string // box_url metadata JSON; empty ⇒ resolve Name from the registry
}

// vagrantBoxes maps "os/version" to the Vagrant box that backs it, over the same OS
// matrix DBCanvas builds Docker images for (Oracle Linux 8/9/10, Ubuntu 22.04/24.04).
// Oracle Linux uses Oracle's official boxes off oracle.github.io (box_url JSON pointing
// at yum.oracle.com); Ubuntu uses the HashiCorp `cloud-image/ubuntu-*` boxes. Override
// any entry's name with DBCANVAS_BOX_<OS>_<VER> (dots/dashes in the version become
// underscores), e.g. DBCANVAS_BOX_UBUNTU_24_04.
var vagrantBoxes = map[string]vagrantBoxSpec{
	"oraclelinux/8":  {"oraclelinux/8", "https://oracle.github.io/vagrant-projects/boxes/oraclelinux/8.json"},
	"oraclelinux/9":  {"oraclelinux/9", "https://oracle.github.io/vagrant-projects/boxes/oraclelinux/9.json"},
	"oraclelinux/10": {"oraclelinux/10", "https://oracle.github.io/vagrant-projects/boxes/oraclelinux/10.json"},
	"ubuntu/22.04":   {"cloud-image/ubuntu-22.04", ""},
	"ubuntu/24.04":   {"cloud-image/ubuntu-24.04", ""},
}

// vagrantBox resolves the box for an OS + version, honoring env overrides. An env
// override sets the box name only (URL cleared — it names a plain registry box).
func vagrantBox(os_, version string) (vagrantBoxSpec, bool) {
	key := os_ + "/" + version
	envKey := "DBCANVAS_BOX_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(os_+"_"+version))
	if v := os.Getenv(envKey); v != "" {
		return vagrantBoxSpec{Name: v}, true
	}
	b, ok := vagrantBoxes[key]
	return b, ok
}

// osFromImage recovers (os, version) from a dbcanvas-systemd image tag
// ("dbcanvas-systemd:<os>-<version>-<arch>"), the only image kind the vagrant
// backend provisions. Returns ok=false for any other image (e.g. a pulled infra
// image), which the caller reports as unsupported in vagrant mode.
func osFromImage(image string) (os_, version string, ok bool) {
	_, rest, found := strings.Cut(image, ":")
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "-")
	if len(parts) != 3 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// --- command helpers -------------------------------------------------------

// vmDir is the working directory (holding the Vagrantfile) for a VM.
func (v *Vagrant) vmDir(name string) string { return filepath.Join(v.root, "vms", name) }

// run executes a command in dir, returning combined stdout, stderr and error.
func (v *Vagrant) run(ctx context.Context, dir, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// vagrantCmd runs `vagrant <args>` in a VM's dir.
func (v *Vagrant) vagrantCmd(ctx context.Context, name string, args ...string) (string, string, error) {
	return v.run(ctx, v.vmDir(name), v.vagant, args...)
}

// --- Engine: reachability / host facts -------------------------------------

func (v *Vagrant) Ping(ctx context.Context) error {
	_, _, err := v.run(ctx, v.root, v.vbox, "--version")
	return err
}

func (v *Vagrant) HostArch(ctx context.Context) string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

// HostResources returns 0,0 ("unknown") so budget sanity-checks are skipped — a VM
// fleet's real limits are per-VM, not a single daemon's.
func (v *Vagrant) HostResources(ctx context.Context) (int, int64) { return 0, 0 }

// --- Engine: images / boxes ------------------------------------------------

// ImageExists always reports true: for the vagrant backend the box is ensured at
// create time, so the generic "is the image present?" pre-check is a no-op here.
func (v *Vagrant) ImageExists(ctx context.Context, ref string) (bool, error) { return true, nil }

// EnsureImage is a no-op: box acquisition happens in ContainerCreate, keyed off the
// node's OS, not the Docker image ref the generic caller passes.
func (v *Vagrant) EnsureImage(ctx context.Context, repo, tag, platform string) error { return nil }

// ensureBox adds the box locally if it isn't already present. A box_url is added by
// URL (its metadata JSON declares the name); a plain box is added by name off the
// registry.
func (v *Vagrant) ensureBox(ctx context.Context, box vagrantBoxSpec) error {
	out, _, err := v.run(ctx, v.root, v.vagant, "box", "list")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, box.Name+" ") {
				return nil
			}
		}
	}
	ref := box.Name
	if box.URL != "" {
		ref = box.URL
	}
	if _, errb, err := v.run(ctx, v.root, v.vagant, "box", "add", "--provider", "virtualbox", ref); err != nil {
		return fmt.Errorf("vagrant box add %s: %v: %s", box.Name, err, errb)
	}
	return nil
}

// --- Engine: networks ------------------------------------------------------

// netState is the persisted per-stack-network allocation: its /24 subnet plus the
// static IPs handed to each VM.
type netState struct {
	Subnet string            `json:"subnet"` // e.g. "172.28.5.0/24"
	Prefix string            `json:"prefix"` // e.g. "172.28.5"
	Next   int               `json:"next"`   // next host octet to hand out (>=10)
	Hosts  map[string]string `json:"hosts"`  // vmName -> ip
}

func (v *Vagrant) netFile(name string) string {
	return filepath.Join(v.root, "net-"+safeFile(name)+".json")
}

func (v *Vagrant) loadNet(name string) (netState, bool) {
	b, err := os.ReadFile(v.netFile(name))
	if err != nil {
		return netState{}, false
	}
	var ns netState
	if json.Unmarshal(b, &ns) != nil {
		return netState{}, false
	}
	return ns, true
}

func (v *Vagrant) saveNet(name string, ns netState) error {
	b, _ := json.MarshalIndent(ns, "", "  ")
	return os.WriteFile(v.netFile(name), b, 0o644)
}

// VirtualBox only allows host-only adapters inside 192.168.56.0/21 unless the host
// has an /etc/vbox/networks.conf widening that (a root-only change). Draw each stack
// network a /24 from that default-allowed range (192.168.56–63) so `vagrant up`
// works out of the box; DBCANVAS_VM_SUBNET_BASE (first two octets) overrides the
// range for hosts configured to allow more.
const vmSubnetOctetMin, vmSubnetOctetMax = 56, 63

func vmSubnetBase() string {
	if b := os.Getenv("DBCANVAS_VM_SUBNET_BASE"); b != "" {
		return b
	}
	return "192.168"
}

// NetworkEnsure allocates a /24 for the network on first use, at the next free third
// octet across all networks within the allowed range.
func (v *Vagrant) NetworkEnsure(ctx context.Context, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.loadNet(name); ok {
		return nil
	}
	used := map[int]bool{}
	entries, _ := filepath.Glob(filepath.Join(v.root, "net-*.json"))
	for _, e := range entries {
		var ns netState
		if b, err := os.ReadFile(e); err == nil && json.Unmarshal(b, &ns) == nil {
			if p := strings.Split(ns.Prefix, "."); len(p) == 3 {
				if oct, err := strconv.Atoi(p[2]); err == nil {
					used[oct] = true
				}
			}
		}
	}
	oct := vmSubnetOctetMin
	for used[oct] && oct <= vmSubnetOctetMax {
		oct++
	}
	if oct > vmSubnetOctetMax {
		return fmt.Errorf("no free host-only /24 in %s.%d-%d — add a wider range to /etc/vbox/networks.conf and set DBCANVAS_VM_SUBNET_BASE",
			vmSubnetBase(), vmSubnetOctetMin, vmSubnetOctetMax)
	}
	prefix := fmt.Sprintf("%s.%d", vmSubnetBase(), oct)
	return v.saveNet(name, netState{Subnet: prefix + ".0/24", Prefix: prefix, Next: 10, Hosts: map[string]string{}})
}

// allocIP returns the VM's static IP on the network, assigning the next free one on
// first request and reusing it on redeploy.
func (v *Vagrant) allocIP(network, vmName string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ns, ok := v.loadNet(network)
	if !ok {
		return "", fmt.Errorf("network %q not ensured", network)
	}
	if ip, ok := ns.Hosts[vmName]; ok {
		return ip, nil
	}
	ip := fmt.Sprintf("%s.%d", ns.Prefix, ns.Next)
	ns.Next++
	ns.Hosts[vmName] = ip
	if err := v.saveNet(network, ns); err != nil {
		return "", err
	}
	return ip, nil
}

func (v *Vagrant) NetworkRemove(ctx context.Context, name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	os.Remove(v.netFile(name))
}

// NetworkConnect / NetworkDisconnect are no-ops: a VM's networks are fixed in its
// Vagrantfile at create time.
func (v *Vagrant) NetworkConnect(ctx context.Context, network, container string) error { return nil }
func (v *Vagrant) NetworkDisconnect(ctx context.Context, network, container string)    {}

func (v *Vagrant) NetworkSubnet(ctx context.Context, name string) (string, error) {
	if ns, ok := v.loadNet(name); ok {
		return ns.Subnet, nil
	}
	return "", fmt.Errorf("network %q not found", name)
}

// --- Engine: volumes (no-op; VMs carry their own disks) --------------------

func (v *Vagrant) VolumeCreate(ctx context.Context, name string) error { return nil }
func (v *Vagrant) VolumeRemove(ctx context.Context, name string)       {}

// --- Engine: port publishing state -----------------------------------------

// portState maps host ports to the "vmName:guestPort" they forward to, shared
// across the fleet so auto-assigned host ports never collide.
type portState struct {
	Next  int            `json:"next"`  // next auto host port to hand out
	Ports map[string]int `json:"ports"` // "vmName/guestPort" -> hostPort
}

func (v *Vagrant) portFile() string { return filepath.Join(v.root, "ports.json") }

func (v *Vagrant) loadPorts() portState {
	ps := portState{Next: 20000, Ports: map[string]int{}}
	if b, err := os.ReadFile(v.portFile()); err == nil {
		json.Unmarshal(b, &ps)
		if ps.Ports == nil {
			ps.Ports = map[string]int{}
		}
		if ps.Next < 20000 {
			ps.Next = 20000
		}
	}
	return ps
}

func (v *Vagrant) savePorts(ps portState) {
	b, _ := json.MarshalIndent(ps, "", "  ")
	os.WriteFile(v.portFile(), b, 0o644)
}

// assignHostPort returns the host port forwarding to vmName's guest port, using
// desired when non-zero (explicit publish) or the next free auto port otherwise.
func (v *Vagrant) assignHostPort(vmName string, guest, desired int) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	ps := v.loadPorts()
	key := vmName + "/" + strconv.Itoa(guest)
	if hp, ok := ps.Ports[key]; ok {
		return hp
	}
	inUse := map[int]bool{}
	for _, hp := range ps.Ports {
		inUse[hp] = true
	}
	hp := desired
	if hp == 0 {
		hp = ps.Next
		for inUse[hp] {
			hp++
		}
		ps.Next = hp + 1
	}
	ps.Ports[key] = hp
	v.savePorts(ps)
	return hp
}

func (v *Vagrant) forgetPorts(vmName string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ps := v.loadPorts()
	for k := range ps.Ports {
		if strings.HasPrefix(k, vmName+"/") {
			delete(ps.Ports, k)
		}
	}
	v.savePorts(ps)
}

// --- Engine: container/VM lifecycle ----------------------------------------

// vmForward is one guest→host TCP forward for the Vagrantfile.
type vmForward struct{ Guest, Host int }

// ContainerCreate renders a single-machine Vagrantfile and `vagrant up`s it,
// returning the VM name as the id. The OS is recovered from the dbcanvas-systemd
// image tag and mapped to a box; a static IP is allocated on spec.Network.
func (v *Vagrant) ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error) {
	os_, version, ok := osFromImage(spec.Image)
	if !ok {
		return "", fmt.Errorf("vagrant backend cannot provision image %q (only dbcanvas-systemd OS nodes are supported)", spec.Image)
	}
	box, ok := vagrantBox(os_, version)
	if !ok {
		return "", fmt.Errorf("no vagrant box mapped for %s %s", os_, version)
	}
	if err := v.ensureBox(ctx, box); err != nil {
		return "", err
	}

	ip := spec.IPv4Address
	if ip == "" && spec.Network != "" {
		var err error
		if ip, err = v.allocIP(spec.Network, spec.Name); err != nil {
			return "", err
		}
	}

	var fwds []vmForward
	add := func(guest, host int) {
		if guest == 0 {
			return
		}
		fwds = append(fwds, vmForward{Guest: guest, Host: v.assignHostPort(spec.Name, guest, host)})
	}
	add(spec.PublishPort, 0)
	for _, p := range spec.PublishPorts {
		add(p, 0)
	}
	for _, pm := range spec.PublishMap {
		add(pm.ContainerPort, pm.HostPort)
	}

	dir := v.vmDir(spec.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	vf := renderVagrantfile(spec, box, ip, fwds)
	if err := os.WriteFile(filepath.Join(dir, "Vagrantfile"), []byte(vf), 0o644); err != nil {
		return "", err
	}

	if _, errb, err := v.vagrantCmd(ctx, spec.Name, "up", "--provider", "virtualbox"); err != nil {
		return "", fmt.Errorf("vagrant up %s: %v: %s", spec.Name, err, tail(errb, 800))
	}
	return spec.Name, nil
}

// renderVagrantfile builds a minimal single-machine Vagrantfile. DNS/resolv.conf is
// intentionally left to the generic provisioners (they point it at the Intranet once
// it is up), so bring-up keeps the box's own working resolver.
func renderVagrantfile(spec ContainerSpec, box vagrantBoxSpec, ip string, fwds []vmForward) string {
	host := spec.Hostname
	if host == "" {
		host = spec.Name
	}
	mem := envIntOr("DBCANVAS_VM_MEMORY", 2048)
	cpus := envIntOr("DBCANVAS_VM_CPUS", 2)

	var b strings.Builder
	fmt.Fprintf(&b, "Vagrant.configure(\"2\") do |config|\n")
	fmt.Fprintf(&b, "  config.vm.define %q do |m|\n", spec.Name)
	fmt.Fprintf(&b, "    m.vm.box = %q\n", box.Name)
	if box.URL != "" {
		fmt.Fprintf(&b, "    m.vm.box_url = %q\n", box.URL)
	}
	fmt.Fprintf(&b, "    m.vm.hostname = %q\n", host)
	if ip != "" {
		fmt.Fprintf(&b, "    m.vm.network \"private_network\", ip: %q, netmask: \"255.255.255.0\"\n", ip)
	}
	for _, f := range fwds {
		fmt.Fprintf(&b, "    m.vm.network \"forwarded_port\", guest: %d, host: %d, auto_correct: false\n", f.Guest, f.Host)
	}
	fmt.Fprintf(&b, "    m.vm.provider \"virtualbox\" do |vb|\n")
	fmt.Fprintf(&b, "      vb.name = %q\n", spec.Name)
	fmt.Fprintf(&b, "      vb.memory = %d\n", mem)
	fmt.Fprintf(&b, "      vb.cpus = %d\n", cpus)
	fmt.Fprintf(&b, "    end\n")
	fmt.Fprintf(&b, "  end\n")
	fmt.Fprintf(&b, "end\n")
	return b.String()
}

func (v *Vagrant) ContainerStart(ctx context.Context, id string) error {
	_, errb, err := v.vagrantCmd(ctx, id, "up")
	if err != nil {
		return fmt.Errorf("vagrant up %s: %v: %s", id, err, tail(errb, 400))
	}
	return nil
}

func (v *Vagrant) ContainerStop(ctx context.Context, id string) error {
	_, errb, err := v.vagrantCmd(ctx, id, "halt")
	if err != nil {
		return fmt.Errorf("vagrant halt %s: %v: %s", id, err, tail(errb, 400))
	}
	return nil
}

func (v *Vagrant) ContainerRestart(ctx context.Context, id string) error {
	_, errb, err := v.vagrantCmd(ctx, id, "reload")
	if err != nil {
		return fmt.Errorf("vagrant reload %s: %v: %s", id, err, tail(errb, 400))
	}
	return nil
}

// ContainerRemove destroys the VM and drops its directory and port reservations.
func (v *Vagrant) ContainerRemove(ctx context.Context, id string) {
	v.vagrantCmd(ctx, id, "destroy", "-f")
	v.forgetPorts(id)
	v.dropSSH(id)
	os.RemoveAll(v.vmDir(id))
}

// ContainerUpdate resizes the VM's CPU/memory (best-effort; VirtualBox needs the VM
// powered off, so this is applied only when it is not running).
func (v *Vagrant) ContainerUpdate(ctx context.Context, id string, nanoCPUs, memBytes int64) error {
	if st, _ := v.state(ctx, id); st == "running" {
		return nil // can't modifyvm a live VM; skip rather than force a reboot
	}
	if memBytes > 0 {
		v.run(ctx, v.root, v.vbox, "modifyvm", id, "--memory", strconv.FormatInt(memBytes/(1<<20), 10))
	}
	if nanoCPUs > 0 {
		if c := int(nanoCPUs / 1e9); c > 0 {
			v.run(ctx, v.root, v.vbox, "modifyvm", id, "--cpus", strconv.Itoa(c))
		}
	}
	return nil
}

// --- Engine: lookup / introspection ----------------------------------------

// ContainerByName treats the name as the id: a VM "exists" if its dir has a
// Vagrantfile and the machine is created.
func (v *Vagrant) ContainerByName(ctx context.Context, name string) (string, bool, error) {
	if _, err := os.Stat(filepath.Join(v.vmDir(name), "Vagrantfile")); err != nil {
		return "", false, nil
	}
	st, _ := v.state(ctx, name)
	if st == "not_created" || st == "" {
		return "", false, nil
	}
	return name, true, nil
}

func (v *Vagrant) ContainersByNamePrefix(ctx context.Context, prefix string) ([]string, error) {
	entries, _ := os.ReadDir(filepath.Join(v.root, "vms"))
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (v *Vagrant) ContainerIP(ctx context.Context, id, network string) (string, error) {
	if ns, ok := v.loadNet(network); ok {
		if ip, ok := ns.Hosts[id]; ok {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no ip for %s on %s", id, network)
}

// ContainerPort returns the host port forwarding to the given "<port>/tcp" guest
// port, matching Docker's ContainerPort contract (host port as a string).
func (v *Vagrant) ContainerPort(ctx context.Context, id, portProto string) (string, error) {
	guest, _, _ := strings.Cut(portProto, "/")
	v.mu.Lock()
	ps := v.loadPorts()
	v.mu.Unlock()
	if hp, ok := ps.Ports[id+"/"+guest]; ok {
		return strconv.Itoa(hp), nil
	}
	return "", fmt.Errorf("port %s not published for %s", portProto, id)
}

func (v *Vagrant) ListPublishedPorts(ctx context.Context) (map[int]string, error) {
	v.mu.Lock()
	ps := v.loadPorts()
	v.mu.Unlock()
	out := map[int]string{}
	for k, hp := range ps.Ports {
		out[hp] = k
	}
	return out, nil
}

// ListManaged lists every dbcanvas VM and its state, for the dashboard.
func (v *Vagrant) ListManaged(ctx context.Context) ([]ContainerInfo, error) {
	entries, _ := os.ReadDir(filepath.Join(v.root, "vms"))
	var out []ContainerInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		st, _ := v.state(ctx, name)
		out = append(out, ContainerInfo{ID: name, Name: name, State: vagrantStateToDocker(st)})
	}
	return out, nil
}

// ContainerStats has no cheap equivalent for a VM; report zeros (the dashboard
// treats these as "unknown" rather than failing).
func (v *Vagrant) ContainerStats(ctx context.Context, id string) (ContainerStat, error) {
	return ContainerStat{}, nil
}

// state returns vagrant's machine state ("running", "poweroff", "not_created", …).
func (v *Vagrant) state(ctx context.Context, id string) (string, error) {
	out, _, err := v.vagrantCmd(ctx, id, "status", "--machine-readable")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		cols := strings.Split(line, ",")
		if len(cols) >= 4 && cols[2] == "state" {
			return cols[3], nil
		}
	}
	return "", nil
}

func vagrantStateToDocker(st string) string {
	switch st {
	case "running":
		return "running"
	case "poweroff", "saved", "aborted":
		return "exited"
	default:
		return st
	}
}

// --- small utilities -------------------------------------------------------

// safeFile makes a network/VM name safe to embed in a filename.
func safeFile(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// tail returns the last n bytes of s (for trimming noisy command output).
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
