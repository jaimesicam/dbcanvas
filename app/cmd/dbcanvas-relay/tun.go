//go:build linux

package main

// tun.go — a virtual machine's share of a stack network, carried as packets over stdin and
// stdout. DBCanvas runs one end in the stack's Intranet container (through the Docker API) and
// the other in the VM (over ssh), and copies each end's stdout into the other's stdin, so the VM
// sits on the stack's Docker network with an address of its own:
//
//	dbcanvas-relay tun <ifname> router <vm-ip>         in the Intranet
//	dbcanvas-relay tun <ifname> vm <vm-ip> <stack-cidr> in the VM
//
// Each end opens a TUN device and sets it up. The router routes the VM's address into its
// device and answers ARP for it on the stack network (proxy ARP), so containers reach the VM as
// a neighbour without any change of their own. The VM takes the address and routes the stack's
// subnet into its device; when firewalld runs, the device goes in the trusted zone, as the
// stack's containers have no firewall between them either. Then it announces readiness on
// stderr, as the TCP relay does, and copies packets — a 2-byte length, then the packet — until
// either side closes. The device goes away with the process, so a restart sets it up again.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

func tunMain(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: dbcanvas-relay tun <ifname> router <vm-ip> | tun <ifname> vm <vm-ip> <stack-cidr>")
		return 2
	}
	name, role, ip := args[0], args[1], args[2]
	if net.ParseIP(ip).To4() == nil || len(name) > 15 {
		fmt.Fprintln(os.Stderr, "tun: bad interface name or address")
		return 2
	}
	dev, err := openTun(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tun: open:", err)
		return 3
	}
	switch {
	case role == "router":
		err = setupRouter(name, ip)
	case role == "vm" && len(args) == 4:
		err = setupVM(name, ip, args[3])
	default:
		err = fmt.Errorf("unknown role %q", role)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tun: setup:", err)
		return 4
	}
	os.Stderr.WriteString(ready)
	go func() {
		toStream(dev, os.Stdout)
		os.Exit(0)
	}()
	fromStream(os.Stdin, dev)
	return 0
}

func openTun(name string) (*os.File, error) {
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var ifr [40]byte
	copy(ifr[:16], name)
	binary.LittleEndian.PutUint16(ifr[16:], 0x0001|0x1000) // IFF_TUN | IFF_NO_PI
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x400454ca /* TUNSETIFF */, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		syscall.Close(fd)
		return nil, e
	}
	return os.NewFile(uintptr(fd), name), nil
}

func run(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sysctl writes a value under /proc/sys; pattern may hold a * (every interface).
func sysctl(pattern, value string) {
	paths, _ := filepath.Glob(filepath.Join("/proc/sys", pattern))
	for _, p := range paths {
		os.WriteFile(p, []byte(value), 0o644)
	}
}

func setupRouter(name, ip string) error {
	if err := run("ip", "link", "set", name, "up"); err != nil {
		return err
	}
	if err := run("ip", "route", "replace", ip+"/32", "dev", name); err != nil {
		return err
	}
	sysctl("net/ipv4/ip_forward", "1")
	sysctl("net/ipv4/conf/*/proxy_arp", "1")
	// Answer at once rather than after a random wait of up to 0.8 s.
	sysctl("net/ipv4/neigh/*/proxy_delay", "0")
	// The VM's packets arrive on the TUN device and leave on the stack network.
	sysctl("net/ipv4/conf/*/rp_filter", "0")
	return nil
}

func setupVM(name, ip, cidr string) error {
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"ip", "link", "set", name, "up"},
		{"ip", "addr", "replace", ip + "/32", "dev", name},
		{"ip", "route", "replace", cidr, "dev", name, "src", ip},
	} {
		if err := run(args...); err != nil {
			return err
		}
	}
	if exec.Command("firewall-cmd", "--state").Run() == nil {
		if err := run("firewall-cmd", "--zone=trusted", "--change-interface="+name); err != nil {
			return err
		}
	}
	return nil
}

// toStream copies packets from the device to w, each behind its length.
func toStream(dev *os.File, w io.Writer) {
	out := bufio.NewWriterSize(w, 1<<16)
	buf := make([]byte, 65535)
	var hdr [2]byte
	for {
		n, err := dev.Read(buf)
		if err != nil {
			return
		}
		binary.BigEndian.PutUint16(hdr[:], uint16(n))
		out.Write(hdr[:])
		out.Write(buf[:n])
		if out.Flush() != nil {
			return
		}
	}
}

// fromStream writes the packets read from r into the device.
func fromStream(r io.Reader, dev *os.File) {
	in := bufio.NewReaderSize(r, 1<<16)
	buf := make([]byte, 65535)
	var hdr [2]byte
	for {
		if _, err := io.ReadFull(in, hdr[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(hdr[:]))
		if _, err := io.ReadFull(in, buf[:n]); err != nil {
			return
		}
		dev.Write(buf[:n])
	}
}
