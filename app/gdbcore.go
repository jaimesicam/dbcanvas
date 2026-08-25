package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// gdbcore.go — turning a Linux Client node into a core-dump analysis host.
//
// The situation this exists for: mysqld crashed on a production server, you have the core file,
// and you cannot analyse it there. Installing gdb and a few hundred megabytes of debug symbols on
// a production box is not something anyone will sign off, so the core and the origin host's
// shared libraries get copied somewhere else and read with
//
//	gdb -ex "set solib-search-path <libs>" -ex "set sysroot <libs>" \
//	    -ex "set pagination 0" -ex "thread apply all bt" /usr/sbin/mysqld <core>
//
// Three things have to be true for that command to produce anything but hex addresses, and all
// three are what this file arranges:
//
//  1. **The core and the libraries have to be reachable.** They are large — the sample core is
//     811 MB — so they are *mounted*, never copied: two read-only bind mounts onto the node,
//     /coredumps and /sysroot. See gdbBinds.
//
//  2. **The exact matching mysqld and its debug symbols have to be installed.** Not "a" mysqld:
//     the same build, from the same series, for the same OS. That is why the node asks for the
//     product, the major and the pinned minor at deploy time, and why the client's OS has to match
//     the crashed server's — an el8 build and an el9 build of one version have different build-ids
//     and the symbols simply will not apply. See gdbPackages.
//
//  3. **Somebody has to say when 1 or 2 is wrong**, because gdb will not. Point it at the wrong
//     libraries and it prints a backtrace; point it at the wrong build and it prints a backtrace.
//     Both are fiction. gdbCoreProbe answers that before a session is opened.
//
// The mounts are always read-only and always confined to GDB_MOUNT_ROOT: a bind mount is the one
// place in DBCanvas where a string a user typed reaches the Docker daemon as a host path.

const (
	// Where the two host directories appear inside the node. /sysroot is gdb's own name for
	// "the root of the target system's filesystem", which is exactly what the copied libraries
	// are a fragment of.
	gdbCoreMount    = "/coredumps"
	gdbSysrootMount = "/sysroot"
	// The host directory every mounted path must live under. A bind mount hands the daemon a
	// path with no sandbox of any kind, so this is the sandbox: without it, anyone who can add
	// a node to a stack can read any directory the daemon can see.
	gdbMountRootEnv = "GDB_MOUNT_ROOT"
	gdbMountRootDef = "/srv/coredumps"
	// Where GDB_MOUNT_ROOT is mounted inside the pre-flight probe container.
	gdbProbeMount = "/probe"
	// How long the pre-flight mount probe gets. It runs `ls` in a throwaway container; a second
	// would do, but an image that has to be pulled first would not.
	gdbProbeTimeout = 60 * time.Second
)

// gdbMountRoot is the host directory the two bind mounts are confined to.
func gdbMountRoot() string {
	return path.Clean(envOr(gdbMountRootEnv, gdbMountRootDef))
}

// gdbCleanHostPath validates one of the two host paths a node asks to mount.
//
// It cannot check that the path *exists*: DBCanvas talks to the daemon over a socket and does not
// share its filesystem (docker-compose mounts only /data, versions.yaml and the socket), so a host
// path is a string this process can never stat. gdbProbeMounts answers existence later, from the
// daemon's side. What is checkable here is shape and confinement, and both are worth refusing
// early — a design that fails validation costs nothing, a deploy that fails costs minutes.
func gdbCleanHostPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("no path")
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%q is not an absolute path", p)
	}
	// A bind spec is colon-separated, so a colon in the path would silently become a mode or a
	// second destination.
	if strings.ContainsAny(p, ":\x00\n") {
		return "", fmt.Errorf("%q contains a character a bind mount cannot carry", p)
	}
	clean := path.Clean(p)
	root := gdbMountRoot()
	if clean != root && !strings.HasPrefix(clean, root+"/") {
		return "", fmt.Errorf("%s is outside %s=%s", clean, gdbMountRootEnv, root)
	}
	return clean, nil
}

// gdbBinds are the two read-only bind mounts a core-dump client gets. Read-only is not politeness:
// gdb writes nothing, and a core file that a session could truncate is a core file you only get to
// read once.
func gdbBinds(coreDir, libDir string) []string {
	return []string{
		coreDir + ":" + gdbCoreMount + ":ro",
		libDir + ":" + gdbSysrootMount + ":ro",
	}
}

