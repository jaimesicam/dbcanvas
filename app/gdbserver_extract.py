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
import gdb, json, re, struct, time

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

# fld(p, name): the field `name` of the struct `p` points at, read at its offset. Subscripting a
# pointer to THD (`thd["m_thread_id"]`) costs ~0.75s *every time* on a release mysqld — gdb
# materialises the whole 14 KB polymorphic object for each access — which made a 15-session
# processlist take 85s. Resolving each field's offset once from the type (base classes included)
# and reading just that field is ~1ms. Offsets are cached per (type, field).
_OFFS = {}
def _find(t, name):
    t = t.strip_typedefs()
    for f in t.fields():
        if f.name == name and not f.is_base_class:
            return f.bitpos // 8, f.type
    for f in t.fields():
        if f.is_base_class:
            r = _find(f.type, name)
            if r: return f.bitpos // 8 + r[0], r[1]
    return None

def fld(p, name):
    t = p.type.strip_typedefs().target().strip_typedefs()
    key = (str(t), name)
    if key not in _OFFS:
        _OFFS[key] = _find(t, name)
    r = _OFFS[key]
    if r is None:
        raise gdb.error("no field %s in %s" % (name, key[0]))
    return gdb.Value(int(p) + r[0]).cast(r[1].pointer()).dereference()

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
def _core_fsbase():
    # {LWP: fs_base} straight from the core's NT_PRSTATUS notes (x86-64 only; {} otherwise). fs_base
    # is the thread pointer, and gdb would otherwise build a frame per thread to read it (~0.5s each).
    out = {}
    try:
        m = re.search(r"Local core dump file:\s*`([^']+)'", gdb.execute("info files", to_string=True))
        if not m: return out
        with open(m.group(1), "rb") as f:
            h = f.read(64)
            if h[:4] != b"\x7fELF" or h[4] != 2 or struct.unpack_from("<H", h, 0x12)[0] != 62:
                return out
            phoff, = struct.unpack_from("<Q", h, 0x20)
            phsz, phn = struct.unpack_from("<HH", h, 0x36)
            f.seek(phoff); ph = f.read(phsz * phn)
            for i in range(phn):
                if struct.unpack_from("<I", ph, i*phsz)[0] != 4:      # PT_NOTE
                    continue
                off, = struct.unpack_from("<Q", ph, i*phsz + 8)
                sz, = struct.unpack_from("<Q", ph, i*phsz + 32)
                f.seek(off); d = f.read(sz); p = 0
                while p + 12 <= len(d):
                    nn, dn, ty = struct.unpack_from("<III", d, p); p += 12
                    p += (nn + 3) & ~3; desc = d[p:p+dn]; p += (dn + 3) & ~3
                    if ty == 1 and dn >= 112 + 27*8:                   # NT_PRSTATUS
                        pid, = struct.unpack_from("<i", desc, 32)      # pr_pid
                        out[pid], = struct.unpack_from("<Q", desc, 112 + 21*8)   # pr_reg.fs_base
    except Exception:
        return {}
    return out

