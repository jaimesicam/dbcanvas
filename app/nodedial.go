package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
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
// the Intranet, VNC, Kubeadm). A container without bash (a k3s node, a distroless image)
// runs a small forwarder instead, copied into it through the archive API (noderelay.go). DBCANVAS_NODE_DIAL picks the mode: auto (default), direct, or tunnel —
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

// DialContext makes a dialFunc a dialer for the drivers that take one (the MongoDB
// driver's ContextDialer, http.Transport.DialContext).
func (f dialFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

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

// stackNodeDial is stackDial for a connection to containerID's node. A node on another
// engine than Docker is dialled directly.
func (a *App) stackNodeDial(stackID int64, containerID string) dialFunc {
	if containerID != "" && nodeDialMode() == nodeDialTunnel {
		if d := asDocker(a.dialEngine(stackID, containerID), containerID); d != a.docker {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext
		}
	}
	return a.stackDial(stackID, containerID)
}

// mysqlStackNet is the go-sql-driver "net" for a connection to containerID's node on
// stack stackID: plain "tcp" where addresses are routable, else a dial registered under
// a name of its own that goes through stackNodeDial.
func (a *App) mysqlStackNet(stackID int64, containerID string) string {
	if nodeDialMode() == nodeDialDirect {
		return "tcp"
	}
	name := fmt.Sprintf("dbcanvas-stack-%d-%s", stackID, containerID)
	dial := a.stackNodeDial(stackID, containerID)
	mysql.RegisterDialContext(name, func(ctx context.Context, addr string) (net.Conn, error) {
		return dial(ctx, "tcp", addr)
	})
	return name
}

// openStackDB is sql.Open for a "mysql" or "pgx" DSN that addresses a node on stack
// stackID's network (containerID's, when known), dialling through stackNodeDial.
func (a *App) openStackDB(stackID int64, containerID, driver, dsn string) (*sql.DB, error) {
	if nodeDialMode() == nodeDialDirect {
		return sql.Open(driver, dsn)
	}
	switch driver {
	case "mysql":
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return nil, err
		}
		cfg.Net = a.mysqlStackNet(stackID, containerID)
		c, err := mysql.NewConnector(cfg)
		if err != nil {
			return nil, err
		}
		return sql.OpenDB(c), nil
	case "pgx":
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			return nil, err
		}
		dial := a.stackNodeDial(stackID, containerID)
		cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dial(ctx, network, addr)
		}
		// Names are resolved by the relay, on the stack network, where the Intranet's
		// DNS answers; the host's resolver does not know them.
		cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
			return []string{host}, nil
		}
		return stdlib.OpenDB(*cfg), nil
	}
	return sql.Open(driver, dsn)
}

// stackTransports keeps one HTTP transport per node reached through the tunnel, so a
// poll reuses its connection instead of opening (and leaving idle) one per call.
var stackTransports sync.Map // "<stack>/<container>" → *http.Transport

// stackHTTPClient is an HTTP client for a service on containerID's node.
func (a *App) stackHTTPClient(stackID int64, containerID string, timeout time.Duration) *http.Client {
	if nodeDialMode() == nodeDialDirect {
		return &http.Client{Timeout: timeout}
	}
	key := fmt.Sprintf("%d/%s", stackID, containerID)
	tr, ok := stackTransports.Load(key)
	if !ok {
		tr, _ = stackTransports.LoadOrStore(key, &http.Transport{
			DialContext:     a.stackNodeDial(stackID, containerID).DialContext,
			MaxIdleConns:    2,
			IdleConnTimeout: 30 * time.Second,
		})
	}
	return &http.Client{Timeout: timeout, Transport: tr.(*http.Transport)}
}

// relayScript connects to $H:$P, announces it on stderr (relayOK), and copies both ways:
// the exec's stdin to the socket and the socket to the exec's stdout. When the caller
// closes its side, the reader is stopped rather than left waiting on a server that may
// never close.
const relayScript = `exec 3<>"/dev/tcp/$H/$P" || exit 7; printf 'dbcanvas-relay-ok\n' >&2; cat <&3 & r=$!; cat >&3; kill "$r" 2>/dev/null; wait "$r" 2>/dev/null; exit 0`

// relayOK is what the relay writes on stderr once its socket is open.
const relayOK = "dbcanvas-relay-ok\n"