// ---------------------------------------------------------------- packages

// gdbProducts are the products whose debug symbols can be installed, in the order the designer
// offers them. Keyed by the same strings the version catalogs use.
var gdbProducts = []string{"ps", "pxc"}

// gdbProductLabel names a product the way its own project does.
func gdbProductLabel(p string) string {
	switch p {
	case "ps":
		return "Percona Server for MySQL"
	case "pxc":
		return "Percona XtraDB Cluster"
	}
	return p
}

// gdbPackages is the package list to install for one product on one OS family.
//
// The RHEL list has three entries and the third looks redundant. It is not, and getting this wrong
// produces the exact failure this whole feature exists to avoid — a backtrace that resolves to
// nothing, with no error anywhere:
//
//   - percona-server-server-debuginfo carries mysqld's symbols…
//   - …but they are DWZ-compressed, and the common file they refer into ships *separately*, in
//     percona-server-debuginfo. That package is three files and no symbols of its own. Install
//     only the first and gdb has half a symbol table; install only the second (which is what a
//     `dnf install percona-server-debuginfo` gets you, and what was on the sample node) and gdb
//     has none, while `rpm -q` cheerfully reports that debuginfo is installed.
//
// Debian is simpler: one -dbg package. It has its own trap next door — percona-xtradb-cluster-
// server-debug also exists and is a *debug build of the server*, not symbols for the release
// build, so it must never be substituted here.
//
// The *-debugsource package is the fourth, and it is what turns a stack into an explanation. Debug
// symbols give a frame a file and a line number; without the source, `fts0que.cc:2815` is a
// coordinate for a file you do not have. debugsource ships the code those coordinates point into,
// and — the part that makes it free — it installs to exactly the path the DWARF records, e.g.
// /usr/src/debug/percona-server-8.0.30-22.1.el9.x86_64/percona-server-8.0.30-22/sql/item_sum.cc.
// So gdb, and the analyzer's source pane, find it with no configuration at all.
//
// Debian has no equivalent: its -dbg package carries symbols only, and percona-server-source is an
// upstream tarball laid out differently. A Debian client still resolves frames to file and line;
// it just cannot show the line.
//
// The server package itself is installed for one reason: /usr/sbin/mysqld, the executable gdb
// needs alongside the core. It is never started.
func gdbPackages(product, os string) []string {
	debian := isDebianOS(os)
	switch product {
	case "pxc":
		if debian {
			return []string{"percona-xtradb-cluster-server", "percona-xtradb-cluster-dbg"}
		}
		return []string{
			"percona-xtradb-cluster-server",
			"percona-xtradb-cluster-server-debuginfo",
			"percona-xtradb-cluster-debuginfo",
			"percona-xtradb-cluster-debugsource",
		}
	default: // ps
		if debian {
			return []string{"percona-server-server", "percona-server-dbg"}
		}
		return []string{
			"percona-server-server",
			"percona-server-server-debuginfo",
			"percona-server-debuginfo",
			"percona-server-debugsource",
		}
	}
}

// gdbReleaseProduct is the percona-release product name that enables the right repository, reusing
// the mappings the database node types already keep for the same series.
func gdbReleaseProduct(product, major string) string {
	if product == "pxc" {
		return pxcProduct(major)
	}
	return psClientProduct(major)
}

// gdbReleaseRepo is the repository path for the series percona-release has no working alias for
// (see psRepoRHEL) — the same escape hatch the Percona Server node uses.
func gdbReleaseRepo(product, major string) string {
	if product == "pxc" {
		switch major {
		case "8.4":
			return "pxc-84-lts"
		case "5.7":
			return "pxc-57"
		}
		return "pxc-80"
	}
	return psRepoName(major)
}

// gdbToolPackages are the analysis tools themselves. elfutils is not optional decoration: eu-unstrip
// reading a core's build-id notes is the only way to answer "are these the right libraries?"
// without guessing, and that question is half of what goes wrong here.
func gdbToolPackages(os string) []string {
	if isDebianOS(os) {
		return []string{"gdb", "elfutils"}
	}
	return []string{"gdb", "elfutils", "binutils"}
}

