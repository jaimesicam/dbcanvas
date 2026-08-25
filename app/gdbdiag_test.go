package main

import (
	"strconv"
	"strings"
	"testing"
)

// A stack shaped like the real one: a few C-library frames on top, ~1,060 frames of a five-frame
// cycle, then the SQL layer that started it. The numbers come from the core this was built against.
func realisticRecursionStack() []miFrame {
	f := func(name, file string, line, level int) miFrame {
		return miFrame{Level: level, Func: name, File: file, Line: line}
	}
	lib := func(name string, level int) miFrame {
		return miFrame{Level: level, Func: name, From: "/lib64/libc-2.28.so"}
	}
	frames := []miFrame{lib("_int_malloc", 0), lib("calloc", 1),
		f("ut_allocator<unsigned char>::allocate(unsigned long)", "ut0new.h", 655, 2),
		f("rbt_create(unsigned long)", "ut0new.h", 587, 3)}
	lvl := 4
	for i := 0; i < 212; i++ {
		frames = append(frames,
			f("fts_ast_visit_sub_exp(fts_ast_node_t*)", "fts0que.cc", 2857, lvl),
			f("fts_query_visitor(fts_ast_oper_t, fts_ast_node_t*, void*)", "fts0que.cc", 2815, lvl+1),
			f("fts_ast_visit(fts_ast_oper_t, fts_ast_node_t*)", "fts0ast.cc", 611, lvl+2),
			f("fts_ast_visit(fts_ast_oper_t, fts_ast_node_t*)", "fts0ast.cc", 630, lvl+3),
			f("fts_ast_visit(fts_ast_oper_t, fts_ast_node_t*)", "fts0ast.cc", 568, lvl+4))
		lvl += 5
	}
	for _, name := range []string{
		"fts_query(trx_t*, dict_index_t*, uint, char const*)",
		"ha_innobase::ft_init_ext(uint, uint, Item*)",
		"Item_func_match::init_search(THD*)",
		"JOIN::optimize()", "mysql_execute_command(THD*, bool)", "do_command(THD*)",
	} {
		frames = append(frames, f(name, "sql_parse.cc", 100, lvl))
		lvl++
	}
	return frames
}

// The whole point of the analysis: the top frame is not the bug, the window's repeat count is not
// the real one, and the crash has a *class*.
func TestDiagnoseStackExhaustion(t *testing.T) {
	frames := realisticRecursionStack()
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	v.Repeats = reps
	for _, f := range frames[start : start+period] {
		v.Cycle = append(v.Cycle, shortFuncName(f.Func))
	}
	gdbClassify(v, frames, start, period, reps, "SIGSEGV", "Segmentation fault")

	if v.Class != "stack-exhaustion" {
		t.Fatalf("class = %q, want stack-exhaustion — headline was %q", v.Class, v.Headline)
	}
	if period != 5 {
		t.Errorf("cycle period = %d, want 5", period)
	}
	if reps != 212 {
		t.Errorf("repeats = %d, want 212 — counting inside a window is what made this 39", reps)
	}
	// The culprit must be a frame in the recursion, never the allocator that faulted.
	if v.Culprit == nil || !strings.HasPrefix(shortFuncName(v.Culprit.Func), "fts_") {
		t.Errorf("culprit = %v, want a frame from the recursion", v.Culprit)
	}
	for _, wrong := range []string{"_int_malloc", "calloc", "ut_allocator<unsigned char>::allocate"} {
		if v.Culprit != nil && shortFuncName(v.Culprit.Func) == wrong {
			t.Errorf("culprit is %s — that is what touched the guard page, not the bug", wrong)
		}
	}
	// The evidence has to carry the numbers the conclusion rests on.
	joined := strings.Join(v.Evidence, " | ")
	for _, want := range []string{strconv.Itoa(len(frames)), "1060", "SIGSEGV", "_int_malloc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("evidence does not mention %s: %s", want, joined)
		}
	}
	if !strings.Contains(v.Why, "guard page") {
		t.Errorf("the explanation does not say why the signal landed in malloc: %s", v.Why)
	}
}

