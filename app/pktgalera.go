package main

// pktgalera.go — PXC / Galera traffic, which is not the MySQL protocol.
//
// A PXC node carries four conversations, on four ports, and only one of them is the
// client/server protocol pktmysql.go decodes:
//
//	3306  clients ↔ server, MySQL protocol
//	4567  group communication (gcs/gcomm) — the cluster's heartbeat, quorum votes and
//	      write-set replication, TCP and sometimes UDP multicast. Every member talks to
//	      every other member on this port, continuously, forever.
//	4568  IST — Incremental State Transfer. A rejoining member catching up from the
//	      donor's writeset cache: cheap, bounded, and the outcome you want.
//	4444  SST — State Snapshot Transfer. A full physical copy of the dataset from a
//	      donor, streamed by xtrabackup/xbstream (or rsync, or mysqldump). Expensive,
//	      and on a big dataset the thing that makes a rejoin take an hour.
//
// Decoding these as MySQL would produce exactly the confident nonsense this tool is
// built to avoid: gcomm messages would parse as absurd result sets, and an xbstream
// byte stream would parse as anything at all. So they are classified by port, described
// by volume and direction, and — where the wire says something definite, like the
// xbstream magic at the head of an SST — named.
//
// The gcomm message format is internal to Galera and not a documented, stable protocol,
// so nothing here pretends to decode its fields. What matters operationally is visible
// without that: which members are talking, how much, how continuously, and whether the
// transport under them is losing packets — and that last one comes from the TCP layer,
// which is the same for every port.

import (
	"fmt"
	"strings"
)

// Galera's default ports. A PXC node in dbcanvas uses these; an All-in-One PXC instance
// uses its slot's ports instead (aioPortsFor), which is why the capture request carries
// a port→role map rather than assuming these constants.
const (
	galeraGCSPort = 4567
	galeraISTPort = 4568
	galeraSSTPort = 4444
)

// Port roles. pktRoleMySQL is the default; the rest mean "do not run the MySQL decoder".
const (
	pktRoleMySQL     = "mysql"
	pktRoleGaleraGCS = "galera-gcs"
	pktRoleGaleraIST = "galera-ist"
	pktRoleGaleraSST = "galera-sst"
)

// pktGaleraPortRoles is the role map for a classic PXC node.
func pktGaleraPortRoles(mysqlPort int) map[int]string {
	return map[int]string{
		mysqlPort:     pktRoleMySQL,
		galeraGCSPort: pktRoleGaleraGCS,
		galeraISTPort: pktRoleGaleraIST,
		galeraSSTPort: pktRoleGaleraSST,
	}
}

// pktRoleLabel is how a role appears in the protocol column.
func pktRoleLabel(role string) string {
	switch role {
	case pktRoleGaleraGCS:
		return "Galera/GCS"
	case pktRoleGaleraIST:
		return "Galera/IST"
	case pktRoleGaleraSST:
		return "Galera/SST"
	}
	return "MySQL"
}

// pktRoleDescription explains a role once, for the UI and the docs.
func pktRoleDescription(role string) string {
	switch role {
	case pktRoleGaleraGCS:
		return "group communication: heartbeats, quorum and write-set replication between all members"
	case pktRoleGaleraIST:
		return "incremental state transfer: a rejoining member catching up from the donor's writeset cache"
	case pktRoleGaleraSST:
		return "state snapshot transfer: a full physical copy of the dataset from a donor"
	}
	return "MySQL client/server protocol"
}

// pktGaleraDecode annotates one frame of Galera traffic.
//
// It reports volume and, for a state transfer, what the stream is and how much of it
// has crossed — the two facts that decide whether a rejoin is healthy or is about to
// take an hour. The first payload of an SST also raises an issue, because an SST
// starting is an operational event in its own right: the donor is now streaming its
// whole dataset.
func pktGaleraDecode(p *pktPacket, c *pktConn, dir *pktDirState, role string, payload []byte) {
	p.Proto = pktRoleLabel(role)
	dir.appBytes += len(payload)

	switch role {
	case pktRoleGaleraGCS:
		p.Info = fmt.Sprintf("Group communication, %d bytes", len(payload))

	case pktRoleGaleraIST:
		if !c.xferAnnounced {
			c.xferAnnounced, dir.xferStarted = true, true
			p.Issues = append(p.Issues,
				"Galera IST started — a member is catching up incrementally (the cheap path)")
			p.Info = fmt.Sprintf("IST stream begins, %d bytes", len(payload))
			return
		}
		p.Info = fmt.Sprintf("IST data, %d bytes (%s so far)", len(payload), pktBytes(dir.appBytes))

	case pktRoleGaleraSST:
		if !c.xferAnnounced {
			c.xferAnnounced, dir.xferStarted = true, true
			dir.xferFormat = pktSSTFormat(payload)
			c.xferFormat = dir.xferFormat
			p.Issues = append(p.Issues, fmt.Sprintf(
				"Galera SST started (%s) — the donor is streaming its full dataset", dir.xferFormat))
			p.Info = fmt.Sprintf("SST stream begins: %s, %d bytes", dir.xferFormat, len(payload))
			return
		}
		format := dir.xferFormat
		if format == "" {
			format = c.xferFormat // the other direction opened the transfer
		}
		p.Info = fmt.Sprintf("SST data (%s), %d bytes (%s so far)",
			format, len(payload), pktBytes(dir.appBytes))
		// A transfer past this size is the difference between a quick rejoin and an
		// outage-length one; worth one flag, not one per frame.
		if dir.appBytes >= pktBigResultBytes && !c.xferBig {
			c.xferBig = true
			p.Issues = append(p.Issues, fmt.Sprintf(
				"Galera SST is large — %s transferred so far on this connection", pktBytes(dir.appBytes)))
		}
	}
}

// pktSSTFormat names the stream at the head of an SST. The method is configurable
// (wsrep_sst_method), and which one ran is not otherwise visible in a capture.
func pktSSTFormat(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "XBSTCK01":
		return "xbstream / xtrabackup"
	case len(b) >= 8 && strings.HasPrefix(string(b), "@RSYNCD:"):
		return "rsync"
	case len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b:
		return "gzip stream"
	case len(b) >= 4 && string(b[:4]) == "\x28\xb5\x2f\xfd":
		return "zstd stream"
	case len(b) >= 3 && string(b[:3]) == "-- ":
		return "mysqldump (SQL text)"
	case pktMostlyPrintable(b[:min(len(b), 64)]):
		return "text stream"
	}
	return "binary stream"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
