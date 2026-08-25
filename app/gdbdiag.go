package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// gdbdiag.go — saying what went wrong, not just showing the stack.
//
// A backtrace is evidence, not a diagnosis, and the gap between the two is where this tool earns
// its keep. Reading the stack of the crash it was built against, three separate things mislead a
// reader who only has frames:
//
//   - **The faulting frame is not the bug.** The signal landed in `_int_malloc`, because that is
//     what happened to touch the guard page first. Nothing is wrong with malloc. The frame below
//     it — the allocation — is not the bug either; it is the innocent bystander that made the
//     allocation.
//   - **The size of the problem is invisible.** The stack is 1,085 frames deep. A pane showing 200
//     of them shows a repeating cycle repeating 39 times, which is true of the window and wrong
//     about the crash by a factor of twenty-five.
//   - **The cause is at the bottom.** What made it recurse — a 2,748-byte query — is an argument of
//     frame 1,068. Nobody scrolls to frame 1,068.
//
// So the analysis below does what a person would do with unlimited patience: read the *whole*
// stack, find the repetition, work out which frame drives it, then go and find the input that
// started it. Everything it concludes is reported next to the facts it concluded it from, because
// a diagnosis you cannot check is just a different thing to distrust.

// gdbVerdict is the answer to "what went wrong and why".
type gdbVerdict struct {
	Class    string    `json:"class"`             // stack-exhaustion | null-deref | heap-corruption | exception | assertion | bad-pointer | signal | unknown
	Headline string    `json:"headline"`          // what went wrong, in one sentence
	Why      string    `json:"why,omitempty"`     // why it happened, in one sentence
	Evidence []string  `json:"evidence"`          // the facts it was derived from — each one checkable in the panes
	Depth    int       `json:"depth"`             // the stack's real depth, not the window's
	Cycle    []string  `json:"cycle,omitempty"`   // the repeating functions, outermost first
	Repeats  int       `json:"repeats,omitempty"` // how many times that cycle runs
	Culprit  *miFrame  `json:"culprit,omitempty"` // the frame that is actually the bug
	Trigger  *gdbTrig  `json:"trigger,omitempty"` // the input that set it off
	Handler  bool      `json:"handler,omitempty"` // the server's own crash handler ran
	Query    *gdbQuery `json:"query,omitempty"`   // the SQL that was running, if any thread had one
}

// gdbQuery is the statement a THD was executing, read directly out of its own m_query_string —
// not guessed from a nearby argument. Any frame with a `thd` (or running_thd/target_thd) argument
// answers this the same way, so it applies far beyond the one crash class a "trigger" search is
// built for: a null-pointer crash, a bad config value hit mid-statement, an assertion — all of
// them are more useful next to the query that was running, which nobody has to scroll to frame
// 1,068 to find.
type gdbQuery struct {
	Text   string `json:"text"`
	Length int    `json:"length,omitempty"` // the real length; the text may be gdb-truncated
}

// gdbTrig is the input that caused the crash — the string argument, somewhere near the bottom of
// the stack, that the whole runaway was working on.
type gdbTrig struct {
	Frame int    `json:"frame"`
	Func  string `json:"func"`
	Where string `json:"where,omitempty"`
	Name  string `json:"name"`
	Value string `json:"value"`
	Extra string `json:"extra,omitempty"` // a sibling argument worth showing, e.g. query_len
}

// gdbDiagDepth is how many frames the analysis reads. The whole stack, in practice: a runaway
// recursion is the case that matters and it is the case with the most frames, so a ceiling here
// would blind the analysis to exactly what it is for. Ten thousand frames is ~1.5 MB of MI, well
// inside the client's line budget.
const gdbDiagDepth = 10000

// gdbDiagArgWindow is how many frames from the bottom are searched for the triggering input.
// The recursion's own frames carry pointers; the frame that *started* it carries the string, and
// it sits just below the cycle.
const gdbDiagArgWindow = 24

// gdbHandlerFrames are the frames that run *after* something has already gone wrong: the C
// library's kill/abort machinery, and the server's own crash handler.
//
// A MySQL core is almost always written by the server itself — handle_fatal_signal catches the
// signal, prints a backtrace to the error log, and calls my_write_core — so the top of every such
// stack is the handler, and a tool that reports the top frame reports the handler. That is not a
// small cosmetic problem: it is the difference between "my_write_core" and
// "temptable::Table::number_of_rows was called on a null this".
var gdbHandlerFrames = map[string]bool{
	"__pthread_kill_implementation": true, "__pthread_kill": true, "pthread_kill": true,
	"raise": true, "__GI_raise": true, "abort": true, "__GI_abort": true,
	"__assert_fail": true, "__assert_fail_base": true, "__cxa_throw": true, "_Unwind_Resume": true,
	"my_write_core": true, "handle_fatal_signal": true, "print_fatal_signal": true,
	"my_print_stacktrace": true, "ut_dbg_assertion_failed": true, "ib::fatal::~fatal": true,
	"my_server_abort": true, "my_abort": true, "signal_handler": true,
}

// gdbSignalHandlerMark is the frame gdb inserts where the kernel delivered the signal. Everything
// above it is the handler; the frame immediately below it is where the program actually was when
// it faulted. It is the single most reliable landmark in a crash stack and it costs nothing to
// look for.
const gdbSignalHandlerMark = "<signal handler called>"

