package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nodedial.go — every connection DBCanvas opens to an address on a stack's network goes
// through stackDial.
//
// Inside the app container DBCanvas joins the stack network (joinStackForDial) and dials
// directly, and on a Linux host the host routes to the bridge networks itself. Where
// neither holds — DBCanvas running on macOS, where Docker's networks live inside a VM —
// a container's address cannot be reached, but the Docker API can: a connection is
// tunnelled through an exec in a container on the stack's network (a "relay"), whose
// stdin and stdout carry the connection's bytes. The relay connects from inside the
// network, so whatever a direct dial reaches works the same: container IPs, the
// Intranet's DNS names, MetalLB addresses, a service bound to a container's own IP.
//
// The relay is bash's /dev/tcp, which every DBCanvas node image has (the systemd bases,
// the Intranet, VNC, Kubeadm); a distroless container (a demo application) is never
// chosen as one. DBCANVAS_NODE_DIAL picks the mode: auto (default), direct, or tunnel —
// tunnel forces the macOS path anywhere, which is how it is tested on Linux.

const (
	nodeDialDirect = "direct"
	nodeDialTunnel = "tunnel"
)

// nodeDialMode is how this process reaches addresses on a stack network.
func nodeDialMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DBCANVAS_NODE_DIAL"))) {
	case nodeDialDirect:
		return nodeDialDirect
	case nodeDialTunnel:
		return nodeDialTunnel
	}
	if appIsContainerized() || runtime.GOOS == "linux" {
		return nodeDialDirect
	}
	return nodeDialTunnel
}

// dialFunc is the shape every driver's dial hook takes.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// stackDial is the dialer for addresses on stack stackID's network. prefer, when not empty,
// is the container the address belongs to, tried first as the relay.
func (a *App) stackDial(stackID int64, prefer string) dialFunc {
	if nodeDialMode() == nodeDialDirect {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return a.tunnelDial(ctx, stackID, prefer, addr)
	}
}

// relayScript connects to $H:$P and copies both ways: the exec's stdin to the socket and
// the socket to the exec's stdout. When the caller closes its side, the reader is stopped
// rather than left waiting on a server that may never close.
const relayScript = `exec 3<>"/dev/tcp/$H/$P" || exit 7; cat <&3 & r=$!; cat >&3; kill "$r" 2>/dev/null; wait "$r" 2>/dev/null; exit 0`

// relays remembers the relay found for a stack, so a dial does not search every time.
var relays = struct {
	sync.Mutex
	m map[int64]string
}{m: map[int64]string{}}

// tunnelDial opens addr through a relay container on the stack's network.
func (a *App) tunnelDial(ctx context.Context, stackID int64, prefer, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("tunnel to %s: bad port", addr)
	}
	relays.Lock()
	cached := relays.m[stackID]
	relays.Unlock()
	tried := map[string]bool{}
	try := func(cid string) (net.Conn, error) {
		tried[cid] = true
		c, err := a.docker.StreamExec(ctx, cid, []string{"bash", "-c", relayScript},
			[]string{"H=" + host, "P=" + port})
		if err != nil {
			return nil, err
		}
		return c.ready(addr)
	}
	for _, cid := range []string{prefer, cached} {
		if cid == "" || tried[cid] {
			continue
		}
		if c, err := try(cid); err == nil {
			a.rememberRelay(stackID, cid)
			return c, nil
		} else if !errors.Is(err, errRelayUnusable) {
			return nil, err
		}
	}
	for _, cid := range a.relayCandidates(stackID) {
		if tried[cid] {
			continue
		}
		c, err := try(cid)
		if err == nil {
			a.rememberRelay(stackID, cid)
			return c, nil
		}
		if !errors.Is(err, errRelayUnusable) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("tunnel to %s: no running container on the stack network can relay it (one with bash is needed)", addr)
}

func (a *App) rememberRelay(stackID int64, cid string) {
	relays.Lock()
	relays.m[stackID] = cid
	relays.Unlock()
}

// relayCandidates lists the stack's running containers, the Intranet first: it is on
// every stack that has one and always has bash.
func (a *App) relayCandidates(stackID int64) []string {
	deps, err := a.store.ListDeployments(stackID)
	if err != nil {
		return nil
	}
	var first, rest []string
	for _, d := range deps {
		if d.ContainerID == "" || d.State != DeployRunning {
			continue
		}
		if strings.Contains(d.NodeID, "intranet") {
			first = append(first, d.ContainerID)
		} else {
			rest = append(rest, d.ContainerID)
		}
	}
	return append(first, rest...)
}

// errRelayUnusable is a relay container that cannot run the relay (no bash): try another.
var errRelayUnusable = errors.New("relay container cannot run the relay")

// execStream is a non-TTY exec whose stdin and stdout carry a connection. Docker frames
// the output of a non-TTY exec (an 8-byte header per chunk: stream, then length); Read
// strips the frames and keeps stdout, and holds stderr for an error message.
type execStream struct {
	c      net.Conn
	r      *bufio.Reader
	d      *Docker
	execID string
	addr   string

	mu      sync.Mutex
	pending []byte // stdout bytes of the current frame not yet read
	left    int    // bytes of the current frame still in r
	stream  byte
	stderr  strings.Builder
}

