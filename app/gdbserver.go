package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// gdbserver.go — reading the server, not just the stack.
//
// A backtrace says where mysqld was when it died. It does not say what the server was *doing*: the
// sixty sessions and the queries in their hands, the transaction that held the row lock everything
// else was queued behind, how far the binlog had been written, the last thing the error log
// recorded before the signal, or — on a PXC node — whether this member still thought it was in the
// Primary component. All of that is in the core too; none of it is in a stack.
//
// The server already knows how to read each of these at runtime: SHOW PROCESSLIST walks
// Global_THD_manager, SHOW ENGINE INNODB STATUS walks trx_sys, performance_schema.error_log reads
// the log ring-buffer. On a core there is no running server to ask, but the same structures are
// sitting in the dump at the same offsets, and gdb with the matching debug symbols can read them.
// gdbserver_extract.py does exactly that walk and prints the result as one line of JSON.
//
// **Why a Python script rather than a hundred MI expressions.** Each distinct C++ type gdb touches
// on a server binary costs a one-time DWARF load measured in tens of seconds; the error-log walk
// alone is hundreds of events. Done as individual -data-evaluate-expression round trips that is
// both slow and chatty. The script touches each type once, in one gdb invocation, and hands back a
// structured answer to parse once.
//
// **Why that does not reopen the python hole the console closes.** The user console refuses
// `python` (gdbUnsafeCommand) because a *person* typing python into a debugger pointed at a mounted
// directory is arbitrary code execution. This script is neither typed by anyone nor loaded from the
// mount: it is embedded in the DBCanvas binary, substituted only with the product string, and run
// through the MI client directly (cli.console), not through the gated session console. The gate on
// user input is unchanged.
//
//go:embed gdbserver_extract.py
var gdbExtractScript string

// gdbServerState is the server's own view of itself at the moment of the crash, read from the core.
// Every field is best-effort: a section the symbols or the build did not permit is simply absent,
// and Notes carries why.
type gdbServerState struct {
	Meta     gdbSrvMeta      `json:"meta"`
	Sessions []gdbSrvSession `json:"sessions"`
	InnoDB   *gdbSrvInnoDB   `json:"innodb,omitempty"`
	Binlog   *gdbSrvBinlog   `json:"binlog,omitempty"`
	ErrorLog []gdbSrvLogEvt  `json:"errorLog,omitempty"`
	Wsrep    *gdbSrvWsrep    `json:"wsrep,omitempty"`
	Notes    []string        `json:"notes,omitempty"`
}

type gdbSrvMeta struct {
	Product   string `json:"product"`
	Version   string `json:"version,omitempty"`
	StartTime int64  `json:"startTime,omitempty"` // unix seconds; shown as when the server started
}

// gdbSrvSession is one row of the processlist, reconstructed from a THD in the core.
type gdbSrvSession struct {
	ThreadID int64  `json:"threadId,omitempty"`
	Command  string `json:"command,omitempty"`
	DB       string `json:"db,omitempty"`
	State    string `json:"state,omitempty"`
	Query    string `json:"query,omitempty"`
	Killed   bool   `json:"killed,omitempty"`
	// Account, read from the THD's security context — who this connection is. User and Host are the
	// authenticated account (user@host), IP the peer address; together they are the "who" the flat
	// processlist never had.
	User string `json:"user,omitempty"`
	Host string `json:"host,omitempty"`
	IP   string `json:"ip,omitempty"`
	// StartTime is when this connection's current command began (THD::start_time, unix seconds), so
	// the page can show how long it had been running at the crash.
	StartTime int64 `json:"startTime,omitempty"`
	QueryID   int64 `json:"queryId,omitempty"`
	// The session's diagnostics area — the last error it had raised, if any. On a core this is often
	// the clue: the connection that was mid-error when everything stopped.
	LastErrno int    `json:"lastErrno,omitempty"`
	LastError string `json:"lastError,omitempty"`
	SQLState  string `json:"sqlState,omitempty"`
	InTrans   bool   `json:"inTrans,omitempty"`
	// GdbThread is this session's gdb thread number, present only when the connection was actually
	// running in the server (and so carried a current_thd to map it by) — which is what lets the
	// page open the session's own stack. Idle connections have none and no stack to open.
	GdbThread int `json:"gdbThread,omitempty"`
}

