package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

// Docker is a tiny Docker Engine API client speaking HTTP over the unix socket
// using only the standard library — no SDK, no docker CLI, so the app stays a
// static distroless binary. Container paths are unversioned (the daemon uses its
// default API version).
type Docker struct {
	http *http.Client
	sock string
}

// NewDocker returns a client bound to the given unix socket path.
func NewDocker(sock string) *Docker {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}
	return &Docker{http: &http.Client{Transport: tr}, sock: sock}
}

func (d *Docker) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return d.http.Do(req)
}

// drain reads and closes a response body, returning its bytes.
func drain(resp *http.Response) []byte {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return b
}

// errBody builds an error from a non-2xx Docker response.
func errBody(action string, resp *http.Response) error {
	b := drain(resp)
	msg := strings.TrimSpace(string(b))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		msg = e.Message
	}
	return fmt.Errorf("docker %s: %s (%d)", action, msg, resp.StatusCode)
}

// Ping reports whether the Docker daemon is reachable.
func (d *Docker) Ping(ctx context.Context) error {
	resp, err := d.do(ctx, "GET", "/_ping", nil)
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode != 200 {
		return errBody("ping", resp)
	}
	return nil
}

// ImageExists reports whether an image reference is present locally. The ref
// (repo:tag) is used verbatim — Docker matches the literal name, and escaping
// the ':' would break the lookup.
func (d *Docker) ImageExists(ctx context.Context, ref string) (bool, error) {
	resp, err := d.do(ctx, "GET", "/images/"+ref+"/json", nil)
	if err != nil {
		return false, err
	}
	drain(resp)
	switch resp.StatusCode {
	case 200:
		return true, nil
	case 404:
		return false, nil
	default:
		return false, errBody("inspect image", resp)
	}
}

// NetworkEnsure creates a user-defined bridge network if it does not exist.
func (d *Docker) NetworkEnsure(ctx context.Context, name string) error {
	resp, err := d.do(ctx, "GET", "/networks/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode == 200 {
		return nil
	}
	resp, err = d.do(ctx, "POST", "/networks/create", map[string]any{"Name": name, "Driver": "bridge"})
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode != 201 {
		return errBody("create network", resp)
	}
	return nil
}

// NetworkRemove deletes a network (best-effort).
func (d *Docker) NetworkRemove(ctx context.Context, name string) {
	resp, err := d.do(ctx, "DELETE", "/networks/"+url.PathEscape(name), nil)
	if err == nil {
		drain(resp)
	}
}

// ContainerByName returns the id of a container with the exact name, if any.
func (d *Docker) ContainerByName(ctx context.Context, name string) (string, bool, error) {
	filters := `{"name":["^/` + name + `$"]}`
	resp, err := d.do(ctx, "GET", "/containers/json?all=true&filters="+url.QueryEscape(filters), nil)
	if err != nil {
		return "", false, err
	}
	if resp.StatusCode != 200 {
		return "", false, errBody("list containers", resp)
	}
	var arr []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(drain(resp), &arr); err != nil {
		return "", false, err
	}
	if len(arr) > 0 {
		return arr[0].ID, true, nil
	}
	return "", false, nil
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name        string
	Image       string
	Hostname    string
	Env         []string
	Network     string
	Aliases     []string
	Privileged  bool
	PublishPort int // container TCP port to publish to an auto-assigned host port (0 = none)
}

