# Core Dump Analyzer — server-state extractor.
#
# Runs inside the gdb that already has the core open, driven by the DBCanvas server (NOT by the
# user console, whose `python` is blocked). It reads MySQL/Percona-Server/PXC server-internal
# state straight out of the core — the processlist, InnoDB transactions and their lock-wait graph,
# the binlog position, the error-log ring buffer, and (PXC) the wsrep cluster membership — and
# prints exactly one line: a sentinel followed by JSON. Everything is best-effort: a section that
# cannot be read records a note and the rest still comes back.
#
# The keys are camelCase to match the Go structs in gdbserver.go and the JS that renders them.
import gdb, json, time

# The live session runs with `set print pretty on` and print-object on (gdbStartScript), which make
# gdb resolve dynamic types (RTTI/vtables) when it touches a value — slow on a polymorphic type like
# THD, and unnecessary here, where every read is an explicit field. Turning them off for this pass
# is the difference between seconds and minutes. Restored at the end so the console a person uses
# afterwards is unchanged.
try: _pretty = gdb.parameter("print pretty")
except Exception: _pretty = None
try: _object = gdb.parameter("print object")
except Exception: _object = None
try: gdb.execute("set print pretty off", to_string=True)
except Exception: pass
try: gdb.execute("set print object off", to_string=True)
except Exception: pass

SENT = "<<CDA_SERVERSTATE>>"
product = "PRODUCT_PLACEHOLDER"   # "ps" | "pxc", substituted by the server
MAXSESS = 512
MAXTRX  = 256
MAXLOG  = 300

notes = []
def note(s):
    if s not in notes: notes.append(s)

def ev(e):
    return gdb.parse_and_eval(e)

def as_int(v):
    try: return int(v)
    except Exception: return None

def atom_i(v):          # std::atomic<integral/enum>
    try: return v["_M_i"]
    except Exception: return v

def atom_p(v):          # std::atomic<pointer>
    try: return v["_M_b"]["_M_p"]
    except Exception:
        try: return v["_M_i"]
        except Exception: return v

def cstr(v, maxlen=4096):
    # char* / char[] -> python str, null-terminated and defensively bounded. The length cap stops a
    # corrupt pointer from reading forever; the split stops a fixed-size buffer (m_proc_info points
    # into one) from dragging in whatever sits after the string's own NUL — the PSI stage names that
    # live right after it, in the first version of this.
    try:
        if v.type.code == gdb.TYPE_CODE_PTR and as_int(v) == 0:
            return ""
    except Exception:
        pass
    try:
        s = v.string(length=maxlen, errors="replace")
    except Exception:
        try: s = v.string(errors="replace")
        except Exception: return ""
    return s.split("\x00", 1)[0]

def lexcstr(v):         # LEX_CSTRING {str,length}
    try:
        p = v["str"]
        if as_int(p) == 0: return ""
        n = as_int(v["length"]) or 0
        if n <= 0: return ""
        return p.string(length=min(n, 1<<20), errors="replace")
    except Exception:
        return ""

CHARP = None
def charp():
    global CHARP
    if CHARP is None:
        CHARP = gdb.lookup_type("char").pointer()
    return CHARP

# enum_server_command names (the ones worth showing).
COMMANDS = {
    0:"Sleep",1:"Quit",2:"Init DB",3:"Query",4:"Field List",5:"Create DB",6:"Drop DB",
    7:"Refresh",8:"Shutdown",9:"Statistics",10:"Processlist",11:"Connect",12:"Kill",
    13:"Debug",14:"Ping",15:"Time",16:"Delayed insert",17:"Change user",18:"Binlog Dump",
    19:"Table Dump",20:"Connect Out",21:"Register Replica",22:"Prepare",23:"Execute",
    24:"Long Data",25:"Close stmt",26:"Reset stmt",27:"Set option",28:"Fetch",29:"Daemon",
    30:"Binlog Dump GTID",31:"Reset Connection",32:"Clone",
}

