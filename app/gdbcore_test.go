package main

import (
	"strings"
	"testing"
)

// A bind mount is the one place in DBCanvas where a string somebody typed reaches the Docker
// daemon as a host path, and the daemon applies no confinement of its own. This is the
// confinement, so it is tested against the ways round it rather than only the happy path.
func TestGDBCleanHostPath(t *testing.T) {
	t.Setenv("GDB_MOUNT_ROOT", "/srv/coredumps")
	ok := []struct{ in, want string }{
		{"/srv/coredumps", "/srv/coredumps"},
		{"/srv/coredumps/db7/cores", "/srv/coredumps/db7/cores"},
		{"  /srv/coredumps/db7/libs  ", "/srv/coredumps/db7/libs"},
		// Cleaning happens before the check, so a path that *resolves* inside is accepted.
		{"/srv/coredumps/db7/../db8/cores", "/srv/coredumps/db8/cores"},
		{"/srv/coredumps//db7///cores/", "/srv/coredumps/db7/cores"},
	}
	for _, c := range ok {
		got, err := gdbCleanHostPath(c.in)
		if err != nil {
			t.Errorf("%q was refused: %v", c.in, err)
		} else if got != c.want {
			t.Errorf("%q cleaned to %q, want %q", c.in, got, c.want)
		}
	}

	bad := []string{
		"",
		"relative/path",
		"/etc",
		"/var/run/docker.sock",
		"/srv/coredumps/../../etc", // escapes after cleaning
		"/srv/coredumps-elsewhere", // a prefix match is not a path prefix
		"/srv/coredumpsevil",
		"/srv/coredumps/x:/etc",  // a colon would become a second destination or a mode
		"/srv/coredumps/x\nmore", // a newline in a bind spec
	}
	for _, p := range bad {
		if got, err := gdbCleanHostPath(p); err == nil {
			t.Errorf("%q was accepted as %q, want a refusal", p, got)
		}
	}
}

// The mount root is configurable, and an unset one still has to confine.
func TestGDBMountRootDefaults(t *testing.T) {
	t.Setenv("GDB_MOUNT_ROOT", "")
	if got := gdbMountRoot(); got != gdbMountRootDef {
		t.Errorf("unset mount root = %q, want %q", got, gdbMountRootDef)
	}
	if _, err := gdbCleanHostPath("/tmp/anything"); err == nil {
		t.Error("with the default root, /tmp must still be refused")
	}
	t.Setenv("GDB_MOUNT_ROOT", "/data/dumps/")
	if got := gdbMountRoot(); got != "/data/dumps" {
		t.Errorf("mount root = %q, want it cleaned to /data/dumps", got)
	}
	if _, err := gdbCleanHostPath("/data/dumps/a"); err != nil {
		t.Errorf("a path under the configured root was refused: %v", err)
	}
}

// Both mounts are read-only, always. gdb writes nothing, and a core file a session could truncate
// is a core file you only get to read once.
func TestGDBBindsAreReadOnly(t *testing.T) {
	binds := gdbBinds("/srv/coredumps/cores", "/srv/coredumps/libs")
	if len(binds) != 2 {
		t.Fatalf("binds = %v", binds)
	}
	if binds[0] != "/srv/coredumps/cores:"+gdbCoreMount+":ro" {
		t.Errorf("core bind = %q", binds[0])
	}
	if binds[1] != "/srv/coredumps/libs:"+gdbSysrootMount+":ro" {
		t.Errorf("sysroot bind = %q", binds[1])
	}
	for _, b := range binds {
		if !strings.HasSuffix(b, ":ro") {
			t.Errorf("bind %q is not read-only", b)
		}
	}
}

