package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Per-node disk throttling — the API equivalents of `docker run
// --device-read-bps/--device-write-bps`. Both take a *host block device path*
// plus a rate, and the kernel's blk-throttle applies the limit to that device's
// major:minor. Everything a stack container touches (its writable layer and any
// named volume) lives under the daemon's DockerRootDir, so a single device —
// the one backing that directory — covers all of a node's disk I/O.
//
// Two behaviours make guessing the path dangerous, both confirmed against a
// live daemon:
//
//   - A path that does not exist is a hard failure: the daemon refuses to
//     create the container ("stat /dev/nope: no such file or directory").
//   - A path that exists but is not the right *block* device fails silently.
//     Passing /dev/null is accepted, writes "1:3 wbps=..." into the container's
//     io.max, and throttles nothing at all.
//
// So the device is resolved once from the daemon's own root directory rather
// than assumed, and a user-supplied override is validated before use.
//
// What the limits actually govern, measured on cgroup v2 + ext4:
//
//   - Writes: both direct and buffered writes are throttled, because writeback
//     is attributed to the originating cgroup when the memory controller is
//     enabled alongside io. The limit applies to writeback reaching the device,
//     not to the write() syscall — a process that never syncs fills page cache
//     at full speed and pays on flush. Databases fsync on commit, so they feel
//     it.
//   - Reads: only reads that reach the device are throttled. Buffered re-reads
//     served from page cache bypass the limit entirely.
//
// Neither limit has a Vagrant/VirtualBox equivalent, so both are Docker-only.

// throttleDevice is the resolved host block device that stack containers do
// their I/O against, e.g. "/dev/sdd" with major:minor 8:48.
type throttleDevice struct {
	Path  string // "/dev/sdd" — what the Docker API's ThrottleDevice.Path wants
	Major int
	Minor int
}

func (t throttleDevice) String() string {
	return fmt.Sprintf("%s (%d:%d)", t.Path, t.Major, t.Minor)
}

// resolveThrottleDevice finds the block device backing the Docker daemon's root
// directory. The result never changes for a given daemon, so it is cached.
//
// Resolution has to work in both deployment shapes:
//
//   - app on the host: DockerRootDir ("/var/lib/docker") is stat-able directly.
//   - app in a container (docker-compose.yml, only the socket bind-mounted):
//     DockerRootDir does *not* exist in the app's mount namespace. But the app's
//     own SQLite volume is a named Docker volume, which by definition lives
//     under DockerRootDir, so stat-ing that gives the same filesystem — and
//     therefore the same st_dev — as the daemon's root.
//
// Either way st_dev yields the major:minor, and sysfs turns that back into a
// /dev name. sysfs is not namespaced for block devices, so /sys/dev/block is
// readable from inside the app container.
func (d *Docker) resolveThrottleDevice(ctx context.Context) (throttleDevice, error) {
	d.throttleOnce.Do(func() {
		d.throttleDev, d.throttleErr = d.detectThrottleDevice(ctx)
	})
	return d.throttleDev, d.throttleErr
}

func (d *Docker) detectThrottleDevice(ctx context.Context) (throttleDevice, error) {
	var probes []string
	if root := d.dockerRootDir(ctx); root != "" {
		probes = append(probes, root)
	}
	// The app's own data volume — a named volume, hence under DockerRootDir.
	probes = append(probes, filepath.Dir(envOr("DB_PATH", "dbcanvas.db")))

	var lastErr error
	for _, p := range probes {
		var st syscall.Stat_t
		if err := syscall.Stat(p, &st); err != nil {
			lastErr = err
			continue
		}
		dev, err := blockDeviceFor(uint64(st.Dev))
		if err != nil {
			lastErr = err
			continue
		}
		return dev, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no probe path available")
	}
	return throttleDevice{}, fmt.Errorf("resolve docker root device: %w", lastErr)
}

// dockerRootDir asks the daemon where it stores images, containers and volumes.
func (d *Docker) dockerRootDir(ctx context.Context) string {
	resp, err := d.do(ctx, "GET", "/info", nil)
	if err != nil {
		return ""
	}
	defer drain(resp)
	if resp.StatusCode != 200 {
		return ""
	}
	var info struct {
		DockerRootDir string `json:"DockerRootDir"`
	}
	if json.NewDecoder(resp.Body).Decode(&info) != nil {
		return ""
	}
	return info.DockerRootDir
}

