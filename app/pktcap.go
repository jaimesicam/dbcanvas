package main

// pktcap.go — running tcpdump on a deployed node and getting the bytes back.
//
// The Packet Inspector captures where the traffic is: inside the database node's
// own container, on the interface that carries its stack address. That is the only
// vantage point that sees what the server actually received — a capture taken from
// the app would miss everything, since the app is not on the path between a client
// node and the server.
//
// The command is the one a DBA would type, and is shown in the UI verbatim:
//
//	tcpdump -i eth0 -s 65535 -n -q port 3306 -w /var/tmp/dbcanvas-pkt/<id>.cap
//
// Three things make it survive being run this way rather than from a shell:
//
//   - setsid + nohup, because an exec'd child dies with its exec session (the same
//     lesson the labs learned — see the labs exec notes in the repo history).
//   - a hard stop: -G/-W would rotate files forever, so every capture carries both
//     a duration (`timeout`) and a packet ceiling (`-c`), whichever comes first.
//   - the pcap is read back over the exec channel as base64 (readContainerFile),
//     because the Engine interface can write files into a container but has no way
//     to read one out.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// pktCapDir is where captures live inside the node. /var/tmp survives a systemd
// container restart, unlike /run, and is not on the database's data volume.
const pktCapDir = "/var/tmp/dbcanvas-pkt"

// Capture ceilings. A capture is bounded three ways at once — time, packets and bytes —
// because any one of them alone fails on some real workload: an hour on an idle node
// yields nothing, 100k packets on a busy one arrives in seconds, and a big-blob workload
// can blow past a size budget while still under both other limits.
const (
	// pktMaxSeconds is the longest capture. An hour is enough to sit and wait for an
	// intermittent problem, which is the whole reason to want more than a minute.
	pktMaxSeconds = 3600
	// pktMaxCapPackets is the highest -c ceiling. It is the limit that actually bounds
	// a long capture on a busy server, and it is well inside what the decoder and the
	// browser handle comfortably.
	pktMaxCapPackets = 100000
	// pktMaxCapBytes caps what will be pulled back and decoded (base64 over an exec
	// channel, then held in memory). The poll loop stops a capture that reaches this
	// rather than letting it run to a file that cannot be fetched.
	pktMaxCapBytes = 192 << 20
)

// pktSafeName is what may appear in a capture id used inside a shell command.
var pktSafeName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// pktIfaceScript picks the interface to listen on: the one holding the address the
// rest of the stack reaches this node by. `-i any` would also work but yields
// Linux-cooked frames and duplicates loopback traffic, and a hard-coded eth0 is
// wrong on a Vagrant node, where the stack network is eth1.
const pktIfaceScript = `set -e
IF=""
# The interface owning the default route's source address is the stack-facing one.
IP=$(ip -o -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.*src \([0-9.]*\).*/\1/p')
[ -n "$IP" ] && IF=$(ip -o -4 addr show | awk -v ip="$IP" '$4 ~ "^"ip"/" {print $2; exit}')
# Fall back to the first non-loopback interface with an address.
[ -z "$IF" ] && IF=$(ip -o -4 addr show scope global | awk '{print $2; exit}')
[ -z "$IF" ] && IF=lo
echo "$IF"`

// pktInstallScript makes sure tcpdump exists. Nodes are provisioned without it —
// it is a diagnostic tool, not a database dependency — so the first capture on a
// node installs it and later ones find it.
const pktInstallScript = `set -e
command -v tcpdump >/dev/null 2>&1 && exit 0
if command -v dnf >/dev/null 2>&1; then
  dnf -y -q install tcpdump >/dev/null 2>&1
elif command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >/dev/null 2>&1 || true
  apt-get install -y -qq tcpdump >/dev/null 2>&1
fi
command -v tcpdump >/dev/null 2>&1 || { echo "tcpdump could not be installed on this node"; exit 1; }
exit 0`

// pktStartScript launches one capture, detached, and returns its pid.
//
// tcpdump drops privileges to the `tcpdump` user on Red Hat builds, which cannot
// write into a root-owned directory — hence the world-writable capture dir. The
// file is pre-created and made writable for the same reason.
const pktStartScript = `set -e
mkdir -p "$DIR"
chmod 1777 "$DIR"
: > "$DIR/$ID.cap"
chmod 666 "$DIR/$ID.cap"
setsid nohup timeout "$SECS" tcpdump -i "$IFACE" -s "$SNAPLEN" -n -q -c "$COUNT" \
  $FILTER -w "$DIR/$ID.cap" >"$DIR/$ID.log" 2>&1 &
PID=$!
# Give it a moment to fail loudly (a bad filter, no permission) rather than
# reporting a running capture that died immediately.
sleep 1
if ! kill -0 "$PID" 2>/dev/null; then
  if ! grep -qi "packets captured" "$DIR/$ID.log" 2>/dev/null; then
    echo "tcpdump exited immediately:"; cat "$DIR/$ID.log" 2>/dev/null; exit 1
  fi
fi
echo "$PID"`