// gdbInstallRHEL / gdbInstallDebian install the tools and the pinned symbol packages.
//
// The server package is installed with its service left alone — dnf does not start anything, and
// nothing here enables it. On a node whose whole purpose is reading a core file, a running mysqld
// would only compete for memory.
const gdbInstallRHEL = pinInstallRHEL + `set -e
dnf -y -q module disable mysql >/dev/null 2>&1 || true
dnf -y -q install $TOOLS
` + psRepoRHEL + `pin_install $PKGS
`

const gdbInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq $TOOLS
` + psRepoDebian + `apt-get update -qq >/dev/null
pin_install $PKGS
`

// gdbInstallEnv is the environment the install script reads: which repository to enable, which
// minor to pin to, and the two package lists.
func gdbInstallEnv(product, os, major, version string) []string {
	return []string{
		"PRODUCT=" + gdbReleaseProduct(product, major),
		"REPO=" + gdbReleaseRepo(product, major),
		"VER=" + version,
		"PKGS=" + strings.Join(gdbPackages(product, os), " "),
		"TOOLS=" + strings.Join(gdbToolPackages(os), " "),
	}
}

// gdbServerBinary is where the installed package puts the daemon. It is the *fallback*: the
// binary gdb should read is the one that crashed, and that one usually arrives in the mounted
// library directory alongside the `ldd` output it was copied with. See gdbBinaryProbeScript.
const gdbServerBinary = "/usr/sbin/mysqld"

// ---------------------------------------------------------------- on-node probes

// gdbNodeConfig is the core-dump half of a Linux Client's recorded config: what was mounted, what
// was installed, and what the deploy found once it had. The page reads it instead of re-deriving
// any of it.
type gdbNodeConfig struct {
	Enabled    bool   `json:"gdbEnabled"`
	CoreDir    string `json:"gdbCoreDir"` // the host path, for display
	LibDir     string `json:"gdbLibDir"`  // the host path, for display
	Product    string `json:"gdbProduct"`
	Major      string `json:"gdbMajor"`
	Version    string `json:"gdbVersion"`
	Binary     string `json:"gdbBinary"`     // resolved on the node — the mounted copy when there is one
	BinaryFrom string `json:"gdbBinaryFrom"` // "mounted" | "installed"
	BuildID    string `json:"gdbBuildId"`    // of that binary
	// InstalledID is the build-id of the package's own mysqld, kept only to compare against the
	// mounted copy: they differ exactly when the version or the OS picked for this node is not
	// the one the crashed server ran, which is the one failure that produces a wrong stack
	// rather than no stack.
	InstalledID string `json:"gdbInstalledId,omitempty"`
	HasSyms     bool   `json:"gdbHasSymbols"`
	Status      string `json:"gdbStatus"` // "ready", or why not
	CoreCount   int    `json:"gdbCoreCount"`
}

// gdbProbeMounts checks that both host directories exist *on the Docker host* before the node is
// created, and reports what is in them.
//
// This is not defensive programming, it is the difference between an error and a mystery. Docker
// resolves a bind source on the daemon's side and **creates a missing one as an empty directory**
// rather than failing, so a typo in the core path otherwise produces a node that comes up
// perfectly and an analyzer page reporting an empty directory — with nothing in any log to say the
// path was wrong.
//
// What it does *not* do is bind the two paths themselves, which was the obvious implementation and
// is quietly wrong: the probe's own mount is what makes the daemon create the missing directory.
// The check would still fail correctly, and every failed check would leave a root-owned empty
// directory on the host — one per typo. So the probe binds GDB_MOUNT_ROOT, which the administrator
// created deliberately and every allowed path lives under, and looks *inside* it. One bind, no
// side effects, and a root that does not exist is a single clear failure rather than an accident.
func (a *App) gdbProbeMounts(ctx context.Context, image, coreDir, libDir string) (cores, libs, binaries int, err error) {
	root := gdbMountRoot()
	rel := func(p string) (string, bool) {
		if p == root {
			return ".", true
		}
		if !strings.HasPrefix(p, root+"/") {
			return "", false
		}
		return strings.TrimPrefix(p, root+"/"), true
	}
	coreRel, ok1 := rel(coreDir)
	libRel, ok2 := rel(libDir)
	if !ok1 || !ok2 {
		return 0, 0, 0, fmt.Errorf("a path is outside %s=%s", gdbMountRootEnv, root)
	}

	ctx, cancel := context.WithTimeout(ctx, gdbProbeTimeout)
	defer cancel()
	eng := a.engCtx(ctx)
	// The node images declare CMD (not ENTRYPOINT), so overriding the command with a sleep gives
	// a plain container rather than a second systemd — this probe wants a shell, not an init.
	cid, err := eng.ContainerCreate(ctx, ContainerSpec{
		Name:      fmt.Sprintf("dbcanvas-gdbprobe-%d", time.Now().UnixNano()),
		Image:     image,
		Cmd:       []string{"sleep", "60"},
		NoRestart: true,
		Binds:     []string{root + ":" + gdbProbeMount + ":ro"},
	})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("create the mount probe: %w", err)
	}
	defer eng.ContainerRemove(context.WithoutCancel(ctx), cid)
	if err := eng.ContainerStart(ctx, cid); err != nil {
		return 0, 0, 0, fmt.Errorf("start the mount probe: %w", err)
	}
	res, err := eng.Exec(ctx, cid, []string{"sh", "-c",
		`C="` + gdbProbeMount + `/$CORE"; L="` + gdbProbeMount + `/$LIBS"
