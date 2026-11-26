package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// lima.go — the Lima engine: nodes as virtual machines instead of containers.
//
// It implements Engine (engine.go) by driving the `limactl` command, and ssh for everything
// that runs inside a VM (lima_ssh.go). A node becomes one Lima instance, made from Lima's own
// template for the node's OS (oraclelinux-9, ubuntu-24.04, …), in plain mode: no shared folders,
// no port forwarding to the host and no containerd. On macOS the VM runs on Apple's
// Virtualization framework, on Linux on QEMU with KVM; a VM of the other architecture runs on
// QEMU under emulation.
//
// Lima keeps its instances under its own folder (LimaHome, by default <data folder>/lima), apart
// from the user's own Lima instances in ~/.lima. Every instance is named "dbc-" plus a hash of
// the node's container name — Lima keeps Unix sockets in the instance folder, and their paths
// must stay short — and the folder holds dbcanvas.json with the container name, so lookups by
// name work as they do on Docker. The instance name is the "container id" everywhere the Engine
// returns or takes one.
//
// Only nodes built on the dbcanvas-systemd OS images can run as a VM: the OS and architecture
// are read from the image tag, and the VM is brought to the same baseline the image has.

// limaMinVersion is the oldest limactl this engine drives: `template:` references and the
// plain mode it relies on are both there from 2.0.
var limaMinVersion = [3]int{2, 0, 0}

// limaPrefix starts every instance name this engine creates.
const limaPrefix = "dbc-"

// Lima is an Engine backed by limactl.
type Lima struct {
	limactl string // path to limactl
	ssh     string // path to ssh
	home    string // LIMA_HOME for every limactl call
	version string // limactl's version, for doctor

	createMu sync.Mutex // one `limactl create` at a time: parallel nodes share image downloads
	terms    sync.Map   // execID → *limaTerm, for ResizeExec; "ssh/"+id → limaSSHInfo
}

// limaMeta is what dbcanvas keeps in each instance's folder (dbcanvas.json).
type limaMeta struct {
	Name     string `json:"name"`     // the node's container name
	Hostname string `json:"hostname"` // the node's hostname
	OS       string `json:"os"`       // oraclelinux, ubuntu, debian
	Version  string `json:"version"`  // 9, 24.04, …
	Arch     string `json:"arch"`     // amd64, arm64
	Network  string `json:"network,omitempty"`
	IP       string `json:"ip,omitempty"` // the node's address on Network
}

// NewLima returns a Lima engine when limactl and ssh are installed and limactl is new enough,
// else nil and the reason (doctor shows it). home is Lima's folder for dbcanvas's instances.
func NewLima(home string) (*Lima, string) {
	lc, err := exec.LookPath("limactl")
	if err != nil {
		return nil, "limactl is not installed (https://lima-vm.io)"
	}
	sh, err := exec.LookPath("ssh")
	if err != nil {
		return nil, "ssh is not installed"
	}
	out, err := exec.Command(lc, "--version").Output()
	if err != nil {
		return nil, fmt.Sprintf("limactl --version: %v", err)
	}
	ver := parseLimaVersion(string(out))
	if !versionAtLeast(ver, limaMinVersion) {
		return nil, fmt.Sprintf("limactl %s is too old: dbcanvas needs %d.%d.%d or newer", strings.TrimSpace(string(out)), limaMinVersion[0], limaMinVersion[1], limaMinVersion[2])
	}
	if reason := limaHomeProblem(home); reason != "" {
		return nil, reason
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Sprintf("create %s: %v", home, err)
	}
	return &Lima{limactl: lc, ssh: sh, home: home, version: fmt.Sprintf("%d.%d.%d", ver[0], ver[1], ver[2])}, ""
}

// limaHomeFor is the Lima folder dbcanvas uses: DBCANVAS_LIMA_HOME, else <data folder>/lima.
func limaHomeFor(dataFolder string) string {
	if h := strings.TrimSpace(os.Getenv("DBCANVAS_LIMA_HOME")); h != "" {
		return h
	}
	return filepath.Join(dataFolder, "lima")
}

// limaSocketMax is the longest Unix socket path that works everywhere (macOS allows 104 bytes,
// Linux 108); Lima keeps its sockets in the instance folder.
const limaSocketMax = 100

