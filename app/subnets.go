package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strconv"
	"strings"
)

// subnets.go — the address range of a stack's network.
//
// Left to itself, Docker gives every network a /16 from its default address pools (172.17–
// 172.31 and 192.168, unless the daemon is configured otherwise), which hold about 31 networks
// for the whole Docker host. On a host shared by many users — anydbver namespaces, compose
// projects, k3d clusters, other dbcanvas users — they run out, and creating a network fails
// with plenty of addresses to spare. So DBCanvas chooses each stack network's subnet itself: a
// /24, found free at the moment it is created. It avoids:
//
//   - every network on that Docker, whoever made it (Docker refuses an overlap);
//   - Docker's default address pools, all of them: a network placed inside one makes Docker
//     skip that whole pool, so a single /24 there would take a /16 away from everyone who lets
//     Docker choose (checked: with 172.22.5.0/24 taken, Docker's next network was 172.23/16);
//   - this host's own networks: its interfaces and, on Linux, its routes (the LAN, a VPN);
//   - the ranges Kubernetes uses inside a K3D or Kubeadm cluster, where a node network that
//     overlapped would break routing.
//
// It searches 10.0.0.0/8, or the ranges DBCANVAS_NETWORK_RANGE (networkRange in config.yaml)
// names, starting at a point derived from the instance name, so that users of one host spread
// out instead of all taking the first gap. Docker assigns containers the lower half of the
// /24; the upper half is left to K3D's MetalLB address blocks (pickMetalLBBlock). A network
// that exists keeps whatever subnet it has.

// stackPrefix is the size of a stack network.
const stackPrefix = 24

// k8sInternalRanges are the pod and service ranges inside DBCanvas's Kubernetes clusters:
// k3s's (K3D) and Kubeadm's (kubeadmPodCIDR, kubeadmServiceCIDR).
var k8sInternalRanges = []string{"10.42.0.0/16", "10.43.0.0/16", kubeadmPodCIDR, kubeadmServiceCIDR}

// dockerBuiltinPools are Docker's default address pools when the daemon sets none.
var dockerBuiltinPools = []string{"172.16.0.0/12", "192.168.0.0/16"}

// errNoSubnet is a search that found no free range.
var errNoSubnet = errors.New("no free address range for a stack network")

func mustCIDRs(list []string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range list {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(s)); err == nil && n.IP.To4() != nil {
			out = append(out, n)
		}
	}
	return out
}

// subnetSearchSpace is where stack networks are looked for.
func subnetSearchSpace() ([]*net.IPNet, error) {
	if v := strings.TrimSpace(os.Getenv("DBCANVAS_NETWORK_RANGE")); v != "" {
		space := mustCIDRs(strings.Split(v, ","))
		if len(space) == 0 {
			return nil, fmt.Errorf("DBCANVAS_NETWORK_RANGE %q: give IPv4 CIDRs, comma separated (e.g. 10.20.0.0/16)", v)
		}
		for _, n := range space {
			if ones, _ := n.Mask.Size(); ones > stackPrefix {
				return nil, fmt.Errorf("DBCANVAS_NETWORK_RANGE %s is smaller than a stack network (/%d)", n, stackPrefix)
			}
		}
		return space, nil
	}
	return mustCIDRs([]string{"10.0.0.0/8"}), nil
}

// DefaultPools are the daemon's default address pools.
func (d *Docker) DefaultPools(ctx context.Context) []*net.IPNet {
	resp, err := d.do(ctx, "GET", "/info", nil)
	if err == nil && resp.StatusCode == 200 {
		var info struct {
			DefaultAddressPools []struct {
				Base string `json:"Base"`
			} `json:"DefaultAddressPools"`
		}
		if json.Unmarshal(drain(resp), &info) == nil && len(info.DefaultAddressPools) > 0 {
			var bases []string
			for _, p := range info.DefaultAddressPools {
				bases = append(bases, p.Base)
			}
			return mustCIDRs(bases)
		}
	} else if err == nil {
		drain(resp)
	}
	return mustCIDRs(dockerBuiltinPools)
}

// namedNet is a Docker network's subnet, with the network's name.
type namedNet struct {
	Name string
	Net  *net.IPNet
}