[ -d "$C" ] || { echo "missing=core"; exit 0; }
[ -d "$L" ] || { echo "missing=libs"; exit 0; }
printf 'cores=%s
libs=%s
mysqld=%s
'   "$(find "$C" -maxdepth 1 -type f 2>/dev/null | wc -l)"   "$(find "$L" -name '*.so*' 2>/dev/null | wc -l)"   "$(find "$L" -maxdepth 3 -name mysqld -type f 2>/dev/null | wc -l)"`},
		[]string{"CORE=" + coreRel, "LIBS=" + libRel})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("run the mount probe: %w", err)
	}
	for _, ln := range strings.Split(res.Stdout, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		switch k {
		case "missing":
			which := map[string]string{"core": coreDir, "libs": libDir}[v]
			return 0, 0, 0, fmt.Errorf("%s does not exist on the Docker host", which)
		case "cores":
			cores = n
		case "libs":
			libs = n
		case "mysqld":
			binaries = n
		}
	}
	return cores, libs, binaries, nil
}

// gdbResolveBinary picks the executable gdb will read and reports what is known about it.
func (a *App) gdbResolveBinary(ctx context.Context, id string) gdbBinaryInfo {
	var info gdbBinaryInfo
	res, err := a.engCtx(ctx).Exec(ctx, id, []string{"sh", "-c", gdbBinaryProbeScript}, nil)
	if err != nil || res.Code != 0 {
		return info
	}
	for _, ln := range strings.Split(res.Stdout, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
		if !ok {
			continue
		}
		switch k {
		case "binary":
			info.Path = v
		case "source":
			info.From = v
		case "buildid":
			info.BuildID = v
		case "debuglink":
			info.DebugLink = v
		case "sameas":
			info.SameAsInstalled = v
		case "syms":
			info.HasSyms = v == "yes"
		}
	}
	return info
}

// gdbBinaryInfo is what the node knows about the executable gdb will read.
type gdbBinaryInfo struct {
	Path      string
	From      string // "mounted" | "installed"
	BuildID   string // usually empty: Percona's mysqld carries no build-id note
	DebugLink string // what .gnu_debuglink names, which is how its symbols are actually found
	HasSyms   bool
	// SameAsInstalled compares the mounted copy with the package's own binary: "yes", "no", or
	// "" when there is nothing to compare against. With no build-id this is the only way to
	// answer "are these symbols for this build?".
	SameAsInstalled string
}