// gdbFaultFrame is the frame the program actually crashed in.
//
// Preferred: the frame just below gdb's signal-handler marker, skipping any C-library helper the
// fault happened inside (memcpy, memmove, strlen — the fault is the caller's bad pointer, not
// libc's). Failing that, the first frame that is neither the crash machinery nor the C runtime.
func gdbFaultFrame(frames []miFrame) int {
	mark := -1
	for i, f := range frames {
		if strings.Contains(f.Func, gdbSignalHandlerMark) {
			mark = i
			break
		}
	}
	start := 0
	if mark >= 0 {
		start = mark + 1
	}
	for i := start; i < len(frames); i++ {
		f := frames[i]
		if f.Func == "" || f.Func == "??" {
			continue
		}
		if gdbHandlerFrames[shortFuncName(f.Func)] {
			continue
		}
		// A fault inside memcpy is the caller's bad pointer or bad length, never memcpy's.
		if f.From != "" && gdbSystemObject(f.From) {
			continue
		}
		if gdbLibcHelper(shortFuncName(f.Func)) {
			continue
		}
		// The same reasoning as memcpy, for the C++ standard library: a fault inside
		// std::equal_to's own operator(), reached through an unordered_map's bucket lookup, is
		// the caller handing the container a stale iterator or a freed backing array — not a bug
		// in libstdc++'s hash table. PS-9719's real core (a heap-use-after-free the bug's own
		// ASAN report confirms is in Writeset_trx_dependency_tracker, three frames below this one)
		// is why this check exists here and not only in gdbFirstOwnFrame: the two used to disagree
		// on whether inlined STL template code counts as "the program", so a bad-pointer fault
		// landing in one was reported as std::equal_to's problem instead of its caller's.
		if gdbSystemHeaderFrame(f) {
			continue
		}
		return i
	}
	if mark >= 0 && mark+1 < len(frames) {
		return mark + 1
	}
	return -1
}

// gdbLibcHelper names the byte-shuffling functions a bad pointer most often dies inside. They are
// where the fault *lands*; the frame above them is where it was caused.
func gdbLibcHelper(fn string) bool {
	for _, p := range []string{"__memmove", "__memcpy", "__memset", "__strlen", "__strcpy",
		"__strcmp", "__mempcpy", "memcpy", "memmove", "memset", "strlen"} {
		if fn == p || strings.HasPrefix(fn, p+"_") {
			return true
		}
	}
	return false
}

// gdbNullish reports whether a printed pointer value is null or a small offset from null — the
// signature of a member access on a null object. `this=0x0` is a null receiver; `this=0x30` is a
// null receiver plus a field offset, which is the same bug seen 48 bytes later.
// gdbParsePointer reads a gdb-printed bare pointer value — not `0x7f… "text"` (a string) and not
// `0x… <fn>` (a function pointer), just the address.
func gdbParsePointer(v string) (uint64, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "0x") || strings.ContainsAny(v, " \t") {
		return 0, false
	}
	n, err := strconv.ParseUint(v[2:], 16, 64)
	return n, err == nil
}

func gdbNullish(v string) (uint64, bool) {
	n, ok := gdbParsePointer(v)
	if !ok {
		return 0, false
	}
	return n, n < 0x1000
}

// diagnose reads the whole stack of the signalled thread and works out what happened.
func (s *gdbSession) diagnose(ctx context.Context, cli *miClient, thread, signal, sigText string) *gdbVerdict {
	v := &gdbVerdict{Class: "unknown", Evidence: []string{}}

	depth, err := cli.stackDepth(ctx, thread)
	if err != nil || depth == 0 {
		return nil
	}
	v.Depth = depth

	frames, err := cli.frames(ctx, thread, 0, min(depth, gdbDiagDepth))
	if err != nil || len(frames) == 0 {
		return nil
	}

	// The arguments of the frames around the fault. A null `this` is *the* root cause of a whole
	// class of crashes and it is sitting right here, in an argument, where a stack pane that
	// only lists function names will never show it.
	faultArgs := map[int][]miVar{}
	if fa, err := cli.stackArguments(ctx, thread, 0, min(len(frames), 24)-1); err == nil {
		for _, f := range fa {
			faultArgs[f.Level] = f.Args
		}
	}
	for i := range frames {
		if a, ok := faultArgs[frames[i].Level]; ok && len(frames[i].Args) == 0 {
			frames[i].Args = a
		}
	}

	start, period, reps := gdbFindCycle(frames)
	if reps >= 3 {
		v.Repeats = reps
		for _, f := range frames[start : start+period] {
			v.Cycle = append(v.Cycle, shortFuncName(f.Func))
		}
	}
	v.Handler = gdbHasFrame(frames, "handle_fatal_signal", "my_print_stacktrace", "print_fatal_signal")

	gdbClassify(v, frames, start, period, reps, signal, sigText)

	// The input. Only worth looking for when something ran away with it — a plain null
	// dereference has no "trigger" in this sense, and guessing one would be noise.
	if v.Class == "stack-exhaustion" {
		below := start + period*reps
		v.Trigger = s.findTrigger(ctx, cli, thread, frames, below)
		if v.Trigger != nil {
			v.Evidence = append(v.Evidence, fmt.Sprintf(
				"the recursion was started by %s at frame #%d, working on %s",
				shortFuncName(v.Trigger.Func), v.Trigger.Frame, v.Trigger.Name))
		}
	}

	// The query, unconditionally — this is not specific to one crash class. Any thread that was
	// mid-statement when the signal arrived has one, and knowing it is useful for a config crash,
	// a null pointer, an assertion, or a class this analysis does not even have a name for.
	if q := s.findQuery(ctx, cli, thread, frames); q != nil {
		v.Query = q
		shown := q.Text
		if q.Length > 0 && q.Length > len(shown) {
			shown = fmt.Sprintf("%s… (%d bytes)", shown, q.Length)
		}
		v.Evidence = append(v.Evidence, "the thread was running: "+shown)
	}
	return v
}

