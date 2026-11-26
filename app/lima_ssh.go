package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// lima_ssh.go — the half of the Lima engine that runs inside a VM: commands, files and the web
// terminal. Commands go through the ssh client with the config Lima writes for each instance
// (<instance>/ssh.config, which keeps one shared connection open, so a command costs a few
// milliseconds); the terminal uses x/crypto/ssh so it can resize its PTY.
//
// Docker runs a command as root unless told otherwise. Lima logs in as the host's user, who has
// passwordless sudo, so every command is wrapped in sudo; a user of "<name>" runs it as that
// account (`sudo -u <name>`).

// limaSSHInfo is how to reach one instance, read from its ssh.config.
type limaSSHInfo struct {
	cfgPath  string
	alias    string // the Host entry, lima-<instance>
	host     string
	port     int
	user     string
	identity string
}

func (l *Lima) sshInfoFor(id string) (limaSSHInfo, error) {
	if cached, ok := l.terms.Load("ssh/" + id); ok {
		return cached.(limaSSHInfo), nil
	}
	cfgPath := filepath.Join(l.dir(id), "ssh.config")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return limaSSHInfo{}, fmt.Errorf("VM %s is not running (no ssh config): %v", id, err)
	}
	info := parseLimaSSHConfig(string(b))
	info.cfgPath = cfgPath
	if info.alias == "" || info.host == "" || info.port == 0 {
		return limaSSHInfo{}, fmt.Errorf("VM %s: unreadable ssh config %s", id, cfgPath)
	}
	l.terms.Store("ssh/"+id, info)
	return info, nil
}

func (l *Lima) dropSSH(id string) { l.terms.Delete("ssh/" + id) }

// parseLimaSSHConfig reads the fields dbcanvas needs from an OpenSSH config Lima wrote. The
// first IdentityFile that exists wins.
func parseLimaSSHConfig(cfg string) limaSSHInfo {
	var info limaSSHInfo
	for _, line := range strings.Split(cfg, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.ToLower(key) {
		case "host":
			if info.alias == "" {
				info.alias = val
			}
		case "hostname":
			info.host = val
		case "port":
			info.port, _ = strconv.Atoi(val)
		case "user":
			info.user = val
		case "identityfile":
			if info.identity == "" {
				if _, err := os.Stat(val); err == nil {
					info.identity = val
				}
			}
		}
	}
	return info
}

// limaPath is the PATH a command on a VM gets: a container's default. sudo would otherwise use
// its secure_path, which on Oracle Linux leaves out /usr/local/bin, where provisioning puts
// links to the tools it installs elsewhere (psql under /usr/pgsql-<major>/bin, for one). An env
// entry for PATH still wins, as it comes after.
const limaPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// limaRemoteCommand is the command line run on the VM: sudo, the user, the environment and the
// command, each argument quoted. user "", "0" or "root" is root.
func limaRemoteCommand(user string, cmd, env []string) string {
	var b strings.Builder
	b.WriteString("sudo")
	if user != "" && user != "0" && user != "root" {
		b.WriteString(" -u " + shellQuote(user))
	}
	b.WriteString(" env " + shellQuote(limaPath))
	for _, e := range env {
		b.WriteString(" " + shellQuote(e))
	}
	for _, c := range cmd {
		b.WriteString(" " + shellQuote(c))
	}
	return b.String()
}

func (l *Lima) sshCmd(ctx context.Context, id, remote string) (*exec.Cmd, error) {
	info, err := l.sshInfoFor(id)
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, l.ssh, "-F", info.cfgPath, "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", info.alias, remote), nil
}