// gdbBinaryProbeScript picks the executable gdb will read, and reports how its debug symbols can
// be found — which for a Percona MySQL package is not the way anyone expects.
//
// **The mounted copy wins.** The recipe this feature automates copies `mysqld` off the crashed host
// together with everything `ldd $(which mysqld)` named, into one directory — so the binary sitting
// next to the libraries *is the one that produced the core*, byte for byte, and no version guess
// can be wrong about it. The installed package is the fallback for a directory that holds only
// libraries.
//
// **There is no build-id.** Every description of how gdb finds separate debug symbols starts with
// the build-id note, and Percona's mysqld does not have one: `readelf -n` shows only
// `.gnu.build.attributes`, and the `.build-id` tree the debuginfo package ships is for its other
// files. What it does have is a `.gnu_debuglink` naming `mysqld-8.0.16-7.1.el8.x86_64.debug` — and
// gdb resolves a debuglink **relative to the directory the binary is in**: `<dir>/<link>`,
// `<dir>/.debug/<link>`, then `/usr/lib/debug/<dir>/<link>`.
//
// That is what makes preferring the mounted copy a trap rather than a free win. The package puts
// its debug file under /usr/lib/debug/usr/sbin/, which is where gdb looks for /usr/sbin/mysqld and
// nowhere near where it looks for /sysroot/mysqld. gdbLinkDebugScript bridges the two; this probe
// checks all three rules, so "symbols" means gdb will actually find them rather than that a
// package is installed.
//
// With no build-id there is also nothing to compare builds *by*, so the two binaries are compared
// directly instead: `cmp` over 80 MB out of the page cache is cheaper than being wrong about which
// build produced the core.
const gdbBinaryProbeScript = `set -e
BIN=""
SRC=""
for c in ` + gdbSysrootMount + `/mysqld ` + gdbSysrootMount + `/usr/sbin/mysqld ` + gdbSysrootMount + `/sbin/mysqld; do
  [ -f "$c" ] && { BIN="$c"; SRC=mounted; break; }
done
if [ -z "$BIN" ]; then
  for c in ` + gdbServerBinary + ` /usr/sbin/mysqld-debug /usr/bin/mysqld; do
    [ -x "$c" ] && { BIN="$c"; SRC=installed; break; }
  done
fi
echo "binary=$BIN"
echo "source=$SRC"
[ -n "$BIN" ] || exit 0

ID=$(readelf -n "$BIN" 2>/dev/null | awk '/Build ID:/ { print $3; exit }')
echo "buildid=$ID"

# The debuglink is a NUL-terminated name in its own section; a string dump is the readable way to
# it, and readelf is there because binutils is installed with gdb.
LINK=$(readelf -p .gnu_debuglink "$BIN" 2>/dev/null | awk -F']' '/\.debug/ { gsub(/^ +/, "", $2); print $2; exit }')
echo "debuglink=$LINK"

DIR=$(dirname "$BIN")
SYMS=no
if [ -n "$ID" ]; then
  P="/usr/lib/debug/.build-id/$(echo "$ID" | cut -c1-2)/$(echo "$ID" | cut -c3-).debug"
  [ -e "$P" ] && SYMS=yes
fi
if [ "$SYMS" = no ] && [ -n "$LINK" ]; then
  for p in "$DIR/$LINK" "$DIR/.debug/$LINK" "/usr/lib/debug$DIR/$LINK"; do
    [ -e "$p" ] && { SYMS=yes; break; }
  done
fi
echo "syms=$SYMS"

# Is the mounted copy the same build as the packages installed here? With no build-id to compare,
# the bytes are the comparison.
if [ "$SRC" = mounted ] && [ -f ` + gdbServerBinary + ` ]; then
  if cmp -s "$BIN" ` + gdbServerBinary + `; then echo "sameas=yes"; else echo "sameas=no"; fi
fi
`

// gdbLinkDebugScript makes the package's debug files reachable for a binary that is not where the
// package put it.
//
// gdb searches /usr/lib/debug/<the binary's own directory>/<debuglink>. The package installed its
// debug file under /usr/lib/debug/usr/sbin/; the binary being read is at /sysroot/mysqld. So the
// same files are linked into /usr/lib/debug/sysroot/ as well — symlinks, into a directory on the
// node's own filesystem, leaving the read-only mount untouched. It is two lines, and it is the
// difference between a backtrace with arguments and line numbers and one without.
const gdbLinkDebugScript = `set -e
[ -n "$BIN" ] || exit 0
DIR=$(dirname "$BIN")
case "$DIR" in /usr/sbin|/usr/bin) exit 0 ;; esac
[ -d /usr/lib/debug/usr/sbin ] || exit 0
mkdir -p "/usr/lib/debug$DIR"
for f in /usr/lib/debug/usr/sbin/*.debug; do
  [ -e "$f" ] || continue
  ln -sf "$f" "/usr/lib/debug$DIR/$(basename "$f")"
done
`

// ---------------------------------------------------------------- listing cores