// abort() on the stack means the server chose to die, and that is a completely different
// conversation from a memory fault — even though the signal is often SIGABRT and the top frames
// look just as alarming.
func TestDiagnoseAssertion(t *testing.T) {
	frames := []miFrame{
		{Level: 0, Func: "__pthread_kill_implementation", From: "/lib64/libpthread-2.28.so"},
		{Level: 1, Func: "raise", From: "/lib64/libc-2.28.so"},
		{Level: 2, Func: "abort", From: "/lib64/libc-2.28.so"},
		{Level: 3, Func: "ut_dbg_assertion_failed(char const*, char const*, ulint)", File: "ut0dbg.cc", Line: 67},
		{Level: 4, Func: "buf_page_get_gen(page_id_t const&)", File: "buf0buf.cc", Line: 4128},
		{Level: 5, Func: "row_search_mvcc(uchar*, page_cur_mode_t)", File: "row0sel.cc", Line: 5121},
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGABRT", "Aborted")
	if v.Class != "assertion" {
		t.Fatalf("class = %q, want assertion", v.Class)
	}
	if !strings.Contains(v.Headline, "InnoDB assertion") {
		t.Errorf("headline = %q", v.Headline)
	}
	// The frame that matters is the check, not raise/abort above it.
	if v.Culprit == nil || !strings.HasPrefix(v.Culprit.Func, "buf_page_get_gen") {
		t.Errorf("culprit = %v, want the frame below the abort machinery", v.Culprit)
	}
}

// A short stack with no repetition is a bad pointer, and must not be reported as a stack overflow
// just because the signal is the same.
func TestDiagnoseBadPointer(t *testing.T) {
	frames := []miFrame{
		{Level: 0, Func: "Item_field::val_str(String*)", File: "item.cc", Line: 2201},
		{Level: 1, Func: "JOIN::exec()", File: "sql_select.cc", Line: 1200},
		{Level: 2, Func: "do_command(THD*)", File: "sql_parse.cc", Line: 1293},
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGSEGV", "Segmentation fault")
	if v.Class != "bad-pointer" {
		t.Fatalf("class = %q, want bad-pointer", v.Class)
	}
	if !strings.Contains(v.Why, "rather than a stack overflow") {
		t.Errorf("the explanation does not rule out a stack overflow: %s", v.Why)
	}
	if v.Culprit == nil || !strings.HasPrefix(v.Culprit.Func, "Item_field::val_str") {
		t.Errorf("culprit = %v", v.Culprit)
	}
}

// The cycle finder has to report the *whole* run, and prefer the real unit over a fragment of it.
func TestGDBFindCycle(t *testing.T) {
	frames := realisticRecursionStack()
	start, period, reps := gdbFindCycle(frames)
	if period != 5 || reps != 212 {
		t.Fatalf("cycle = %d frames × %d, want 5 × 212 (start %d)", period, reps, start)
	}
	if start != 4 {
		t.Errorf("cycle starts at %d, want 4 — the four frames above it are the fault, not the loop", start)
	}
	// No cycle at all must be reported as no cycle, not as a one-frame cycle repeating forever.
	plain := []miFrame{{Func: "a"}, {Func: "b"}, {Func: "c"}, {Func: "d"}}
	if _, _, r := gdbFindCycle(plain); r >= 3 {
		t.Errorf("a plain stack reported a cycle repeating %d times", r)
	}
}

// The triggering input is a string argument, and the recursion's own frames carry pointers that
// must not be mistaken for one.
func TestGDBLooksLikeText(t *testing.T) {
	yes := []string{
		`0x76026c02d950 "+(+(+(+(+(+(+(+(+(+(+(+("...`,
		`0x7f00 "SELECT 1 FROM test_table"`,
	}
	for _, v := range yes {
		if !gdbLooksLikeText(v) {
			t.Errorf("%q was not recognised as text", v)
		}
	}
	no := []string{
		"0x76026cb72a18",                     // a plain pointer — every recursion frame has these
		"FTS_EXIST",                          // an enum
		"2748",                               // a length
		`0x22f9ad0 <fts_query_visitor(int)>`, // a function pointer
		`0x7f00 ""`,                          // an empty string
	}
	for _, v := range no {
		if gdbLooksLikeText(v) {
			t.Errorf("%q was mistaken for the triggering input", v)
		}
	}
	if got := gdbCleanText(`0x76026c02d950 "+(+(+(+("...`); got != "+(+(+(+(" {
		t.Errorf("gdbCleanText = %q", got)
	}
}

// The frames below are real, from the two PS 8.0.30 cores this analysis was rebuilt against. Both
// are the *normal* shape of a MySQL crash and both defeated the first version of the diagnosis:
// the server catches its own SIGSEGV, so the top of the stack is always the crash handler, and a
// tool that reports the top frame reports `my_write_core` every single time.
func handlerToppedStack(fault []miFrame) []miFrame {
	lib := func(n string) miFrame { return miFrame{Func: n, From: "/usr/lib64/libc.so.6"} }
	frames := []miFrame{
		lib("__pthread_kill_implementation"),
		{Func: "my_write_core", File: "mysys/stacktrace.cc", Line: 396, Args: []miVar{{Name: "sig", Value: "11"}}},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 228},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 200},
		{Func: "<signal handler called>"},
	}
	frames = append(frames, fault...)
	for i := range frames {
		frames[i].Level = i
	}
	return frames
}