// relayConnectTimeout bounds a relay's connect, like the direct dialer's timeout: bash's
// /dev/tcp has none of its own, and an address that drops packets would hang for minutes.
const relayConnectTimeout = 10 * time.Second

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
		return a.relayVia(ctx, cid, host, port, addr)
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
	return nil, fmt.Errorf("tunnel to %s: no running container on the stack network can relay it", addr)
}

// noBash remembers the containers where bash is missing, so the forwarder is used at once.
var noBash sync.Map // container ID → true

// relayVia opens addr through one container: bash's /dev/tcp when it has bash, else the
// forwarder copied in (noderelay.go).
func (a *App) relayVia(ctx context.Context, cid, host, port, addr string) (net.Conn, error) {
	if _, skip := noBash.Load(cid); !skip {
		c, err := a.docker.StreamExec(ctx, cid, []string{"bash", "-c", relayScript},
			[]string{"H=" + host, "P=" + port})
		if err == nil {
			conn, err := c.ready(ctx, addr)
			if !errors.Is(err, errRelayUnusable) {
				return conn, err
			}
		} else if !errors.Is(err, errRelayUnusable) {
			return nil, err
		}
		noBash.Store(cid, true)
	}
	for attempt := 0; ; attempt++ {
		if err := a.docker.installRelayBin(ctx, cid); err != nil {
			return nil, err
		}
		c, err := a.docker.StreamExec(ctx, cid, []string{relayBinDir + "/" + relayBinName, net.JoinHostPort(host, port)}, nil)
		if err != nil {
			return nil, err
		}
		conn, err := c.ready(ctx, addr)
		if errors.Is(err, errRelayUnusable) && attempt == 0 {
			forgetRelayBin(cid) // gone since it was copied in: copy it again
			continue
		}
		return conn, err
	}
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

// ready waits for the relay to connect or fail. The relay writes relayOK on stderr once
// its socket is open, so a connection is ready as soon as that arrives, whichever side of
// the protocol speaks first (anything the target sent before it is kept). A failed
// connect or a container without bash ends the exec instead, and its exit code and
// output say which.
func (s *execStream) ready(ctx context.Context, addr string) (net.Conn, error) {
	s.addr = addr
	deadline := time.Now().Add(relayConnectTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	s.c.SetReadDeadline(deadline)
	defer s.c.SetReadDeadline(time.Time{})
	var hdr [8]byte
	for {
		_, err := io.ReadFull(s.r, hdr[:])
		if err == nil {
			buf := make([]byte, binary.BigEndian.Uint32(hdr[4:]))
			_, err = io.ReadFull(s.r, buf)
			if err == nil {
				if hdr[0] == 1 {
					// Output before the announcement is either the target's first bytes
					// (the two streams are copied separately, so they can overtake the
					// announcement) or Docker's own failure to start the command; the
					// announcement or the exit tells which.
					s.pending = append(s.pending, buf...)
					continue
				}
				s.stderr.Write(buf)
				if msg := s.stderr.String(); strings.Contains(msg, relayOK) {
					s.stderr.Reset()
					s.stderr.WriteString(strings.Replace(msg, relayOK, "", 1))
					return s, nil
				}
				continue
			}
		}
		s.c.Close()
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, fmt.Errorf("tunnel to %s: timed out connecting", addr)
		}
		return nil, s.relayFailure(addr)
	}
}

// relayFailure explains a relay that ended before connecting, from its exit code and stderr.
func (s *execStream) relayFailure(addr string) error {
	code := -1
	for i := 0; i < 10; i++ {
		c, running, err := s.d.execState(context.Background(), s.execID)
		if err != nil {
			break
		}
		if !running {
			code = c
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	msg := strings.TrimSpace(s.stderr.String())
	if msg == "" && len(s.pending) > 0 && len(s.pending) < 4096 {
		msg = strings.TrimSpace(string(s.pending)) // Docker reports a failed start on stdout
	}
	if code == 126 || code == 127 || strings.Contains(msg, "executable file not found") {
		return fmt.Errorf("%w: %s", errRelayUnusable, firstNonEmpty(msg, "no bash"))
	}
	if msg != "" {
		// bash's message names the script; keep its last part ("Connection refused").
		if k := strings.LastIndex(msg, ": "); k >= 0 {
			msg = msg[k+2:]
		}
	}
	return fmt.Errorf("tunnel to %s: %s", addr, firstNonEmpty(msg, "connection refused"))
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