# thread_map: MySQL connection id (THD::m_thread_id) -> gdb thread number. The processlist is keyed
# by connection id; the stack panes are keyed by gdb's own thread number, and they are not the same
# number. Each OS thread that is *inside* the server carries a current_thd (thread-local), so this
# maps the two for every session that was actually running — which is exactly the set the page
# wants to be able to jump from. Idle (sleeping) connections have no current_thd and so no stack to
# jump to anyway.
def thread_map():
    m = {}
    try:
        inf = gdb.selected_inferior()
    except Exception:
        return m
    saved = None
    try: saved = gdb.selected_thread()
    except Exception: pass
    # Reading current_thd is a thread-local lookup, which gdb resolves per thread off the core's own
    # TLS — cheap each, but there can be hundreds of threads, so it is bounded by a wall-clock budget
    # rather than let a very wide server hold up the whole read. It is only the click-through
    # convenience; the processlist itself does not depend on it.
    budget = time.time() + 20.0
    try:
        for t in inf.threads():
            if time.time() > budget:
                note("thread→stack map stopped at the time budget; some sessions are not clickable")
                break
            try:
                t.switch()
                thd = gdb.parse_and_eval("current_thd")
                if as_int(thd) in (0, None):
                    continue
                tid = as_int(thd["m_thread_id"])
                if tid is not None and tid not in m:
                    m[tid] = t.num
            except Exception:
                continue
    except Exception as e:
        note("thread map: %s" % e)
    finally:
        try:
            if saved is not None: saved.switch()
        except Exception:
            pass
    return m

TMAP = {}

def meta():
    out = {"product": product}
    try: out["version"] = cstr(ev("server_version"), 64)
    except Exception as e: note("server_version: %s" % e)
    try:
        st = as_int(ev("server_start_time"))
        if st: out["startTime"] = st
    except Exception: pass
    return out

def sessions():
    rows = []
    try:
        mgr = ev("Global_THD_manager::thd_manager")
    except Exception as e:
        note("processlist: no Global_THD_manager (%s)" % e); return rows
    if as_int(mgr) == 0:
        note("processlist: thd_manager is null"); return rows
    THDpp = gdb.lookup_type("THD").pointer().pointer()
    seen = 0
    try:
        for p in range(8):
            arr = mgr["thd_list"][p]
            isz = as_int(arr["m_inline_size"])
            if isz is None: continue
            if isz >= 0:
                n = isz
                base = arr["m_buff"].address.cast(THDpp)
            else:
                n = as_int(arr["m_ext"]["m_alloced_size"]) or 0
                base = arr["m_ext"]["m_array_ptr"]
            for i in range(min(n, MAXSESS)):
                if seen >= MAXSESS: break
                thd = base[i]
                if as_int(thd) in (0, None): continue
                seen += 1
                r = {}
                tid = None
                try: tid = as_int(thd["m_thread_id"])
                except Exception: pass
                if tid is not None: r["threadId"] = tid
                try:
                    c = as_int(thd["m_command"])
                    r["command"] = COMMANDS.get(c, str(c))
                except Exception: pass
                r["db"] = lexcstr(thd["m_db"])
                r["query"] = lexcstr(thd["m_query_string"])
                try:
                    pi = thd["m_proc_info"]
                    r["state"] = cstr(pi, 128) if as_int(pi) else ""
                except Exception: r["state"] = ""
                try: r["killed"] = as_int(atom_i(thd["killed"])) not in (0, None)
                except Exception: pass
                g = TMAP.get(tid)
                if g is not None: r["gdbThread"] = g
                rows.append(r)
    except Exception as e:
        note("processlist: %s" % e)
    return rows

def innodb():
    out = {}
    def g(expr, key):
        try: out[key] = as_int(atom_i(ev(expr)))
        except Exception: pass
    g("trx_sys->rseg_history_len", "historyListLength")
    g("log_sys->flushed_to_disk_lsn", "flushedLsn")
    g("log_sys->last_checkpoint_lsn", "checkpointLsn")
    trx = []
    try:
        base = ev("trx_sys->rw_trx_list")
        cur = base["first_element"]
        n = 0
        while as_int(cur) not in (0, None) and n < MAXTRX:
            t = cur
            row = {}
            row["id"] = as_int(t["id"])
            try: row["state"] = str(atom_i(t["state"])).replace("TRX_STATE_", "")
            except Exception: pass
            try: row["rowsModified"] = as_int(t["undo_no"])
            except Exception: pass
            tid = None
            try:
                thd = t["mysql_thd"]
                if as_int(thd): tid = as_int(thd["m_thread_id"])
            except Exception: pass
            if tid is not None:
                row["threadId"] = tid
                g = TMAP.get(tid)
                if g is not None: row["gdbThread"] = g
            try:
                op = t["op_info"]; row["op"] = cstr(op, 128) if as_int(op) else ""
            except Exception: pass
            try: row["waiting"] = as_int(atom_p(t["lock"]["wait_lock"])) not in (0, None)
            except Exception: pass
            try:
                bt = atom_p(t["lock"]["blocking_trx"])
                if as_int(bt): row["blockedByTrx"] = as_int(bt["id"])
            except Exception: pass
            trx.append(row)
            cur = t["trx_list"]["next"]
            n += 1
    except Exception as e:
        note("innodb transactions: %s" % e)
    out["transactions"] = trx
    return out