// The package sets are the whole feature working or silently not working, and the RHEL one is not
// guessable — see gdbPackages. These names were read off Percona's repositories.
func TestGDBPackages(t *testing.T) {
	cases := []struct {
		product, os, major string
		want               []string
	}{
		{"ps", "oraclelinux", "8.0", []string{
			"percona-server-server", "percona-server-client", "percona-server-shared",
			"percona-icu-data-files", "percona-server-server-debuginfo", "percona-server-debuginfo",
			"percona-server-debugsource"}},
		// 5.7 on EL keeps the suffixed spelling the series shipped with: asking for
		// percona-server-server there matches no package at all, which is what a 5.7 core-dump
		// node actually failed with.
		{"ps", "oraclelinux", "5.7", []string{
			"Percona-Server-server-57", "Percona-Server-client-57", "Percona-Server-shared-57",
			"Percona-Server-server-57-debuginfo", "Percona-Server-57-debuginfo",
			"Percona-Server-57-debugsource"}},
		{"pxc", "oraclelinux", "8.0", []string{
			"percona-xtradb-cluster-server", "percona-xtradb-cluster-server-debuginfo",
			"percona-xtradb-cluster-debuginfo", "percona-xtradb-cluster-debugsource"}},
		{"ps", "ubuntu", "8.0", []string{
			"percona-server-server", "percona-server-client", "percona-server-common",
			"percona-server-dbg"}},
		{"ps", "ubuntu", "5.7", []string{
			"percona-server-server-5.7", "percona-server-client-5.7", "percona-server-common-5.7",
			"percona-server-5.7-dbg"}},
		{"pxc", "debian", "8.0", []string{"percona-xtradb-cluster-server", "percona-xtradb-cluster-dbg"}},
	}
	for _, c := range cases {
		got := gdbPackages(c.product, c.os, c.major)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s %s on %s = %v, want %v", c.product, c.major, c.os, got, c.want)
		}
	}

	// Every Percona Server package must be one pin_install applies $VER to. percona-server-server
	// requires its client/shared/ICU siblings with no version, so anything left to dependency
	// resolution lands at the newest build in the repo — a node whose mysqld and libraries come
	// from different releases, which is precisely the mismatch this node type exists to rule out.
	for _, os := range []string{"oraclelinux", "ubuntu"} {
		for _, major := range []string{"8.0", "8.4", "9.7", "5.7"} {
			pkgs := gdbPackages("ps", os, major)
			for _, want := range psServerPackages(os, major) {
				if !sliceHas(pkgs, want) {
					t.Errorf("ps %s on %s does not pin %s: %v", major, os, want, pkgs)
				}
			}
		}
	}

	// The DWZ common file is the one that looks redundant and is not: the -server-debuginfo
	// package's symbols refer into it, so installing only the first leaves gdb with half a
	// symbol table and no error anywhere.
	for _, os := range []string{"oraclelinux", "rocky"} {
		pkgs := gdbPackages("ps", os, "8.0")
		if !sliceHas(pkgs, "percona-server-debuginfo") {
			t.Errorf("%s is missing the dwz common package: %v", os, pkgs)
		}
		// Symbols give a frame a file and a line; debugsource is what makes that line readable,
		// and it installs to exactly the path the DWARF records.
		if !sliceHas(pkgs, "percona-server-debugsource") {
			t.Errorf("%s is missing debugsource, so no frame can show its code: %v", os, pkgs)
		}
		// Same two, under the names 5.7 publishes them as.
		p57 := gdbPackages("ps", os, "5.7")
		if !sliceHas(p57, "Percona-Server-57-debuginfo") || !sliceHas(p57, "Percona-Server-57-debugsource") {
			t.Errorf("%s 5.7 is missing the dwz common package or debugsource: %v", os, p57)
		}
	}
	// The Debian trap next door: -server-debug is a debug *build* of the server, not symbols for
	// the release build, and must never be substituted for -dbg.
	for _, p := range append(gdbPackages("pxc", "ubuntu", "8.0"), gdbPackages("ps", "ubuntu", "8.0")...) {
		if strings.HasSuffix(p, "-server-debug") {
			t.Errorf("%q is a debug build of the server, not its symbols", p)
		}
	}
}