// blockDeviceFor maps a filesystem's st_dev to the /dev path of the disk behind
// it. A partition is normalised to its parent disk: cgroup v1's blk-throttle
// only accepts whole-disk major:minor, and on cgroup v2 attributing a node's
// I/O to the whole disk is what the caller means anyway.
func blockDeviceFor(stDev uint64) (throttleDevice, error) {
	maj, min := unixMajor(stDev), unixMinor(stDev)
	if maj == 0 {
		// Major 0 is the anonymous/virtual range (overlayfs, tmpfs, btrfs).
		// There is no real block device to throttle.
		return throttleDevice{}, fmt.Errorf("filesystem is not on a block device (dev %d:%d)", maj, min)
	}
	if parent, ok := parentDisk(maj, min); ok {
		maj, min = parent[0], parent[1]
	}
	name, err := sysfsDevName(maj, min)
	if err != nil {
		return throttleDevice{}, err
	}
	return throttleDevice{Path: "/dev/" + name, Major: maj, Minor: min}, nil
}

// parentDisk reports the whole-disk major:minor for a partition, if the given
// device is one. /sys/dev/block/<maj>:<min>/partition exists only for
// partitions, and the parent's numbers are in ../dev.
func parentDisk(maj, min int) ([2]int, bool) {
	base := fmt.Sprintf("/sys/dev/block/%d:%d", maj, min)
	if _, err := os.Stat(base + "/partition"); err != nil {
		return [2]int{}, false
	}
	b, err := os.ReadFile(base + "/../dev")
	if err != nil {
		return [2]int{}, false
	}
	pm, pn, err := parseMajorMinor(strings.TrimSpace(string(b)))
	if err != nil {
		return [2]int{}, false
	}
	return [2]int{pm, pn}, true
}

// sysfsDevName returns the kernel's name for a block device ("sdd", "nvme0n1").
func sysfsDevName(maj, min int) (string, error) {
	uevent := fmt.Sprintf("/sys/dev/block/%d:%d/uevent", maj, min)
	b, err := os.ReadFile(uevent)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", uevent, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "DEVNAME="); ok && name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("no DEVNAME in %s", uevent)
}

func parseMajorMinor(s string) (int, int, error) {
	maj, min, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("malformed major:minor %q", s)
	}
	m, err := strconv.Atoi(maj)
	if err != nil {
		return 0, 0, err
	}
	n, err := strconv.Atoi(min)
	if err != nil {
		return 0, 0, err
	}
	return m, n, nil
}

// unixMajor/unixMinor decode a dev_t the way the kernel encodes it (12-bit
// major split across the word). Same arithmetic as glibc's major()/minor(),
// reimplemented so the app keeps to the standard library.
func unixMajor(dev uint64) int {
	return int((dev>>8)&0xfff | (dev>>32)&^uint64(0xfff))
}

func unixMinor(dev uint64) int {
	return int(dev&0xff | (dev>>12)&^uint64(0xff))
}