// runSSH runs a remote command, feeding it stdin when there is one. A non-zero exit on the VM is
// reported in Code with a nil error, as Docker.Exec does; only failing to run ssh is an error.
func (l *Lima) runSSH(ctx context.Context, id, remote string, stdin io.Reader) (ExecResult, error) {
	cmd, err := l.sshCmd(ctx, id, remote)
	if err != nil {
		return ExecResult{}, err
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	res := ExecResult{Stdout: out.String(), Stderr: errb.String()}
	if err == nil {
		return res, nil
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() != 255 {
		res.Code = ee.ExitCode()
		return res, nil
	}
	// 255 is ssh's own failure (connection refused, VM gone): forget the cached config.
	l.dropSSH(id)
	return res, fmt.Errorf("ssh to VM %s: %v: %s", id, err, limaTail(errb.String(), 300))
}

// --- Engine: commands and files ----------------------------------------------

func (l *Lima) Exec(ctx context.Context, id string, cmd []string, env []string) (ExecResult, error) {
	return l.runSSH(ctx, id, limaRemoteCommand("", cmd, env), nil)
}

func (l *Lima) ExecAs(ctx context.Context, id, user string, cmd []string, env []string) (ExecResult, error) {
	return l.runSSH(ctx, id, limaRemoteCommand(user, cmd, env), nil)
}

func (l *Lima) ExecInput(ctx context.Context, id, user string, cmd, env []string, stdin []byte) (ExecResult, error) {
	return l.runSSH(ctx, id, limaRemoteCommand(user, cmd, env), bytes.NewReader(stdin))
}

// CopyFile writes content to dir/name on the VM with the given mode.
func (l *Lima) CopyFile(ctx context.Context, id, dir, name string, mode int64, content []byte) error {
	path := strings.TrimRight(dir, "/") + "/" + name
	script := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %o %s", shellQuote(dir), shellQuote(path), mode, shellQuote(path))
	res, err := l.runSSH(ctx, id, limaRemoteCommand("", []string{"sh", "-c", script}, nil), bytes.NewReader(content))
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("copy %s to VM %s: %s", path, id, limaTail(res.Stderr, 200))
	}
	return nil
}

func (l *Lima) PutArchive(ctx context.Context, id, dir string, tarball []byte) error {
	return l.PutArchiveStream(ctx, id, dir, bytes.NewReader(tarball))
}

// PutArchiveStream pipes a tar stream into `tar -x` on the VM, never holding it whole.
func (l *Lima) PutArchiveStream(ctx context.Context, id, dir string, r io.Reader) error {
	script := fmt.Sprintf("mkdir -p %s && tar -x -C %s", shellQuote(dir), shellQuote(dir))
	res, err := l.runSSH(ctx, id, limaRemoteCommand("", []string{"sh", "-c", script}, nil), r)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("extract into %s on VM %s: %s", dir, id, limaTail(res.Stderr, 200))
	}
	return nil
}

// GetArchiveStream tars path on the VM and streams it back; closing the reader reaps ssh.
func (l *Lima) GetArchiveStream(ctx context.Context, id, path string) (io.ReadCloser, error) {
	dir, name := filepath.Split(strings.TrimRight(path, "/"))
	if dir == "" {
		dir = "/"
	}
	if name == "" {
		dir, name = "/", "."
	}
	script := fmt.Sprintf("tar -c -C %s -- %s", shellQuote(dir), shellQuote(name))
	cmd, err := l.sshCmd(ctx, id, limaRemoteCommand("", []string{"sh", "-c", script}, nil))
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &limaCmdReader{ReadCloser: out, cmd: cmd}, nil
}

type limaCmdReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c *limaCmdReader) Close() error {
	err := c.ReadCloser.Close()
	c.cmd.Wait()
	return err
}