// pktStatusScript reports whether the capture is still running, and how big the
// file is. "packets captured" only lands in the log once tcpdump exits.
const pktStatusScript = `set -e
if [ -f "$DIR/$ID.pid" ] && kill -0 "$(cat "$DIR/$ID.pid")" 2>/dev/null; then
  echo "state=running"
else
  echo "state=done"
fi
echo "bytes=$(stat -c %s "$DIR/$ID.cap" 2>/dev/null || echo 0)"
sed -n 's/^\([0-9]*\) packets captured$/packets=\1/p' "$DIR/$ID.log" 2>/dev/null | tail -1
sed -n 's/^\([0-9]*\) packets dropped by kernel$/kdropped=\1/p' "$DIR/$ID.log" 2>/dev/null | tail -1
exit 0`

// pktStopScript ends a capture early. tcpdump flushes its buffer on SIGTERM, so a
// stopped capture is a valid pcap; SIGKILL would truncate the last block.
const pktStopScript = `set -e
if [ -f "$DIR/$ID.pid" ]; then
  PID=$(cat "$DIR/$ID.pid")
  kill -TERM "$PID" 2>/dev/null || true
  # The timeout wrapper is the parent; signal the tcpdump child as well.
  pkill -TERM -P "$PID" 2>/dev/null || true
  for i in $(seq 1 20); do kill -0 "$PID" 2>/dev/null || break; sleep 0.2; done
fi
exit 0`

// pktCleanScript removes a capture's files from the node once it has been read
// back into the app. The node is not a storage service, and a 200 MB pcap left on
// a database node is a disk-full incident waiting to happen.
const pktCleanScript = `rm -f "$DIR/$ID.cap" "$DIR/$ID.log" "$DIR/$ID.pid"; exit 0`

// pktCapRequest is what the caller asks for.
type pktCapRequest struct {
	Seconds int // wall-clock ceiling
	Packets int // -c ceiling
	Snaplen int // -s
	Port    int // the MySQL port to filter on
	// Roles maps every port the capture should cover to the protocol it carries
	// (pktRole*). For a plain MySQL node that is one entry; for PXC it is four,
	// because a cluster member's traffic is on 3306 *and* Galera's 4567/4568/4444
	// (or, for an All-in-One PXC instance, that instance's slot ports).
	Roles    map[int]string
	Filter   string // extra BPF, ANDed with the port filter
	Iface    string // "" = auto-detect
	NoFilter bool   // capture everything on the interface (Filter used verbatim)
}

// pktBPF builds the capture filter. The port terms are what make this a *database*
// capture rather than a firehose; an extra expression is ANDed on, so a user can
// narrow to one peer ("host 10.0.0.7") but cannot accidentally widen past the ports.
//
// Every port with a declared role is included, in ascending order so the command line
// reads the same way twice: for PXC that is "port 3306 or port 4444 or port 4567 or
// port 4568" — the client protocol plus Galera's SST, group communication and IST.
func pktBPF(req pktCapRequest) string {
	var terms []string
	if !req.NoFilter {
		ports := pktCapPorts(req)
		var pterms []string
		for _, p := range ports {
			pterms = append(pterms, fmt.Sprintf("port %d", p))
		}
		if len(pterms) == 1 {
			terms = append(terms, pterms[0])
		} else if len(pterms) > 1 {
			terms = append(terms, "("+strings.Join(pterms, " or ")+")")
		}
	}
	if f := strings.TrimSpace(req.Filter); f != "" {
		terms = append(terms, "("+f+")")
	}
	return strings.Join(terms, " and ")
}

// pktCapPorts is every port the capture covers, ascending.
func pktCapPorts(req pktCapRequest) []int {
	seen := map[int]bool{}
	var out []int
	add := func(p int) {
		if p > 0 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(req.Port)
	for p := range req.Roles {
		add(p)
	}
	sort.Ints(out)
	return out
}

// pktShellSafeFilter rejects a BPF expression that could break out of the command.
// The filter is user input that lands in a shell line, so it is restricted to the
// characters BPF actually needs.
var pktFilterOK = regexp.MustCompile(`^[a-zA-Z0-9 ._:/()\[\]<>=!&|+-]*$`)

func pktValidateFilter(f string) error {
	if !pktFilterOK.MatchString(f) {
		return fmt.Errorf("filter contains characters that are not valid in a BPF expression")
	}
	if len(f) > 200 {
		return fmt.Errorf("filter is too long")
	}
	return nil
}

// pktResolveIface asks the node which interface carries its stack address.
func (a *App) pktResolveIface(ctx context.Context, containerID string) (string, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pktIfaceScript}, nil)
	if err != nil {
		return "", err
	}
	iface := strings.TrimSpace(res.Stdout)
	if iface == "" || strings.ContainsAny(iface, " \t\n") {
		return "", fmt.Errorf("could not determine the capture interface")
	}
	return iface, nil
}