// validateThrottleDevice checks a user-supplied device path before it reaches
// the daemon, so the silent-no-op case (a path that exists but is not a block
// device) is reported at deploy time instead of producing a container whose
// limit does nothing.
func validateThrottleDevice(path string) (throttleDevice, error) {
	if !strings.HasPrefix(path, "/dev/") {
		return throttleDevice{}, fmt.Errorf("device path must be under /dev/, got %q", path)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return throttleDevice{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return throttleDevice{}, fmt.Errorf("%s is not a block device — a rate limit on it would be silently ignored", path)
	}
	// For a device node, Rdev (not Dev) carries its own major:minor.
	return throttleDevice{Path: path, Major: unixMajor(uint64(st.Rdev)), Minor: unixMinor(uint64(st.Rdev))}, nil
}

// throttleDeviceFor picks the device a node's rate limits apply to: an explicit
// override when the design supplies one, otherwise the auto-detected device
// behind the Docker root. Both paths are validated.
func (d *Docker) throttleDeviceFor(ctx context.Context, override string) (throttleDevice, error) {
	if override = strings.TrimSpace(override); override != "" {
		return validateThrottleDevice(override)
	}
	return d.resolveThrottleDevice(ctx)
}

// ---------------------------------------------------------------- already-running containers

// Docker honours device rate limits only at *create* time. The update endpoint accepts
// BlkioDeviceReadBps/BlkioDeviceWriteBps and silently discards them — HostConfig reads back
// empty and io.max never changes — and `docker update` has no CLI flag for them at all.
// k3d, for its part, passes through only --servers-memory/--agents-memory/--gpus/ulimits.
//
// So a container DBCanvas did not create itself (every k3s node in a K3D frame) can only be
// throttled by writing the kernel interface directly. Two constraints shape how:
//
//   - It must be written from the *host* side, at the container's own scope. A container
//     cannot limit its own cgroup-namespace root: writing /sys/fs/cgroup/io.max inside a
//     privileged k3s node fails with EPERM. Only a sub-cgroup (/init) is writable from
//     inside, and that does not cover /kubepods — i.e. not the pods, which is the point.
//   - The app's own container mounts only the Docker socket, not a writable cgroup tree,
//     so it cannot do the write itself.
//
// Hence a short-lived privileged helper with the host cgroup namespace and a writable
// /sys/fs/cgroup. It runs from an image the caller already has local, so this costs no pull.
// Verified on a k3s node: 2.7 GB/s → 4.2 MB/s.

// ioMaxLine renders an io.max value. "max" clears a direction, which is how a limit is
// removed — writing nothing would leave a previous deploy's ceiling in place.
func ioMaxLine(dev throttleDevice, readBPS, writeBPS int64) string {
	rate := func(v int64) string {
		if v <= 0 {
			return "max"
		}
		return strconv.FormatInt(v, 10)
	}
	return fmt.Sprintf("%d:%d rbps=%s wbps=%s", dev.Major, dev.Minor, rate(readBPS), rate(writeBPS))
}

// cgroupIOMaxScript writes (and reads back) io.max for each container id it is given.
// Both cgroup drivers put the scope in a different place and a cgroup v1 host has no
// io.max at all, so the script locates the directory rather than assuming, and fails
// loudly if it cannot — a limit that was silently not applied is the failure mode this
// whole feature exists to avoid.
const cgroupIOMaxScript = `set -e
if [ ! -e /sys/fs/cgroup/cgroup.controllers ]; then
  echo "host is on cgroup v1; per-node disk limits need cgroup v2" >&2
  exit 1
fi
for id in $IDS; do
  cg=""
  for p in "/sys/fs/cgroup/system.slice/docker-$id.scope" "/sys/fs/cgroup/docker/$id"; do
    [ -d "$p" ] && cg="$p" && break
  done
  if [ -z "$cg" ]; then
    cg=$(find /sys/fs/cgroup -maxdepth 4 -type d -name "*$id*" 2>/dev/null | head -1)
  fi
  if [ -z "$cg" ] || [ ! -w "$cg/io.max" ]; then
    echo "no writable io.max for container $id" >&2
    exit 1
  fi
  echo "$LIMIT" > "$cg/io.max"
  echo "$id $(cat "$cg/io.max" | head -1)"
done
`

// ApplyDiskLimits imposes read/write rate limits on containers that already exist, by
// writing io.max in each one's host-side cgroup. Returns the helper's output (one line
// per container, echoing the io.max the kernel actually holds) so callers can log what
// was really applied rather than what was requested.
//
// image must already be present locally. Zero for both rates clears any existing limit.
func (d *Docker) ApplyDiskLimits(ctx context.Context, image string, ids []string, devPath string, readBPS, writeBPS int64) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	dev, err := d.throttleDeviceFor(ctx, devPath)
	if err != nil {
		return "", err
	}
	// The helper idles rather than running the script as its command: ContainerCreate
	// applies RestartPolicy=unless-stopped, which would relaunch a one-shot container the
	// moment it exited. Exec gives the exit code and output directly, and the container is
	// force-removed before its sleep ever elapses.
	spec := ContainerSpec{
		Name:       fmt.Sprintf("dbcanvas-iomax-%d", time.Now().UnixNano()),
		Image:      image,
		Privileged: true, // ContainerCreate then adds the host cgroupns + writable /sys/fs/cgroup
		Cmd:        []string{"sleep", "300"},
	}
	cid, err := d.ContainerCreate(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("create io.max helper: %w", err)
	}
	defer d.ContainerRemove(ctx, cid)
	if err := d.ContainerStart(ctx, cid); err != nil {
		return "", fmt.Errorf("start io.max helper: %w", err)
	}
	res, err := d.Exec(ctx, cid, []string{"sh", "-c", cgroupIOMaxScript}, []string{
		"IDS=" + strings.Join(ids, " "),
		"LIMIT=" + ioMaxLine(dev, readBPS, writeBPS),
	})
	if err != nil {
		return "", fmt.Errorf("run io.max helper: %w", err)
	}
	if res.Code != 0 {
		return "", fmt.Errorf("io.max helper exited %d: %s", res.Code, strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// throttleCache is embedded in Docker; the resolved device is daemon-lifetime
// stable so it is computed at most once.
type throttleCache struct {
	throttleOnce sync.Once
	throttleDev  throttleDevice
	throttleErr  error
}