def binlog():
    out = {}
    try:
        f = cstr(ev("mysql_bin_log.log_file_name"), 512)
    except Exception as e:
        note("binlog: %s" % e); return None
    if not f:
        return None
    out["file"] = f
    try: out["position"] = as_int(atom_i(ev("mysql_bin_log.atomic_binlog_end_pos")))
    except Exception: pass
    return out

def errorlog():
    rows = []
    try:
        read = as_int(ev("ring_buffer_read"))
        nev = as_int(ev("log_sink_pfs_buffered_events")) or 0
    except Exception as e:
        note("error log: %s" % e); return rows
    if not read or nev <= 0:
        return rows
    try:
        ET = gdb.lookup_type("log_sink_pfs_event"); hdr = ET.sizeof
    except Exception as e:
        note("error log: no log_sink_pfs_event type (%s)" % e); return rows
    start = None
    try: start = as_int(ev("ring_buffer_start"))
    except Exception: pass
    end = (start + 5*1024*1024) if start else None
    e = read
    for _ in range(min(nev, MAXLOG) + 2):
        if not e: break
        ep = gdb.Value(e).cast(ET.pointer())
        try: mlen = as_int(ep["m_message_length"])
        except Exception: break
        if mlen in (0, None) or mlen > 8192:
            if start and e != start: e = start; continue
            break
        r = {}
        try: r["ts"] = as_int(ep["m_timestamp"])
        except Exception: pass
        try: r["prio"] = as_int(ep["m_prio"])
        except Exception: pass
        try: r["code"] = ep["m_error_code"].string(length=as_int(ep["m_error_code_length"]) or 0)
        except Exception: r["code"] = ""
        try: r["subsys"] = ep["m_subsys"].string(length=as_int(ep["m_subsys_length"]) or 0)
        except Exception: r["subsys"] = ""
        try: r["message"] = gdb.Value(e + hdr).cast(charp()).string(length=mlen, errors="replace")
        except Exception: r["message"] = ""
        rows.append(r)
        if len(rows) >= MAXLOG: break
        e = e + ((hdr + mlen + 1 + 7) & ~7)
        if end and e + hdr > end: e = start
    return rows

def wsrep():
    if product != "pxc":
        return None
    out = {}
    def g(expr, key, string=False):
        try:
            v = ev(expr)
            out[key] = cstr(v, 512) if string else as_int(v)
        except Exception: pass
    try: out["ready"] = as_int(ev("wsrep_ready")) not in (0, None)
    except Exception: pass
    g("wsrep_cluster_status", "clusterStatus", True)
    g("wsrep_cluster_size", "clusterSize")
    g("wsrep_cluster_name", "clusterName", True)
    g("wsrep_node_name", "nodeName", True)
    g("wsrep_node_address", "nodeAddress", True)
    g("wsrep_provider", "provider", True)
    g("wsrep_cluster_address", "clusterAddress", True)
    g("wsrep_sst_method", "sstMethod", True)
    return out

TMAP = thread_map()
result = {
    "meta": meta(),
    "sessions": sessions(),
    "innodb": innodb(),
    "binlog": binlog(),
    "errorLog": errorlog(),
    "wsrep": wsrep(),
    "notes": notes,
}
if _pretty is not None:
    try: gdb.execute("set print pretty %s" % ("on" if _pretty else "off"), to_string=True)
    except Exception: pass
if _object is not None:
    try: gdb.execute("set print object %s" % ("on" if _object else "off"), to_string=True)
    except Exception: pass
print(SENT + json.dumps(result, default=str))
