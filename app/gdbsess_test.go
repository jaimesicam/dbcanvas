package main

import "testing"

// These are the exact console lines gdb emits while loading the real core this feature was built
// against — captured from the node, not invented. Getting the attribution wrong here is not a
// cosmetic bug: the page told the user there were no symbols for the executable while showing
// file, line and arguments that could only have come from them.
func TestGDBReadingAttribution(t *testing.T) {
	lines := []struct {
		in   string
		want string // "" when the line is not a "Reading symbols from"
	}{
		{"Reading symbols from /sysroot/mysqld...", "/sysroot/mysqld"},
		{"Reading symbols from /usr/lib/debug//sysroot/mysqld-8.0.16-7.1.el8.x86_64.debug...",
			"/usr/lib/debug//sysroot/mysqld-8.0.16-7.1.el8.x86_64.debug"},
		// Every one of these has a dot in it, which is what the first version choked on.
		{"Reading symbols from /usr/lib64/libpthread-2.28.so...", "/usr/lib64/libpthread-2.28.so"},
		{"Reading symbols from /usr/lib64/libaio.so.1.0.1...", "/usr/lib64/libaio.so.1.0.1"},
		{"Reading symbols from .gnu_debugdata for /usr/lib64/libnuma.so.1.0.0...",
			".gnu_debugdata for /usr/lib64/libnuma.so.1.0.0"},
		{"(no debugging symbols found)...done.", ""},
		{"Program terminated with signal SIGSEGV, Segmentation fault.", ""},
	}
	for _, c := range lines {
		m := gdbReadingRe.FindStringSubmatch(c.in)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}

// The signal is only ever said once, on the console stream, while the core is loading. `info
// program` cannot be asked afterwards: on a core file it answers "The program being debugged is
// not being run", which is what the first version of this shipped as the crash summary.
func TestGDBTerminatedLine(t *testing.T) {
	m := gdbTerminatedRe.FindStringSubmatch("Program terminated with signal SIGSEGV, Segmentation fault.")
	if m == nil {
		t.Fatal("the terminated line did not match")
	}
	if m[1] != "SIGSEGV" || m[2] != "Segmentation fault." {
		t.Errorf("signal = %q, meaning = %q", m[1], m[2])
	}
	if gdbTerminatedRe.MatchString("The program being debugged is not being run.") {
		t.Error("`info program`'s reply must not be read as a signal")
	}
}

// A whole session's console stream, replayed: only the executable's own verdict may set the
// symbol warning.
func TestGDBSymbolVerdictIsAttributed(t *testing.T) {
	sess := &gdbSession{tgt: gdbTarget{Binary: "/sysroot/mysqld"}, subs: map[int]chan []byte{}}
	for _, ln := range []string{
		"Reading symbols from /sysroot/mysqld...",
		"Reading symbols from /usr/lib/debug//sysroot/mysqld-8.0.16-7.1.el8.x86_64.debug...",
		"Program terminated with signal SIGSEGV, Segmentation fault.",
		"Reading symbols from /usr/lib64/libpthread-2.28.so...",
		"(no debugging symbols found)...done.",
		"Reading symbols from /usr/lib64/libaio.so.1.0.1...",
		"(no debugging symbols found)...done.",
	} {
		sess.onStream(miConsoleOut, ln)
	}
	if sess.symbols != "" {
		t.Errorf("symbols verdict = %q — those warnings were about the libraries", sess.symbols)
	}
	if sess.signal != "SIGSEGV" || sess.sigText != "Segmentation fault" {
		t.Errorf("signal = %q / %q", sess.signal, sess.sigText)
	}

	// And when it really is the executable, it has to be said.
	sess2 := &gdbSession{tgt: gdbTarget{Binary: "/usr/sbin/mysqld"}, subs: map[int]chan []byte{}}
	sess2.onStream(miConsoleOut, "Reading symbols from /usr/sbin/mysqld...")
	sess2.onStream(miConsoleOut, "(no debugging symbols found)...done.")
	if sess2.symbols == "" {
		t.Error("the executable having no symbols was not reported")
	}
}