type gdbSrvInnoDB struct {
	HistoryListLength *int64      `json:"historyListLength,omitempty"`
	FlushedLSN        *int64      `json:"flushedLsn,omitempty"`
	CheckpointLSN     *int64      `json:"checkpointLsn,omitempty"`
	Transactions      []gdbSrvTrx `json:"transactions,omitempty"`
}

// gdbSrvTrx is one active InnoDB transaction. BlockedByTrx and Waiting are what let the page draw
// the lock-wait graph — who is stuck, and on whom — which is the whole point of reading trx_sys.
type gdbSrvTrx struct {
	ID           int64  `json:"id,omitempty"`
	State        string `json:"state,omitempty"`
	RowsModified int64  `json:"rowsModified,omitempty"`
	ThreadID     int64  `json:"threadId,omitempty"`
	Op           string `json:"op,omitempty"`
	Waiting      bool   `json:"waiting,omitempty"`
	BlockedByTrx *int64 `json:"blockedByTrx,omitempty"`
	GdbThread    int    `json:"gdbThread,omitempty"`
}

type gdbSrvBinlog struct {
	File     string `json:"file,omitempty"`
	Position *int64 `json:"position,omitempty"`
}

// gdbSrvLogEvt is one entry of the error-log ring buffer (= performance_schema.error_log).
type gdbSrvLogEvt struct {
	TS      int64  `json:"ts,omitempty"` // microseconds since epoch
	Prio    int    `json:"prio"`         // 0 System, 1 Error, 2 Warning, 3 Note/Info
	Code    string `json:"code,omitempty"`
	Subsys  string `json:"subsys,omitempty"`
	Message string `json:"message,omitempty"`
}

// gdbSrvWsrep is the PXC member's cluster-membership view: present only for product=="pxc", and the
// one thing a plain-MySQL core never has. cluster_status "non-Primary" on a crashed node is the
// difference between "it hit a bug" and "it had already been partitioned off".
type gdbSrvWsrep struct {
	Ready          *bool  `json:"ready,omitempty"`
	ClusterStatus  string `json:"clusterStatus,omitempty"`
	ClusterSize    *int64 `json:"clusterSize,omitempty"`
	ClusterName    string `json:"clusterName,omitempty"`
	NodeName       string `json:"nodeName,omitempty"`
	NodeAddress    string `json:"nodeAddress,omitempty"`
	Provider       string `json:"provider,omitempty"`
	ClusterAddress string `json:"clusterAddress,omitempty"`
	SSTMethod      string `json:"sstMethod,omitempty"`
}

const gdbServerStateSentinel = "<<CDA_SERVERSTATE>>"