// linuxclient2: temptable::Table::number_of_rows called on a null `this`. The root cause is in the
// argument list, which a pane of function names never shows.
func TestDiagnoseNullThis(t *testing.T) {
	frames := handlerToppedStack([]miFrame{
		{Func: "temptable::Storage::size", File: "storage/temptable/include/temptable/storage.h", Line: 505,
			Args: []miVar{{Name: "this", Value: "0x30"}}},
		{Func: "temptable::Table::number_of_rows", File: "storage/temptable/include/temptable/table.h", Line: 190,
			Args: []miVar{{Name: "this", Value: "0x0"}}},
		{Func: "temptable::Handler::info", File: "storage/temptable/src/handler.cc", Line: 742,
			Args: []miVar{{Name: "this", Value: "0x79a2b80ff1d0"}}},
		{Func: "temptable::Handler::open", File: "storage/temptable/src/handler.cc", Line: 207},
		{Func: "handler::ha_open", File: "sql/handler.cc", Line: 2900},
	})
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGSEGV", "Segmentation fault")

	if v.Class != "null-deref" {
		t.Fatalf("class = %q, want null-deref — headline was %q", v.Class, v.Headline)
	}
	// Never the crash handler, and never libc.
	for _, wrong := range []string{"my_write_core", "handle_fatal_signal", "__pthread_kill_implementation"} {
		if v.Culprit != nil && shortFuncName(v.Culprit.Func) == wrong {
			t.Fatalf("culprit is %s — that is the crash handler, which runs after the crash", wrong)
		}
	}
	if v.Culprit == nil || !strings.Contains(v.Culprit.Func, "temptable::Storage::size") {
		t.Errorf("culprit = %v, want the frame below <signal handler called>", v.Culprit)
	}
	if !strings.Contains(v.Headline, "this = 0x30") {
		t.Errorf("headline does not name the null argument: %q", v.Headline)
	}
	// A near-null is a null plus a field offset, and saying so is the difference between "some
	// pointer was wrong" and "an object was never created".
	if !strings.Contains(v.Why, "field offset") {
		t.Errorf("why does not explain the non-zero value: %q", v.Why)
	}
	if !strings.Contains(v.Why, "temptable::Table::number_of_rows") {
		t.Errorf("why does not point at the caller: %q", v.Why)
	}
	if !v.Handler {
		t.Error("the crash handler was on the stack and was not noticed")
	}
}

// linuxclient1: a copy that ran off the end, inside __memmove. The fault lands in libc and the bug
// is the length its caller computed — reporting memcpy would be reporting the messenger.
func TestDiagnoseCopyOverrun(t *testing.T) {
	frames := handlerToppedStack([]miFrame{
		{Func: "__memmove_avx_unaligned_erms", From: "/usr/lib64/libc.so.6"},
		{Func: "memcpy", File: "/usr/include/bits/string_fortified.h", Line: 29},
		{Func: "String::append", File: "sql-common/sql_string.cc", Line: 451},
		{Func: "dump_leaf_key", File: "sql/item_sum.cc", Line: 4115},
		{Func: "Item_func_group_concat::add", File: "sql/item_sum.cc", Line: 4380},
	})
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGSEGV", "Segmentation fault")

	if v.Class != "bad-pointer" {
		t.Fatalf("class = %q, want bad-pointer", v.Class)
	}
	// The culprit must be the first frame that is the server's own code, not memcpy and not the
	// handler above it.
	if v.Culprit == nil || !strings.HasPrefix(v.Culprit.Func, "String::append") {
		t.Fatalf("culprit = %v, want String::append — memcpy and the handler are both wrong answers",
			v.Culprit)
	}
	if !strings.Contains(v.Why, "length") {
		t.Errorf("why does not raise the length, which is the usual cause: %q", v.Why)
	}
	if !strings.Contains(v.Why, "dump_leaf_key") {
		t.Errorf("why does not point one frame down at %q: %q", "dump_leaf_key", v.Why)
	}
	if !strings.Contains(v.Headline, "sql_string.cc:451") {
		t.Errorf("headline does not carry file:line: %q", v.Headline)
	}
}

// The boundary itself, in isolation: gdb's marker is the most reliable landmark in a crash stack.
func TestGDBFaultFrame(t *testing.T) {
	frames := handlerToppedStack([]miFrame{
		{Func: "temptable::Storage::size", File: "storage.h", Line: 505},
		{Func: "temptable::Handler::info", File: "handler.cc", Line: 742},
	})
	i := gdbFaultFrame(frames)
	if i < 0 || frames[i].Func != "temptable::Storage::size" {
		t.Fatalf("fault frame = %d (%v), want the frame below <signal handler called>", i, frames[i].Func)
	}
	// With no marker at all, fall back to the first frame that is neither machinery nor libc.
	plain := []miFrame{
		{Level: 0, Func: "abort", From: "/lib64/libc.so.6"},
		{Level: 1, Func: "ut_dbg_assertion_failed", File: "ut0dbg.cc", Line: 67},
		{Level: 2, Func: "buf_page_get_gen", File: "buf0buf.cc", Line: 4128},
	}
	if i := gdbFaultFrame(plain); i < 0 || plain[i].Func != "buf_page_get_gen" {
		t.Errorf("without a marker the fault frame = %d, want buf_page_get_gen", i)
	}
}

