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
)

// vagrant_ssh.go — the exec / file-transfer / interactive-console half of the
// Vagrant engine. Non-interactive work (Exec, CopyFile, …) shells out to the `ssh`
// client using a cached `vagrant ssh-config`; the web terminal uses x/crypto/ssh so
// it can allocate a PTY and honour live window resizes.
//
// Docker runs execs as the container's default user (root). A VM's ssh login is the
// unprivileged `vagrant` user, so every exec is wrapped in sudo to preserve that
// root-by-default behaviour; ExecAs("<user>") maps to `sudo -u <user>`.

// sshInfo is the connection detail parsed from `vagrant ssh-config`.
type sshInfo struct {
	cfgPath  string // path to the written OpenSSH config (for the ssh CLI)
	host     string // HostName
	port     int    // Port
	user     string // User
	identity string // IdentityFile
	alias    string // the machine name used as the ssh "Host" (== vmName)
}

// ssh returns cached connection info for a VM, materialising it from
// `vagrant ssh-config` on first use.
func (v *Vagrant) sshInfoFor(ctx context.Context, id string) (sshInfo, error) {
	if cached, ok := v.terms.Load("ssh/" + id); ok {
		return cached.(sshInfo), nil
	}
	out, errb, err := v.vagrantCmd(ctx, id, "ssh-config")
	if err != nil {
		return sshInfo{}, fmt.Errorf("vagrant ssh-config %s: %v: %s", id, err, tail(errb, 300))
	}
	cfgPath := filepath.Join(v.vmDir(id), "ssh_config")
	if err := os.WriteFile(cfgPath, []byte(out), 0o600); err != nil {
		return sshInfo{}, err
	}
	info := sshInfo{cfgPath: cfgPath, alias: id, port: 22}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "HostName":
			info.host = f[1]
		case "User":
			info.user = f[1]
		case "Port":
			info.port, _ = strconv.Atoi(f[1])
		case "IdentityFile":
			info.identity = strings.Trim(f[1], `"`)
		}
	}
	if info.host == "" {
		return sshInfo{}, fmt.Errorf("vagrant ssh-config %s: no HostName", id)
	}
	v.terms.Store("ssh/"+id, info)
	return info, nil
}

func (v *Vagrant) dropSSH(id string) { v.terms.Delete("ssh/" + id) }

// shellQuote single-quotes a string for safe embedding in a remote sh command.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// remoteCommand composes the sudo-wrapped command line run on the guest. user ""
// / "0" / "root" means root; anything else runs under `sudo -u <user>`.
func remoteCommand(user string, cmd, env []string) string {
	var b strings.Builder
	b.WriteString("sudo")
	if user != "" && user != "0" && user != "root" {
		b.WriteString(" -u " + shellQuote(user))
	}
	b.WriteString(" env")
	for _, e := range env {
		b.WriteString(" " + shellQuote(e))
	}
	for _, c := range cmd {
		b.WriteString(" " + shellQuote(c))
	}
	return b.String()
}

// runSSH runs a composed remote command over the ssh CLI, capturing stdout/stderr
// and the guest exit code. A non-zero guest exit is reported in Code (err nil),
// matching Docker.Exec; only a failure to launch/connect returns a non-nil error.
func (v *Vagrant) runSSH(ctx context.Context, id, remote string, stdin []byte) (ExecResult, error) {
	info, err := v.sshInfoFor(ctx, id)
	if err != nil {
		return ExecResult{}, err
	}
	args := []string{"-F", info.cfgPath, "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", info.alias, remote}
	cmd := exec.CommandContext(ctx, v.ssh, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	res := ExecResult{Stdout: out.String(), Stderr: errb.String()}
	if err == nil {
		return res, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Code = ee.ExitCode()
		return res, nil
	}
	return res, err
}

// --- Engine: exec / file transfer ------------------------------------------

func (v *Vagrant) Exec(ctx context.Context, id string, cmd []string, env []string) (ExecResult, error) {
	return v.runSSH(ctx, id, remoteCommand("", cmd, env), nil)
}

func (v *Vagrant) ExecAs(ctx context.Context, id, user string, cmd []string, env []string) (ExecResult, error) {
	return v.runSSH(ctx, id, remoteCommand(user, cmd, env), nil)
}

func (v *Vagrant) ExecInput(ctx context.Context, id, user string, cmd, env []string, stdin []byte) (ExecResult, error) {
	return v.runSSH(ctx, id, remoteCommand(user, cmd, env), stdin)
}

// CopyFile writes content to dir/name on the guest with the given mode.
func (v *Vagrant) CopyFile(ctx context.Context, id, dir, name string, mode int64, content []byte) error {
	path := strings.TrimRight(dir, "/") + "/" + name
	script := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %o %s",
		shellQuote(dir), shellQuote(path), mode, shellQuote(path))
	remote := remoteCommand("", []string{"sh", "-c", script}, nil)
	res, err := v.runSSH(ctx, id, remote, content)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("copy %s: %s", path, tail(res.Stderr, 200))
	}
	return nil
}