def thread_map():
    m = {}
    try:
        inf = gdb.selected_inferior()
        THDp = gdb.lookup_type("THD").pointer()
    except Exception:
        return m
    # Fast path. current_thd is static TLS in mysqld, so on every thread it sits at the same offset
    # from the thread pointer. gdb resolves a TLS variable through libthread_db at ~0.7s a thread
    # (60 threads: ~45s), so resolve it once on the selected thread, take its distance from that
    # thread's fs_base, and read every other thread's current_thd at fs_base + that distance.
    fsm = _core_fsbase()
    if fsm:
        try:
            cur = gdb.selected_thread()
            delta = as_int(gdb.parse_and_eval("&current_thd")) - fsm[cur.ptid[1]]
            for t in inf.threads():
                fs = fsm.get(t.ptid[1])
                if not fs: continue
                try:
                    p = struct.unpack("<Q", bytes(inf.read_memory(fs + delta, 8)))[0]
                    if not p: continue
                    tid = as_int(fld(gdb.Value(p).cast(THDp), "m_thread_id"))
                    if tid is not None and tid not in m:
                        m[tid] = t.num
                except Exception:
                    continue
            return m
        except Exception:
            m = {}
    # Slow path (not x86-64, or the core's notes unreadable): ask gdb per thread, bounded by a
    # wall-clock budget. It is only the click-through convenience; the processlist does not need it.
    saved = None
    try: saved = gdb.selected_thread()
    except Exception: pass
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
                tid = as_int(fld(thd, "m_thread_id"))
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
                tid = as_int(fld(thd, "m_thread_id"))
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
                try: tid = as_int(fld(thd, "m_thread_id"))
                except Exception: pass
                if tid is not None: r["threadId"] = tid
                try:
                    c = as_int(fld(thd, "m_command"))
                    r["command"] = COMMANDS.get(c, str(c))
                except Exception: pass
                r["db"] = lexcstr(fld(thd, "m_db"))
                r["query"] = lexcstr(fld(thd, "m_query_string"))
                try:
                    pi = fld(thd, "m_proc_info")
                    r["state"] = cstr(pi, 128) if as_int(pi) else ""
                except Exception: r["state"] = ""
                try: r["killed"] = as_int(atom_i(fld(thd, "killed"))) not in (0, None)
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
            row["id"] = as_int(fld(t, "id"))
            try: row["state"] = str(atom_i(fld(t, "state"))).replace("TRX_STATE_", "")
            except Exception: pass
            try: row["rowsModified"] = as_int(fld(t, "undo_no"))
            except Exception: pass
            tid = None
            try:
                thd = fld(t, "mysql_thd")
                if as_int(thd): tid = as_int(fld(thd, "m_thread_id"))
            except Exception: pass
            if tid is not None:
                row["threadId"] = tid
                g = TMAP.get(tid)
                if g is not None: row["gdbThread"] = g
            try:
                op = fld(t, "op_info"); row["op"] = cstr(op, 128) if as_int(op) else ""
            except Exception: pass
            try: row["waiting"] = as_int(atom_p(fld(t, "lock")["wait_lock"])) not in (0, None)
            except Exception: pass
            try:
                bt = atom_p(fld(t, "lock")["blocking_trx"])
                if as_int(bt): row["blockedByTrx"] = as_int(fld(bt, "id"))
            except Exception: pass
            trx.append(row)
            cur = fld(t, "trx_list")["next"]
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
    # Two passes. The ring is walked oldest-first from ring_buffer_read, and a crash wants the
    # *newest* lines — the first version kept the oldest MAXLOG and dropped exactly the ones before
    # the crash. So pass one only steps through the headers (one raw read of each event's length),
    # and pass two decodes the last MAXLOG of them.
    try:
        lo, lt = _find(ET, "m_message_length")
        lsz = lt.strip_typedefs().sizeof
    except Exception as ex:
        note("error log: %s" % ex); return rows
    inf = gdb.selected_inferior()
    addrs = []
    e = read
    for _ in range(min(nev, 200000) + 2):
        if not e: break
        try: mlen = int.from_bytes(bytes(inf.read_memory(e + lo, lsz)), "little")
        except Exception: break
        if mlen == 0 or mlen > 8192:
            if start and e != start: e = start; continue
            break
        addrs.append((e, mlen))
        if len(addrs) >= nev: break
        e = e + ((hdr + mlen + 1 + 7) & ~7)
        if end and e + hdr > end: e = start
    if len(addrs) > MAXLOG:
        note("error log: showing the newest %d of %d buffered events" % (MAXLOG, len(addrs)))
    for e, mlen in addrs[-MAXLOG:]:
        ep = gdb.Value(e).cast(ET.pointer())
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
