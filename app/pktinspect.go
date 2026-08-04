package main

// pktinspect.go — the Packet Inspector's HTTP surface and capture registry.
//
// A capture is: pick a running MySQL node, run tcpdump on it for a few seconds
// (pktcap.go), pull the pcap back, decode it (pktdecode.go / pktmysql.go), and keep
// the result in memory for the browser to page through.
//
// Captures live in memory rather than in SQLite, like Query Runner's and
// Benchmark's runs: a decoded capture is large, worthless once the stack is gone,
// and nothing here needs to survive a restart. The raw bytes are kept alongside the
// decode so the detail panel can show a real hex dump and the file can be
// downloaded into Wireshark.
//
// Every list endpoint is range-addressable — by packet number, by time, by stream,
// by protocol, and by "only rows with issues". The browser holds no more than a
// window of a capture that may be hundreds of thousands of packets, and the
// timeline asks for bucket counts over exactly the range it is drawing.

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- registry

// pktCapture is one capture and its decode.
type pktCapture struct {
	ID       string `json:"id"`
	StackID  int64  `json:"stackId"`
	Stack    string `json:"stackName"`
	NodeID   string `json:"nodeId"`
	Label    string `json:"label"`
	Engine   string `json:"engine"`
	Port     int    `json:"port"`
	Iface    string `json:"iface"`
	Command  string `json:"command"` // the tcpdump line that ran, shown in the UI
	Filter   string `json:"filter"`
	Source   string `json:"source"`   // node | upload
	NodeType string `json:"nodeType"` // pxc, mysql, mariadbgalera, an AiO kind…
	// Ports maps every captured port to the protocol it carries, so the UI can say
	// what a Galera/GCS row is and the decoder knows not to read it as MySQL.
	Ports map[int]string `json:"ports,omitempty"`

	State  string `json:"state"` // capturing | decoding | ready | error | stopped
	Error  string `json:"error,omitempty"`
	Start  string `json:"start"`
	End    string `json:"end,omitempty"`
	Wanted int    `json:"wantedSeconds"`

	// Live progress while tcpdump runs.
	Bytes         int64 `json:"bytes"`
	NodePackets   int   `json:"nodePackets"`
	KernelDropped int   `json:"kernelDropped"`
	// SizeCapped is set when the capture was stopped early because the file reached
	// the byte ceiling — the decode is still valid, it just does not cover the whole
	// requested duration.
	SizeCapped bool `json:"sizeCapped"`

	// A server error log uploaded alongside the pcap. The records a capture cannot
	// contain (aborted connections, DNS, TLS, listener) live in the server's own log,
	// and an uploaded capture has no node to read one from — so it can be carried with
	// the pcap instead. See pktserverlog.go.
	LogFile  string `json:"logFile,omitempty"`
	LogCount int    `json:"logCount,omitempty"`

	// Decode results.
	Summary pktSummary `json:"summary"`

	owner      int64
	raw        []byte
	logEntries []pktLogEntry
	decoded    *pktDecoded
	cancel     context.CancelFunc
	// A POINTER mutex, so public() can hand back a copy of this struct without
	// copying a lock (go vet rejects that, rightly: two copies of a mutex protect
	// nothing). Everything unexported here is skipped by encoding/json.
	mu *sync.Mutex
}

// pktSummary is the headline the UI shows above the timeline.
type pktSummary struct {
	Packets   int            `json:"packets"`
	Streams   int            `json:"streams"`
	Bytes     int            `json:"bytes"`
	FirstTS   float64        `json:"firstTs"`
	LastTS    float64        `json:"lastTs"`
	Protos    map[string]int `json:"protos"`
	IssueTop  []pktIssueStat `json:"issueTop"`
	Queries   int            `json:"queries"`
	Errors    int            `json:"errors"`
	TLSStream int            `json:"tlsStreams"`
	Dropped   int            `json:"dropped"`
	Truncated int            `json:"truncated"`
	Format    string         `json:"format"`
	LinkType  int            `json:"linkType"`
}

// pktIssueStat is one issue kind and how often it appeared, so the summary can say
// "17 retransmissions" without the browser walking every packet.
type pktIssueStat struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

var pktCaptures = struct {
	sync.Mutex
	m     map[string]*pktCapture
	order []string
}{m: map[string]*pktCapture{}}

// pktKeep is how many captures are retained per app process. Each one holds its raw
// bytes, so this is a memory budget as much as a history length.
const pktKeep = 12