// gdbCore is one file in the mounted core directory, with everything that can be said about it
// before gdb is started.
type gdbCore struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
	// From the core's own notes.
	Executable string `json:"executable"` // what the kernel recorded as the crashed program
	Signal     string `json:"signal"`     // e.g. "SIGSEGV (11)", when readable
	BuildID    string `json:"buildId"`    // the executable's build-id as recorded in the core
	// The verdict. Missing is what makes a backtrace fiction, so it is a list, not a count.
	BuildIDMatch bool     `json:"buildIdMatch"`
	Missing      []string `json:"missing"`
	Resolved     int      `json:"resolved"`
	Note         string   `json:"note,omitempty"` // why the verdict could not be reached
}

// gdbListCores reads the mounted directory and, for each core, asks eu-unstrip which of the objects
// the process had mapped can be found — the question that decides whether the libraries somebody
// copied off the crashed host are the right ones.
func (a *App) gdbListCores(ctx context.Context, id, wantBuildID string) ([]gdbCore, error) {
	res, err := a.engCtx(ctx).Exec(ctx, id, []string{"sh", "-c", gdbListCoresScript}, nil)
	if err != nil {
		return nil, fmt.Errorf("list the core files: %w", err)
	}
	if res.Code != 0 && strings.TrimSpace(res.Stdout) == "" {
		return nil, fmt.Errorf("list the core files: %s", lastLines(res.Stderr, 200))
	}
	out := []gdbCore{}
	var cur *gdbCore
	for _, ln := range strings.Split(res.Stdout, "\n") {
		k, v, ok := strings.Cut(ln, "\t")
		if !ok {
			continue
		}
		switch k {
		case "core":
			out = append(out, gdbCore{Name: v})
			cur = &out[len(out)-1]
		case "size":
			if cur != nil {
				cur.Size, _ = strconv.ParseInt(v, 10, 64)
			}
		case "mtime":
			if cur != nil {
				if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
					cur.Modified = time.Unix(sec, 0).UTC().Format(time.RFC3339)
				}
			}
		case "exe":
			if cur != nil {
				cur.Executable = v
			}
		case "buildid":
			if cur != nil {
				cur.BuildID = v
			}
		case "found":
			if cur != nil {
				cur.Resolved++
			}
		case "missing":
			if cur != nil && v != "" {
				cur.Missing = append(cur.Missing, v)
			}
		case "note":
			if cur != nil {
				cur.Note = v
			}
		}
	}
	for i := range out {
		sort.Strings(out[i].Missing)
		out[i].BuildIDMatch = wantBuildID != "" && out[i].BuildID != "" &&
			strings.EqualFold(out[i].BuildID, wantBuildID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified > out[j].Modified })
	return out, nil
}

// gdbListCoresScript emits a tab-separated record per core file.
//
// eu-unstrip -n --core prints one line per mapped object: address range, build-id, the path to the
// debug file it found (or "-"), the path to the object (or "-"), and the soname. An object it could
// not find anywhere is the interesting case, and it is exactly what "I copied the libraries" gets
// wrong when the copy misses one. `file` gives the executable and signal from the core's psinfo
// note without loading the (possibly gigabyte) core into gdb.
const gdbListCoresScript = `set -e
cd ` + gdbCoreMount + ` 2>/dev/null || exit 0
for f in *; do
  [ -f "$f" ] || continue
  printf 'core\t%s\n' "$f"
  printf 'size\t%s\n' "$(stat -c %s "$f" 2>/dev/null)"
  printf 'mtime\t%s\n' "$(stat -c %Y "$f" 2>/dev/null)"
  desc=$(file -b "$f" 2>/dev/null)
  case "$desc" in
    *core\ file*)
      exe=$(printf '%s' "$desc" | sed -n "s/.*execfn: '\\([^']*\\)'.*/\\1/p")
      [ -z "$exe" ] && exe=$(printf '%s' "$desc" | sed -n "s/.*from '\\([^' ]*\\).*/\\1/p")
      printf 'exe\t%s\n' "$exe"
      sig=$(printf '%s' "$desc" | sed -n 's/.*\(SIG[A-Z]*\).*/\1/p')
      [ -n "$sig" ] && printf 'signal\t%s\n' "$sig"
      ;;
    *) printf 'note\tnot an ELF core file\n'; continue ;;
  esac
  if command -v eu-unstrip >/dev/null 2>&1; then
    eu-unstrip -n --core "$f" 2>/dev/null | while IFS= read -r line; do
      obj=$(printf '%s' "$line" | awk '{print $NF}')
      src=$(printf '%s' "$line" | awk '{print $(NF-1)}')
      bid=$(printf '%s' "$line" | awk '{print $2}')
      case "$bid" in *@*) bid=$(printf '%s' "$bid" | cut -d@ -f1);; esac
      case "$obj" in
        *mysqld*|*/usr/sbin/*) printf 'buildid\t%s\n' "$bid" ;;
      esac
      if [ "$src" = "-" ] && [ "$obj" != "-" ]; then
        printf 'missing\t%s\n' "$obj"
      else
        printf 'found\t1\n'
      fi
    done
  else
    printf 'note\teu-unstrip is not installed, so library coverage is unknown\n'
  fi
done
`