// ContainerCreate creates a container and returns its id.
func (d *Docker) ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error) {
	host := map[string]any{
		"Privileged":    spec.Privileged,
		"Tmpfs":         map[string]string{"/run": "", "/run/lock": ""},
		"RestartPolicy": map[string]any{"Name": "unless-stopped"},
	}
	if spec.Privileged {
		// systemd as PID 1 needs the host cgroup namespace and a writable
		// cgroup mount (verified on cgroup v2 hosts).
		host["CgroupnsMode"] = "host"
		host["Binds"] = []string{"/sys/fs/cgroup:/sys/fs/cgroup:rw"}
	}
	body := map[string]any{
		"Image":    spec.Image,
		"Hostname": spec.Hostname,
		"Env":      spec.Env,
	}
	if spec.Network != "" {
		host["NetworkMode"] = spec.Network
		body["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{
				spec.Network: map[string]any{"Aliases": spec.Aliases},
			},
		}
	}
	if spec.PublishPort > 0 {
		port := fmt.Sprintf("%d/tcp", spec.PublishPort)
		body["ExposedPorts"] = map[string]any{port: map[string]any{}}
		// empty HostPort → Docker assigns a free ephemeral host port (unused).
		host["PortBindings"] = map[string]any{port: []map[string]string{{"HostPort": ""}}}
	}
	body["HostConfig"] = host

	resp, err := d.do(ctx, "POST", "/containers/create?name="+url.QueryEscape(spec.Name), body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 201 {
		return "", errBody("create container", resp)
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(drain(resp), &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (d *Docker) simple(ctx context.Context, method, path, action string, okCodes ...int) error {
	resp, err := d.do(ctx, method, path, nil)
	if err != nil {
		return err
	}
	drain(resp)
	for _, c := range okCodes {
		if resp.StatusCode == c {
			return nil
		}
	}
	return errBody(action, resp)
}

func (d *Docker) ContainerStart(ctx context.Context, id string) error {
	return d.simple(ctx, "POST", "/containers/"+id+"/start", "start", 204, 304)
}

func (d *Docker) ContainerStop(ctx context.Context, id string) error {
	return d.simple(ctx, "POST", "/containers/"+id+"/stop?t=5", "stop", 204, 304)
}

func (d *Docker) ContainerRestart(ctx context.Context, id string) error {
	return d.simple(ctx, "POST", "/containers/"+id+"/restart?t=5", "restart", 204)
}

// ContainerRemove force-removes a container and its anonymous volumes (best-effort).
func (d *Docker) ContainerRemove(ctx context.Context, id string) {
	resp, err := d.do(ctx, "DELETE", "/containers/"+id+"?force=true&v=true", nil)
	if err == nil {
		drain(resp)
	}
}

// ContainerState returns the running state string (e.g. "running", "exited").
func (d *Docker) ContainerState(ctx context.Context, id string) (string, error) {
	resp, err := d.do(ctx, "GET", "/containers/"+id+"/json", nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", errBody("inspect", resp)
	}
	var out struct {
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := json.Unmarshal(drain(resp), &out); err != nil {
		return "", err
	}
	return out.State.Status, nil
}

// ContainerPort returns the host port a container port (e.g. "80/tcp") is
// published on, or "" if not published.
func (d *Docker) ContainerPort(ctx context.Context, id, portProto string) (string, error) {
	resp, err := d.do(ctx, "GET", "/containers/"+id+"/json", nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", errBody("inspect", resp)
	}
	var out struct {
		NetworkSettings struct {
			Ports map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(drain(resp), &out); err != nil {
		return "", err
	}
	if b := out.NetworkSettings.Ports[portProto]; len(b) > 0 {
		return b[0].HostPort, nil
	}
	return "", nil
}

// ExecResult captures a non-TTY exec's output and exit code.
type ExecResult struct {
	Stdout string
	Stderr string
	Code   int
}

// Exec runs a command in a container, capturing stdout/stderr and the exit code.
func (d *Docker) Exec(ctx context.Context, id string, cmd []string, env []string) (ExecResult, error) {
	create := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"Cmd":          cmd,
	}
	if len(env) > 0 {
		create["Env"] = env
	}
	resp, err := d.do(ctx, "POST", "/containers/"+id+"/exec", create)
	if err != nil {
		return ExecResult{}, err
	}
	if resp.StatusCode != 201 {
		return ExecResult{}, errBody("exec create", resp)
	}
	var ec struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(drain(resp), &ec); err != nil {
		return ExecResult{}, err
	}

	resp, err = d.do(ctx, "POST", "/exec/"+ec.ID+"/start", map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return ExecResult{}, err
	}
	if resp.StatusCode != 200 {
		return ExecResult{}, errBody("exec start", resp)
	}
	stdout, stderr := demuxStream(resp.Body)
	resp.Body.Close()

	// fetch exit code
	resp, err = d.do(ctx, "GET", "/exec/"+ec.ID+"/json", nil)
	if err != nil {
		return ExecResult{}, err
	}
	var info struct {
		ExitCode int `json:"ExitCode"`
	}
	json.Unmarshal(drain(resp), &info)
	return ExecResult{Stdout: string(stdout), Stderr: string(stderr), Code: info.ExitCode}, nil
}

// demuxStream splits Docker's multiplexed stdout/stderr stream (8-byte frame
// headers: [type][000][big-endian size]).
func demuxStream(r io.Reader) (stdout, stderr []byte) {
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			break
		}
		n := binary.BigEndian.Uint32(hdr[4:8])
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		if hdr[0] == 2 {
			stderr = append(stderr, buf...)
		} else {
			stdout = append(stdout, buf...)
		}
	}
	return
}

// CopyFile writes a single file into a container at dir (e.g. "/tmp") via the
// archive endpoint.
func (d *Docker) CopyFile(ctx context.Context, id, dir, name string, mode int64, content []byte) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content))}); err != nil {
		return err
	}
	if _, err := tw.Write(content); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "PUT",
		"http://docker/containers/"+id+"/archive?path="+url.QueryEscape(dir), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode != 200 {
		return errBody("copy archive", resp)
	}
	return nil
}