// gdbClassify decides what kind of crash this is, from evidence rather than from the top frame.
func gdbClassify(v *gdbVerdict, frames []miFrame, start, period, reps int, signal, sigText string) {
	covered := period * reps
	top := frames[0]
	// Where the program actually was, as opposed to where the crash handler is. On a MySQL core
	// these are never the same frame.
	fi := gdbFaultFrame(frames)
	var fault *miFrame
	if fi >= 0 {
		fault = &frames[fi]
		if gdbHandlerFrames[shortFuncName(top.Func)] {
			v.Handler = true
		}
	}

	switch {
	// Runaway recursion. The test is not "the top frame looks odd" — it is that most of a very
	// deep stack is one repeating unit. That is a fact about the whole stack, and it is why the
	// analysis reads all of it.
	case reps >= 3 && covered*2 > len(frames) && v.Depth > 200:
		v.Class = "stack-exhaustion"
		// The bug is the function that recurses, not the one that faulted. Take the outermost
		// frame of the cycle: it is the one that called itself again.
		culprit := frames[start+period-1]
		v.Culprit = &culprit
		v.Headline = fmt.Sprintf("The thread ran out of stack: %s recursed %d times without a depth limit.",
			shortFuncName(culprit.Func), reps)
		v.Why = fmt.Sprintf(
			"%s is a %d-frame cycle that repeats %d times, %d of the stack's %d frames. "+
				"The signal landed in %s only because an allocation was the first thing to touch "+
				"the guard page — malloc is not the problem, and neither is the frame that called it.",
			strings.Join(v.Cycle, " → "), period, reps, covered, v.Depth, shortFuncName(top.Func))
		v.Evidence = append(v.Evidence,
			fmt.Sprintf("the stack is %d frames deep", v.Depth),
			fmt.Sprintf("%d of them are one repeating %d-frame cycle: %s",
				covered, period, strings.Join(v.Cycle, " → ")),
			fmt.Sprintf("the signal is %s and the faulting frame is %s, in the C library",
				orUnknown(signal), shortFuncName(top.Func)))

	// glibc's own heap consistency check, not the server's. This has to be tested *before* the
	// assertion case below: malloc_printerr calls abort() internally, so a stack carrying it also
	// satisfies "abort() is on the stack" — and reporting a double free as "an assertion failed"
	// sends the reader looking for a check in the server's code that was never there. Nobody wrote
	// an assertion; glibc caught its bookkeeping already being wrong and refused to make it worse.
	case gdbHasFrame(frames, "malloc_printerr", "_int_free", "_int_malloc", "malloc_consolidate"):
		v.Class = "heap-corruption"
		culprit := fault
		if culprit == nil {
			culprit = gdbFirstOwnFrame(frames)
		}
		v.Culprit = culprit
		if culprit != nil {
			v.Headline = fmt.Sprintf("Heap corruption: glibc refused to continue, caught in %s%s.",
				shortFuncName(culprit.Func), locSuffix(*culprit))
		} else {
			v.Headline = "Heap corruption: glibc's allocator refused to continue."
		}
		v.Why = "This is glibc's own malloc/free detecting that its bookkeeping is already wrong — a " +
			"double free, a free of a pointer that was never malloc'd, or a write that ran past the " +
			"end of an earlier allocation — and aborting rather than corrupt memory further. The frame " +
			"below the allocator is where it was *caught*, which is where to start looking, but it is " +
			"not necessarily where the original overflow or double-free happened; that can be an " +
			"earlier call entirely, on the same object."
		v.Evidence = append(v.Evidence,
			"glibc's malloc consistency check is on the stack, not a check the server wrote itself")
		if culprit != nil {
			v.Evidence = append(v.Evidence, "the call that triggered it is "+shortFuncName(culprit.Func)+
				locSuffix(*culprit))
		}

	// An uncaught C++ exception, not a check the server wrote. This also has to be tested before
	// the assertion case: the C++ runtime's own terminate handler calls abort() once it gives up
	// looking for a catch block, so this stack satisfies "abort() is on the stack" too — and
	// PS-9668's real core is why it matters: reported as a bare "an assertion failed", the culprit
	// landed on std::basic_string's own constructor (a libstdc++ header, inlined into mysqld,
	// filtered nowhere) instead of the audit_log_filter code that handed it a null pointer.
	case gdbHasFrame(frames, "__cxa_throw", "__terminate", "__verbose_terminate_handler",
		"_ZSt17__throw_bad_allocv", "__throw_bad_alloc"):
		v.Class = "exception"
		culprit := gdbFirstOwnFrame(frames)
		v.Culprit = culprit
		// __cxa_throw is what actually raises a C++ exception — if it is not on the stack, nothing
		// was thrown at all: std::terminate() was called directly, which is what a std::thread's
		// destructor does if it is destroyed while still joinable. PS-11273's real core is this
		// exact case — Worker::~Worker, no throw anywhere in the stack — and the wording for a
		// genuine uncaught exception ("propagated all the way out") is simply false here; the
		// server's own error log even says so verbatim: "terminate called without an active
		// exception".
		thrown := gdbHasFrame(frames, "__cxa_throw")
		kind := gdbUncaughtExceptionType(frames)
		switch {
		case thrown && kind != "":
			if culprit != nil {
				v.Headline = fmt.Sprintf("Uncaught %s in %s%s.", kind, shortFuncName(culprit.Func), locSuffix(*culprit))
			} else {
				v.Headline = fmt.Sprintf("Uncaught %s reached the top of the stack.", kind)
			}
			v.Why = fmt.Sprintf(
				"%s propagated all the way out with nothing to catch it, so the C++ runtime's own "+
					"terminate handler called abort() — this is not a check the server wrote, it is what "+
					"happens to *any* uncaught exception. The frame below the throw machinery is what threw "+
					"it, which is the question worth answering: what argument was it given that it could not "+
					"accept.", kind)
			v.Evidence = append(v.Evidence, kind+" reached std::terminate without being caught")
		case thrown:
			if culprit != nil {
				v.Headline = fmt.Sprintf("An uncaught exception reached the top of the stack, thrown from %s%s.",
					shortFuncName(culprit.Func), locSuffix(*culprit))
			} else {
				v.Headline = "An uncaught exception reached the top of the stack."
			}
			v.Why = "Something was thrown with nothing to catch it, so the C++ runtime's own terminate " +
				"handler called abort() — this is not a check the server wrote, it is what happens to " +
				"any uncaught exception. The frame below the throw machinery is what threw it."
			v.Evidence = append(v.Evidence, "an exception reached std::terminate without being caught")
		default:
			// No __cxa_throw anywhere: std::terminate() was invoked directly, not as the result of
			// an unhandled throw.
			if culprit != nil {
				v.Headline = fmt.Sprintf("std::terminate() called directly, in %s%s.",
					shortFuncName(culprit.Func), locSuffix(*culprit))
			} else {
				v.Headline = "std::terminate() was called directly."
			}
			v.Why = "There is no throw anywhere on this stack, so this is not an uncaught exception — " +
				"the C++ runtime's terminate handler was invoked directly, which is what a std::thread " +
				"does in its own destructor if it is destroyed while still joinable (never joined or " +
				"detached), and what a destructor that throws during stack unwinding does. Either way " +
				"the frame it fired in is the object whose cleanup did not run the way its type requires."
			v.Evidence = append(v.Evidence, "std::terminate() was called with no exception in flight "+
				"(no __cxa_throw on this stack)")
		}
		if culprit != nil {
			v.Evidence = append(v.Evidence, "it fired in "+shortFuncName(culprit.Func)+locSuffix(*culprit))
		}

	// An assertion, or anything else that called abort() on purpose. The server chose to die.
	case gdbHasFrame(frames, "__assert_fail", "abort", "__GI_abort", "ut_dbg_assertion_failed"):
		v.Class = "assertion"
		culprit := gdbFirstOwnFrame(frames)
		v.Culprit = culprit
		name := "an assertion"
		if gdbHasFrame(frames, "ut_dbg_assertion_failed") {
			name = "an InnoDB assertion"
		}
		// The condition itself. ut_dbg_assertion_failed(expr, file, line) and glibc's own
		// __assert_fail(assertion, file, line, function) both take the failed check as their own
		// first argument — not the caller's, *theirs* — so it does not have to be guessed from
		// context. This is the difference between "an assertion failed" and
		// "pos < n_def failed", and it applies to every assertion in the server, not one bug.
		expr, exprFile, exprLine := gdbAssertionCondition(frames)
		var callerNote string
		if culprit != nil {
			callerNote = fmt.Sprintf(" It fired in %s%s.", shortFuncName(culprit.Func), locSuffix(*culprit))
		}
		if expr != "" {
			where := ""
			if exprFile != "" {
				where = fmt.Sprintf(" (%s:%d)", exprFile, exprLine)
			}
			v.Headline = fmt.Sprintf("%s failed: %s%s.", strings.ToUpper(name[:1])+name[1:], expr, where)
			v.Why = "abort() is on the stack, so this is not a memory fault — the code detected a state " +
				"it was written to refuse to continue from, and said exactly which state." + callerNote
			v.Evidence = append(v.Evidence, fmt.Sprintf("the check that failed is `%s`%s", expr, where))
		} else {
			v.Headline = fmt.Sprintf("%s failed and the server aborted deliberately.",
				strings.ToUpper(name[:1])+name[1:])
			v.Why = "abort() is on the stack, so this is not a memory fault — the code detected a state " +
				"it was written to refuse to continue from. The check itself did not print its condition " +
				"(or the symbols to read it are missing); the frame below the abort machinery is where " +
				"it fired." + callerNote
			v.Evidence = append(v.Evidence, "abort() is on the stack, so the process ended on purpose")
		}
		if culprit != nil {
			v.Evidence = append(v.Evidence, "the check that fired is "+shortFuncName(culprit.Func)+
				locSuffix(*culprit))
		}

	// A null pointer, named precisely. This is a class of its own rather than a flavour of
	// "bad pointer" because the evidence is exact and the fix follows from it: some caller passed
	// an object that was never created, and the argument list says which one.
	case (signal == "SIGSEGV" || signal == "SIGBUS") && fault != nil && gdbNullArg(*fault) != nil:
		na := gdbNullArg(*fault)
		v.Class = "null-deref"
		v.Culprit = fault
		v.Headline = fmt.Sprintf("Null pointer: %s was called with %s = %s%s.",
			shortFuncName(fault.Func), na.Name, na.Value, locSuffix(*fault))
		off := ""
		if n, _ := gdbNullish(na.Value); n > 0 {
			off = fmt.Sprintf(" The value is not exactly zero because the code had already added a "+
				"field offset of %d bytes to a null pointer before dereferencing it, which is the "+
				"same bug read a few instructions later.", n)
		}
		caller := gdbCallerOf(frames, fault)
		v.Why = fmt.Sprintf("The fault is not in %s — it is in whatever handed it that pointer.%s%s",
			shortFuncName(fault.Func), off, gdbCallerSentence(caller))
		v.Evidence = append(v.Evidence,
			fmt.Sprintf("%s is %s in %s%s", na.Name, na.Value, shortFuncName(fault.Func), locSuffix(*fault)))
		if caller != nil {
			v.Evidence = append(v.Evidence, "it was called from "+shortFuncName(caller.Func)+locSuffix(*caller))
		}
		v.Evidence = append(v.Evidence, fmt.Sprintf("the signal is %s (%s)", signal, orUnknown(sigText)))

	// A memory fault that is not a stack overflow and not an obvious null: a bad pointer or a bad
	// length. Which of the two is worth saying, because a fault inside memcpy is nearly always a
	// length the caller got wrong rather than a pointer.
	case signal == "SIGSEGV" || signal == "SIGBUS":
		v.Class = "bad-pointer"
		culprit := fault
		if culprit == nil {
			culprit = gdbFirstOwnFrame(frames)
		}
		v.Culprit = culprit
		inCopy := ""
		for i := 0; i < min(len(frames), 8); i++ {
			if gdbLibcHelper(shortFuncName(frames[i].Func)) {
				inCopy = shortFuncName(frames[i].Func)
				break
			}
		}
		if culprit != nil {
			v.Headline = fmt.Sprintf("%s in %s%s.", signal, shortFuncName(culprit.Func), locSuffix(*culprit))
		} else {
			v.Headline = signal + ": the process touched memory it could not."
		}
		caller := gdbCallerOf(frames, culprit)
		switch {
		case inCopy != "":
			v.Why = fmt.Sprintf(
				"The fault landed inside %s, so this is a copy that ran off the end: either the "+
					"source or destination pointer is wrong, or — far more often — the length is. "+
					"%s is where that length was computed.%s",
				inCopy, shortFuncName(culprit.Func), gdbCallerSentence(caller))
			v.Evidence = append(v.Evidence, "the fault is inside "+inCopy+
				", which means a bad length or a bad pointer handed to it, not a bug in the C library")
		default:
			v.Why = "The stack is not unusually deep and has no runaway recursion, so this is a bad " +
				"pointer rather than a stack overflow — a freed object, an index past the end, or a " +
				"field read through a pointer that was never valid." + gdbCallerSentence(caller)
		}
		v.Evidence = append(v.Evidence,
			fmt.Sprintf("the signal is %s (%s)", signal, orUnknown(sigText)),
			fmt.Sprintf("the stack is %d frames deep, with no repeating cycle", v.Depth))
		if culprit != nil {
			v.Evidence = append(v.Evidence, "the program was in "+shortFuncName(culprit.Func)+
				locSuffix(*culprit)+" when the signal arrived")
		}

	case signal != "":
		v.Class = "signal"
		v.Headline = fmt.Sprintf("The process was killed by %s (%s).", signal, orUnknown(sigText))
		v.Culprit = gdbFirstOwnFrame(frames)
		v.Evidence = append(v.Evidence, fmt.Sprintf("the stack is %d frames deep", v.Depth))

	default:
		v.Headline = "No signal was recorded in this core."
		v.Why = "The core may have been written by gcore or by the process itself rather than by a " +
			"crash, in which case there is nothing here that went wrong."
		v.Culprit = gdbFirstOwnFrame(frames)
	}

	if v.Handler {
		v.Evidence = append(v.Evidence,
			"the server's own crash handler ran, so its error log has a backtrace of this too")
	}

	// A culprit with a name but no file:line is almost always a plugin — PS-8273's and PS-9666's
	// real cores are both this shape, in ha_rocksdb.so — because the deploy installs debug symbols
	// for the server package, not for a storage-engine plugin nobody asked this node to know about.
	// The name still resolved (the .so's own export table has it), so the diagnosis is not wrong,
	// but a reader comparing this frame to any other in the panel — all of which carry a source
	// line — deserves to know why this one does not, rather than guessing the symbol search failed.
	if v.Culprit != nil && v.Culprit.File == "" && v.Culprit.From != "" && !gdbSystemObject(v.Culprit.From) {
		v.Evidence = append(v.Evidence, fmt.Sprintf(
			"%s resolved from %s's own export table — no source line, because debug symbols were "+
				"installed for the server, not for this plugin",
			shortFuncName(v.Culprit.Func), gdbBasename(v.Culprit.From)))
	}
}