// pktEnsureTcpdump installs tcpdump on first use.
func (a *App) pktEnsureTcpdump(ctx context.Context, containerID string) error {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pktInstallScript}, nil)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stdout+res.Stderr))
	}
	return nil
}

// pktStartCapture launches tcpdump and returns the command line it ran (for the UI)
// and the interface chosen.
func (a *App) pktStartCapture(ctx context.Context, containerID, id string, req pktCapRequest) (cmdLine, iface string, err error) {
	if !pktSafeName.MatchString(id) {
		return "", "", fmt.Errorf("invalid capture id")
	}
	if err := pktValidateFilter(req.Filter); err != nil {
		return "", "", err
	}
	if err := a.pktEnsureTcpdump(ctx, containerID); err != nil {
		return "", "", fmt.Errorf("tcpdump: %w", err)
	}
	iface = req.Iface
	if iface == "" {
		if iface, err = a.pktResolveIface(ctx, containerID); err != nil {
			return "", "", err
		}
	} else if !pktSafeName.MatchString(iface) {
		return "", "", fmt.Errorf("invalid interface name")
	}

	bpf := pktBPF(req)
	env := []string{
		"DIR=" + pktCapDir, "ID=" + id, "IFACE=" + iface,
		fmt.Sprintf("SECS=%d", req.Seconds), fmt.Sprintf("COUNT=%d", req.Packets),
		fmt.Sprintf("SNAPLEN=%d", req.Snaplen), "FILTER=" + bpf,
	}
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pktStartScript}, env)
	if err != nil {
		return "", "", err
	}
	if res.Code != 0 {
		return "", "", fmt.Errorf("%s", lastLines(strings.TrimSpace(res.Stdout+res.Stderr), 300))
	}
	pid := strings.TrimSpace(res.Stdout)
	if i := strings.LastIndex(pid, "\n"); i >= 0 {
		pid = strings.TrimSpace(pid[i+1:])
	}
	// Record the pid where the status and stop scripts can find it.
	if err := a.engCtx(ctx).CopyFile(ctx, containerID, pktCapDir, id+".pid", 0o644, []byte(pid+"\n")); err != nil {
		return "", "", fmt.Errorf("record capture pid: %w", err)
	}
	cmdLine = fmt.Sprintf("tcpdump -i %s -s %d -n -q -c %d %s -w %s/%s.cap",
		iface, req.Snaplen, req.Packets, bpf, pktCapDir, id)
	return cmdLine, iface, nil
}

// pktCapStatus is what the node reports about a running capture.
type pktCapStatus struct {
	Running       bool
	Bytes         int64
	Packets       int
	KernelDropped int
}

// pktPollCapture asks the node how the capture is going.
func (a *App) pktPollCapture(ctx context.Context, containerID, id string) (pktCapStatus, error) {
	var st pktCapStatus
	if !pktSafeName.MatchString(id) {
		return st, fmt.Errorf("invalid capture id")
	}
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pktStatusScript},
		[]string{"DIR=" + pktCapDir, "ID=" + id})
	if err != nil {
		return st, err
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "state":
			st.Running = v == "running"
		case "bytes":
			st.Bytes = int64(atoiSafe(v))
		case "packets":
			st.Packets = atoiSafe(v)
		case "kdropped":
			st.KernelDropped = atoiSafe(v)
		}
	}
	return st, nil
}

// pktStopCapture signals tcpdump to finish and flush.
func (a *App) pktStopCapture(ctx context.Context, containerID, id string) error {
	if !pktSafeName.MatchString(id) {
		return fmt.Errorf("invalid capture id")
	}
	_, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pktStopScript},
		[]string{"DIR=" + pktCapDir, "ID=" + id})
	return err
}

// pktFetchCapture reads the finished pcap out of the node and deletes it there.
func (a *App) pktFetchCapture(ctx context.Context, containerID, id string) ([]byte, error) {
	if !pktSafeName.MatchString(id) {
		return nil, fmt.Errorf("invalid capture id")
	}
	path := fmt.Sprintf("%s/%s.cap", pktCapDir, id)
	buf, err := a.readContainerFile(ctx, containerID, path)
	if err != nil {
		return nil, fmt.Errorf("read capture from node: %w", err)
	}
	if len(buf) > pktMaxCapBytes {
		return nil, fmt.Errorf("capture is %s, over the %s limit — shorten the duration or narrow the filter",
			pktBytes(len(buf)), pktBytes(pktMaxCapBytes))
	}
	a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pktCleanScript},
		[]string{"DIR=" + pktCapDir, "ID=" + id})
	return buf, nil
}

// atoiSafe parses a decimal integer, returning 0 for anything unparsable — the
// status script's output is trusted only to the extent that it is numeric.
func atoiSafe(s string) int {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