// StreamExec starts cmd in the container without a TTY and returns its stdin/stdout as a
// stream.
func (d *Docker) StreamExec(ctx context.Context, containerID string, cmd, env []string) (*execStream, error) {
	create := map[string]any{"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": cmd}
	if len(env) > 0 {
		create["Env"] = env
	}
	resp, err := d.do(ctx, "POST", "/containers/"+containerID+"/exec", create)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 201 {
		err := errBody("exec create", resp)
		if resp.StatusCode == 409 || resp.StatusCode == 404 {
			return nil, fmt.Errorf("%w: %v", errRelayUnusable, err)
		}
		return nil, err
	}
	var ec struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(drain(resp), &ec); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", d.sock)
	if err != nil {
		return nil, err
	}
	body := `{"Detach":false,"Tty":false}`
	req := "POST /exec/" + ec.ID + "/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\n" +
		"Connection: Upgrade\r\nUpgrade: tcp\r\n" + fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)) + body
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(status, " 101") && !strings.Contains(status, " 200") {
		conn.Close()
		return nil, fmt.Errorf("exec start: unexpected status %q", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &execStream{c: conn, r: br, d: d, execID: ec.ID}, nil
}

// ready waits for the relay to either connect or fail. Nothing is written before the
// target answers in some protocols (MySQL's server speaks first), so this cannot wait for
// data; it waits a moment for an early exit instead, which is how a refused connection
// or a container without bash shows.
func (s *execStream) ready(addr string) (net.Conn, error) {
	s.addr = addr
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		code, running, err := s.d.execState(context.Background(), s.execID)
		if err != nil {
			break
		}
		if !running {
			s.c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			io.Copy(io.Discard, s) // collect stderr for the message
			s.c.Close()
			msg := strings.TrimSpace(s.stderr.String())
			if code == 126 || code == 127 || strings.Contains(msg, "executable file not found") {
				return nil, fmt.Errorf("%w: %s", errRelayUnusable, firstNonEmpty(msg, "no bash"))
			}
			if code == 0 && msg == "" {
				// It connected and the target closed at once: still a connection, but empty.
				return nil, fmt.Errorf("tunnel to %s: the connection closed at once", addr)
			}
			return nil, fmt.Errorf("tunnel to %s: %s", addr, firstNonEmpty(msg, "connection refused"))
		}
		time.Sleep(40 * time.Millisecond)
	}
	return s, nil
}

// execState reports an exec's exit code and whether it still runs.
func (d *Docker) execState(ctx context.Context, execID string) (int, bool, error) {
	resp, err := d.do(ctx, "GET", "/exec/"+execID+"/json", nil)
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode != 200 {
		return 0, false, errBody("exec inspect", resp)
	}
	var out struct {
		Running  bool
		ExitCode int
	}
	if err := json.Unmarshal(drain(resp), &out); err != nil {
		return 0, false, err
	}
	return out.ExitCode, out.Running, nil
}

func (s *execStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if len(s.pending) > 0 {
			n := copy(p, s.pending)
			s.pending = s.pending[n:]
			return n, nil
		}
		if s.left == 0 {
			var hdr [8]byte
			if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
				if errors.Is(err, io.ErrUnexpectedEOF) {
					err = io.EOF
				}
				return 0, err
			}
			s.stream = hdr[0]
			s.left = int(binary.BigEndian.Uint32(hdr[4:]))
			continue
		}
		if s.stream != 1 { // stderr: kept for the message, not part of the connection
			buf := make([]byte, s.left)
			n, err := io.ReadFull(s.r, buf)
			s.left -= n
			if s.stderr.Len() < 4096 {
				s.stderr.Write(buf[:n])
			}
			if err != nil {
				return 0, err
			}
			continue
		}
		want := s.left
		if want > len(p) {
			want = len(p)
		}
		n, err := s.r.Read(p[:want])
		s.left -= n
		if n > 0 {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

func (s *execStream) Write(p []byte) (int, error) { return s.c.Write(p) }
func (s *execStream) Close() error                { return s.c.Close() }

// CloseWrite ends the exec's stdin, which ends the relay's writer.
func (s *execStream) CloseWrite() error {
	if cw, ok := s.c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (s *execStream) LocalAddr() net.Addr                { return tunnelAddr("dbcanvas") }
func (s *execStream) RemoteAddr() net.Addr               { return tunnelAddr(s.addr) }
func (s *execStream) SetDeadline(t time.Time) error      { return s.c.SetDeadline(t) }
func (s *execStream) SetReadDeadline(t time.Time) error  { return s.c.SetReadDeadline(t) }
func (s *execStream) SetWriteDeadline(t time.Time) error { return s.c.SetWriteDeadline(t) }

// tunnelAddr is an address as a tunnelled connection reports it.
type tunnelAddr string

func (t tunnelAddr) Network() string { return "tcp" }
func (t tunnelAddr) String() string  { return string(t) }