// The repository a series lives in, reusing the mappings the database node types already keep.
func TestGDBReleaseNames(t *testing.T) {
	cases := []struct{ product, major, wantRepo string }{
		{"ps", "8.0", "ps-80"},
		{"ps", "8.4", "ps-84-lts"},
		{"ps", "5.7", "ps-57"},
		{"pxc", "8.0", "pxc-80"},
		{"pxc", "8.4", "pxc-84-lts"},
		{"pxc", "5.7", "pxc-57"},
	}
	for _, c := range cases {
		if got := gdbReleaseRepo(c.product, c.major); got != c.wantRepo {
			t.Errorf("%s %s repo = %q, want %q", c.product, c.major, got, c.wantRepo)
		}
	}
	if got := gdbReleaseProduct("pxc", "8.0"); got != "pxc80" {
		t.Errorf("pxc 8.0 percona-release product = %q", got)
	}
	if got := gdbReleaseProduct("ps", "8.0"); got != "ps80" {
		t.Errorf("ps 8.0 percona-release product = %q", got)
	}
}

// The install script must pin the minor and must install the analysis tools. eu-unstrip is not
// decoration: it is the only way to answer "are these the right libraries?" without guessing.
func TestGDBInstallEnv(t *testing.T) {
	env := gdbInstallEnv("ps", "oraclelinux", "8.0", "8.0.16-7.1")
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["VER"] != "8.0.16-7.1" {
		t.Errorf("VER = %q — pin_install needs the catalog minor", got["VER"])
	}
	if got["REPO"] != "ps-80" || got["PRODUCT"] != "ps80" {
		t.Errorf("repo/product = %q/%q", got["REPO"], got["PRODUCT"])
	}
	if !strings.Contains(got["PKGS"], "percona-server-server-debuginfo") {
		t.Errorf("PKGS = %q", got["PKGS"])
	}
	if !strings.Contains(got["TOOLS"], "gdb") || !strings.Contains(got["TOOLS"], "elfutils") {
		t.Errorf("TOOLS = %q, want gdb and elfutils", got["TOOLS"])
	}
	// 5.7 is the environment that used to install nothing at all: the EL package names are
	// suffixed, and every one of them has to reach pin_install so $VER applies to it.
	env57 := gdbInstallEnv("ps", "oraclelinux", "5.7", "5.7.44-48.1")
	got57 := map[string]string{}
	for _, kv := range env57 {
		k, v, _ := strings.Cut(kv, "=")
		got57[k] = v
	}
	if got57["REPO"] != "ps-57" || got57["PRODUCT"] != "ps57" {
		t.Errorf("5.7 repo/product = %q/%q", got57["REPO"], got57["PRODUCT"])
	}
	if strings.Contains(got57["PKGS"], "percona-server-server ") {
		t.Errorf("5.7 PKGS asks for the 8.0 package name, which the ps-57 repository does not carry: %q", got57["PKGS"])
	}
	for _, want := range []string{"Percona-Server-server-57", "Percona-Server-client-57",
		"Percona-Server-shared-57", "Percona-Server-server-57-debuginfo"} {
		if !strings.Contains(got57["PKGS"], want) {
			t.Errorf("5.7 PKGS = %q, missing %s", got57["PKGS"], want)
		}
	}
	// The one sibling that cannot be listed as required: percona-server-shared pulls it in
	// unversioned, so it drifts to the newest build unless it joins the same transaction — and
	// it ships only in the el8 builds, so naming it outright would fail the install on el9.
	if got57["OPT"] != "Percona-Server-shared-compat-57" {
		t.Errorf("5.7 OPT = %q, want the 5.7 compat library", got57["OPT"])
	}
	if got["OPT"] != "percona-server-shared-compat" {
		t.Errorf("8.0 OPT = %q, want the compat library", got["OPT"])
	}
	// Debian has no such package, and PXC pins its own siblings exactly.
	for _, e := range append(gdbInstallEnv("ps", "ubuntu", "8.0", "8.0.43-34-1"),
		gdbInstallEnv("pxc", "oraclelinux", "8.0", "8.0.43-34.1")...) {
		if k, v, _ := strings.Cut(e, "="); k == "OPT" && v != "" {
			t.Errorf("OPT = %q, want empty", v)
		}
	}
	// Every script that reads $OPT must filter it first — an unfiltered name fails the install
	// on an EL whose repository does not carry it.
	for _, script := range []string{gdbInstallRHEL, gdbInstallDebian, mysqlInstallRHEL, mysqlInstallDebian} {
		if strings.Contains(script, "$OPT") && !strings.Contains(script, "pin_present $OPT") {
			t.Error("an install script uses $OPT without pin_present")
		}
	}
	// The scripts have to consume every one of those.
	for _, script := range []string{gdbInstallRHEL, gdbInstallDebian} {
		for _, v := range []string{"$TOOLS", "$PKGS"} {
			if !strings.Contains(script, v) {
				t.Errorf("an install script never uses %s", v)
			}
		}
		if !strings.Contains(script, "pin_install") {
			t.Error("an install script does not pin the version")
		}
	}
}