func pktRemember(c *pktCapture) {
	pktCaptures.Lock()
	defer pktCaptures.Unlock()
	pktCaptures.m[c.ID] = c
	pktCaptures.order = append(pktCaptures.order, c.ID)
	for len(pktCaptures.order) > pktKeep {
		old := pktCaptures.order[0]
		pktCaptures.order = pktCaptures.order[1:]
		delete(pktCaptures.m, old)
	}
}

func pktGet(id string) *pktCapture {
	pktCaptures.Lock()
	defer pktCaptures.Unlock()
	return pktCaptures.m[id]
}

// pktList returns the caller's captures, newest first.
func pktList(u User) []*pktCapture {
	pktCaptures.Lock()
	defer pktCaptures.Unlock()
	out := []*pktCapture{}
	for i := len(pktCaptures.order) - 1; i >= 0; i-- {
		c := pktCaptures.m[pktCaptures.order[i]]
		if c == nil {
			continue
		}
		if c.owner == u.ID || u.Role == RoleAdmin {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------- targets

// handlePktTargets lists the nodes a capture can be taken on: the MySQL family and
// the PostgreSQL family, which are the two protocols the decoder speaks. A MongoDB
// node is deliberately absent — a capture of one would decode as "TCP data", which
// is worse than not offering it.
//
// The same list logic serves the Query Runner, so an All-in-One instance shows up
// with its own slot port rather than 3306 or 5432.
func (a *App) handlePktTargets(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	out := []qrTarget{}
	for _, t := range a.listSQLTargets(u) {
		if t.Engine == pktEngineMySQL || t.Engine == pktEnginePostgres {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// pktTargetRoles is every port a capture of this target should cover, mapped to the
// protocol it carries.
//
// A clustered node is the case that makes this necessary, and the two engines put
// their cluster traffic in different places. A PXC member's is on Galera's 4567
// (group communication), 4568 (IST) and 4444 (SST), none of which is the MySQL
// protocol, and a capture of 3306 alone shows none of the replication that makes PXC
// interesting. A Patroni member's is on 8008 (Patroni's own REST API) and 2379/2380
// (etcd, where the leader lock lives) — while its PostgreSQL replication is on 5432
// with everything else, because a walsender connection is just a connection.
//
// An All-in-One instance uses its slot's ports instead of the defaults — several
// instances share one container, so 4567 or 8008 can only belong to one of them (see
// aioPortsFor).
func (a *App) pktTargetRoles(u User, stackID int64, target string, port int, engine string) (map[int]string, string) {
	baseRole := pktRoleMySQL
	if engine == pktEnginePostgres {
		baseRole = pktRolePostgres
	}
	roles := map[int]string{port: baseRole}
	nodeID, inst := aioSplitTarget(target)
	st, err := a.store.GetStack(stackID)
	if err != nil {
		return roles, ""
	}
	if inst != "" {
		dep, err := a.store.GetDeployment(stackID, nodeID)
		if err != nil {
			return roles, ""
		}
		m, ok := aioFindInstance(dep, inst)
		if !ok {
			return roles, ""
		}
		switch {
		case aioMySQLShape(m.Kind) == shapeGalera:
			// The instance's own slot: group/IST/SST are fixed offsets inside it.
			roles[m.Ports.Group] = pktRoleGaleraGCS
			roles[m.Ports.IST] = pktRoleGaleraIST
			roles[m.Ports.SST] = pktRoleGaleraSST
		case m.Kind == "patroni":
			// Likewise for a Patroni instance: REST and both etcd ports come from
			// the slot, never from the well-known numbers.
			roles[m.Ports.REST] = pktRolePatroniREST
			roles[m.Ports.EtcdCli] = pktRoleEtcdClient
			roles[m.Ports.EtcdPr] = pktRoleEtcdPeer
		}
		return roles, m.Kind
	}
	for _, n := range buildDoc(st).Nodes {
		if n.ID != nodeID {
			continue
		}
		switch n.Type {
		case "pxc", "mariadbgalera":
			// PXC and MariaDB Galera both speak wsrep on the standard ports.
			return pktGaleraPortRoles(port), n.Type
		case "patroni":
			return pktPGPortRoles(port), n.Type
		case "pg", "repmgr", "spock":
			// No sidecar protocols: repmgr's daemon and Spock's apply workers are
			// PostgreSQL connections like any other, and both are on 5432.
			return roles, n.Type
		}
		return roles, n.Type
	}
	return roles, ""
}

// ---------------------------------------------------------------- start / stop

// pktStartBody is the capture request from the browser.
type pktStartBody struct {
	StackID  int64  `json:"stackId"`
	NodeID   string `json:"nodeId"`
	Seconds  int    `json:"seconds"`
	Packets  int    `json:"packets"`
	Snaplen  int    `json:"snaplen"`
	Filter   string `json:"filter"`
	Iface    string `json:"iface"`
	AllPorts bool   `json:"allPorts"`
}

func (a *App) handlePktStart(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body pktStartBody
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	engine, containerID, label, _, _, port, err := a.resolveNodeCredsPort(u, body.StackID, body.NodeID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if engine != pktEngineMySQL && engine != pktEnginePostgres {
		writeErr(w, http.StatusBadRequest,
			"the Packet Inspector decodes MySQL and PostgreSQL traffic; pick a MySQL/PXC/MariaDB or PostgreSQL/Patroni/repmgr/Spock target")
		return
	}
	if err := pktValidateFilter(body.Filter); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	roles, nodeType := a.pktTargetRoles(u, body.StackID, body.NodeID, port, engine)
	req := pktCapRequest{
		Engine:  engine,
		Seconds: pktClamp(body.Seconds, 1, pktMaxSeconds, 20),
		Packets: pktClamp(body.Packets, 100, pktMaxCapPackets, 50000),
		Snaplen: pktClamp(body.Snaplen, 64, 262144, 65535),
		Port:    port,
		Roles:   roles,
		Filter:  body.Filter,
		Iface:   strings.TrimSpace(body.Iface),
	}
	// "All ports" keeps the user's own filter but drops the port term, for chasing
	// something that is not on the database port at all (a health check, DNS).
	req.NoFilter = body.AllPorts

	st, _ := a.store.GetStack(body.StackID)
	id := qrNewID()
	c := &pktCapture{
		ID: id, StackID: body.StackID, Stack: st.Name, NodeID: body.NodeID, Label: label,
		Engine: engine, Port: port, Filter: pktBPF(req), Source: "node",
		NodeType: nodeType, Ports: roles,
		State: "capturing", Start: time.Now().UTC().Format(time.RFC3339), Wanted: req.Seconds,
		owner: u.ID, mu: &sync.Mutex{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.Seconds+120)*time.Second)
	c.cancel = cancel
	cmdLine, iface, err := a.pktStartCapture(ctx, containerID, id, req)
	if err != nil {
		cancel()
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	c.Command, c.Iface = cmdLine, iface
	pktRemember(c)

	// The capture runs on the node; this goroutine watches it, then reads the file
	// back and decodes it. The browser polls the capture until it is ready.
	go a.pktRunCapture(ctx, cancel, c, containerID, req)

	writeJSON(w, http.StatusAccepted, c.public())
}

// pktRunCapture polls the node until tcpdump finishes, then fetches and decodes.
func (a *App) pktRunCapture(ctx context.Context, cancel context.CancelFunc, c *pktCapture, containerID string, req pktCapRequest) {
	defer cancel()
	deadline := time.Now().Add(time.Duration(req.Seconds)*time.Second + 30*time.Second)
	for {
		select {
		case <-ctx.Done():
			// A stop request cancels the context; the capture is still on the node
			// and worth decoding, so fall through to the fetch below.
		case <-time.After(time.Second):
		}
		stat, err := a.pktPollCapture(context.WithoutCancel(ctx), containerID, c.ID)
		if err == nil {
			c.mu.Lock()
			c.Bytes, c.NodePackets, c.KernelDropped = stat.Bytes, stat.Packets, stat.KernelDropped
			c.mu.Unlock()
		}
		// A capture may now run for an hour, which on a busy node can reach a file
		// size that cannot be read back at all. Stop at the byte ceiling and keep
		// what was captured, rather than letting it run to something unusable.
		if err == nil && stat.Bytes >= pktMaxCapBytes {
			a.pktStopCapture(context.WithoutCancel(ctx), containerID, c.ID)
			c.mu.Lock()
			c.SizeCapped = true
			c.mu.Unlock()
			break
		}
		if (err == nil && !stat.Running) || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
	}

	// Read and decode on a context of their own: the capture's own deadline may
	// have just expired, and the file still has to come back.
	fetchCtx, fcancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer fcancel()

	c.mu.Lock()
	if c.State == "capturing" {
		c.State = "decoding"
	}
	c.mu.Unlock()

	// tcpdump may still be flushing its last buffer.
	a.pktStopCapture(fetchCtx, containerID, c.ID)
	buf, err := a.pktFetchCapture(fetchCtx, containerID, c.ID)
	if err != nil {
		c.fail(err)
		return
	}
	c.finish(buf, req.Port, req.Roles, req.Engine)
}

// fail records a capture-level failure.
func (c *pktCapture) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.State, c.Error = "error", err.Error()
	c.End = time.Now().UTC().Format(time.RFC3339)
}

// finish decodes the capture bytes and marks it ready.
func (c *pktCapture) finish(buf []byte, serverPort int, roles map[int]string, engine string) {
	d, err := pktDecode(buf, pktDecodeOpts{ServerPort: serverPort, PortRoles: roles, Engine: engine})
	if err != nil {
		c.fail(err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raw, c.decoded = buf, d
	c.Summary = pktSummarize(d)
	// An upload may not have said which protocol it holds; the decoder worked it out
	// from the bytes, and that answer is what the UI should show.
	c.Engine = d.Engine
	c.State = "ready"
	c.End = time.Now().UTC().Format(time.RFC3339)
}

// public is a copy safe to serialise (no mutex, no payload bytes).
func (c *pktCapture) public() pktCapture {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *c
	cp.raw, cp.decoded, cp.cancel = nil, nil, nil
	return cp
}

// pktSummarize builds the headline stats for a decoded capture.
func pktSummarize(d *pktDecoded) pktSummary {
	s := pktSummary{
		Packets: len(d.Packets), Streams: len(d.Streams), Protos: map[string]int{},
		Dropped: d.Dropped, Truncated: d.Truncat, Format: d.Format, LinkType: d.LinkType,
	}
	counts := map[string]int{}
	for i, p := range d.Packets {
		s.Bytes += p.FrameLen
		s.Protos[p.Proto]++
		if i == 0 {
			s.FirstTS = p.TSUnix
		}
		s.LastTS = p.TSUnix
		for _, is := range p.Issues {
			counts[pktIssueKind(is)]++
		}
	}
	for _, st := range d.Streams {
		s.Queries += st.Queries
		s.Errors += st.Errors
		if st.TLS {
			s.TLSStream++
		}
	}
	for k, n := range counts {
		s.IssueTop = append(s.IssueTop, pktIssueStat{Kind: k, Count: n})
	}
	sort.Slice(s.IssueTop, func(i, j int) bool {
		if s.IssueTop[i].Count != s.IssueTop[j].Count {
			return s.IssueTop[i].Count > s.IssueTop[j].Count
		}
		return s.IssueTop[i].Kind < s.IssueTop[j].Kind
	})
	return s
}

// pktIssueKind reduces an issue message to its kind, so "TCP gap — 1448 bytes
// missing" and "TCP gap — 2896 bytes missing" count as one thing in the summary and
// as one entry in the UI's filter.
func pktIssueKind(s string) string {
	if i := strings.Index(s, " — "); i > 0 {
		return s[:i]
	}
	if i := strings.Index(s, ":"); i > 0 {
		return s[:i]
	}
	if i := strings.Index(s, " ("); i > 0 {
		return s[:i]
	}
	return s
}

func (a *App) handlePktStop(w http.ResponseWriter, r *http.Request) {
	c, ok := a.pktOwned(w, r)
	if !ok {
		return
	}
	c.mu.Lock()
	if c.State == "capturing" {
		c.State = "decoding"
	}
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel() // pktRunCapture notices, stops tcpdump, and decodes what there is
	}
	writeJSON(w, http.StatusOK, c.public())
}

// ---------------------------------------------------------------- list / detail

func (a *App) handlePktList(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	out := []pktCapture{}
	for _, c := range pktList(u) {
		out = append(out, c.public())
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handlePktGet(w http.ResponseWriter, r *http.Request) {
	c, ok := a.pktOwned(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, c.public())
}

// pktOwned resolves the capture in the URL and checks the caller may see it.
func (a *App) pktOwned(w http.ResponseWriter, r *http.Request) (*pktCapture, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	c := pktGet(r.PathValue("id"))
	if c == nil {
		writeErr(w, http.StatusNotFound, "capture not found (captures are kept in memory and do not survive an app restart)")
		return nil, false
	}
	if c.owner != u.ID && u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "not your capture")
		return nil, false
	}
	return c, true
}

// pktQuery is the range/filter a list or timeline request asks for. Every field is
// optional; together they are what makes the timeline configurable rather than a
// fixed window.
type pktQuery struct {
	FromNo  int     // packet number, inclusive (1-based)
	ToNo    int     // packet number, inclusive
	FromTS  float64 // epoch seconds
	ToTS    float64
	Stream  int    // -1 = all
	Proto   string // "" = all
	Issue   string // issue kind, or "any"
	Dir     string // c2s | s2c
	Search  string // substring of Info/Query, case-insensitive
	Limit   int
	Offset  int
	Buckets int
	// Around asks for the page containing the packet nearest this timestamp, instead
	// of a page at a fixed offset. It is what lets a click on a server-log record jump
	// the packet list to that moment WITHOUT disturbing the range or the filters the
	// user set — only the paging moves.
	Around float64
}

func pktParseQuery(r *http.Request) pktQuery {
	q := r.URL.Query()
	out := pktQuery{
		FromNo: atoiDef(q.Get("fromNo"), 0), ToNo: atoiDef(q.Get("toNo"), 0),
		Stream: atoiDef(q.Get("stream"), -1),
		Proto:  strings.TrimSpace(q.Get("proto")),
		Issue:  strings.TrimSpace(q.Get("issue")),
		Dir:    strings.TrimSpace(q.Get("dir")),
		Search: strings.ToLower(strings.TrimSpace(q.Get("q"))),
		Limit:  pktClamp(atoiDef(q.Get("limit"), 0), 1, 5000, 500),
		Offset: atoiDef(q.Get("offset"), 0),
	}
	// As few as two buckets is a legitimate ask when zoomed into a handful of
	// packets; the ceiling is what a strip a few hundred pixels wide can show.
	out.Buckets = pktClamp(atoiDef(q.Get("buckets"), 0), 2, 2000, 200)
	out.FromTS, _ = strconv.ParseFloat(q.Get("fromTs"), 64)
	out.ToTS, _ = strconv.ParseFloat(q.Get("toTs"), 64)
	out.Around, _ = strconv.ParseFloat(q.Get("around"), 64)
	return out
}

// match reports whether a packet belongs in the requested range.
func (q pktQuery) match(p *pktPacket) bool {
	if q.FromNo > 0 && p.No < q.FromNo {
		return false
	}
	if q.ToNo > 0 && p.No > q.ToNo {
		return false
	}
	if q.FromTS > 0 && p.TSUnix < q.FromTS {
		return false
	}
	if q.ToTS > 0 && p.TSUnix > q.ToTS {
		return false
	}
	if q.Stream >= 0 && p.Stream != q.Stream {
		return false
	}
	if q.Proto != "" && !strings.EqualFold(q.Proto, p.Proto) {
		return false
	}
	if q.Dir != "" && q.Dir != p.Dir {
		return false
	}
	switch {
	case q.Issue == "any":
		if len(p.Issues) == 0 {
			return false
		}
	case q.Issue != "":
		found := false
		for _, is := range p.Issues {
			if pktIssueKind(is) == q.Issue {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if q.Search != "" &&
		!strings.Contains(strings.ToLower(p.Info), q.Search) &&
		!strings.Contains(strings.ToLower(p.Query), q.Search) &&
		!strings.Contains(strings.ToLower(p.Src), q.Search) &&
		!strings.Contains(strings.ToLower(p.Dst), q.Search) {
		return false
	}
	return true
}

// pktAroundPage finds the packet nearest q.Around among the ones the query admits, and
// the page offset that puts it in the middle of a page.
//
// Only matched packets are candidates: sending the list to a packet the user's own filters
// exclude would misrepresent what the list contains. Extracted from the handler so the
// rule is tested directly rather than re-implemented in a test.
func pktAroundPage(d *pktDecoded, q pktQuery) (offset, nearestNo int, delta float64) {
	idx, best, n := -1, math.MaxFloat64, 0
	for i := range d.Packets {
		p := &d.Packets[i]
		if !q.match(p) {
			continue
		}
		if dist := math.Abs(p.TSUnix - q.Around); dist < best {
			best, idx, nearestNo, delta = dist, n, p.No, p.TSUnix-q.Around
		}
		n++
	}
	if idx < 0 {
		return 0, 0, 0
	}
	// Centre it, so the rows either side of the moment are visible too.
	if offset = idx - q.Limit/2; offset < 0 {
		offset = 0
	}
	return offset, nearestNo, delta
}

// handlePktPackets returns one page of the packet list for a range.
//
// With `around=<epoch seconds>` it returns the page holding the packet nearest that
// instant instead of a page at a fixed offset, and names that packet — the reverse of
// following a packet into the log: a server-log record can send the list to the moment it
// describes without touching the range or the filters.
func (a *App) handlePktPackets(w http.ResponseWriter, r *http.Request) {
	c, ok := a.pktOwned(w, r)
	if !ok {
		return
	}
	d := c.snapshot()
	if d == nil {
		writeErr(w, http.StatusConflict, "capture is not decoded yet")
		return
	}
	q := pktParseQuery(r)

	nearestNo, nearestDelta := 0, 0.0
	if q.Around > 0 {
		q.Offset, nearestNo, nearestDelta = pktAroundPage(d, q)
	}

	matched, skipped := 0, 0
	out := make([]pktPacket, 0, q.Limit)
	for i := range d.Packets {
		p := &d.Packets[i]
		if !q.match(p) {
			continue
		}
		matched++
		if skipped < q.Offset {
			skipped++
			continue
		}
		if len(out) < q.Limit {
			out = append(out, *p)
		}
	}
	res := map[string]any{
		"packets": out, "matched": matched, "offset": q.Offset, "limit": q.Limit,
		"total": len(d.Packets), "streams": d.Streams,
	}
	if q.Around > 0 {
		res["nearestNo"] = nearestNo
		res["nearestDelta"] = nearestDelta
		res["around"] = q.Around
	}
	writeJSON(w, http.StatusOK, res)
}

// handlePktTimeline returns bucket counts over a range: the data behind the
// Traffic Timeline. Buckets are computed server-side over the *whole* capture (or
// the requested window when zoomed), so the browser never has to hold every packet
// to draw the density strip.
func (a *App) handlePktTimeline(w http.ResponseWriter, r *http.Request) {
	c, ok := a.pktOwned(w, r)
	if !ok {
		return
	}
	d := c.snapshot()
	if d == nil {
		writeErr(w, http.StatusConflict, "capture is not decoded yet")
		return
	}
	q := pktParseQuery(r)
	writeJSON(w, http.StatusOK, pktBuildTimeline(d, q))
}

// pktTimeline is the timeline payload: the window it covers, and per-bucket counts
// split by severity so the strip can colour itself.
type pktTimeline struct {
	FromTS  float64          `json:"fromTs"`
	ToTS    float64          `json:"toTs"`
	FromNo  int              `json:"fromNo"`
	ToNo    int              `json:"toNo"`
	Buckets []pktTimeBucket  `json:"buckets"`
	Total   int              `json:"total"`
	Kinds   []pktIssueStat   `json:"kinds"`
	Streams []pktStreamBrief `json:"streams"`
}

type pktTimeBucket struct {
	TS       float64 `json:"ts"` // bucket start
	FirstNo  int     `json:"firstNo"`
	LastNo   int     `json:"lastNo"`
	Count    int     `json:"count"`
	Bytes    int     `json:"bytes"`
	Warnings int     `json:"warnings"`
	Errors   int     `json:"errors"`
	Queries  int     `json:"queries"`
}

type pktStreamBrief struct {
	Index  int    `json:"index"`
	Client string `json:"client"`
	Server string `json:"server"`
	Label  string `json:"label"`
}

// pktSevereIssues are the issue kinds the timeline paints red rather than amber:
// something was lost, refused or reset, as opposed to merely slow or large.
//
// The MySQL half is generated from pktErrCatalog so the two cannot drift — adding a
// code to the catalog is enough to make the timeline colour it correctly.
var pktSevereIssues = func() map[string]bool {
	m := map[string]bool{
		"TCP retransmission":             true,
		"TCP reset":                      true,
		"TCP gap":                        true,
		"TCP zero window":                true,
		"TCP duplicate ACK":              true,
		"Connection refused":             true,
		"Connection attempt unanswered":  true,
		"Server closed the connection":   true,
		"TLS alert":                      true,
		"MySQL packet sequence":          true,
		"Compressed protocol negotiated": true,
		"ICMP destination unreachable":   true,
		"ICMP port unreachable":          true,
	}
	for _, label := range pktErrLabels(true) {
		m[label] = true
	}
	return m
}()

func pktBuildTimeline(d *pktDecoded, q pktQuery) pktTimeline {
	tl := pktTimeline{Buckets: []pktTimeBucket{}, Kinds: []pktIssueStat{}, Streams: []pktStreamBrief{}}
	// Range: what the caller asked for, else the whole capture.
	first, last := 0.0, 0.0
	firstNo, lastNo := 0, 0
	var sel []*pktPacket
	kinds := map[string]int{}
	for i := range d.Packets {
		p := &d.Packets[i]
		if !q.match(p) {
			continue
		}
		if len(sel) == 0 {
			first, firstNo = p.TSUnix, p.No
		}
		last, lastNo = p.TSUnix, p.No
		sel = append(sel, p)
		for _, is := range p.Issues {
			kinds[pktIssueKind(is)]++
		}
	}
	tl.FromTS, tl.ToTS, tl.FromNo, tl.ToNo, tl.Total = first, last, firstNo, lastNo, len(sel)
	for k, n := range kinds {
		tl.Kinds = append(tl.Kinds, pktIssueStat{Kind: k, Count: n})
	}
	sort.Slice(tl.Kinds, func(i, j int) bool { return tl.Kinds[i].Count > tl.Kinds[j].Count })
	for _, s := range d.Streams {
		label := fmt.Sprintf("#%d %s → %s", s.Index, s.Client, s.Server)
		if s.RoleLabel != "" && s.RoleLabel != "MySQL" {
			label += " · " + s.RoleLabel
		}
		if s.User != "" {
			label += " (" + s.User + ")"
		}
		if s.TLS {
			label += " TLS"
		}
		tl.Streams = append(tl.Streams, pktStreamBrief{Index: s.Index, Client: s.Client, Server: s.Server, Label: label})
	}
	if len(sel) == 0 {
		return tl
	}

	n := q.Buckets
	span := last - first
	buckets := make([]pktTimeBucket, n)
	for i := range buckets {
		buckets[i].TS = first + span*float64(i)/float64(n)
	}
	for _, p := range sel {
		idx := 0
		if span > 0 {
			idx = int((p.TSUnix - first) / span * float64(n))
			if idx >= n {
				idx = n - 1
			}
			if idx < 0 {
				idx = 0
			}
		}
		b := &buckets[idx]
		if b.Count == 0 || p.No < b.FirstNo {
			b.FirstNo = p.No
		}
		if p.No > b.LastNo {
			b.LastNo = p.No
		}
		b.Count++
		b.Bytes += p.FrameLen
		if p.Command == "COM_QUERY" || p.Command == "COM_STMT_EXECUTE" || p.Command == "COM_STMT_PREPARE" {
			b.Queries++
		}
		for _, is := range p.Issues {
			if pktSevereIssues[pktIssueKind(is)] || p.ErrCode != 0 {
				b.Errors++
			} else {
				b.Warnings++
			}
		}
	}
	tl.Buckets = buckets
	return tl
}

// handlePktPacket returns one packet with its bytes, for the detail panel: the
// decoded fields, a hex dump, and the printable payload.
func (a *App) handlePktPacket(w http.ResponseWriter, r *http.Request) {
	c, ok := a.pktOwned(w, r)
	if !ok {
		return
	}
	d := c.snapshot()
	if d == nil {
		writeErr(w, http.StatusConflict, "capture is not decoded yet")
		return
	}
	no := atoiDef(r.PathValue("no"), 0)
	var p *pktPacket
	for i := range d.Packets {
		if d.Packets[i].No == no {
			p = &d.Packets[i]
			break
		}
	}
	if p == nil {
		writeErr(w, http.StatusNotFound, "no such packet in this capture")
		return
	}
	c.mu.Lock()
	raw := c.raw
	c.mu.Unlock()
	frame := []byte(nil)
	if p.off >= 0 && p.off+p.cap <= len(raw) {
		frame = raw[p.off : p.off+p.cap]
	}
	var stream *pktStream
	for i := range d.Streams {
		if d.Streams[i].Index == p.Stream {
			stream = &d.Streams[i]
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"packet": p,
		"stream": stream,
		"hex":    pktHexDump(frame),
		"bytes":  len(frame),
	})
}

// pktHexDump renders bytes the way `tcpdump -x` and every hex editor do: offset,
// hex, ASCII. Capped so one enormous frame cannot dominate a response.
func pktHexDump(b []byte) string {
	const max = 8192
	truncated := false
	if len(b) > max {
		b, truncated = b[:max], true
	}
	var sb strings.Builder
	for i := 0; i < len(b); i += 16 {
		end := i + 16
		if end > len(b) {
			end = len(b)
		}
		fmt.Fprintf(&sb, "%04x  ", i)
		for j := i; j < i+16; j++ {
			if j < end {
				fmt.Fprintf(&sb, "%02x ", b[j])
			} else {
				sb.WriteString("   ")
			}
			if j == i+7 {
				sb.WriteByte(' ')
			}
		}
		sb.WriteString(" |")
		for j := i; j < end; j++ {
			if b[j] >= 0x20 && b[j] < 0x7f {
				sb.WriteByte(b[j])
			} else {
				sb.WriteByte('.')
			}
		}
		sb.WriteString("|\n")
	}
	if truncated {
		sb.WriteString("… frame truncated for display\n")
	}
	return sb.String()
}

// snapshot returns the decode, or nil when the capture is not ready.
func (c *pktCapture) snapshot() *pktDecoded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.decoded
}

// ---------------------------------------------------------------- download / upload

// handlePktDownload serves the raw pcap, so a capture can be finished in Wireshark.
// Everything this tool shows is derived from this file — handing it over is the
// honest way to end an analysis it cannot complete.
func (a *App) handlePktDownload(w http.ResponseWriter, r *http.Request) {
	c, ok := a.pktOwned(w, r)
	if !ok {
		return
	}
	c.mu.Lock()
	raw := c.raw
	c.mu.Unlock()
	if len(raw) == 0 {
		writeErr(w, http.StatusConflict, "capture has no bytes yet")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.tcpdump.pcap")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", c.Label+"-"+c.ID+".pcap"))
	w.Write(raw)
}

// handlePktUpload decodes a pcap the user already has — the mock's "Upload
// PCAP/TCPDUMP" button. Same decoder, so a file from a production server outside
// dbcanvas reads exactly like one captured here.
func (a *App) handlePktUpload(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "expected a multipart upload with a 'file' part")
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing 'file' part")
		return
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, pktMaxCapBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read the upload")
		return
	}
	if len(buf) > pktMaxCapBytes {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("capture is over the %s limit", pktBytes(pktMaxCapBytes)))
		return
	}
	// Which protocol the capture holds. "auto" (or nothing) leaves it to
	// pktSniffEngine, which reads the answer out of the bytes; the port's default
	// then follows whatever was chosen or asked for.
	engine := strings.ToLower(strings.TrimSpace(r.FormValue("engine")))
	if engine != pktEngineMySQL && engine != pktEnginePostgres {
		engine = ""
	}
	// The port's default follows the protocol, so it cannot be chosen until the protocol
	// is known: defaulting to 3306 and then sniffing would hand the decoder a MySQL port
	// for a PostgreSQL capture, and the port is what decides which end is the server.
	sniffed := engine
	if sniffed == "" {
		sniffed = pktSniffEngine(buf, 0)
	}
	defPort := 3306
	if sniffed == pktEnginePostgres {
		defPort = pgClientPort
	}
	port := pktClamp(atoiDef(r.FormValue("port"), 0), 1, 65535, defPort)

	// An optional MySQL error log, uploaded with the capture so the two can be read
	// together. Its absence is normal, not an error.
	var logEntries []pktLogEntry
	logName := ""
	if lf, lhdr, lerr := r.FormFile("log"); lerr == nil {
		defer lf.Close()
		lb, rerr := io.ReadAll(io.LimitReader(lf, pktMaxLogBytes+1))
		switch {
		case rerr != nil:
			writeErr(w, http.StatusBadRequest, "could not read the uploaded server log")
			return
		case len(lb) > pktMaxLogBytes:
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("server log is over the %s limit", pktBytes(pktMaxLogBytes)))
			return
		}
		// The engine chosen for the capture also decides how the log is read; when
		// the capture is on "auto", the log is sniffed on its own (the two formats
		// look nothing alike).
		logEntries, logName = pktParseServerLog(lb, engine), lhdr.Filename
		if len(logEntries) == 0 {
			writeErr(w, http.StatusBadRequest,
				"no server-log records were recognised in "+lhdr.Filename+
					" — expected MySQL lines like \"2026-08-03T19:19:01.501234Z 12 [Note] [MY-010914] [Server] …\""+
					" or PostgreSQL lines like \"2026-08-04 06:15:44.142 UTC [2948] ERROR:  …\"")
			return
		}
	}

	c := &pktCapture{
		ID: qrNewID(), Label: hdr.Filename, Source: "upload", Engine: engine, Port: port,
		State: "decoding", Start: time.Now().UTC().Format(time.RFC3339), owner: u.ID, mu: &sync.Mutex{},
		Command:  fmt.Sprintf("(uploaded %s, decoded with server port %d)", hdr.Filename, port),
		LogFile:  logName,
		LogCount: len(logEntries),
	}
	c.logEntries = logEntries
	pktRemember(c)
	c.finish(buf, port, nil, engine)
	if c.State == "error" {
		writeErr(w, http.StatusBadRequest, c.Error)
		return
	}
	writeJSON(w, http.StatusCreated, c.public())
}

// ---------------------------------------------------------------- helpers

// pktClamp bounds a value, substituting def for anything at or below zero.
// (benchmark.go has a three-argument clampInt; this one carries the default.)
func pktClamp(v, lo, hi, def int) int {
	if v <= 0 {
		v = def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func atoiDef(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}