// serverState runs the extractor against the open core and parses its one line of JSON.
//
// It is cached on the session: the walk is seconds of work and the answer does not change for a
// dead process, so a second viewer, or the same viewer reopening the panel, pays nothing. Opening
// a different core clears it (open() resets srvState), the same as the verdict.
//
// Only one read runs at a time: a page that asks again while the first is still going (it changed
// tabs and came back, or a second viewer opened the panel) waits for that read instead of queueing
// another minute of gdb work behind it.
func (s *gdbSession) serverState(ctx context.Context) (*gdbServerState, error) {
	var (
		done    chan struct{}
		product string
		cli     *miClient
	)
	for done == nil {
		s.mu.Lock()
		if s.srvState != nil {
			st := s.srvState
			s.mu.Unlock()
			return st, nil
		}
		if w := s.srvBusy; w != nil {
			s.mu.Unlock()
			select {
			case <-w:
				continue // finished (or failed): take its answer, or run it ourselves
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done = make(chan struct{})
		s.srvBusy = done
		product, cli = s.tgt.Product, s.cli
		s.mu.Unlock()
	}
	defer func() {
		s.mu.Lock()
		if s.srvBusy == done {
			s.srvBusy = nil
		}
		s.mu.Unlock()
		close(done)
	}()
	if product == "" {
		product = "ps"
	}

	// The script goes onto the node as a file and is `source`d, rather than sent inline. gdb runs
	// behind a PTY (see gdbStartScript's stty), and a PTY in canonical mode will not deliver a
	// single input line past MAX_CANON — a few KB — so a `-interpreter-exec console "python …"`
	// carrying the base64'd script (~20 KB) arrives truncated and gdb answers "Problem parsing
	// arguments". Writing the file is one short Exec and `source` is one short line.
	if err := s.writeExtractor(ctx, product); err != nil {
		return nil, err
	}
	var raw string
	err := s.withClient(ctx, func(ctx context.Context, cli *miClient) error {
		var err error
		raw, err = cli.console(ctx, "source "+gdbExtractPath)
		return err
	})
	if err != nil {
		return nil, err
	}
	st, err := gdbParseServerState(raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	// A core opened while this was reading is a different process; its answer is not this one.
	if s.cli == cli {
		s.srvState = st
	}
	s.mu.Unlock()
	s.logf("info", "read server state: %d session(s), %d transaction(s), %d log event(s)",
		len(st.Sessions), gdbTrxCount(st), len(st.ErrorLog))
	return st, nil
}

func gdbTrxCount(st *gdbServerState) int {
	if st.InnoDB == nil {
		return 0
	}
	return len(st.InnoDB.Transactions)
}

// gdbParseServerState pulls the sentinel-prefixed JSON line out of the console output and decodes
// it. The sentinel exists because gdb's console carries library-load chatter and the extractor's
// own notes on the same stream; the one line that starts with it is the payload.
func gdbParseServerState(out string) (*gdbServerState, error) {
	for _, ln := range strings.Split(out, "\n") {
		i := strings.Index(ln, gdbServerStateSentinel)
		if i < 0 {
			continue
		}
		payload := strings.TrimSpace(ln[i+len(gdbServerStateSentinel):])
		var st gdbServerState
		if err := json.Unmarshal([]byte(payload), &st); err != nil {
			return nil, fmt.Errorf("decode server state: %w", err)
		}
		return &st, nil
	}
	return nil, fmt.Errorf("the extractor produced no server-state line — the symbols may not cover this build")
}

// gdbExtractPath is where the extractor is written on the node before it is `source`d. It is under
// /var/tmp (not /tmp, which systemd-tmpfiles may sweep out from under a long read), overwritten each
// read, and holds only the product-substituted copy of code already in the binary — nothing secret,
// nothing a reader on the node could not already see with `info functions`.
const gdbExtractPath = "/var/tmp/.cda-serverstate.py"

// writeExtractor substitutes the product into the embedded script and writes it onto the node.
//
// The write goes through one Exec that base64-decodes the script into the file, so the script's own
// quotes and newlines never have to survive a shell argument — only the base64, which is
// [A-Za-z0-9+/=]. That keeps the write robust regardless of what the script contains, the same
// reason the inline attempt used base64, without the PTY line-length limit `source` sidesteps.
func (s *gdbSession) writeExtractor(ctx context.Context, product string) error {
	script := strings.ReplaceAll(gdbExtractScript, "PRODUCT_PLACEHOLDER", gdbSanitizeProduct(product))
	enc := base64.StdEncoding.EncodeToString([]byte(script))
	res, err := s.a.engCtx(ctx).Exec(ctx, s.tgt.ContainerID,
		[]string{"sh", "-c", "printf %s " + enc + " | base64 -d > " + gdbExtractPath}, nil)
	if err != nil {
		return fmt.Errorf("stage the extractor on the node: %w", err)
	}
	if res.Code != 0 {
		return fmt.Errorf("stage the extractor on the node: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// gdbSanitizeProduct keeps the substituted product to the known set, so the placeholder can never
// carry anything but "ps" or "pxc" into the script.
func gdbSanitizeProduct(p string) string {
	if p == "pxc" {
		return "pxc"
	}
	return "ps"
}
