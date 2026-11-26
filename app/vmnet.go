package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// vmnet.go — a VM node on its stack's network.
//
// A stack's network is a Docker bridge, which a virtual machine cannot join. The stack's
// Intranet container carries it to the VM instead: DBCanvas runs dbcanvas-relay's tun mode in the
// Intranet (through the Docker API) and in the VM (over ssh) and copies packets between the two,
// so the VM has an address on the stack's subnet, the Intranet routes it and answers ARP for it,
// and containers reach the VM as a neighbour without any change of their own. Nothing changes on
// the host: no routes, no firewall rules, no ports.
//
// Each VM's link is supervised: when either end goes away — the Intranet restarts, the VM
// reboots, dbcanvas restarts — the link is set up again once both are back. The VM's addresses
// come from the top of the half of the subnet Docker assigns containers from; networks created
// since keep them out of Docker's hands (vmAuxAddresses).

// vmAddrCount is how many addresses a stack network keeps for VM nodes.
const vmAddrCount = 16

// vmAddresses is the addresses a stack network keeps for VM nodes, highest first: the top of
// Docker's half of the subnet.
func vmAddresses(subnet *net.IPNet) []net.IP {
	half := lowerHalf(subnet)
	ones, bits := half.Mask.Size()
	size := uint32(1) << (bits - ones)
	if size < 4*vmAddrCount {
		return nil // too small to share
	}
	top := binary.BigEndian.Uint32(half.IP.To4()) + size - 1
	out := make([]net.IP, 0, vmAddrCount)
	for i := uint32(0); i < vmAddrCount; i++ {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, top-i)
		out = append(out, ip)
	}
	return out
}

// vmAuxAddresses reserves vmAddresses with Docker's IPAM, so it never gives one to a container.
func vmAuxAddresses(subnet *net.IPNet) map[string]string {
	out := map[string]string{}
	for i, ip := range vmAddresses(subnet) {
		out[fmt.Sprintf("dbcanvas-vm-%d", i)] = ip.String()
	}
	return out
}

// pickVMAddress is the highest VM address not in used.
func pickVMAddress(subnet *net.IPNet, used map[string]bool) (string, bool) {
	for _, ip := range vmAddresses(subnet) {
		if !used[ip.String()] {
			return ip.String(), true
		}
	}
	return "", false
}

// networkIPs is the addresses Docker has given containers on a network.
func (d *Docker) networkIPs(ctx context.Context, name string) (map[string]bool, error) {
	resp, err := d.do(ctx, "GET", "/networks/"+name, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, errBody("inspect network", resp)
	}
	var out struct {
		Containers map[string]struct {
			IPv4Address string `json:"IPv4Address"`
		} `json:"Containers"`
	}
	if err := json.Unmarshal(drain(resp), &out); err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, c := range out.Containers {
		ip, _, _ := strings.Cut(c.IPv4Address, "/")
		used[ip] = true
	}
	return used, nil
}

// setAddress records a VM's address on a stack network.
func (l *Lima) setAddress(id, network, ip string) error {
	m, ok := l.loadMeta(id)
	if !ok {
		return fmt.Errorf("VM %s is unknown", id)
	}
	m.Network, m.IP = network, ip
	return l.saveMeta(id, m)
}

// vmAddress gives a VM an address on its stack's network, keeping the one it has.
func (a *App) vmAddress(ctx context.Context, stackID int64, id string) (ip, cidr string, err error) {
	netName := networkName(stackID)
	cidr, err = a.docker.NetworkSubnet(ctx, netName)
	if err != nil {
		return "", "", err
	}
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", err
	}
	if m, ok := a.lima.loadMeta(id); ok && m.Network == netName && m.IP != "" && subnet.Contains(net.ParseIP(m.IP)) {
		return m.IP, cidr, nil
	}
	used, err := a.docker.networkIPs(ctx, netName)
	if err != nil {
		return "", "", err
	}
	used[firstHost(subnet).String()] = true
	for other, m := range a.lima.managed() {
		if other != id && m.Network == netName && m.IP != "" {
			used[m.IP] = true
		}
	}
	ip, ok := pickVMAddress(subnet, used)
	if !ok {
		return "", "", fmt.Errorf("stack network %s has no address left for another VM (%d at most)", cidr, vmAddrCount)
	}
	return ip, cidr, a.lima.setAddress(id, netName, ip)
}

// --- the link ------------------------------------------------------------------

// vmRelayPath is where the relay is kept in a VM.
const vmRelayDir, vmRelayName = "/usr/local/libexec", "dbcanvas-relay"

// vmLink is one VM's supervised link to its stack network.
type vmLink struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	up     bool
	err    error         // why the last attempt failed
	change chan struct{} // closed and replaced whenever up or err changes
}

func (l *vmLink) set(up bool, err error) {
	l.mu.Lock()
	l.up, l.err = up, err
	close(l.change)
	l.change = make(chan struct{})
	l.mu.Unlock()
}