// gdbQueryWindow is how many frames from the top are searched for a live THD. The SQL layer —
// dispatch_command, mysql_execute_command, Query_expression::execute — sits well within this on
// every crash examined so far; the deepest was around frame 23.
const gdbQueryWindow = 60

// gdbThdArgNames are the parameter names a THD pointer turns up under. Most code calls it `thd`;
// a few functions that read one thread's state from another's (get_one_variable_ext, seen in
// PS-4785) take both a `running_thd` and a `target_thd`, and it is the target's query that
// matters.
var gdbThdArgNames = map[string]bool{"thd": true, "running_thd": true, "target_thd": true}

// findQuery looks for a live THD anywhere in the top of the stack and reads its query straight
// out of m_query_string, rather than hoping a nearby argument happens to carry the SQL as text.
//
// It is deliberately not scoped to one crash class: a config value can be invalid only in the
// context of one particular statement, a null pointer can be a state a specific query put the
// server into, and "here is the SQL, look for yourself" is useful context for a class this
// analysis does not recognize at all. m_query_string.str is empty between statements and for
// threads that never ran one (a background thread, a connection still authenticating), and gdb's
// "cannot access memory" for a THD gone stale is treated the same as not finding one — both mean
// there is nothing here worth showing.
func (s *gdbSession) findQuery(ctx context.Context, cli *miClient, thread string, frames []miFrame) *gdbQuery {
	n := min(len(frames), gdbQueryWindow)
	// Always fetched fresh for the full window, rather than reusing whatever a caller already
	// attached to frames[0:24] for its own purposes: a first version tried to reuse that and only
	// fell back to fetching when nothing had been attached at all — so as soon as *any* frame in
	// 0-23 had args, frames 24-59 were silently never asked for, and a THD sitting at frame 30
	// (not unusual — dispatch_command runs deeper than that once a few wrapper layers are in
	// play) was invisible with no error to say so.
	byLevel := map[int][]miVar{}
	fa, err := cli.stackArguments(ctx, thread, 0, n-1)
	if err != nil {
		return nil
	}
	for _, f := range fa {
		byLevel[f.Level] = f.Args
	}
	for _, f := range frames[:n] {
		for _, a := range byLevel[f.Level] {
			if !gdbThdArgNames[a.Name] {
				continue
			}
			if ptr, ok := gdbParsePointer(a.Value); !ok || ptr < 0x1000 {
				continue
			}
			val, err := cli.evaluate(ctx, thread, 0,
				fmt.Sprintf("((THD*)(%s))->m_query_string", a.Value))
			if err != nil || !gdbLooksLikeText(val) {
				continue
			}
			q := &gdbQuery{Text: gdbCleanText(val)}
			if m := gdbLexLengthRe.FindStringSubmatch(val); m != nil {
				q.Length, _ = strconv.Atoi(m[1])
			}
			if q.Text != "" {
				return q
			}
		}
	}
	return nil
}