// The binary probe must look in the mounted directory first. The recipe copies mysqld off the
// crashed host next to its libraries, and that copy *is* the binary that produced the core — no
// version guess can be wrong about it, where the installed package's can.
func TestGDBBinaryProbePrefersTheMountedCopy(t *testing.T) {
	mounted := strings.Index(gdbBinaryProbeScript, gdbSysrootMount+"/mysqld")
	installed := strings.Index(gdbBinaryProbeScript, gdbServerBinary)
	if mounted < 0 {
		t.Fatal("the probe never looks in the mounted library directory")
	}
	if installed < 0 || mounted > installed {
		t.Error("the probe checks the installed package before the mounted copy")
	}
	for _, want := range []string{"source=", "buildid=", "debuglink=", "syms=", "sameas="} {
		if !strings.Contains(gdbBinaryProbeScript, want) {
			t.Errorf("the probe never reports %s", want)
		}
	}
	// Percona's mysqld has no build-id note, so the probe must check the debuglink rules too —
	// all three of them, because gdb tries all three and "symbols" has to mean gdb will find them.
	for _, rule := range []string{`"$DIR/$LINK"`, `"$DIR/.debug/$LINK"`, `"/usr/lib/debug$DIR/$LINK"`} {
		if !strings.Contains(gdbBinaryProbeScript, rule) {
			t.Errorf("the probe never tries the debuglink rule %s", rule)
		}
	}
	// And with no build-id there is nothing to compare builds by except the bytes.
	if !strings.Contains(gdbBinaryProbeScript, "cmp -s") {
		t.Error("the probe never compares the mounted copy with the installed build")
	}
}

// A binary read from the mount needs the package's debug files linked where gdb looks for *it* —
// /usr/lib/debug/<its own directory>/ — or the debuglink resolves to nothing and every frame loses
// its arguments and line numbers.
func TestGDBLinkDebugScript(t *testing.T) {
	for _, want := range []string{`mkdir -p "/usr/lib/debug$DIR"`, "/usr/lib/debug/usr/sbin/*.debug", "ln -sf"} {
		if !strings.Contains(gdbLinkDebugScript, want) {
			t.Errorf("the link step is missing %q", want)
		}
	}
	// A binary that is already where the package put it needs no linking, and linking a directory
	// onto itself would be a loop.
	if !strings.Contains(gdbLinkDebugScript, "/usr/sbin|/usr/bin) exit 0") {
		t.Error("the link step does not skip a binary that is already in the package's own directory")
	}
}

// gdb is a programmable debugger: `shell` is a root shell on the node. None of these is part of
// reading a core, so they are refused until somebody explicitly says otherwise.
func TestGDBUnsafeCommand(t *testing.T) {
	refused := []string{
		"shell rm -rf /", "SHELL id", "!id", "pipe bt | sh", "python import os",
		"pi", "guile (system \"id\")", "source /sysroot/evil.gdb", "define hook-stop",
		"file /etc/shadow", "core-file /etc/passwd", "add-symbol-file /tmp/x",
		"gcore /tmp/out", "dump binary memory /tmp/x 0 1",
		"set startup-with-shell on", "set auto-load on",
	}
	for _, c := range refused {
		if why := gdbUnsafeCommand(c); why == "" {
			t.Errorf("%q was allowed", c)
		}
	}
	// Everything that is actually reading a stack has to stay allowed, including the `set`s a
	// person legitimately reaches for.
	allowed := []string{
		"", "bt", "thread apply all bt", "info sharedlibrary", "info threads",
		"info registers", "frame 4", "p *node", "x/16xb $rsp", "list", "disassemble",
		"set print elements 0", "set backtrace limit 1000", "set listsize 20",
	}
	for _, c := range allowed {
		if why := gdbUnsafeCommand(c); why != "" {
			t.Errorf("%q was refused: %s", c, why)
		}
	}
}