// wait blocks until the link is up, the timeout passes or ctx ends.
func (l *vmLink) wait(ctx context.Context, timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		l.mu.Lock()
		up, err, ch := l.up, l.err, l.change
		l.mu.Unlock()
		if up {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			if err != nil {
				return fmt.Errorf("VM's link to the stack network: %v", err)
			}
			return fmt.Errorf("VM's link to the stack network did not come up within %s", timeout)
		}
	}
}

// vmStack is the stack a VM belongs to, from its container name.
func (a *App) vmStack(id string) (int64, bool) {
	m, ok := a.lima.loadMeta(id)
	if !ok {
		return 0, false
	}
	return stackIDFromInstanceName(m.Name)
}

// ensureVMLink starts the VM's link unless it runs already.
func (a *App) ensureVMLink(id string) *vmLink {
	ctx, cancel := context.WithCancel(context.Background())
	l := &vmLink{cancel: cancel, change: make(chan struct{})}
	if old, loaded := a.vmLinks.LoadOrStore(id, l); loaded {
		cancel()
		return old.(*vmLink)
	}
	go a.superviseVMLink(ctx, id, l)
	return l
}

// stopVMLink ends the VM's link.
func (a *App) stopVMLink(id string) {
	if old, ok := a.vmLinks.LoadAndDelete(id); ok {
		old.(*vmLink).cancel()
	}
}