var gdbLexLengthRe = regexp.MustCompile(`length = (\d+)`)

// findTrigger looks for the input the runaway was working on.
//
// It searches the frames *below* the recursion, because that is where the call that started it
// lives, and it looks for a string-valued argument — a query, a statement, a path. That is the
// difference between "fts_ast_visit recursed 212 times" and "this query made fts_ast_visit recurse
// 212 times", and only the second one tells you what to do about it.
func (s *gdbSession) findTrigger(ctx context.Context, cli *miClient, thread string, frames []miFrame, below int) *gdbTrig {
	if below >= len(frames) {
		return nil
	}
	last := min(below+gdbDiagArgWindow, len(frames))
	args, err := cli.stackArguments(ctx, thread, below, last-1)
	if err != nil {
		return nil
	}
	for _, fa := range args {
		var best *miVar
		var extra string
		for i := range fa.Args {
			a := fa.Args[i]
			if strings.HasSuffix(a.Name, "@entry") {
				continue // gdb reports the entry value as a duplicate argument
			}
			if gdbLooksLikeText(a.Value) && (best == nil || len(a.Value) > len(best.Value)) {
				best = &fa.Args[i]
			}
			if strings.Contains(strings.ToLower(a.Name), "len") && extra == "" {
				extra = a.Name + " = " + a.Value
			}
		}
		if best == nil {
			continue
		}
		f := gdbFrameAt(frames, fa.Level)
		t := &gdbTrig{
			Frame: fa.Level, Name: best.Name, Value: gdbCleanText(best.Value), Extra: extra,
		}
		if f != nil {
			t.Func, t.Where = f.Func, strings.TrimPrefix(locSuffix(*f), " at ")
		}
		return t
	}
	return nil
}

