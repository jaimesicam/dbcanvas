package main

// pktvalkeybus.go — the Valkey cluster bus, which is not RESP at all.
//
// A clustered Valkey node listens on two ports: the client port for RESP, and the client
// port + 10000 for a **binary gossip protocol** between nodes. That second port is where
// the cluster decides what it is — who is reachable, who owns which slots, and who becomes
// primary when one fails. None of it is visible on the client port, exactly like Galera's
// group communication and Patroni's etcd traffic.
//
// Unlike Galera's gcomm, this one is decodable without guessing: the message header is a
// documented fixed layout that begins with the four bytes "RCmb" and states its own
// length, version, type, sender name, epochs and replication offset. So the bus is
// decoded rather than merely described — the header is enough to say *who* is talking,
// *what* they are saying, and whether an election is in progress.
//
//	offset  size  field
//	     0     4  signature "RCmb"
//	     4     4  total length
//	     8     2  protocol version
//	    10     2  the sender's client port
//	    12     2  message type
//	    14     2  count (of gossip sections)
//	    16     8  currentEpoch
//	    24     8  configEpoch
//	    32     8  the sender's replication offset
//	    40    40  the sender's node name (40 hex characters)
//	    80  2048  the slots this sender claims, as a bitmap
//	  2128    40  the name of the primary this sender replicates, or all zeroes
//	  2168    46  the sender's IP, as text
//	   …          extensions, ports, flags, cluster state
//
// The slot bitmap is the most useful part after the type: a node that claims no slots is
// a replica, and the count of claimed slots is how you see a resharding in progress or an
// incomplete cluster.

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// The bus header, and the offsets worth reading.
const (
	valkeyBusSig     = "RCmb"
	valkeyBusHdrLen  = 2256 // the fixed header, before any gossip sections
	valkeyBusMinLen  = 2256
	valkeyBusSlotsAt = 80
	valkeyBusSlotsSz = 2048
)

// Message types, from the cluster's own definition.
const (
	busPing            = 0
	busPong            = 1
	busMeet            = 2
	busFail            = 3
	busPublish         = 4
	busFailoverAuthReq = 5
	busFailoverAuthAck = 6
	busUpdate          = 7
	busMFStart         = 8
	busModule          = 9
	busPublishShard    = 10
)

func valkeyBusTypeName(t uint16) string {
	switch t {
	case busPing:
		return "PING"
	case busPong:
		return "PONG"
	case busMeet:
		return "MEET"
	case busFail:
		return "FAIL"
	case busPublish:
		return "PUBLISH"
	case busFailoverAuthReq:
		return "FAILOVER_AUTH_REQUEST"
	case busFailoverAuthAck:
		return "FAILOVER_AUTH_ACK"
	case busUpdate:
		return "UPDATE"
	case busMFStart:
		return "MFSTART"
	case busModule:
		return "MODULE"
	case busPublishShard:
		return "PUBLISHSHARD"
	}
	return fmt.Sprintf("type %d", t)
}

// pktValkeyBusDir is one direction's bus state.
type pktValkeyBusDir struct {
	aligned  bool
	desynced bool
}

// pktValkeyBusConn is per-connection bus state.
type pktValkeyBusConn struct {
	sender     string
	msgs       int
	failNoted  bool
	voteNoted  bool
	epochNoted bool
	lastEpoch  uint64
}

func (c *pktConn) valkeyBusConn() *pktValkeyBusConn {
	if c.valkeybus == nil {
		c.valkeybus = &pktValkeyBusConn{}
	}
	return c.valkeybus
}

func (d *pktDirState) valkeyBusDir() *pktValkeyBusDir {
	if d.valkeybus == nil {
		d.valkeybus = &pktValkeyBusDir{}
	}
	return d.valkeybus
}

// pktValkeyBusDecode reads cluster-bus messages out of a direction's stream.
func pktValkeyBusDecode(p *pktPacket, c *pktConn, dir *pktDirState, payload []byte) {
	bc, bd := c.valkeyBusConn(), dir.valkeyBusDir()
	p.Proto = pktValkeyRoleLabel(pktRoleValkeyBus)
	dir.appBytes += len(payload)
	dir.buf = append(dir.buf, payload...)

	var infos []string
	for {
		if len(dir.buf) < 8 {
			break
		}
		// The signature is the anchor, and it is a strong one: four fixed bytes plus a
		// self-consistent length.
		if string(dir.buf[:4]) != valkeyBusSig {
			if !valkeyBusReanchor(dir) {
				bd.desynced, bd.aligned = true, false
				break
			}
			continue
		}
		total := int(binary.BigEndian.Uint32(dir.buf[4:]))
		if total < valkeyBusMinLen || total > 8<<20 {
			// Not a header after all.
			dir.buf = dir.buf[1:]
			bd.desynced = true
			continue
		}
		if len(dir.buf) < total {
			break // the rest is still arriving
		}
		msg := dir.buf[:total]
		dir.buf = dir.buf[total:]
		if len(dir.buf) == 0 {
			dir.buf = nil
		}
		bd.aligned = true
		bc.msgs++
		infos = append(infos, valkeyBusMessage(p, bc, msg))
	}

	switch {
	case len(infos) > 0:
		p.Info = strings.Join(infos, " | ")
		if len(infos) > 2 {
			p.Info = fmt.Sprintf("%s | +%d more", strings.Join(infos[:2], " | "), len(infos)-2)
		}
	case bd.desynced && !bd.aligned:
		p.Info = fmt.Sprintf("[framing lost] %d bytes, hunting for the next \"RCmb\" header", len(payload))
	case len(dir.buf) > 0:
		p.Info = fmt.Sprintf("[continuation] %d bytes, %d buffered of a cluster-bus message",
			len(payload), len(dir.buf))
	default:
		p.Info = fmt.Sprintf("Cluster bus data, %d bytes", len(payload))
	}
}

