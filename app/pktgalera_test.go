package main

// pktgalera_test.go — PXC's cluster ports.
//
// A Galera member's traffic is on four ports and only 3306 is the MySQL protocol.
// These tests pin two things: that a capture of a PXC target actually covers
// 4567/4568/4444, and that traffic on them is described rather than run through the
// MySQL decoder — which would turn group communication into invented result sets.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The filter is what decides whether the cluster's replication is in the capture at
// all. A PXC member's is not a database capture without Galera's ports.
func TestPktGaleraCaptureFilter(t *testing.T) {
	pxc := pktCapRequest{Port: 3306, Roles: pktGaleraPortRoles(3306)}
	got := pktBPF(pxc)
	for _, want := range []string{"port 3306", "port 4444", "port 4567", "port 4568"} {
		if !strings.Contains(got, want) {
			t.Errorf("PXC filter %q is missing %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "(") || !strings.Contains(got, " or ") {
		t.Errorf("several ports should be ORed inside one group: %q", got)
	}
	// Ascending order, so the command line is stable between runs.
	if got != "(port 3306 or port 4444 or port 4567 or port 4568)" {
		t.Errorf("filter = %q", got)
	}
	// A plain MySQL node keeps the single-port form.
	if got := pktBPF(pktCapRequest{Port: 3306, Roles: map[int]string{3306: pktRoleMySQL}}); got != "port 3306" {
		t.Errorf("plain MySQL filter = %q", got)
	}
	// An extra expression is still ANDed on, and cannot widen the port set.
	got = pktBPF(pktCapRequest{Port: 3306, Roles: pktGaleraPortRoles(3306), Filter: "host 10.0.0.7"})
	if !strings.Contains(got, ") and (host 10.0.0.7)") {
		t.Errorf("extra filter should be ANDed: %q", got)
	}
	// An All-in-One PXC instance is on slot ports, never the defaults.
	ports := aioPortsFor("pxc", 0, 0)
	aio := pktCapRequest{Port: ports.Client, Roles: map[int]string{
		ports.Client: pktRoleMySQL, ports.Group: pktRoleGaleraGCS,
		ports.IST: pktRoleGaleraIST, ports.SST: pktRoleGaleraSST,
	}}
	got = pktBPF(aio)
	for _, p := range []int{ports.Client, ports.Group, ports.IST, ports.SST} {
		if !strings.Contains(got, fmt.Sprintf("port %d", p)) {
			t.Errorf("All-in-One PXC filter %q is missing port %d", got, p)
		}
	}
	if strings.Contains(got, "4567") {
		t.Errorf("an All-in-One instance must not capture the default Galera ports: %q", got)
	}
}

// galeraFrame builds a frame on a Galera port. The "client" is the joining member.
func galeraFrame(port int, fromJoiner bool, seq uint32, payload []byte) []byte {
	if fromJoiner {
		return ethIPv4TCP(cliIP, srvIP, 55000, port, seq, 1, tcpACK|tcpPSH, 64240, payload)
	}
	return ethIPv4TCP(srvIP, cliIP, port, 55000, seq, 1, tcpACK|tcpPSH, 64240, payload)
}

// Group communication, IST and SST must be labelled by what they are — and must not be
// decoded as MySQL, which is what would happen if the port roles were not honoured.
func TestPktGaleraTrafficIsClassified(t *testing.T) {
	roles := pktGaleraPortRoles(3306)
	b := newPcap(pktLinkEther)
	// Group communication: continuous small messages between members.
	for i := 0; i < 3; i++ {
		b.frame(time.Duration(i)*time.Millisecond,
			galeraFrame(galeraGCSPort, true, uint32(1+i*40), append([]byte{0x00, 0x01, 0x02}, make([]byte, 37)...)))
	}
	// An IST stream.
	b.frame(10*time.Millisecond, galeraFrame(galeraISTPort, false, 1, make([]byte, 512)))
	b.frame(11*time.Millisecond, galeraFrame(galeraISTPort, false, 513, make([]byte, 512)))
	// An SST stream, xbstream-flavoured.
	sst := append([]byte("XBSTCK01"), make([]byte, 1000)...)
	b.frame(20*time.Millisecond, galeraFrame(galeraSSTPort, false, 1, sst))
	b.frame(21*time.Millisecond, galeraFrame(galeraSSTPort, false, uint32(1+len(sst)), make([]byte, 1400)))

	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: 3306, PortRoles: roles})
	if err != nil {
		t.Fatal(err)
	}
	protos := map[string]int{}
	for _, p := range d.Packets {
		protos[p.Proto]++
		if p.Query != "" || strings.Contains(p.Info, "Result set") || strings.Contains(p.Info, "OK:") {
			t.Errorf("#%d: Galera traffic was decoded as MySQL: proto=%s info=%q query=%q",
				p.No, p.Proto, p.Info, p.Query)
		}
	}
	for proto, want := range map[string]int{"Galera/GCS": 3, "Galera/IST": 2, "Galera/SST": 2} {
		if protos[proto] != want {
			t.Errorf("%s frames = %d, want %d (all: %v)", proto, protos[proto], want, protos)
		}
	}
	// The state transfers are announced, with the SST's stream format named.
	ist, sstStart, big := false, false, false
	for _, p := range d.Packets {
		for _, is := range p.Issues {
			switch {
			case strings.Contains(is, "IST started"):
				ist = true
			case strings.Contains(is, "SST started"):
				sstStart = true
				if !strings.Contains(is, "xbstream") {
					t.Errorf("the SST's stream format should be named: %q", is)
				}
			case strings.Contains(is, "SST is large"):
				big = true
			}
		}
	}
	if !ist || !sstStart {
		t.Errorf("state transfers not announced: ist=%v sst=%v", ist, sstStart)
	}
	if big {
		t.Error("a 2 KB transfer must not be called large")
	}
	// Cumulative volume is reported, which is the number that matters during a rejoin.
	found := false
	for _, p := range d.Packets {
		if strings.Contains(p.Info, "so far") {
			found = true
		}
	}
	if !found {
		t.Error("a state transfer should report how much has crossed so far")
	}
	// The streams carry their role for the connection picker.
	roleSeen := map[string]bool{}
	for _, s := range d.Streams {
		roleSeen[s.RoleLabel] = true
	}
	for _, want := range []string{"Galera/GCS", "Galera/IST", "Galera/SST"} {
		if !roleSeen[want] {
			t.Errorf("no stream labelled %s: %v", want, roleSeen)
		}
	}
}

// The SST method is configurable and invisible except at the head of the stream.
func TestPktSSTFormatDetection(t *testing.T) {
	for _, tc := range []struct{ head, want string }{
		{"XBSTCK01\x00\x00", "xbstream"},
		{"@RSYNCD: 31.0\n", "rsync"},
		{"\x1f\x8b\x08\x00", "gzip"},
		{"\x28\xb5\x2f\xfd\x00", "zstd"},
		{"-- MySQL dump 10.13", "mysqldump"},
		{"\x00\x01\x02\x03\x04\x05\x06\x07", "binary"},
	} {
		if got := pktSSTFormat([]byte(tc.head)); !strings.Contains(got, tc.want) {
			t.Errorf("pktSSTFormat(%q) = %q, want something containing %q", tc.head, got, tc.want)
		}
	}
}

// A large SST is flagged once, not once per frame: a 5 GB transfer would otherwise
// produce millions of identical issues.
func TestPktLargeSSTFlaggedOnce(t *testing.T) {
	b := newPcap(pktLinkEther)
	seq := uint32(1)
	head := append([]byte("XBSTCK01"), make([]byte, 1400)...)
	b.frame(0, galeraFrame(galeraSSTPort, false, seq, head))
	seq += uint32(len(head))
	for i := 0; i < 900; i++ { // ~1.3 MB, past the "large" threshold
		b.frame(time.Duration(i)*time.Millisecond, galeraFrame(galeraSSTPort, false, seq, make([]byte, 1400)))
		seq += 1400
	}
	d, err := pktDecode(b.buf, pktDecodeOpts{ServerPort: 3306, PortRoles: pktGaleraPortRoles(3306)})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range d.Packets {
		for _, is := range p.Issues {
			if strings.Contains(is, "SST is large") {
				n++
			}
		}
	}
	if n != 1 {
		t.Errorf("the large-SST flag was raised %d times, want exactly 1", n)
	}
}