// gdbLooksLikeText reports whether an argument's printed value carries a string. gdb prints a
// char* as `0x7f… "the text"`, so the quote is the marker — and a pointer with no text after it is
// exactly what the recursion's own frames carry, which is why they are skipped.
func gdbLooksLikeText(v string) bool {
	i := strings.IndexByte(v, '"')
	if i < 0 {
		return false
	}
	// At least a few characters of actual content, so a `""` or a one-character flag is not
	// mistaken for the input that crashed a server.
	return len(v)-i > 8
}

// gdbCleanText pulls the quoted text out of gdb's `0x7f… "…"` rendering and drops its ellipsis.
func gdbCleanText(v string) string {
	i := strings.IndexByte(v, '"')
	if i < 0 {
		return v
	}
	out := v[i+1:]
	if j := strings.LastIndexByte(out, '"'); j > 0 {
		out = out[:j]
	}
	return strings.TrimSuffix(out, `"...`)
}

// gdbFindCycle finds the repeating run of frames that covers the most of the stack.
//
// Coverage rather than length or position, because a runaway recursion is by definition the thing
// that most of the stack is. The period ceiling started at eight, from PS-5712's real core: its
// unit is five frames (fts_query_visitor → fts_ast_visit ×3 → fts_ast_visit_sub_exp), and a
// smaller ceiling finds the inner three-frame run instead — true, and a quarter of the answer.
// Raised to 24 for PXC-3848's real core: reporting `CURRENT_USER()` invalid in cluster mode
// re-enters the same check through the query-rewriter's own audit-logging path
// (get_current_user → my_message_sql → raise_condition → mysql_audit_notify →
// mysql_rewrite_query → Rewriter_user::rewrite_users → get_current_user, ten frames start to
// finish, 1,022 deep) — a ceiling of eight found nothing at all here, not even the inner fragment
// PS-5712 fell back to, because ten does not divide into eight.
func gdbFindCycle(frames []miFrame) (start, period, reps int) {
	bestCover := 0
	for i := 0; i < len(frames); i++ {
		found := 0
		for p := 1; p <= 24 && i+2*p <= len(frames); p++ {
			r := 1
			for {
				n := i + r*p
				if n+p > len(frames) || !gdbSameCycle(frames[i:i+p], frames[n:n+p]) {
					break
				}
				r++
			}
			if r >= 3 && r*p > bestCover {
				bestCover, start, period, reps = r*p, i, p, r
			}
			if r >= 3 && r*p > found {
				found = r * p
			}
		}
		// Skip past a run rather than re-scanning every frame inside it: a 1,000-frame recursion
		// would otherwise be examined a thousand times over.
		if found > 1 {
			i += found - 1
		}
	}
	return start, period, reps
}