// namedNetworkSubnets are the IPv4 subnets of every network on the daemon, by name.
func (d *Docker) namedNetworkSubnets(ctx context.Context) ([]namedNet, error) {
	resp, err := d.do(ctx, "GET", "/networks", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, errBody("list networks", resp)
	}
	var nets []struct {
		Name string `json:"Name"`
		IPAM struct {
			Config []struct {
				Subnet string `json:"Subnet"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	if err := json.Unmarshal(drain(resp), &nets); err != nil {
		return nil, err
	}
	var out []namedNet
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			for _, sn := range mustCIDRs([]string{c.Subnet}) {
				out = append(out, namedNet{n.Name, sn})
			}
		}
	}
	return out, nil
}

// NetworkSubnets are the subnets of every network on the daemon.
func (d *Docker) NetworkSubnets(ctx context.Context) ([]*net.IPNet, error) {
	named, err := d.namedNetworkSubnets(ctx)
	if err != nil {
		return nil, err
	}
	var out []*net.IPNet
	for _, n := range named {
		out = append(out, n.Net)
	}
	return out, nil
}

// hostSubnets are this host's own networks: its interfaces' and, on Linux, its routes'.
func hostSubnets() []*net.IPNet {
	var out []*net.IPNet
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
				_, masked, _ := net.ParseCIDR(n.String())
				out = append(out, masked)
			}
		}
	}
	if f, err := os.Open("/proc/net/route"); err == nil { // Linux
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) < 8 {
				continue
			}
			dst, err1 := strconv.ParseUint(fs[1], 16, 32)
			mask, err2 := strconv.ParseUint(fs[7], 16, 32)
			if err1 != nil || err2 != nil || mask == 0 {
				continue // the default route covers everything; it is not a network to avoid
			}
			ip, m := make(net.IP, 4), make(net.IPMask, 4)
			binary.LittleEndian.PutUint32(ip, uint32(dst))
			binary.LittleEndian.PutUint32(m, uint32(mask))
			out = append(out, &net.IPNet{IP: ip.Mask(m), Mask: m})
		}
	}
	return out
}

func overlaps(a, b *net.IPNet) bool { return a.Contains(b.IP) || b.Contains(a.IP) }

// pickSubnet returns the first /24 in space that overlaps nothing in avoid, searching from a
// point derived from seed and wrapping round.
func pickSubnet(space, avoid []*net.IPNet, seed string) (*net.IPNet, bool) {
	type block struct {
		base  uint32
		count uint32
	}
	var blocks []block
	total := uint32(0)
	for _, s := range space {
		ones, _ := s.Mask.Size()
		n := uint32(1) << (stackPrefix - ones)
		blocks = append(blocks, block{binary.BigEndian.Uint32(s.IP.To4()), n})
		total += n
	}
	if total == 0 {
		return nil, false
	}
	h := fnv.New32a()
	h.Write([]byte(seed))
	start := h.Sum32() % total
	mask := net.CIDRMask(stackPrefix, 32)
	for i := uint32(0); i < total; i++ {
		k := (start + i) % total
		for _, b := range blocks {
			if k >= b.count {
				k -= b.count
				continue
			}
			ip := make(net.IP, 4)
			binary.BigEndian.PutUint32(ip, b.base+k<<(32-stackPrefix))
			cand := &net.IPNet{IP: ip, Mask: mask}
			free := true
			for _, a := range avoid {
				if overlaps(cand, a) {
					free = false
					break
				}
			}
			if free {
				return cand, true
			}
			break
		}
	}
	return nil, false
}

// stackSubnetAvoid is everything a new stack network must not overlap.
func (d *Docker) stackSubnetAvoid(ctx context.Context) ([]*net.IPNet, error) {
	used, err := d.NetworkSubnets(ctx)
	if err != nil {
		return nil, err
	}
	avoid := append(used, d.DefaultPools(ctx)...)
	avoid = append(avoid, hostSubnets()...)
	return append(avoid, mustCIDRs(k8sInternalRanges)...), nil
}

// lowerHalf is the first half of a subnet, the range Docker assigns containers from.
func lowerHalf(n *net.IPNet) *net.IPNet {
	ones, bits := n.Mask.Size()
	return &net.IPNet{IP: n.IP, Mask: net.CIDRMask(ones+1, bits)}
}

// firstHost is a subnet's first usable address, the gateway's.
func firstHost(n *net.IPNet) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, binary.BigEndian.Uint32(n.IP.To4())+1)
	return ip
}