// WaitSystemd polls until systemd reports the system is up (running/degraded)
// or the timeout elapses.
func (d *Docker) WaitSystemd(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := d.Exec(ctx, id, []string{"systemctl", "is-system-running"}, nil)
		if err == nil {
			switch strings.TrimSpace(res.Stdout) {
			case "running", "degraded":
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("systemd did not become ready within %s", timeout)
}

// ExecConn is a hijacked interactive (TTY) exec stream plus its exec id.
type ExecConn struct {
	r      *bufio.Reader
	c      net.Conn
	ExecID string
}

func (e *ExecConn) Read(p []byte) (int, error)  { return e.r.Read(p) }
func (e *ExecConn) Write(p []byte) (int, error) { return e.c.Write(p) }
func (e *ExecConn) Close() error                { return e.c.Close() }

// HijackExec creates a TTY exec in the container and returns the raw
// bidirectional stream (used to bridge a browser terminal). With Tty:true the
// stream is *not* multiplexed — it is raw pty bytes both ways.
func (d *Docker) HijackExec(ctx context.Context, containerID string, cmd, env []string) (*ExecConn, error) {
	create := map[string]any{
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true,
		"Tty": true, "Cmd": cmd,
	}
	if len(env) > 0 {
		create["Env"] = env
	}
	resp, err := d.do(ctx, "POST", "/containers/"+containerID+"/exec", create)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 201 {
		return nil, errBody("exec create", resp)
	}
	var ec struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(drain(resp), &ec); err != nil {
		return nil, err
	}

	conn, err := net.Dial("unix", d.sock)
	if err != nil {
		return nil, err
	}
	body := `{"Detach":false,"Tty":true}`
	req := "POST /exec/" + ec.ID + "/start HTTP/1.1\r\n" +
		"Host: docker\r\n" +
		"Content-Type: application/json\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)) + body
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	// read status line + headers (until blank line); accept 101 or 200
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(statusLine, " 101") && !strings.Contains(statusLine, " 200") {
		conn.Close()
		return nil, fmt.Errorf("exec start: unexpected status %q", strings.TrimSpace(statusLine))
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
	return &ExecConn{r: br, c: conn, ExecID: ec.ID}, nil
}

// ResizeExec sets the TTY size for an exec session.
func (d *Docker) ResizeExec(ctx context.Context, execID string, w, h int) error {
	path := fmt.Sprintf("/exec/%s/resize?w=%d&h=%d", execID, w, h)
	resp, err := d.do(ctx, "POST", path, nil)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// hostArch maps the Go runtime arch to the image tag suffix used by make images.
func hostArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}