func TestGDBNullish(t *testing.T) {
	for _, v := range []string{"0x0", "0x30", "0x8", "0xfff"} {
		if _, ok := gdbNullish(v); !ok {
			t.Errorf("%s is a null or a field offset from one, and was not recognised", v)
		}
	}
	for _, v := range []string{"0x79a2b80ff1d0", "0x1000", `0x7f00 "text"`, "0x22f9ad0 <fn(int)>", "11", ""} {
		if _, ok := gdbNullish(v); ok {
			t.Errorf("%q was treated as a null pointer", v)
		}
	}
}

// A source path from the crashed build must not become a way to read anything else on the node.
func TestGDBSafeSourcePath(t *testing.T) {
	for _, ok := range []string{
		"/usr/src/debug/percona-server-8.0.30-22.1.el9.x86_64/percona-server-8.0.30-22/sql/item_sum.cc",
		"/usr/include/bits/string_fortified.h",
		"/sysroot/mysqld",
	} {
		if err := gdbSafeSourcePath(ok); err != nil {
			t.Errorf("%s was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"/etc/shadow", "relative/path", "", "/root/.ssh/id_rsa",
		"/usr/src/debug/../../etc/shadow"} {
		if err := gdbSafeSourcePath(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// The frames below are real, from PS-8797 (install audit_log after innodb_force_recovery=1 →
// free(): invalid pointer) and PS-10332 (invalid audit-log-filter.compression → double free). Both
// hit the exact same glibc internal chain — abort → __libc_message → malloc_printerr → _int_free —
// and both had `abort` on the stack, which is what the first version of the assertion case matched
// on: it would have reported "an assertion failed" for a bug where no assertion exists anywhere in
// the code, sending the reader looking for a check that was never written.
func heapCorruptionStack(fault []miFrame) []miFrame {
	lib := func(n string) miFrame { return miFrame{Func: n, From: "/lib64/libc.so.6"} }
	frames := []miFrame{
		lib("__pthread_kill_implementation"),
		{Func: "my_write_core", File: "mysys/stacktrace.cc", Line: 396},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 228},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 200},
		{Func: "<signal handler called>"},
		lib("__GI_raise"), lib("__GI_abort"),
		lib("__libc_message"), lib("malloc_printerr"), lib("_int_free"),
	}
	frames = append(frames, fault...)
	for i := range frames {
		frames[i].Level = i
	}
	return frames
}

func TestDiagnoseHeapCorruption_PluginDeinit(t *testing.T) {
	frames := heapCorruptionStack([]miFrame{
		{Func: "finalize_audit_plugin", File: "sql/sql_plugin.cc", Line: 1310},
		{Func: "plugin_deinitialize", File: "sql/sql_plugin.cc", Line: 782},
		{Func: "mysql_install_plugin", File: "sql/sql_plugin.cc", Line: 2270},
	})
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGABRT", "Aborted")

	if v.Class != "heap-corruption" {
		t.Fatalf("class = %q, want heap-corruption — headline was %q", v.Class, v.Headline)
	}
	if strings.Contains(v.Headline, "assertion") || strings.Contains(v.Why, "the code detected a state") {
		t.Errorf("a glibc heap check was described as the server's own assertion: %q / %q", v.Headline, v.Why)
	}
	if v.Culprit == nil || v.Culprit.Func != "finalize_audit_plugin" {
		t.Fatalf("culprit = %v, want finalize_audit_plugin — never malloc_printerr or _int_free", v.Culprit)
	}
	if !strings.Contains(v.Why, "double free") {
		t.Errorf("why does not name the usual causes: %q", v.Why)
	}
}

func TestDiagnoseHeapCorruption_ComponentDeinit(t *testing.T) {
	frames := heapCorruptionStack([]miFrame{
		{Func: "Mysql_thd_store_service_imp::unregister_slot",
			File: "sql/server_component/mysql_thd_store_imp.cc", Line: 147},
		{Func: "audit_log_filter::SysVars::deinit", File: "include/mysql/components/my_service.h", Line: 98},
	})
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGABRT", "Aborted")
	if v.Class != "heap-corruption" {
		t.Fatalf("class = %q, want heap-corruption", v.Class)
	}
	if v.Culprit == nil || !strings.HasPrefix(v.Culprit.Func, "Mysql_thd_store_service_imp") {
		t.Fatalf("culprit = %v", v.Culprit)
	}
}

// PS-8877: an InnoDB assertion whose condition is readable straight off ut_dbg_assertion_failed's
// own arguments — not guessed from the caller, and not the generic "an assertion failed" the first
// version of this shipped.
func TestDiagnoseAssertionReadsTheCondition(t *testing.T) {
	frames := []miFrame{
		{Func: "__pthread_kill_implementation", From: "/lib64/libc.so.6"},
		{Func: "my_write_core", File: "mysys/stacktrace.cc", Line: 396},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 228},
		{Func: "my_server_abort"},
		{Func: "my_abort"},
		{Func: "ut_dbg_assertion_failed", File: "ut0dbg.cc", Line: 67, Args: []miVar{
			{Name: "expr", Value: `0x1a2b3c "m_position == m_rows_n + 1"`},
			{Name: "file", Value: `0x1a2b40 "log0pfs.cc"`},
			{Name: "line", Value: "263"},
		}},
		{Func: "ha_perfschema::rnd_next", File: "storage/perfschema/ha_perfschema.cc", Line: 249},
		{Func: "handler::ha_rnd_next", File: "sql/handler.cc", Line: 3050},
	}
	for i := range frames {
		frames[i].Level = i
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGABRT", "Aborted")

	if v.Class != "assertion" {
		t.Fatalf("class = %q, want assertion", v.Class)
	}
	if !strings.Contains(v.Headline, "m_position == m_rows_n + 1") {
		t.Fatalf("headline does not carry the actual failed check: %q", v.Headline)
	}
	if !strings.Contains(v.Headline, "log0pfs.cc:263") {
		t.Errorf("headline is missing the file:line of the check itself (not the caller's): %q", v.Headline)
	}
	// The culprit is still the caller — ha_perfschema::rnd_next, the code whose expectation broke —
	// not the assertion helper, and not the InnoDB frame it's declared in (ut0dbg.cc is the macro's
	// own file, not where the bug is).
	if v.Culprit == nil || v.Culprit.Func != "ha_perfschema::rnd_next" {
		t.Fatalf("culprit = %v, want ha_perfschema::rnd_next", v.Culprit)
	}
}

// Without a readable condition (older build, stripped strings, a plain abort()), the assertion
// case must still say *something* useful rather than an empty headline.
func TestDiagnoseAssertionWithoutReadableCondition(t *testing.T) {
	frames := []miFrame{
		{Func: "raise", From: "/lib64/libc.so.6"},
		{Func: "abort", From: "/lib64/libc.so.6"},
		{Func: "some_check_that_failed", File: "foo.cc", Line: 10},
	}
	for i := range frames {
		frames[i].Level = i
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGABRT", "Aborted")
	if v.Class != "assertion" || v.Headline == "" {
		t.Fatalf("class=%q headline=%q", v.Class, v.Headline)
	}
	if !strings.Contains(v.Headline, "assertion") {
		t.Errorf("fell back to the generic headline incorrectly: %q", v.Headline)
	}
}

// PS-7958: MATCH ... AGAINST on an ngram fulltext index with a special character hits
// `ut_a(arg3)` at eval0eval.cc:130. Real gdb output for a signal-delivered abort always inserts a
// `<signal handler called>` frame between handle_fatal_signal and the libc raise/abort below it —
// this one is captured off the real core rather than hand-built, specifically because the earlier
// hand-built fixtures never included that frame and so never exercised the bug it caused:
// gdbFirstOwnFrame had no rule for the marker, took it as "the first frame that belongs to the
// program", and reported every assertion reached through the standard handler chain as failing in
// "<signal handler called>" itself. Also real: `expr="arg3"` is only 4 characters of quoted
// content, below gdbLooksLikeText's 8-character floor — which silently discarded a condition that
// had, in fact, printed.
func TestDiagnoseAssertionSkipsTheSignalHandlerMarker(t *testing.T) {
	frames := []miFrame{
		{Func: "pthread_kill", From: "/usr/lib64/libpthread-2.28.so"},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 194},
		{Func: "<signal handler called>"},
		{Func: "raise", From: "/usr/lib64/libc-2.28.so"},
		{Func: "abort", From: "/usr/lib64/libc-2.28.so"},
		{Func: "ut_dbg_assertion_failed", File: "storage/innobase/ut/ut0dbg.cc", Line: 99, Args: []miVar{
			{Name: "expr", Value: `0x2a49642 "arg3"`, Arg: true},
			{Name: "expr@entry", Value: `0x2a49642 "arg3"`, Arg: true},
			{Name: "file", Value: `0x329a298 "/mnt/jenkins/workspace/ps8.0-autobuild-RELEASE/test/rpmbuild/BUILD/percona-server-8.0.26-16/percona-server-8.0.26-16/storage/innobase/eval/eval0eval.cc"`, Arg: true},
			{Name: "line", Value: "130", Arg: true},
		}},
		{Func: "eval_cmp_like", File: "storage/innobase/eval/eval0eval.cc", Line: 130},
		{Func: "eval_cmp", File: "storage/innobase/eval/eval0eval.cc", Line: 197},
	}
	for i := range frames {
		frames[i].Level = i
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGABRT", "Aborted")

	if v.Class != "assertion" {
		t.Fatalf("class = %q, want assertion", v.Class)
	}
	if !strings.Contains(v.Headline, `arg3`) || !strings.Contains(v.Headline, "eval0eval.cc:130") {
		t.Fatalf("headline does not carry the real condition and location: %q", v.Headline)
	}
	if v.Culprit == nil || v.Culprit.Func != "eval_cmp_like" {
		t.Fatalf("culprit = %v, want eval_cmp_like (not <signal handler called>)", v.Culprit)
	}
}

// PS-9668: enabling the audit_log_filter component and then running `LOCK TABLES FOR BACKUP`
// (what xtrabackup does to start a hot backup) throws std::logic_error — "basic_string:
// construction from null is not valid" — out of the record formatter and straight past every
// catch block. Before this test existed, the "abort() is on the stack" assertion case matched
// first and reported the culprit as std::basic_string's own constructor: real code, technically
// "not a system object" by the old check (it is a template instantiated into mysqld, so it has no
// `From` of its own), but a libstdc++ header nonetheless — one frame above the actual bug.
func TestDiagnoseUncaughtException(t *testing.T) {
	frames := []miFrame{
		{Func: "pthread_kill", From: "/usr/lib64/libpthread-2.28.so"},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 429},
		{Func: "<signal handler called>"},
		{Func: "raise", From: "/usr/lib64/libc-2.28.so"},
		{Func: "abort", From: "/usr/lib64/libc-2.28.so"},
		{Func: "__gnu_cxx::__verbose_terminate_handler", From: "/usr/lib64/libstdc++.so.6.0.25"},
		{Func: "__cxxabiv1::__terminate", From: "/usr/lib64/libstdc++.so.6.0.25"},
		{Func: "__cxa_call_terminate", From: "/usr/lib64/libstdc++.so.6.0.25"},
		{Func: "_Unwind_RaiseException", From: "/usr/lib64/libgcc_s-8-20210514.so.1"},
		{Func: "__cxa_throw", From: "/usr/lib64/libstdc++.so.6.0.25"},
		{Func: "std::__throw_logic_error", From: "/usr/lib64/libstdc++.so.6.0.25"},
		{Func: "std::__cxx11::basic_string<char>::basic_string<std::allocator<char> >",
			File: "/opt/rh/gcc-toolset-12/root/usr/include/c++/12/bits/basic_string.h", Line: 431},
		{Func: "audit_log_filter::log_record_formatter::LogRecordFormatter::apply",
			File: "components/audit_log_filter/log_record_formatter/new.cc", Line: 204},
		{Func: "audit_log_filter::log_writer::LogWriterBase::write",
			File: "components/audit_log_filter/log_writer/base.cc", Line: 40},
	}
	for i := range frames {
		frames[i].Level = i
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGABRT", "Aborted")

	if v.Class != "exception" {
		t.Fatalf("class = %q, want exception", v.Class)
	}
	if !strings.Contains(v.Headline, "std::logic_error") {
		t.Fatalf("headline does not name the exception type: %q", v.Headline)
	}
	if v.Culprit == nil || v.Culprit.Func != "audit_log_filter::log_record_formatter::LogRecordFormatter::apply" {
		t.Fatalf("culprit = %v, want LogRecordFormatter::apply (not basic_string's own constructor)", v.Culprit)
	}
}

// PS-11273: an invalid `authentication_policy` value aborts startup before component deinit runs,
// so a component's global std::unique_ptr-held state is torn down late by static destruction —
// and one of the things it owns is a still-joinable std::thread. Destroying a joinable
// std::thread calls std::terminate() *directly*: there is no throw anywhere on this stack, and
// the server's own error log says so verbatim ("terminate called without an active exception").
// Before this test, that shared "an uncaught exception" wording was applied here too, which is
// simply false — nothing was thrown.
func TestDiagnoseTerminateWithoutException(t *testing.T) {
	frames := []miFrame{
		{Func: "my_write_core", File: "mysys/stacktrace.cc", Line: 361},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 427},
		{Func: "<signal handler called>"},
		{Func: "raise", From: "/lib64/libc.so.6"},
		{Func: "abort", From: "/lib64/libc.so.6"},
		{Func: "__gnu_cxx::__verbose_terminate_handler", From: "/lib64/libstdc++.so.6"},
		{Func: "__cxxabiv1::__terminate", From: "/lib64/libstdc++.so.6"},
		{Func: "std::terminate", From: "/lib64/libstdc++.so.6"},
		{Func: "std::__terminate", File: "/opt/rh/gcc-toolset-12/root/usr/include/c++/12/x86_64-redhat-linux/bits/c++config.h", Line: 2552},
		{Func: "std::thread::~thread",
			File: "/opt/rh/gcc-toolset-12/root/usr/include/c++/12/bits/std_thread.h", Line: 94},
		{Func: "Worker::~Worker", File: "components/percona_telemetry/worker.h", Line: 32},
	}
	for i := range frames {
		frames[i].Level = i
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGABRT", "Aborted")

	if v.Class != "exception" {
		t.Fatalf("class = %q, want exception", v.Class)
	}
	if strings.Contains(v.Why, "propagated all the way out") {
		t.Fatalf("why claims something was thrown when there is no __cxa_throw on the stack: %q", v.Why)
	}
	if !strings.Contains(v.Headline, "terminate()") {
		t.Fatalf("headline does not say std::terminate was called directly: %q", v.Headline)
	}
	if v.Culprit == nil || v.Culprit.Func != "Worker::~Worker" {
		t.Fatalf("culprit = %v, want Worker::~Worker", v.Culprit)
	}
}

// PS-10990 (and its near-duplicates PXC-4794 and MySQL#115885): a stale Item_cache from a
// previous statement re-execution is walked again while checking column privileges on a query
// with a correlated subquery inside a stored procedure, and by then it is a use-after-free — not
// a null, since the freed memory usually still looks like a plausible pointer. Chased for real —
// a stored procedure calling a UNION of correlated subqueries under column-level GRANTs, called
// repeatedly under concurrent DDL for several minutes — and it did not reproduce in the time
// available; the original reporters needed real production data and called it "not consistently
// reproducible" even with that. Verified here against the bug's own real, published backtrace
// instead, the same honest fallback used for PS-8877 in the previous session.
func TestDiagnoseUseAfterFreeInItemCacheWalk(t *testing.T) {
	frames := []miFrame{
		{Func: "Item_cache::walk(bool (Item::*)(unsigned char*), enum_walk, unsigned char*)", File: "sql/item.cc", Line: 9626},
		{Func: "Item_ref::walk(bool (Item::*)(unsigned char*), enum_walk, unsigned char*)", File: "sql/item.h", Line: 5866},
		{Func: "Item_func::walk(bool (Item::*)(unsigned char*), enum_walk, unsigned char*)", File: "sql/item_func.cc", Line: 619},
		{Func: "Item_cond::walk(bool (Item::*)(unsigned char*), enum_walk, unsigned char*)", File: "sql/item_cmpfunc.cc", Line: 5785},
		{Func: "Query_block::check_column_privileges(THD*)", File: "sql/sql_select.cc", Line: 2042},
		{Func: "Query_block::check_privileges_for_subqueries(THD*)", File: "sql/sql_select.cc", Line: 2141},
		{Func: "Query_block::check_column_privileges(THD*)", File: "sql/sql_select.cc", Line: 2075},
		{Func: "Sql_cmd_select::check_privileges(THD*)", File: "sql/sql_select.cc", Line: 1156},
		{Func: "Sql_cmd_dml::execute(THD*)", File: "sql/sql_select.cc", Line: 724},
		{Func: "mysql_execute_command(THD*, bool)", File: "sql/sql_parse.cc", Line: 4983},
		{Func: "sp_instr_stmt::exec_core(THD*, unsigned int*)", File: "sql/sp_instr.cc", Line: 1013},
	}
	for i := range frames {
		frames[i].Level = i
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGSEGV", "Segmentation fault")

	if v.Class != "bad-pointer" {
		t.Fatalf("class = %q, want bad-pointer", v.Class)
	}
	if v.Culprit == nil || !strings.HasPrefix(v.Culprit.Func, "Item_cache::walk") {
		t.Fatalf("culprit = %v, want Item_cache::walk (the frame that actually dereferenced the freed object)", v.Culprit)
	}
}

// PS-8273 and PS-9666 (MyRocks insert into a table with a unique key and a TTL): both real cores
// resolve the culprit's *name* — myrocks::rdb_should_hide_ttl_rec, straight off ha_rocksdb.so's
// own export table — but carry no file or line, because the deploy installs debug symbols for the
// server package, not for a storage-engine plugin. That gap needs to be said, not left silent,
// since every other culprit this tool has ever shown carries a source line and a reader would
// otherwise have no way to tell "no debug info for this plugin" apart from "the search failed."
func TestDiagnosePluginCulpritExplainsMissingSource(t *testing.T) {
	frames := []miFrame{
		{Func: "pthread_kill", From: "/usr/lib64/libpthread-2.28.so"},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 228},
		{Func: "<signal handler called>"},
		{Func: "myrocks::rdb_should_hide_ttl_rec(myrocks::Rdb_key_def const&, rocksdb::Slice const&, myrocks::Rdb_transaction*)",
			From: "/sysroot/ha_rocksdb.so"},
		{Func: "myrocks::Rdb_iterator_base::get", From: "/sysroot/ha_rocksdb.so"},
		{Func: "myrocks::ha_rocksdb::check_and_lock_unique_pk", From: "/sysroot/ha_rocksdb.so"},
	}
	for i := range frames {
		frames[i].Level = i
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGSEGV", "Segmentation fault")
	if v.Culprit == nil || !strings.HasPrefix(v.Culprit.Func, "myrocks::rdb_should_hide_ttl_rec") {
		t.Fatalf("culprit = %v, want rdb_should_hide_ttl_rec", v.Culprit)
	}
	found := false
	for _, e := range v.Evidence {
		if strings.Contains(e, "ha_rocksdb.so") && strings.Contains(e, "no source line") {
			found = true
		}
	}
	if !found {
		t.Fatalf("evidence does not explain the missing file:line for a plugin frame: %v", v.Evidence)
	}
}

// PS-9719: changing binlog_transaction_dependency_tracking under concurrent write load
// heap-use-after-frees Writeset_trx_dependency_tracker's own std::unordered_map, and the fault
// lands eight frames deep inside libstdc++'s own hashtable template — std::equal_to's operator(),
// called through _M_key_equals, _M_equals, _M_find_before_node, _M_find_node, and two overloads
// of find, all in <bits/hashtable.h> and <bits/hashtable_policy.h>, none of them the bug.
// gdbFaultFrame (used by the bad-pointer/null-deref classes) only skipped a frame by checking its
// `From` shared object — which catches libstdc++.so itself, but not a template *instantiated* for
// Percona's own unordered_map<uint64_t, int64_t> and compiled straight into mysqld, exactly the
// gap gdbFirstOwnFrame had for std::basic_string in the previous session's PS-9668 fix. The real
// culprit, Writeset_trx_dependency_tracker::get_dependency, is real Percona code nine frames
// below the top — reading a hashtable that was already being replaced by another thread.
func TestDiagnoseBadPointerSkipsStdlibHashtableTemplate(t *testing.T) {
	stl := func(fn, file string, line int) miFrame {
		return miFrame{Func: fn, File: "/opt/rh/gcc-toolset-12/root/usr/include/c++/12/bits/" + file, Line: line}
	}
	frames := []miFrame{
		{Func: "pthread_kill", From: "/usr/lib64/libpthread-2.28.so"},
		{Func: "handle_fatal_signal", File: "sql/signal_handler.cc", Line: 253},
		{Func: "<signal handler called>"},
		stl("std::equal_to<unsigned long>::operator()", "hashtable.h", 1924),
		stl("std::__detail::_Hashtable_base<...>::_M_key_equals", "hashtable_policy.h", 1688),
		stl("std::__detail::_Hashtable_base<...>::_M_equals", "hashtable_policy.h", 1707),
		stl("std::_Hashtable<...>::_M_find_before_node", "hashtable.h", 1937),
		stl("std::_Hashtable<...>::_M_find_node", "hashtable.h", 816),
		stl("std::_Hashtable<...>::find", "hashtable.h", 1655),
		stl("std::unordered_map<...>::find", "unordered_map.h", 869),
		{Func: "Writeset_trx_dependency_tracker::get_dependency", File: "sql/rpl_trx_tracking.cc", Line: 265},
		{Func: "Transaction_dependency_tracker::get_dependency", File: "sql/rpl_trx_tracking.cc", Line: 348},
		{Func: "MYSQL_BIN_LOG::write_transaction", File: "sql/binlog.cc", Line: 1731},
	}
	for i := range frames {
		frames[i].Level = i
	}
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}, Depth: len(frames)}
	start, period, reps := gdbFindCycle(frames)
	gdbClassify(v, frames, start, period, reps, "SIGSEGV", "Segmentation fault")

	if v.Class != "bad-pointer" {
		t.Fatalf("class = %q, want bad-pointer", v.Class)
	}
	if v.Culprit == nil || v.Culprit.Func != "Writeset_trx_dependency_tracker::get_dependency" {
		t.Fatalf("culprit = %v, want Writeset_trx_dependency_tracker::get_dependency (not std::equal_to)", v.Culprit)
	}
}

// PXC-3848 (the first PXC core this project has analyzed): reporting that CURRENT_USER() cannot
// be used for a USER operation in cluster mode re-enters the same check through its own
// audit-logging path — get_current_user fails, raises a condition, which triggers
// mysql_audit_notify to log the query, which calls the rewriter, which calls get_current_user
// again, forever. The cycle is genuinely ten frames long — one more than the eight-frame ceiling
// gdbFindCycle shipped with (chosen from PS-5712's five-frame cycle, an unrelated bug) could ever
// find, and ten does not divide evenly into eight, so the old ceiling did not even find the inner
// fragment the way it degraded gracefully in other cases — it found *nothing*, on a real,
// 1,022-frame-deep core.
func TestGDBFindCycleTenFramePeriod(t *testing.T) {
	unit := []string{
		"get_current_user", "Rewriter_user::rewrite_users", "Rewriter_user::rewrite",
		"Rewriter_alter_user::rewrite", "(anonymous namespace)::rewrite_query",
		"mysql_rewrite_query", "thd_get_audit_query", "mysql_audit_notify",
		"THD::raise_condition", "my_message_sql",
	}
	var frames []miFrame
	frames = append(frames, miFrame{Func: "buffered_vfprintf", From: "/usr/lib64/libc-2.28.so"})
	for i := 0; i < 100; i++ {
		for _, fn := range unit {
			frames = append(frames, miFrame{Func: fn})
		}
	}
	for i := range frames {
		frames[i].Level = i
	}
	start, period, reps := gdbFindCycle(frames)
	if period != 10 || reps != 100 {
		t.Fatalf("cycle = %d frames × %d, want 10 × 100 (start %d)", period, reps, start)
	}
}