// gdbSolibPath is the solib-search-path gdb is given: the mounted directory itself, plus every
// subdirectory of it that holds a shared object.
//
// Both layouts people arrive with are covered by that. Copying the output of `ldd` gives a flat
// directory of .so files, which only solib-search-path can use; copying /lib64 and /usr/lib64 off
// the host gives a tree, which is what sysroot is for. The hand-written recipe sets both to the
// same path and gets the flat case; walking one level down also gets the tree case, at the cost of
// one `find`.
func (a *App) gdbSolibPath(ctx context.Context, id string) string {
	res, err := a.engCtx(ctx).Exec(ctx, id, []string{"sh", "-c",
		`find ` + gdbSysrootMount + ` -name '*.so*' -printf '%h\n' 2>/dev/null | sort -u | head -200`}, nil)
	dirs := []string{gdbSysrootMount}
	if err == nil {
		for _, ln := range strings.Split(res.Stdout, "\n") {
			if d := strings.TrimSpace(ln); d != "" && d != gdbSysrootMount {
				dirs = append(dirs, d)
			}
		}
	}
	return strings.Join(dirs, ":")
}

// ---------------------------------------------------------------- validation

// gdbNodeIssues checks a Linux Client that asked to be a core-dump analysis host.
//
// Every one of these is an error rather than a warning, with one exception, because each describes
// a node that would deploy successfully and then be useless: mounts pointing nowhere, or symbols
// that cannot be installed. A design that fails validation costs nothing; a deploy that produces a
// node you only discover is wrong when a backtrace is full of question marks costs the time it
// took to install several hundred megabytes of packages.
func (a *App) gdbNodeIssues(n designNode, st Stack) []issue {
	var out []issue
	name := "Linux Client " + n.Label

	// The Vagrant engine ignores Binds entirely, so the two mounts would simply not exist and
	// nothing would say so.
	if st.Backend == BackendVagrant && a.vagrant != nil {
		return []issue{{"error", name + " is set up for core-dump analysis, which needs bind mounts — those are available on the Docker backend only"}}
	}

	for label, p := range map[string]string{"core-dump directory": n.GDBCoreDir, "library directory": n.GDBLibDir} {
		if _, err := gdbCleanHostPath(p); err != nil {
			out = append(out, issue{"error", fmt.Sprintf("%s: the %s %s", name, label, err)})
		}
	}
	if cores, libs := path.Clean(n.GDBCoreDir), path.Clean(n.GDBLibDir); cores != "" && cores == libs {
		out = append(out, issue{"warning", name + " mounts the same directory as both the core dumps and the libraries — that works, but the libraries are usually a separate copy of the crashed host's /lib64"})
	}

	product := strings.TrimSpace(n.GDBProduct)
	if product == "" {
		product = "ps"
	}
	if !sliceHas(gdbProducts, product) {
		out = append(out, issue{"error", name + " asks for debug symbols for an unknown product " + strconv.Quote(product)})
		return out
	}
	if strings.TrimSpace(n.GDBMajor) == "" {
		out = append(out, issue{"error", name + " needs the " + gdbProductLabel(product) + " major series the core came from — that is what selects the debug-symbol packages"})
		return out
	}

	// The catalog is per OS image, which is exactly the constraint that matters here: an el8
	// build and an el9 build of one version have different build-ids, so symbols for the wrong
	// OS do not apply to the core at all. Offering only what exists for this node's OS is what
	// keeps that honest, and a version that is not in it is an error rather than a surprise.
	cat := loadPSCatalog()
	if product == "pxc" {
		cat = loadPXCCatalog()
	}
	var entry *PXCImage
	for i := range cat {
		if cat[i].OS == n.OS && cat[i].OSVersion == n.OSVersion {
			entry = &cat[i]
			break
		}
	}
	if entry == nil {
		out = append(out, issue{"error", fmt.Sprintf("%s runs %s %s, which %s publishes no packages for — set the node's OS to the one the crashed server ran",
			name, n.OS, n.OSVersion, gdbProductLabel(product))})
		return out
	}
	minors := entry.Versions[strings.TrimSpace(n.GDBMajor)]
	if len(minors) == 0 {
		out = append(out, issue{"error", fmt.Sprintf("%s asks for %s %s on %s %s, which the catalog has no builds of — pick one from the list, or run `make versions`",
			name, gdbProductLabel(product), n.GDBMajor, n.OS, n.OSVersion)})
		return out
	}
	if v := strings.TrimSpace(n.GDBVersion); v != "" && !sliceHas(minors, v) {
		out = append(out, issue{"error", fmt.Sprintf("%s asks for %s %s, which is not in the catalog for %s %s — pick one from the list, or run `make versions`",
			name, gdbProductLabel(product), v, n.OS, n.OSVersion)})
	}
	return out
}