// valkeyBusReanchor hunts for the next "RCmb" signature.
func valkeyBusReanchor(dir *pktDirState) bool {
	if i := strings.Index(string(dir.buf), valkeyBusSig); i > 0 {
		dir.buf = dir.buf[i:]
		return true
	}
	// Keep only a tail: a signature cannot span more than four bytes, but a partial
	// message can, so keep enough to complete one.
	if len(dir.buf) > 4<<20 {
		dir.buf = dir.buf[len(dir.buf)-(1<<16):]
	}
	return false
}

// valkeyBusMessage decodes one message's header.
func valkeyBusMessage(p *pktPacket, bc *pktValkeyBusConn, msg []byte) string {
	typ := binary.BigEndian.Uint16(msg[12:])
	count := binary.BigEndian.Uint16(msg[14:])
	currentEpoch := binary.BigEndian.Uint64(msg[16:])
	configEpoch := binary.BigEndian.Uint64(msg[24:])
	offset := binary.BigEndian.Uint64(msg[32:])
	sender := strings.TrimRight(string(msg[40:80]), "\x00")
	name := valkeyBusTypeName(typ)
	if bc.sender == "" {
		bc.sender = sender
	}
	slots := valkeyBusSlotCount(msg)
	primary := strings.TrimRight(string(msg[2128:2168]), "\x00")

	p.Command = "bus " + name
	out := fmt.Sprintf("%s from %s", name, pktEllipsis(sender, 12))
	switch {
	case slots > 0:
		out += fmt.Sprintf(", claims %d slot(s)", slots)
	case primary != "" && strings.Trim(primary, "0") != "":
		out += ", replica of " + pktEllipsis(primary, 12)
	}
	out += fmt.Sprintf(", epoch %d/%d, offset %d, %d gossip section(s)",
		currentEpoch, configEpoch, offset, count)

	// The three message types that are events rather than heartbeats.
	switch typ {
	case busMeet:
		p.Issues = append(p.Issues, fmt.Sprintf(
			"MEET from %s — a node is being introduced into the cluster. This is how a cluster is formed or grown, and it is not something that happens on its own",
			pktEllipsis(sender, 12)))
	case busFail:
		if !bc.failNoted {
			bc.failNoted = true
			failing := ""
			if len(msg) >= valkeyBusHdrLen+40 {
				failing = strings.TrimRight(string(msg[valkeyBusHdrLen:valkeyBusHdrLen+40]), "\x00")
			}
			p.Issues = append(p.Issues, fmt.Sprintf(
				"FAIL message — %s is telling the cluster that %s is failed. A majority of primaries agreed it was unreachable, so its slots are now uncovered until a replica takes over. Clients hitting those slots get CLUSTERDOWN or a MOVED to a node that no longer serves them",
				pktEllipsis(sender, 12), pktEllipsis(failing, 12)))
		}
	case busFailoverAuthReq:
		if !bc.voteNoted {
			bc.voteNoted = true
			p.Issues = append(p.Issues, fmt.Sprintf(
				"FAILOVER_AUTH_REQUEST — a replica is asking the primaries to vote for its promotion (epoch %d). This is a cluster election: the slots it wants are unserved until it wins, and clients see CLUSTERDOWN or MOVED for them",
				currentEpoch))
		}
	case busFailoverAuthAck:
		p.Issues = append(p.Issues,
			"FAILOVER_AUTH_ACK — a primary has voted for a replica's promotion. Once a majority acks, the slots move and every client's cached slot map is stale")
	case busUpdate:
		p.Issues = append(p.Issues, fmt.Sprintf(
			"UPDATE — a node is being told its configuration is out of date (config epoch %d wins). This is how a node that was partitioned learns it no longer owns its slots",
			configEpoch))
	case busMFStart:
		p.Issues = append(p.Issues,
			"MFSTART — a manual failover is starting: the primary pauses writes so its replica can catch up completely before taking over. Coordinated, but writes stop for the duration")
	}
	// A rising currentEpoch is the cluster's own clock for configuration changes, and a
	// jump in it is worth one line.
	if bc.lastEpoch != 0 && currentEpoch > bc.lastEpoch && !bc.epochNoted {
		bc.epochNoted = true
		p.Issues = append(p.Issues, fmt.Sprintf(
			"Cluster epoch rose from %d to %d — the cluster's configuration changed, which is what a failover or a resharding leaves behind",
			bc.lastEpoch, currentEpoch))
	}
	if currentEpoch > bc.lastEpoch {
		bc.lastEpoch = currentEpoch
	}
	return out
}

// valkeyBusSlotCount counts the bits set in the sender's slot bitmap: how many of the
// 16 384 hash slots this node claims. Zero means a replica (or a primary that has been
// stripped of its slots, which is what a resharding looks like halfway through).
func valkeyBusSlotCount(msg []byte) int {
	if len(msg) < valkeyBusSlotsAt+valkeyBusSlotsSz {
		return 0
	}
	n := 0
	for _, b := range msg[valkeyBusSlotsAt : valkeyBusSlotsAt+valkeyBusSlotsSz] {
		for b != 0 {
			n += int(b & 1)
			b >>= 1
		}
	}
	return n
}