// gdbHasFrame reports whether any frame's function starts with one of these names.
func gdbHasFrame(frames []miFrame, names ...string) bool {
	for _, f := range frames {
		fn := shortFuncName(f.Func)
		for _, n := range names {
			if fn == n || strings.HasSuffix(fn, "::"+n) {
				return true
			}
		}
	}
	return false
}

// gdbFirstOwnFrame is the topmost frame that belongs to the program rather than to the C runtime
// or to the crash-handling machinery that runs after the fact.

// gdbSystemHeaderFrame reports whether a frame's source line comes from the C++ standard
// library's own headers rather than the server's source tree — a template instantiated for a
// Percona type (basic_string, variant, unique_ptr) has no separate `From` shared object to catch
// it by, since it is compiled straight into mysqld, but its file path still says what it is: a
// release build's headers live wherever the builder's toolchain put them
// (`/usr/include/c++/12/…` or, on this box's RHEL-family builds, `/opt/rh/gcc-toolset-12/…`), and
// every one of those paths contains `/include/c++/`.
func gdbSystemHeaderFrame(f miFrame) bool {
	return strings.Contains(f.File, "/include/c++/")
}

// gdbUncaughtExceptionType names which exception reached std::terminate, read off the specific
// std::__throw_* helper on the stack (__throw_logic_error, __throw_out_of_range, __throw_bad_alloc,
// …) rather than guessed from the class hierarchy — libstdc++ names its own throw sites this way
// for exactly this purpose.
func gdbUncaughtExceptionType(frames []miFrame) string {
	for _, f := range frames {
		fn := shortFuncName(f.Func)
		if i := strings.Index(fn, "__throw_"); i >= 0 {
			name := strings.TrimSuffix(fn[i+len("__throw_"):], "v")
			if name != "" {
				return "std::" + name
			}
		}
	}
	return ""
}

// gdbAssertionCondition reads the failed check straight out of the assertion helper's own
// arguments, rather than guessing at it from a caller. ut_dbg_assertion_failed(expr, file, line)
// and glibc's own __assert_fail(assertion, file, line, function) both take the condition as
// *their* first argument — the frame doing the asserting, not the code that triggered it — which
// is what makes this exact rather than a heuristic.
func gdbAssertionCondition(frames []miFrame) (expr, file string, line int) {
	for _, f := range frames {
		switch shortFuncName(f.Func) {
		case "ut_dbg_assertion_failed":
			return gdbArgText(f, "expr"), gdbBasename(gdbArgText(f, "file")), gdbArgInt(f, "line")
		case "__assert_fail", "__assert_fail_base":
			return gdbArgText(f, "assertion"), gdbBasename(gdbArgText(f, "file")), gdbArgInt(f, "line")
		}
	}
	return "", "", 0
}