func (a *App) superviseVMLink(ctx context.Context, id string, l *vmLink) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.runVMLink(ctx, id, func() { l.set(true, nil) })
		if ctx.Err() != nil {
			return
		}
		l.set(false, err)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if err != nil {
			log.Printf("vm %s: link to the stack network: %v (again in %s)", id, err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// runVMLink sets the link up once and carries it until either end goes away.
func (a *App) runVMLink(ctx context.Context, id string, onUp func()) error {
	stackID, ok := a.vmStack(id)
	if !ok {
		return fmt.Errorf("VM %s belongs to no stack", id)
	}
	m, _ := a.lima.loadMeta(id)
	if m.IP == "" {
		return fmt.Errorf("VM %s has no address on its stack network", id)
	}
	intranetID, _, ok := a.intranetEndpoint(ctx, stackID)
	if !ok {
		return fmt.Errorf("the stack's Intranet is not running")
	}
	cidr, err := a.docker.NetworkSubnet(ctx, networkName(stackID))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The VM's end, over ssh.
	if err := a.installVMRelay(ctx, id, m.Arch); err != nil {
		return err
	}
	client, err := a.lima.sshClient(id)
	if err != nil {
		return err
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	vmIn, _ := sess.StdinPipe()
	vmOut, _ := sess.StdoutPipe()
	vmErr, _ := sess.StderrPipe()
	if err := sess.Start(limaRemoteCommand("", []string{vmRelayDir + "/" + vmRelayName, "tun", "dbc0", "vm", m.IP, cidr}, nil)); err != nil {
		return err
	}
	if err := waitRelayReady(ctx, vmErr); err != nil {
		return fmt.Errorf("VM end: %v", err)
	}

	// The Intranet's end, through the Docker API.
	if err := a.docker.installRelayBin(ctx, intranetID); err != nil {
		return err
	}
	ifname := "dbcvm" + m.IP[strings.LastIndex(m.IP, ".")+1:]
	ds, err := a.docker.StreamExec(ctx, intranetID, []string{relayBinDir + "/" + relayBinName, "tun", ifname, "router", m.IP}, nil)
	if err != nil {
		return err
	}
	conn, err := ds.ready(ctx, "the stack network (Intranet end)")
	if err != nil {
		forgetRelayBin(intranetID)
		return err
	}
	defer conn.Close()

	onUp()
	a.vmHostsEntry(ctx, id, m)
	done := make(chan error, 2)
	go func() { _, err := io.Copy(vmIn, conn); done <- err }()
	go func() { _, err := io.Copy(conn, vmOut); done <- err }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		if err == nil {
			err = io.EOF
		}
		return fmt.Errorf("link closed: %v", err)
	}
}

// waitRelayReady reads the relay's stderr until it announces readiness, and keeps draining it
// after, so the relay never blocks on a full pipe.
func waitRelayReady(ctx context.Context, stderr io.Reader) error {
	ready := make(chan error, 1)
	go func() {
		br := bufio.NewReader(stderr)
		var msg strings.Builder
		for {
			line, err := br.ReadString('\n')
			if line == relayOK {
				ready <- nil
				io.Copy(io.Discard, br)
				return
			}
			msg.WriteString(line)
			if err != nil {
				ready <- fmt.Errorf("relay ended: %s", firstNonEmpty(strings.TrimSpace(msg.String()), err.Error()))
				return
			}
		}
	}()
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(relayConnectTimeout):
		return fmt.Errorf("relay did not start within %s", relayConnectTimeout)
	}
}

// installVMRelay copies the relay into the VM unless it is there.
func (a *App) installVMRelay(ctx context.Context, id, arch string) error {
	path := vmRelayDir + "/" + vmRelayName
	if res, err := a.lima.Exec(ctx, id, []string{"test", "-x", path}, nil); err == nil && res.Code == 0 {
		return nil
	}
	bin, ok := relayBin(archOr(arch))
	if !ok {
		return fmt.Errorf("this build carries no relay for linux/%s (make relaybin)", archOr(arch))
	}
	return a.lima.CopyFile(ctx, id, vmRelayDir, vmRelayName, 0o755, bin)
}

// vmHostsScript gives the VM's hostname its stack address in /etc/hosts, as Docker does for a
// container — without it the name resolves to the VM's link-local addresses — and keeps
// cloud-init from writing the file over at the next boot.
const vmHostsScript = `set -e
if [ -d /etc/cloud/cloud.cfg.d ]; then printf 'manage_etc_hosts: false\n' > /etc/cloud/cloud.cfg.d/99-dbcanvas-hosts.cfg; fi
grep -v "[[:space:]]$HOST\$" /etc/hosts > /etc/hosts.dbcanvas || true
printf '%s\t%s\n' "$IP" "$HOST" >> /etc/hosts.dbcanvas
cat /etc/hosts.dbcanvas > /etc/hosts
rm -f /etc/hosts.dbcanvas`

// vmHostsEntry runs vmHostsScript; best-effort, the link is up either way.
func (a *App) vmHostsEntry(ctx context.Context, id string, m limaMeta) {
	if m.Hostname == "" || m.IP == "" {
		return
	}
	res, err := a.lima.Exec(ctx, id, []string{"bash", "-c", vmHostsScript}, []string{"HOST=" + m.Hostname, "IP=" + m.IP})
	if err != nil || res.Code != 0 {
		log.Printf("vm %s: /etc/hosts: %v %s", id, err, limaTail(res.Stderr, 200))
	}
}

// --- joining -------------------------------------------------------------------

// vmResolverScript makes the VM's /etc/resolv.conf a plain file the node's provisioning owns,
// as a container's is: nothing on the VM (systemd-resolved, NetworkManager, dhclient) rewrites it
// after.
const vmResolverScript = `set -e
if systemctl is-active -q systemd-resolved 2>/dev/null; then systemctl disable --now systemd-resolved; fi
if [ -d /etc/NetworkManager/conf.d ]; then
  printf '[main]\ndns=none\n' > /etc/NetworkManager/conf.d/90-dbcanvas-dns.conf
  systemctl reload NetworkManager 2>/dev/null || true
fi
if [ -d /etc/dhcp/dhclient-enter-hooks.d ]; then
  printf 'make_resolv_conf() { :; }\n' > /etc/dhcp/dhclient-enter-hooks.d/dbcanvas-resolv
fi
rm -f /etc/resolv.conf
printf '%s' "$CONF" > /etc/resolv.conf`

// joinVM puts a freshly created VM on its stack's network: an address, the link, its name in
// the Intranet's DNS, and the resolver the node's spec asks for.
func (a *App) joinVM(ctx context.Context, id string, spec ContainerSpec) error {
	stackID, ok := a.vmStack(id)
	if !ok {
		return fmt.Errorf("VM %s belongs to no stack", id)
	}
	ip, _, err := a.vmAddress(ctx, stackID, id)
	if err != nil {
		return err
	}
	if err := a.ensureVMLink(id).wait(ctx, 2*time.Minute); err != nil {
		return err
	}
	if len(spec.DNS) > 0 {
		var conf strings.Builder
		if len(spec.DNSSearch) > 0 {
			conf.WriteString("search " + strings.Join(spec.DNSSearch, " ") + "\n")
		}
		for _, ns := range spec.DNS {
			conf.WriteString("nameserver " + ns + "\n")
		}
		conf.WriteString("options ndots:1\n")
		res, err := a.lima.Exec(ctx, id, []string{"bash", "-c", vmResolverScript}, []string{"CONF=" + conf.String()})
		if err != nil {
			return err
		}
		if res.Code != 0 {
			return fmt.Errorf("point the VM at the Intranet's DNS: %s", limaTail(res.Stderr, 300))
		}
	}
	a.reconcileStackDNS(ctx, stackID)
	log.Printf("vm %s (%s) is on stack %d's network as %s", id, spec.Name, stackID, ip)
	return nil
}

// resumeVMLinks starts the links of the running VMs, when dbcanvas starts.
func (a *App) resumeVMLinks(ctx context.Context) {
	if a.lima == nil {
		return
	}
	all, err := a.lima.instances(ctx)
	if err != nil {
		return
	}
	for id, m := range a.lima.managed() {
		if in, ok := all[id]; ok && in.Status == "Running" && m.IP != "" {
			a.ensureVMLink(id)
		}
	}
}