// WaitSystemd waits until the VM's systemd has finished booting (running or degraded).
func (l *Lima) WaitSystemd(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		res, err := l.Exec(ctx, id, []string{"systemctl", "is-system-running"}, nil)
		if err == nil {
			switch strings.TrimSpace(res.Stdout) {
			case "running", "degraded":
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd not ready on VM %s within %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// --- Engine: the web terminal -------------------------------------------------

type limaTerm struct{ session *ssh.Session }

// limaSSHPipe makes an ssh session's stdin and stdout a net.Conn, the shape *ExecConn takes.
type limaSSHPipe struct {
	out io.Reader
	in  io.WriteCloser
	c   *ssh.Client
	s   *ssh.Session
}

func (p *limaSSHPipe) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *limaSSHPipe) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *limaSSHPipe) Close() error {
	p.in.Close()
	p.s.Close()
	return p.c.Close()
}
func (p *limaSSHPipe) LocalAddr() net.Addr                { return limaAddr{} }
func (p *limaSSHPipe) RemoteAddr() net.Addr               { return limaAddr{} }
func (p *limaSSHPipe) SetDeadline(t time.Time) error      { return nil }
func (p *limaSSHPipe) SetReadDeadline(t time.Time) error  { return nil }
func (p *limaSSHPipe) SetWriteDeadline(t time.Time) error { return nil }

type limaAddr struct{}

func (limaAddr) Network() string { return "ssh" }
func (limaAddr) String() string  { return "lima" }

// sshClient opens an ssh connection to the VM with Lima's key, checking the VM's host key
// against the one Lima pinned for it (<instance>/ssh.known_hosts), as the ssh command does with
// Lima's config. The VM's port is on 127.0.0.1, where another local user could listen in its
// place while the VM is down; this connection carries commands, files and stack traffic.
func (l *Lima) sshClient(id string) (*ssh.Client, error) {
	info, err := l.sshInfoFor(id)
	if err != nil {
		return nil, err
	}
	knownFile := filepath.Join(l.dir(id), "ssh.known_hosts")
	hostKeys, err := knownhosts.New(knownFile)
	if err != nil {
		return nil, fmt.Errorf("VM %s: Lima's pinned host key: %w", id, err)
	}
	key, err := os.ReadFile(info.identity)
	if err != nil {
		return nil, fmt.Errorf("read Lima's ssh key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse Lima's ssh key: %w", err)
	}
	return ssh.Dial("tcp", net.JoinHostPort(info.host, strconv.Itoa(info.port)), &ssh.ClientConfig{
		User:            info.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeys,
		// Ask for the key types Lima pinned: a VM has several host keys, and the client would
		// otherwise settle on whichever type it prefers, then find no pinned key of that type.
		HostKeyAlgorithms: pinnedHostKeyAlgorithms(knownFile),
		Timeout:           15 * time.Second,
	})
}

// HijackExec opens an interactive PTY on the VM as an *ExecConn.
func (l *Lima) HijackExec(ctx context.Context, id string, cmd, env []string, user string) (*ExecConn, error) {
	client, err := l.sshClient(id)
	if err != nil {
		return nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	stdout, _ := sess.StdoutPipe()
	stdin, _ := sess.StdinPipe()
	if err := sess.Start(limaRemoteCommand(user, cmd, env)); err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	pipe := &limaSSHPipe{out: stdout, in: stdin, c: client, s: sess}
	execID := "lima-term-" + id + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	l.terms.Store(execID, &limaTerm{session: sess})
	go func() { sess.Wait(); l.terms.Delete(execID) }()
	return &ExecConn{r: bufio.NewReader(pipe), c: pipe, ExecID: execID}, nil
}

// ResizeExec pushes a window-size change to a live terminal (w columns, h rows).
func (l *Lima) ResizeExec(ctx context.Context, execID string, w, h int) error {
	if t, ok := l.terms.Load(execID); ok {
		return t.(*limaTerm).session.WindowChange(h, w)
	}
	return nil
}

// pinnedHostKeyAlgorithms is the host key algorithms for the key types a known_hosts file pins,
// for ssh.ClientConfig.HostKeyAlgorithms; nil (the client's own list) when it pins none it reads.
func pinnedHostKeyAlgorithms(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(algos ...string) {
		for _, a := range algos {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || strings.HasPrefix(f[0], "#") || strings.HasPrefix(f[0], "@") {
			continue
		}
		if f[1] == ssh.KeyAlgoRSA {
			// An RSA key signs with SHA-2 on any current server.
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		} else {
			add(f[1])
		}
	}
	return out
}