// sliceHas is a tiny helper the validation above reads better with.
func sliceHas(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- source

// gdbSourceRootsScript lists the source trees debugsource unpacked, newest first.
//
// Two shapes turn up, and both are handled by simply telling gdb about the roots. A recent build
// records an absolute comp_dir — /usr/src/debug/percona-server-8.0.30-…/percona-server-8.0.30-22/
// — and needs nothing at all; an older one (8.0.16) records a relative
// ../../../percona-server-8.0.16-7/storage/… which gdb resolves against its source path.
const gdbSourceRootsScript = `
[ -d /usr/src/debug ] || exit 0
for d in /usr/src/debug/*/; do
  [ -d "$d" ] || continue
  printf '%s\n' "${d%/}"
  for n in "$d"*/; do [ -d "$n" ] && printf '%s\n' "${n%/}"; done
done
`

// gdbSourceRoots is the list of directories to hand gdb as its source search path.
func (a *App) gdbSourceRoots(ctx context.Context, id string) []string {
	res, err := a.engCtx(ctx).Exec(ctx, id, []string{"sh", "-c", gdbSourceRootsScript}, nil)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, ln := range strings.Split(res.Stdout, "\n") {
		if d := strings.TrimSpace(ln); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// gdbSourceMax caps one source read. The largest file in the server is a few hundred KiB; anything
// past this is not source anybody is reading a crash in.
const gdbSourceMax = 2 << 20

// gdbReadSource returns the lines of one source file around a line number, plus where the window
// starts, so the pane can number its gutter.
//
// Read off the node with sed rather than through gdb's `list`, for the same reason the Operator
// Debugger reads the operator's source off the k3s node: the file is right there, the whole file
// is more useful than ten lines of it, and gdb's list output is a format to parse rather than
// content to show.
func (a *App) gdbReadSource(ctx context.Context, id, file string, around, span int) ([]string, int, error) {
	if err := gdbSafeSourcePath(file); err != nil {
		return nil, 0, err
	}
	from := max(1, around-span)
	to := around + span
	res, err := a.engCtx(ctx).Exec(ctx, id, []string{"sh", "-c",
		fmt.Sprintf("head -c %d %s | sed -n '%d,%dp'", gdbSourceMax, shellQuote(file), from, to)}, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", file, err)
	}
	if res.Code != 0 {
		return nil, 0, fmt.Errorf("read %s: %s", file, strings.TrimSpace(res.Stderr))
	}
	lines := strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, 0, fmt.Errorf("%s is not on this node — install the debugsource package for this version", file)
	}
	return lines, from, nil
}

// gdbSafeSourcePath confines a source read to the places source legitimately lives on this node.
// The caller owns the stack and could read any of it through the file manager, so this is not the
// security boundary — it is here so a frame naming a path from the *build* machine cannot turn the
// source pane into a way to cat something unrelated.
func gdbSafeSourcePath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return fmt.Errorf("%q is not an absolute path", p)
	}
	clean := path.Clean(p)
	for _, root := range []string{"/usr/src/debug/", "/usr/include/", "/usr/src/", gdbSysrootMount + "/"} {
		if strings.HasPrefix(clean, root) {
			return nil
		}
	}
	return fmt.Errorf("%s is outside the node's source trees", clean)
}