// gdbBasename trims a compiled-in __FILE__ down to its final component. A release RPM build bakes
// in the builder's own absolute path — PS-7958's real core carries
// "/mnt/jenkins/workspace/ps8.0-autobuild-RELEASE/.../eval0eval.cc" as the assertion macro's file
// argument — and a headline is not the place for that; "eval0eval.cc" is what a reader wants.
func gdbBasename(path string) string {
	return path[strings.LastIndexByte(path, '/')+1:]
}

// gdbArgText reads one named argument as text, unquoting it the same way a query string is
// unquoted — gdb prints a char* the same way whichever function it belongs to.
//
// Unlike gdbLooksLikeText's 8-character floor (needed when *guessing* which of several arguments
// is the interesting one, in findTrigger below), this is reading one specific, already-known
// argument by name, so any quoted content at all is the answer — never noise to filter. PS-7958's
// real core is why this exists as its own check: InnoDB's assertion macro can fail on nothing
// fancier than a single C identifier, and `expr="arg3"` is only 4 characters of content, which the
// shared heuristic silently discarded as if the condition had not printed at all.
func gdbArgText(f miFrame, name string) string {
	for _, a := range f.Args {
		if a.Name != name {
			continue
		}
		if i := strings.IndexByte(a.Value, '"'); i >= 0 {
			return gdbCleanText(a.Value)
		}
	}
	return ""
}

func gdbArgInt(f miFrame, name string) int {
	for _, a := range f.Args {
		if a.Name == name {
			n, _ := strconv.Atoi(strings.TrimSpace(a.Value))
			return n
		}
	}
	return 0
}

func gdbFirstOwnFrame(frames []miFrame) *miFrame {
	for i := range frames {
		f := frames[i]
		if f.Func == "" || f.Func == "??" {
			continue
		}
		// gdb's own marker for where the kernel delivered the signal — not a function that ran,
		// so it can never be the culprit. A real gdb backtrace always carries this frame between
		// handle_fatal_signal and the libc raise/abort below it; a hand-built one easily omits it,
		// which is how this went unnoticed until a live SIGABRT core exercised it: every assertion
		// reached through the standard handler chain was reported as failing in
		// "<signal handler called>" itself.
		if strings.Contains(f.Func, gdbSignalHandlerMark) {
			continue
		}
		if f.From != "" && gdbSystemObject(f.From) {
			continue
		}
		// A standard-library template — basic_string's own constructor, a variant visitor,
		// unique_ptr's deleter — instantiated for a Percona type and compiled straight into
		// mysqld, so it carries no `From` for the libstdc++.so check above to catch. PS-9668's
		// real core is why this exists: the culprit came back as
		// std::basic_string::basic_string at bits/char_traits.h:431, which is libstdc++ doing
		// exactly what it was told (constructing a string from the null pointer it was handed),
		// one frame above the audit_log_filter code that handed it that pointer.
		if gdbSystemHeaderFrame(f) {
			continue
		}
		// The machinery that runs *after* something went wrong is never the thing that went
		// wrong — see gdbHandlerFrames. This used to keep its own copy of that list, which is
		// exactly the kind of thing that drifts: my_server_abort was added to one and not the
		// other, and every InnoDB assertion reached through it was diagnosed as a bug in
		// my_server_abort instead of in the check that actually failed.
		if gdbHandlerFrames[shortFuncName(f.Func)] {
			continue
		}
		return &frames[i]
	}
	if len(frames) > 0 {
		return &frames[0]
	}
	return nil
}

func gdbFrameAt(frames []miFrame, level int) *miFrame {
	for i := range frames {
		if frames[i].Level == level {
			return &frames[i]
		}
	}
	return nil
}

func locSuffix(f miFrame) string {
	if f.File == "" {
		return ""
	}
	base := f.File[strings.LastIndexByte(f.File, '/')+1:]
	if f.Line > 0 {
		return " at " + base + ":" + strconv.Itoa(f.Line)
	}
	return " at " + base
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// gdbNullArg returns the frame's first argument that is a null or near-null pointer, preferring
// `this` — a null receiver is a more specific statement than a null parameter.
func gdbNullArg(f miFrame) *miVar {
	var other *miVar
	for i := range f.Args {
		a := f.Args[i]
		if strings.HasSuffix(a.Name, "@entry") {
			continue
		}
		if _, ok := gdbNullish(a.Value); !ok {
			continue
		}
		if a.Name == "this" {
			return &f.Args[i]
		}
		if other == nil {
			other = &f.Args[i]
		}
	}
	return other
}

// gdbCallerOf is the frame below this one — the code that made the call, which for a null pointer
// is where the bug actually is.
func gdbCallerOf(frames []miFrame, f *miFrame) *miFrame {
	if f == nil {
		return nil
	}
	for i := range frames {
		if frames[i].Level != f.Level {
			continue
		}
		for j := i + 1; j < len(frames); j++ {
			if frames[j].Func != "" && frames[j].Func != "??" && !gdbHandlerFrames[shortFuncName(frames[j].Func)] {
				return &frames[j]
			}
		}
	}
	return nil
}

// gdbCallerSentence names the caller in prose, or says nothing when there is none to name.
func gdbCallerSentence(caller *miFrame) string {
	if caller == nil {
		return ""
	}
	return fmt.Sprintf(" Start at %s%s, one frame down.", shortFuncName(caller.Func), locSuffix(*caller))
}