// The core to open is a plain file name in the mounted directory — not a path.
func TestGDBSafeCoreName(t *testing.T) {
	if err := gdbSafeCoreName("core.mysqld.9712.1787625764"); err != nil {
		t.Errorf("a plain name was refused: %v", err)
	}
	for _, bad := range []string{"", "  ", "..", ".", "../etc/shadow", "/etc/shadow", "sub/dir/core"} {
		if err := gdbSafeCoreName(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// Collapsing a repeating cycle is what makes a stack-exhaustion core readable at all: the crash
// this was built for is hundreds of identical frames, with the interesting top and bottom pushed
// off screen by the middle.
func TestGDBCollapseRecursion(t *testing.T) {
	f := func(name string, level int) miFrame { return miFrame{Level: level, Func: name} }

	// Direct recursion: one function, many times.
	var frames []miFrame
	frames = append(frames, f("_int_malloc", 0), f("calloc", 1))
	for i := 0; i < 120; i++ {
		frames = append(frames, f("fts_ast_visit", 2+i))
	}
	frames = append(frames, f("main", 200))
	got := gdbCollapse(frames)
	if len(got) != 4 {
		t.Fatalf("collapsed to %d rows, want 4: %+v", len(got), got)
	}
	if got[2].Func != "fts_ast_visit" || got[2].Repeat != 120 {
		t.Errorf("the recursive row = %+v, want fts_ast_visit ×120", got[2])
	}
	if got[0].Repeat != 0 || got[3].Func != "main" {
		t.Errorf("non-repeating frames were disturbed: %+v", got)
	}

	// Mutual recursion: a two-frame cycle, which is the shape the real core has.
	frames = nil
	for i := 0; i < 60; i++ {
		frames = append(frames, f("fts_query_visitor", i*2), f("fts_ast_visit", i*2+1))
	}
	got = gdbCollapse(frames)
	if len(got) != 2 {
		t.Fatalf("a two-frame cycle collapsed to %d rows, want 2: %+v", len(got), got)
	}
	if got[0].Repeat != 60 {
		t.Errorf("cycle repeat = %d, want 60", got[0].Repeat)
	}

	// A stack with no cycle must come back untouched — collapsing a normal backtrace would hide
	// frames rather than reveal them.
	frames = []miFrame{f("a", 0), f("b", 1), f("a", 2), f("c", 3)}
	got = gdbCollapse(frames)
	if len(got) != 4 {
		t.Errorf("a non-repeating stack was collapsed: %+v", got)
	}
	for _, fr := range got {
		if fr.Repeat != 0 {
			t.Errorf("frame %s claims a repeat", fr.Func)
		}
	}
	// The real core's cycle is five frames long, and a ceiling of four found the inner
	// three-frame run instead — reporting "fts_ast_visit repeats 3 times", which is true, useless,
	// and leaves the outer cycle spread across the whole pane.
	frames = nil
	for i := 0; i < 40; i++ {
		frames = append(frames,
			f("fts_query_visitor", i*5), f("fts_ast_visit", i*5+1), f("fts_ast_visit", i*5+2),
			f("fts_ast_visit", i*5+3), f("fts_ast_visit_sub_exp", i*5+4))
	}
	got = gdbCollapse(frames)
	if len(got) != 5 {
		t.Fatalf("a five-frame cycle collapsed to %d rows, want 5: %+v", len(got), got)
	}
	if got[0].Func != "fts_query_visitor" || got[0].Repeat != 40 {
		t.Errorf("the cycle row = %+v, want fts_query_visitor ×40", got[0])
	}

	// Two copies is a coincidence, not a cycle: it is shorter to show both than to say "×2".
	frames = []miFrame{f("a", 0), f("a", 1), f("b", 2)}
	if got = gdbCollapse(frames); len(got) != 3 {
		t.Errorf("two copies were collapsed: %+v", got)
	}
	// Frames with no name must never be treated as a cycle — a stack full of ?? is the
	// no-symbols case, and folding it would claim a structure nobody can see.
	frames = []miFrame{{Level: 0}, {Level: 1}, {Level: 2}, {Level: 3}}
	if got = gdbCollapse(frames); len(got) != 4 {
		t.Errorf("nameless frames were collapsed: %+v", got)
	}
}

// The summary picks the frame that names the bug, and the top frame is not it: a stack overflow
// surfaces inside the allocator, an assertion inside abort.
func TestGDBCrashSummary(t *testing.T) {
	frames := []miFrame{
		{Level: 0, Func: "_int_malloc", From: "/lib64/libc.so.6"},
		{Level: 1, Func: "calloc", From: "/lib64/libc.so.6"},
		{Level: 2, Func: "ut_allocator<unsigned char>::allocate(unsigned long)"},
		{Level: 3, Func: "rbt_create(unsigned long)"},
		{Level: 4, Func: "fts_query_visitor(fts_ast_oper_t)", Repeat: 140},
	}
	culprit, recursion := gdbCrashSummary(frames)
	if culprit == nil || !strings.HasPrefix(culprit.Func, "ut_allocator") {
		t.Fatalf("culprit = %+v, want the first frame below libc", culprit)
	}
	if !strings.Contains(recursion, "fts_query_visitor") || !strings.Contains(recursion, "140") {
		t.Errorf("recursion = %q", recursion)
	}
	if shortFuncName("rbt_create(unsigned long)") != "rbt_create" {
		t.Errorf("shortFuncName kept the signature")
	}
	// PS-9668's real core: a non-type template parameter of enum type prints as
	// `<(some::Enum)0>`, which has a `(` inside the template brackets, before the real argument
	// list. The naive first-`(` scan cut the name off mid-template.
	if got := shortFuncName(
		"audit_log_filter::log_record_formatter::LogRecordFormatter<(audit_log_filter::log_record_formatter::AuditLogFormatType)0>::apply[abi:cxx11](audit_log_filter::AuditRecordQuery const&) const"); got != "audit_log_filter::log_record_formatter::LogRecordFormatter<(audit_log_filter::log_record_formatter::AuditLogFormatType)0>::apply[abi:cxx11]" {
		t.Errorf("shortFuncName truncated mid-template: %q", got)
	}
	// PXC-3848's real core: rewrite_query is declared in an anonymous namespace, so its printed
	// name starts with "(anonymous namespace)::" — a `(` at position zero, before the depth
	// tracking has scanned anything, which the naive scan mistook for an empty argument list and
	// returned "" for the whole name.
	if got := shortFuncName("(anonymous namespace)::rewrite_query(THD*, Consumer_type, my_h_string*, Rewritten_query_buffer&)"); got != "(anonymous namespace)::rewrite_query" {
		t.Errorf("shortFuncName mishandled an anonymous-namespace name: %q", got)
	}
	// Both spellings of a C-runtime library have to be recognised. A flat copy taken off the
	// crashed host holds the real file names, not the sonames, and reading the real core is what
	// found this: `_int_malloc` came back attributed to `libc-2.28.so` and was reported as the
	// program's own code — exactly backwards.
	for _, sys := range []string{
		"/lib64/libc.so.6", "/lib64/libc-2.28.so", "/sysroot/libc-2.28.so",
		"/lib64/libpthread.so.0", "/sysroot/libpthread-2.28.so",
		"/lib64/ld-linux-x86-64.so.2", "/sysroot/ld-2.28.so",
		"/lib64/libstdc++.so.6", "/lib64/libgcc_s.so.1",
	} {
		if !gdbSystemObject(sys) {
			t.Errorf("%s is the C runtime and was not recognised", sys)
		}
	}
	for _, own := range []string{
		"/usr/lib64/libjemalloc.so.2", "/sysroot/libcrypto.so.1.1", "/sysroot/libssl.so.1.1",
		"/sysroot/libnuma.so.1", "/sysroot/libaio.so.1", "",
	} {
		if gdbSystemObject(own) {
			t.Errorf("%s is not the C runtime and was treated as it", own)
		}
	}
	// A stack that is entirely libc still has to name something rather than nothing.
	if c, _ := gdbCrashSummary(frames[:2]); c == nil {
		t.Error("an all-libc stack produced no culprit at all")
	}
}