// limaHomeProblem says why home is too long for Lima's sockets, or "".
func limaHomeProblem(home string) string {
	// The longest socket Lima keeps is <home>/<instance>/ssh.sock (and ha.sock, serial*.sock).
	longest := len(home) + 1 + len(limaInstanceName("x")) + len("/serialv.sock")
	if longest > limaSocketMax {
		return fmt.Sprintf("the Lima folder %s is too long for Lima's sockets: set DBCANVAS_LIMA_HOME to a shorter path", home)
	}
	return ""
}

// limaInstanceName is the Lima instance for a node's container name: short and stable.
func limaInstanceName(containerName string) string {
	sum := sha256.Sum256([]byte(containerName))
	return limaPrefix + hex.EncodeToString(sum[:])[:12]
}

// parseLimaVersion reads "limactl version 2.3.0" (or "2.3.0-beta.1") into numbers.
func parseLimaVersion(s string) [3]int {
	var v [3]int
	f := strings.Fields(s)
	if len(f) == 0 {
		return v
	}
	core, _, _ := strings.Cut(strings.TrimPrefix(f[len(f)-1], "v"), "-")
	for i, p := range strings.SplitN(core, ".", 3) {
		v[i], _ = strconv.Atoi(p)
	}
	return v
}

func versionAtLeast(v, min [3]int) bool {
	for i := range v {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

// --- images → Lima templates ------------------------------------------------

// limaTemplates maps "os/version" to the Lima template a VM is made from, over the OS list
// dbcanvas builds its node images for. DBCANVAS_LIMA_TEMPLATE_<OS>_<VER> (dots as underscores)
// overrides one, e.g. DBCANVAS_LIMA_TEMPLATE_UBUNTU_24_04=/path/to/own.yaml.
var limaTemplates = map[string]string{
	"oraclelinux/8":  "template:oraclelinux-8",
	"oraclelinux/9":  "template:oraclelinux-9",
	"oraclelinux/10": "template:oraclelinux-10",
	"ubuntu/22.04":   "template:ubuntu-22.04",
	"ubuntu/24.04":   "template:ubuntu-24.04",
	"debian/12":      "template:debian-12",
	"debian/13":      "template:debian-13",
}

func limaTemplate(os_, version string) (string, bool) {
	env := "DBCANVAS_LIMA_TEMPLATE_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(os_+"_"+version))
	if t := strings.TrimSpace(os.Getenv(env)); t != "" {
		return t, true
	}
	t, ok := limaTemplates[os_+"/"+version]
	return t, ok
}

// systemdImageParts reads os, version and arch from a node image of the dbcanvas-systemd kind:
// "[registry/…/]dbcanvas-systemd:<os>-<version>-<arch>-<release>". ok is false for any other
// image, which a VM cannot stand in for.
func systemdImageParts(image string) (os_, version, arch string, ok bool) {
	repo, tag, found := strings.Cut(image[strings.LastIndex(image, "/")+1:], ":")
	if !found || repo != "dbcanvas-systemd" {
		return "", "", "", false
	}
	p := strings.Split(tag, "-")
	if len(p) < 3 || p[0] == "" || p[1] == "" || (p[2] != "amd64" && p[2] != "arm64") {
		return "", "", "", false
	}
	return p[0], p[1], p[2], true
}

// limaArch is Lima's name for an architecture.
func limaArch(arch string) string {
	if arch == "amd64" {
		return "x86_64"
	}
	return "aarch64"
}

// limaVMType is the VM type for arch on this host: Lima's default for the host's own
// architecture (Apple's Virtualization framework on macOS, QEMU with KVM on Linux), QEMU under
// emulation for the other.
func limaVMType(arch string) string {
	if arch == runtime.GOARCH {
		return ""
	}
	return "qemu"
}

// Default size of a VM whose node does not set one.
const (
	limaDefaultCPUs     = 2
	limaDefaultMemoryMB = 2048
)

// limaCreateArgs is the `limactl create` command line for a node.
func limaCreateArgs(name, template, arch string, cpus, memoryMB int) []string {
	if cpus <= 0 {
		cpus = limaDefaultCPUs
	}
	if memoryMB <= 0 {
		memoryMB = limaDefaultMemoryMB
	}
	args := []string{"create", "--tty=false", "--plain", "--name=" + name,
		"--arch=" + limaArch(arch),
		"--cpus=" + strconv.Itoa(cpus),
		"--memory=" + strconv.FormatFloat(float64(memoryMB)/1024, 'f', -1, 64),
		// The user's own ~/.ssh keys stay out of the VM: dbcanvas reaches it with Lima's key.
		"--set", ".ssh.loadDotSSHPubKeys = false",
	}
	if vt := limaVMType(arch); vt != "" {
		args = append(args, "--vm-type="+vt)
	}
	return append(args, template)
}

// --- the baseline -----------------------------------------------------------

// The tooling the dbcanvas-systemd images bake in (images/rhel.Dockerfile,
// images/debian.Dockerfile), installed on a freshly booted VM: percona-release above all, which
// every database provisioner expects to find. TestLimaBaselineMatchesImages keeps the package
// lists in step with the Dockerfiles. systemd itself and the container unit trimming are left
// out: a VM boots systemd as it is.
const limaBaselineRHEL = `set -e
grep -q '^ip_resolve=' /etc/dnf/dnf.conf 2>/dev/null || echo 'ip_resolve=4' >> /etc/dnf/dnf.conf
yum -y install net-tools openldap-clients sysstat git iproute-tc which hostname sudo
rpm -q percona-release >/dev/null 2>&1 || yum -y install https://repo.percona.com/yum/percona-release-latest.noarch.rpm
percona-release setup pt
percona-release enable tools release
yum -y install percona-toolkit
yum clean all`

const limaBaselineDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends net-tools ldap-utils sysstat git iproute2 sudo wget gnupg2 lsb-release ca-certificates
if ! dpkg -s percona-release >/dev/null 2>&1; then
  wget -qO /tmp/percona-release.deb "https://repo.percona.com/apt/percona-release_latest.$(lsb_release -sc)_all.deb"
  apt-get install -y /tmp/percona-release.deb
  rm -f /tmp/percona-release.deb
fi
percona-release setup pt
apt-get update
apt-get install -y --no-install-recommends percona-toolkit
apt-get clean`

func limaBaselineScript(os_ string) string {
	if isDebianOS(os_) {
		return limaBaselineDebian
	}
	return limaBaselineRHEL
}

// --- running limactl --------------------------------------------------------

// run runs limactl with LIMA_HOME set, returning stdout and stderr.
func (l *Lima) run(ctx context.Context, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, l.limactl, args...)
	cmd.Env = append(os.Environ(), "LIMA_HOME="+l.home)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func (l *Lima) dir(id string) string { return filepath.Join(l.home, id) }

func (l *Lima) metaPath(id string) string { return filepath.Join(l.dir(id), "dbcanvas.json") }

func (l *Lima) loadMeta(id string) (limaMeta, bool) {
	b, err := os.ReadFile(l.metaPath(id))
	if err != nil {
		return limaMeta{}, false
	}
	var m limaMeta
	return m, json.Unmarshal(b, &m) == nil && m.Name != ""
}

func (l *Lima) saveMeta(id string, m limaMeta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.metaPath(id), b, 0o600)
}

// limaInstance is one row of `limactl list --json`.
type limaInstance struct {
	Name   string `json:"name"`
	Status string `json:"status"` // Running, Stopped, Broken, …
}

// instances lists the instances in the Lima folder by name.
func (l *Lima) instances(ctx context.Context) (map[string]limaInstance, error) {
	out, errb, err := l.run(ctx, "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("limactl list: %v: %s", err, limaTail(errb, 300))
	}
	return parseLimaList(out), nil
}

// parseLimaList reads `limactl list --json`: one JSON object per line.
func parseLimaList(out string) map[string]limaInstance {
	res := map[string]limaInstance{}
	for _, line := range strings.Split(out, "\n") {
		var in limaInstance
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &in) == nil && in.Name != "" {
			res[in.Name] = in
		}
	}
	return res
}

// state is an instance's state in Docker's words: running, exited, or "" when it is gone.
func (l *Lima) state(ctx context.Context, id string) string {
	all, err := l.instances(ctx)
	if err != nil {
		return ""
	}
	in, ok := all[id]
	if !ok {
		return ""
	}
	return limaStateToDocker(in.Status)
}

func limaStateToDocker(st string) string {
	switch st {
	case "Running":
		return "running"
	case "Stopped":
		return "exited"
	default:
		return strings.ToLower(st)
	}
}

// --- Engine: host facts and images ------------------------------------------

func (l *Lima) Ping(ctx context.Context) error {
	_, errb, err := l.run(ctx, "list", "--json")
	if err != nil {
		return fmt.Errorf("limactl: %v: %s", err, limaTail(errb, 200))
	}
	return nil
}

// HostArch is the machine's own architecture in Docker's words (x86_64, aarch64).
func (l *Lima) HostArch(ctx context.Context) string { return limaArch(runtime.GOARCH) }

// HostResources is unknown (zeroes): callers then skip their capacity checks.
func (l *Lima) HostResources(ctx context.Context) (int, int64) { return 0, 0 }

// Lima downloads a template's image itself when an instance is created, and caches it.
func (l *Lima) ImageExists(ctx context.Context, ref string) (bool, error)         { return true, nil }
func (l *Lima) EnsureImage(ctx context.Context, repo, tag, platform string) error { return nil }
func (l *Lima) ImageRemove(ctx context.Context, ref string)                       {}

// --- Engine: networks and volumes -------------------------------------------
//
// A VM joins its stack's Docker network through the Intranet, so there is no Lima-side network
// to create, and a VM keeps its data on its own disk.

func (l *Lima) NetworkEnsure(ctx context.Context, name string) error                { return nil }
func (l *Lima) NetworkRemove(ctx context.Context, name string)                      {}
func (l *Lima) NetworkConnect(ctx context.Context, network, container string) error { return nil }
func (l *Lima) NetworkDisconnect(ctx context.Context, network, container string)    {}
func (l *Lima) NetworkSubnet(ctx context.Context, name string) (string, error) {
	return "", fmt.Errorf("network %s: subnets belong to Docker", name)
}
func (l *Lima) VolumeCreate(ctx context.Context, name string) error { return nil }
func (l *Lima) VolumeRemove(ctx context.Context, name string)       {}

// --- Engine: lifecycle -------------------------------------------------------

// limaBootTimeout bounds `limactl start`: an emulated VM takes minutes to boot.
func limaBootTimeout(arch string) time.Duration {
	if arch != runtime.GOARCH {
		return 30 * time.Minute
	}
	return 10 * time.Minute
}

// ContainerCreate makes and boots a VM for the node and brings it to the image's baseline. The
// returned id is the Lima instance name.
func (l *Lima) ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error) {
	os_, version, arch, ok := systemdImageParts(spec.Image)
	if !ok {
		return "", fmt.Errorf("a VM cannot stand in for image %q: only nodes built on the dbcanvas-systemd OS images can run as a VM", spec.Image)
	}
	if p, err := normalizePlatform(spec.Platform); err == nil {
		arch = p
	}
	template, ok := limaTemplate(os_, version)
	if !ok {
		return "", fmt.Errorf("no Lima template for %s %s", os_, version)
	}
	id := limaInstanceName(spec.Name)
	if _, ok := l.loadMeta(id); ok {
		return "", fmt.Errorf("a VM for %s exists already (%s)", spec.Name, id)
	}

	l.createMu.Lock()
	_, errb, err := l.run(ctx, limaCreateArgs(id, template, arch, spec.CPUs, spec.MemoryMB)...)
	l.createMu.Unlock()
	if err != nil {
		l.ContainerRemove(ctx, id)
		return "", fmt.Errorf("limactl create %s (%s): %v: %s", spec.Name, template, err, limaTail(errb, 800))
	}
	meta := limaMeta{Name: spec.Name, Hostname: spec.Hostname, OS: os_, Version: version, Arch: arch, Network: spec.Network, IP: spec.IPv4Address}
	if err := l.saveMeta(id, meta); err != nil {
		l.ContainerRemove(ctx, id)
		return "", err
	}
	if err := l.start(ctx, id, arch); err != nil {
		l.ContainerRemove(ctx, id)
		return "", err
	}
	if err := l.baseline(ctx, id, meta); err != nil {
		l.ContainerRemove(ctx, id)
		return "", err
	}
	return id, nil
}

func (l *Lima) start(ctx context.Context, id, arch string) error {
	timeout := limaBootTimeout(arch)
	_, errb, err := l.run(ctx, "start", "--tty=false", "--timeout="+timeout.String(), id)
	if err != nil {
		return fmt.Errorf("limactl start %s: %v: %s", id, err, limaTail(errb, 800))
	}
	l.dropSSH(id)
	return nil
}

// baseline sets the hostname and installs the image's tooling, retrying the way a flaky mirror
// needs. Idempotent, so a redeploy runs it again harmlessly.
func (l *Lima) baseline(ctx context.Context, id string, m limaMeta) error {
	script := limaBaselineScript(m.OS)
	if m.Hostname != "" {
		script = "hostnamectl set-hostname " + shellQuote(m.Hostname) + "\n" + script
	}
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		res, err := l.Exec(ctx, id, []string{"bash", "-c", script}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("exit %d: %s", res.Code, limaTail(strings.TrimSpace(res.Stderr+"\n"+res.Stdout), 400))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("prepare VM %s: %v", m.Name, last)
}

func (l *Lima) ContainerStart(ctx context.Context, id string) error {
	if l.state(ctx, id) == "running" {
		return nil
	}
	m, _ := l.loadMeta(id)
	return l.start(ctx, id, m.Arch)
}

func (l *Lima) ContainerStop(ctx context.Context, id string) error {
	if l.state(ctx, id) != "running" {
		return nil
	}
	_, errb, err := l.run(ctx, "stop", id)
	l.dropSSH(id)
	if err != nil {
		return fmt.Errorf("limactl stop %s: %v: %s", id, err, limaTail(errb, 400))
	}
	return nil
}

func (l *Lima) ContainerRestart(ctx context.Context, id string) error {
	if err := l.ContainerStop(ctx, id); err != nil {
		return err
	}
	return l.ContainerStart(ctx, id)
}

// ContainerRemove deletes the VM, its disk and its folder. Best-effort, and detached from the
// caller's context: teardown often runs right after a deploy was cancelled.
func (l *Lima) ContainerRemove(ctx context.Context, id string) {
	if !strings.HasPrefix(id, limaPrefix) {
		return // never touch an instance this engine did not create
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if _, errb, err := l.run(ctx, "delete", "--force", id); err != nil && !strings.Contains(errb, "not found") && !strings.Contains(errb, "No instance") {
		log.Printf("lima: delete %s: %v: %s", id, err, limaTail(errb, 300))
	}
	l.dropSSH(id)
	os.RemoveAll(l.dir(id))
}

// ContainerUpdate is not applied to a VM: its CPUs and memory are fixed while it runs.
func (l *Lima) ContainerUpdate(ctx context.Context, id string, nanoCPUs, memBytes int64) error {
	return nil
}

// --- Engine: lookup ----------------------------------------------------------

// managed is every instance this engine made, by id, with its metadata.
func (l *Lima) managed() map[string]limaMeta {
	out := map[string]limaMeta{}
	entries, _ := os.ReadDir(l.home)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), limaPrefix) {
			continue
		}
		if m, ok := l.loadMeta(e.Name()); ok {
			out[e.Name()] = m
		}
	}
	return out
}

func (l *Lima) ContainerByName(ctx context.Context, name string) (string, bool, error) {
	id := limaInstanceName(name)
	if m, ok := l.loadMeta(id); ok && m.Name == name {
		return id, true, nil
	}
	return "", false, nil
}

func (l *Lima) ContainersByNamePrefix(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	for id, m := range l.managed() {
		if strings.HasPrefix(m.Name, prefix) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ContainerIP is the VM's address on the stack network.
func (l *Lima) ContainerIP(ctx context.Context, id, network string) (string, error) {
	if m, ok := l.loadMeta(id); ok && m.IP != "" && (network == "" || m.Network == network) {
		return m.IP, nil
	}
	return "", fmt.Errorf("VM %s has no address on %s", id, network)
}

// A VM publishes no ports to the host: dbcanvas reaches it over ssh.
func (l *Lima) ContainerPort(ctx context.Context, id, portProto string) (string, error) {
	return "", fmt.Errorf("port %s is not published for VM %s", portProto, id)
}
func (l *Lima) ContainerPorts(ctx context.Context, id string) (map[int]int, error) {
	return map[int]int{}, nil
}
func (l *Lima) ListPublishedPorts(ctx context.Context) (map[int]string, error) {
	return map[int]string{}, nil
}

// ListManaged lists every VM this engine made, with its state, by container name.
func (l *Lima) ListManaged(ctx context.Context) ([]ContainerInfo, error) {
	all, err := l.instances(ctx)
	if err != nil {
		return nil, err
	}
	var out []ContainerInfo
	for id, m := range l.managed() {
		st := "missing"
		if in, ok := all[id]; ok {
			st = limaStateToDocker(in.Status)
		}
		out = append(out, ContainerInfo{ID: id, Name: m.Name, State: st})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ContainerStats has no cheap equivalent yet; zeroes read as "unknown".
func (l *Lima) ContainerStats(ctx context.Context, id string) (ContainerStat, error) {
	return ContainerStat{}, nil
}

// limaTail is the last n bytes of s.
func limaTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// Compile-time proof Lima implements Engine.
var _ Engine = (*Lima)(nil)