// PutArchive extracts a tarball into dir on the guest.
func (v *Vagrant) PutArchive(ctx context.Context, id, dir string, tarball []byte) error {
	script := fmt.Sprintf("mkdir -p %s && tar -x -C %s", shellQuote(dir), shellQuote(dir))
	remote := remoteCommand("", []string{"sh", "-c", script}, nil)
	res, err := v.runSSH(ctx, id, remote, tarball)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("put archive %s: %s", dir, tail(res.Stderr, 200))
	}
	return nil
}

// WaitSystemd blocks until the guest's systemd has finished booting (running or
// degraded) or the timeout elapses.
func (v *Vagrant) WaitSystemd(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		res, err := v.runSSH(ctx, id, remoteCommand("", []string{"systemctl", "is-system-running"}, nil), nil)
		if err == nil {
			switch strings.TrimSpace(res.Stdout) {
			case "running", "degraded":
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd not ready on %s within %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// --- Engine: interactive console -------------------------------------------

// vagrantTerm holds a live PTY session so ResizeExec can push window changes.
type vagrantTerm struct {
	session *ssh.Session
	client  *ssh.Client
}

// sshPipe adapts an ssh session's stdin/stdout to net.Conn, so a PTY console can be
// wrapped in the same *ExecConn the web terminal already consumes.
type sshPipe struct {
	out io.Reader
	in  io.WriteCloser
	c   *ssh.Client
	s   *ssh.Session
}

func (p *sshPipe) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *sshPipe) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *sshPipe) Close() error {
	p.in.Close()
	p.s.Close()
	return p.c.Close()
}
func (p *sshPipe) LocalAddr() net.Addr                { return dummyAddr{} }
func (p *sshPipe) RemoteAddr() net.Addr               { return dummyAddr{} }
func (p *sshPipe) SetDeadline(t time.Time) error      { return nil }
func (p *sshPipe) SetReadDeadline(t time.Time) error  { return nil }
func (p *sshPipe) SetWriteDeadline(t time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "ssh" }
func (dummyAddr) String() string  { return "vagrant" }

// HijackExec opens an interactive PTY on the guest and returns it as an *ExecConn.
func (v *Vagrant) HijackExec(ctx context.Context, id string, cmd, env []string, user string) (*ExecConn, error) {
	info, err := v.sshInfoFor(ctx, id)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(info.identity)
	if err != nil {
		return nil, fmt.Errorf("read identity: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse identity: %w", err)
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(info.host, strconv.Itoa(info.port)), &ssh.ClientConfig{
		User:            info.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	})
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
	sess.Stderr = nil // fold stderr into the pty stream via the shell
	if err := sess.Start(remoteCommand(user, cmd, env)); err != nil {
		sess.Close()
		client.Close()
		return nil, err
	}
	pipe := &sshPipe{out: stdout, in: stdin, c: client, s: sess}
	execID := "vagrant-term-" + id + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	v.terms.Store(execID, &vagrantTerm{session: sess, client: client})
	go func() { sess.Wait(); v.terms.Delete(execID) }()
	return &ExecConn{r: bufio.NewReader(pipe), c: pipe, ExecID: execID}, nil
}

// ResizeExec pushes a window-size change to a live PTY console (w=cols, h=rows).
func (v *Vagrant) ResizeExec(ctx context.Context, execID string, w, h int) error {
	if t, ok := v.terms.Load(execID); ok {
		return t.(*vagrantTerm).session.WindowChange(h, w)
	}
	return nil
}
