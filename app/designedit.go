package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// designedit.go — editing one node, cluster or association line of a stack design
// through the API, checked against the catalogue (designcatalog.go).
//
// PUT /api/stacks/{id} replaces the whole design, which is what the designer does and
// what an API client can do too; it takes whatever JSON it is given. These endpoints
// change one thing at a time instead, and refuse what the catalogue does not list for
// the type: an unknown field, a value of the wrong JSON type, a value outside a fixed
// set, a relation to a node of the wrong type. The design is edited as raw JSON maps,
// so keys the Go structs do not declare (the canvas position, the view) are kept.
//
// Every change is saved the way PUT /api/stacks/{id} saves one: the node set is frozen
// while a deploy is in flight, and a node removed from the design has its container
// removed. The validator's findings come back with each change, as they would on the
// canvas; they do not block the edit, because a design is often invalid half-way.

// rawDesign is a stack design as JSON maps.
type rawDesign struct {
	top    map[string]json.RawMessage
	Nodes  []map[string]any
	Frames []map[string]any
	Edges  []map[string]any
}

func parseRawDesign(b json.RawMessage) (*rawDesign, error) {
	d := &rawDesign{top: map[string]json.RawMessage{}}
	if len(b) > 0 && string(b) != "null" {
		if err := json.Unmarshal(b, &d.top); err != nil {
			return nil, err
		}
	}
	for key, dst := range map[string]*[]map[string]any{"nodes": &d.Nodes, "frames": &d.Frames, "edges": &d.Edges} {
		if raw, ok := d.top[key]; ok && string(raw) != "null" {
			if err := json.Unmarshal(raw, dst); err != nil {
				return nil, fmt.Errorf("design %s: %w", key, err)
			}
		}
	}
	return d, nil
}

func (d *rawDesign) encode() (json.RawMessage, error) {
	for key, v := range map[string][]map[string]any{"nodes": d.Nodes, "frames": d.Frames, "edges": d.Edges} {
		if v == nil {
			v = []map[string]any{}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		d.top[key] = b
	}
	return json.Marshal(d.top)
}

func mstr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func mnum(m map[string]any, k string) float64 {
	f, _ := m[k].(float64)
	return f
}

func (d *rawDesign) node(id string) map[string]any {
	for _, n := range d.Nodes {
		if mstr(n, "id") == id {
			return n
		}
	}
	return nil
}

func (d *rawDesign) frame(id string) map[string]any {
	for _, f := range d.Frames {
		if mstr(f, "id") == id {
			return f
		}
	}
	return nil
}

func (d *rawDesign) members(frameID string) []map[string]any {
	var out []map[string]any
	for _, n := range d.Nodes {
		if mstr(n, "frameId") == frameID {
			out = append(out, n)
		}
	}
	return out
}

// newID makes an id in the designer's shape (<prefix>-<base36 time>-<n>) that the
// design does not already use.
func (d *rawDesign) newID(prefix string) string {
	used := map[string]bool{}
	for _, list := range [][]map[string]any{d.Nodes, d.Frames, d.Edges} {
		for _, m := range list {
			used[mstr(m, "id")] = true
		}
	}
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 36)
	for i := 1; ; i++ {
		if id := fmt.Sprintf("%s-%s-%d", prefix, stamp, i); !used[id] {
			return id
		}
	}
}

// labelTaken reports whether a node or frame other than except already has the label.
// Node labels are host names, so they must be unique in the stack.
func (d *rawDesign) labelTaken(label, except string) bool {
	for _, list := range [][]map[string]any{d.Nodes, d.Frames} {
		for _, m := range list {
			if mstr(m, "id") != except && strings.EqualFold(mstr(m, "label"), label) {
				return true
			}
		}
	}
	return false
}

// newLabel numbers a base ("pxc" → "pxc-01") until the label is free.
func (d *rawDesign) newLabel(base string) string {
	for i := 1; ; i++ {
		if l := fmt.Sprintf("%s-%02d", base, i); !d.labelTaken(l, "") {
			return l
		}
	}
}

// rightEdge is the x just past everything on the canvas, for placing a new item where
// it overlaps nothing.
func (d *rawDesign) rightEdge() float64 {
	x := 0.0
	for _, n := range d.Nodes {
		if mstr(n, "frameId") == "" {
			x = math.Max(x, mnum(n, "x")+composeColStep-40)
		}
	}
	for _, f := range d.Frames {
		x = math.Max(x, mnum(f, "x")+mnum(f, "w"))
	}
	return x + composeFrameGap
}

// ------------------------------------------------------------- options and relations

// catalogFields are the JSON shapes of designNode's and designFrame's fields.
var catalogFields = sync.OnceValues(func() (map[string]string, map[string]string) {
	return jsonFieldTypes(reflect.TypeOf(designNode{})), jsonFieldTypes(reflect.TypeOf(designFrame{}))
})

func structFields(frame bool) map[string]string {
	n, f := catalogFields()
	if frame {
		return f
	}
	return n
}

// applyCatalogDefaults gives a new node or frame the defaults the catalogue records.
func applyCatalogDefaults(e *catalogEntry, m map[string]any) {
	for _, o := range e.Options {
		if o.Default != nil {
			// Through JSON, so a Go int default reads back the way a decoded body would.
			b, _ := json.Marshal(o.Default)
			var v any
			json.Unmarshal(b, &v)
			m[o.Field] = v
		}
	}
}

// applyCatalogOptions sets a type's options on a node or frame map. Every option is
// checked; the errors come back together so a client can fix them in one pass.
// A JSON null removes the field, which resets it to its zero value.
func applyCatalogOptions(e *catalogEntry, m map[string]any, opts map[string]json.RawMessage) error {
	var errs []string
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, field := range keys {
		raw := opts[field]
		o, ok := e.option(field)
		if !ok {
			msg := fmt.Sprintf("%q is not an option of %s", field, e.Type)
			for _, rel := range catalogRelations {
				if rel.Field == field {
					msg += fmt.Sprintf(` — it is the %q relation; set it with "relations": {%q: "<id>"}`, rel.Name, rel.Name)
					break
				}
			}
			errs = append(errs, msg)
			continue
		}
		if string(raw) == "null" {
			delete(m, field)
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", field, err))
			continue
		}
		if err := checkOptionValue(o, v); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", field, err))
			continue
		}
		m[field] = v
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s. GET /api/stacks/design/catalog?type=%s lists its options", strings.Join(errs, "; "), e.Type)
	}
	return nil
}

func checkOptionValue(o catalogOption, v any) error {
	got := "object"
	switch t := v.(type) {
	case string:
		got = "string"
	case bool:
		got = "boolean"
	case float64:
		got = "number"
		if t == math.Trunc(t) {
			got = "integer"
		}
	case []any:
		got = "array"
	}
	want := o.Type
	if got != want && !(want == "number" && got == "integer") {
		return fmt.Errorf("wants a JSON %s, got %s", want, got)
	}
	if s, ok := v.(string); ok && len(o.Values) > 0 && !slices.Contains(o.Values, s) {
		return fmt.Errorf("%q is not one of %s", s, strings.Join(o.Values, ", "))
	}
	return nil
}

// applyRelations points a node or frame at other nodes by relation name. A target is a
// node id; null or "" removes the relation. Setting one turns its Enable flags on, as
// the designer's picker does.
func applyRelations(d *rawDesign, e *catalogEntry, m map[string]any, rels map[string]*string) error {
	fields := structFields(e.isFrame())
	var errs []string
	names := make([]string, 0, len(rels))
	for k := range rels {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		target := rels[name]
		rel, ok := catalogRelationByName(name)
		if !ok || !e.offers(name) {
			errs = append(errs, fmt.Sprintf("%s has no %q relation (it has: %s)", e.Type, name, strings.Join(e.Relations, ", ")))
			continue
		}
		if target == nil || *target == "" {
			clearRelation(m, rel, fields)
			continue
		}
		t := d.node(*target)
		if t == nil {
			errs = append(errs, fmt.Sprintf("%s: no node %q in this design", name, *target))
			continue
		}
		if !slices.Contains(rel.Targets, mstr(t, "type")) {
			errs = append(errs, fmt.Sprintf("%s: %q is a %s node; it must be one of %s", name, *target, mstr(t, "type"), strings.Join(rel.Targets, ", ")))
			continue
		}
		m[rel.Field] = *target
		for _, f := range rel.Enable {
			if _, ok := fields[f]; ok {
				m[f] = true
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// clearRelation removes a relation. Two relations can share a field (ldap and
// kerberos both use ldapDirNodeId): removing the narrower one (kerberos) only turns off
// its extra flags; removing the broader one removes the reference and every flag.
func clearRelation(m map[string]any, rel catalogRelation, fields map[string]string) {
	for _, other := range catalogRelations {
		if other.Name != rel.Name && other.Field == rel.Field && len(other.Enable) < len(rel.Enable) && isSubset(other.Enable, rel.Enable) {
			for _, f := range rel.Enable {
				if !slices.Contains(other.Enable, f) {
					m[f] = false
				}
			}
			return
		}
	}
	delete(m, rel.Field)
	for _, other := range catalogRelations {
		if other.Field == rel.Field {
			for _, f := range other.Enable {
				if _, ok := fields[f]; ok {
					m[f] = false
				}
			}
		}
	}
}

func isSubset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// dropReferencesTo clears every relation and edge that points at removed ids.
func (d *rawDesign) dropReferencesTo(removed map[string]bool) {
	for i, list := range [][]map[string]any{d.Nodes, d.Frames} {
		for _, m := range list {
			for _, rel := range catalogRelations {
				if id := mstr(m, rel.Field); id != "" && removed[id] {
					clearRelation(m, rel, structFields(i == 1))
				}
			}
		}
	}
	d.Edges = slices.DeleteFunc(d.Edges, func(e map[string]any) bool {
		from, _ := e["from"].(map[string]any)
		to, _ := e["to"].(map[string]any)
		return removed[mstr(from, "node")] || removed[mstr(to, "node")]
	})
}

// ------------------------------------------------------------- saving

// stackEditLocks serialises design edits per stack, so two API calls changing the same
// design one after the other do not each save over the other's change.
var stackEditLocks sync.Map

func lockStackEdits(id int64) func() {
	mu, _ := stackEditLocks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

// saveStackDesign stores a changed design the way PUT /api/stacks/{id} does. It writes
// the error response itself and reports whether the save happened.
func (a *App) saveStackDesign(w http.ResponseWriter, st Stack, name string, design json.RawMessage) bool {
	// While a deployment is in progress, the node set is frozen: reject adding or
	// removing nodes (option/position edits are still fine). This keeps the canvas
	// consistent with what the in-flight provisioners are building.
	if !sameNodeSet(st.Design, design) && a.deployInProgress(st.ID) {
		writeErr(w, http.StatusConflict, "cannot add or remove nodes while a deployment is in progress")
		return false
	}
	if err := a.store.UpdateStack(st.ID, name, design); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update stack")
		return false
	}
	// Real-time cleanup: a node removed from the canvas has its container + volumes
	// (and deployment record) torn down immediately, not deferred to the next deploy.
	a.cleanupRemovedNodes(st.ID, design)
	return true
}

// designEditResult is what every edit returns: the item it touched, the validator's
// findings for the whole design afterwards, and notes on what the edit did besides.
type designEditResult struct {
	Node    map[string]any   `json:"node,omitempty"`
	Frame   map[string]any   `json:"frame,omitempty"`
	Members []map[string]any `json:"members,omitempty"`
	Edge    map[string]any   `json:"edge,omitempty"`
	Removed []string         `json:"removed,omitempty"`
	Notes   []string         `json:"notes,omitempty"`
	Issues  []issue          `json:"issues"`
	OK      bool             `json:"ok"`
}

// editDesign loads the caller's stack, runs one change on its design, saves it and
// answers with the validator's findings. change returns the HTTP status and message of
// a refused edit, or 0.
func (a *App) editDesign(w http.ResponseWriter, r *http.Request, change func(d *rawDesign, res *designEditResult) (int, error)) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	defer lockStackEdits(st.ID)()
	// Re-read under the lock: loadOwnedStack's copy may predate an edit that just finished.
	if fresh, err := a.store.GetStack(st.ID); err == nil {
		st = fresh
	}
	d, err := parseRawDesign(st.Design)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "the stored design is not readable: "+err.Error())
		return
	}
	res := designEditResult{Issues: []issue{}}
	if code, err := change(d, &res); err != nil {
		writeErr(w, code, err.Error())
		return
	}
	design, err := d.encode()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to encode the design")
		return
	}
	if !a.saveStackDesign(w, st, st.Name, design) {
		return
	}
	st.Design = design
	// The validator starts by pinging Docker; with no engine (a unit test) there is
	// nothing to check the design against.
	if a.docker != nil {
		if issues := a.validateStack(r.Context(), st); issues != nil {
			res.Issues = issues
		}
	}
	res.OK = !hasError(res.Issues)
	writeJSON(w, http.StatusOK, res)
}

// ------------------------------------------------------------- request bodies

// designItemBody is the body of the node and frame create and update calls.
type designItemBody struct {
	Type  string   `json:"type"`
	Label string   `json:"label"`
	X     *float64 `json:"x"`
	Y     *float64 `json:"y"`
	// Options are fields of the node (or, for a cluster, the frame), by JSON name.
	Options map[string]json.RawMessage `json:"options"`
	// Relations point at other nodes by relation name: {"monitor": "<pmm node id>"}.
	Relations map[string]*string `json:"relations"`
	// FrameID puts a new node into an existing cluster (node create only).
	FrameID string `json:"frameId"`
	// Members is a new cluster's member count; MemberOptions apply to every member
	// (frame create only).
	Members       int                        `json:"members"`
	MemberOptions map[string]json.RawMessage `json:"memberOptions"`
}

func decodeItemBody(r *http.Request) (designItemBody, error) {
	var b designItemBody
	if err := decode(r, &b); err != nil {
		return b, errors.New("invalid request body: " + err.Error())
	}
	b.Type, b.Label = strings.TrimSpace(b.Type), strings.TrimSpace(b.Label)
	return b, nil
}

// setLabel applies a requested label, refusing one another item already has.
func setLabel(d *rawDesign, m map[string]any, label string) error {
	if label == "" {
		return nil
	}
	if d.labelTaken(label, mstr(m, "id")) {
		return fmt.Errorf("label %q is already used in this stack (labels are host names)", label)
	}
	m["label"] = label
	return nil
}

// ------------------------------------------------------------- handlers

func (a *App) handleGetStackDesign(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	d, err := parseRawDesign(st.Design)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "the stored design is not readable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nonNil(d.Nodes), "frames": nonNil(d.Frames), "edges": nonNil(d.Edges)})
}

func nonNil(v []map[string]any) []map[string]any {
	if v == nil {
		return []map[string]any{}
	}
	return v
}

// handleDesignAddNode adds a standalone node, or a member to an existing cluster when
// frameId is given.
func (a *App) handleDesignAddNode(w http.ResponseWriter, r *http.Request) {
	body, err := decodeItemBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.editDesign(w, r, func(d *rawDesign, res *designEditResult) (int, error) {
		// A member takes its type from the cluster, so type may be left out.
		if body.FrameID != "" {
			return a.addMember(d, res, body.FrameID, body)
		}
		e, ok := designCatalog().node(body.Type)
		if !ok {
			return http.StatusBadRequest, fmt.Errorf("unknown node type %q — GET /api/stacks/design/catalog lists them", body.Type)
		}
		if body.Members != 0 || body.MemberOptions != nil {
			return http.StatusBadRequest, errors.New("members and memberOptions belong to POST …/design/frames")
		}
		if !strings.Contains(e.Placement, "standalone") {
			return http.StatusBadRequest, fmt.Errorf("a %s node lives in a %s cluster: create one with POST /api/stacks/{id}/design/frames, or pass frameId", e.Type, strings.Join(e.FrameTypes, " or "))
		}
		if e.Singleton {
			for _, n := range d.Nodes {
				if mstr(n, "type") == e.Type {
					return http.StatusConflict, fmt.Errorf("a stack has at most one %s node (%s)", e.Type, mstr(n, "id"))
				}
			}
		}
		n := map[string]any{"id": d.newID(e.Type), "type": e.Type}
		applyCatalogDefaults(e, n)
		n["label"] = d.newLabel(e.Type)
		if err := setLabel(d, n, body.Label); err != nil {
			return http.StatusConflict, err
		}
		n["x"], n["y"] = d.rightEdge(), float64(composeFrameY)
		if body.X != nil {
			n["x"] = *body.X
		}
		if body.Y != nil {
			n["y"] = *body.Y
		}
		if err := applyCatalogOptions(e, n, body.Options); err != nil {
			return http.StatusBadRequest, err
		}
		d.Nodes = append(d.Nodes, n)
		if err := applyRelations(d, e, n, body.Relations); err != nil {
			return http.StatusBadRequest, err
		}
		res.Node = n
		return 0, nil
	})
}

// addMember adds one member node to a cluster frame and widens the frame to hold it.
func (a *App) addMember(d *rawDesign, res *designEditResult, frameID string, body designItemBody) (int, error) {
	f := d.frame(frameID)
	if f == nil {
		return http.StatusNotFound, fmt.Errorf("no cluster frame %q in this design", frameID)
	}
	fe, ok := designCatalog().frame(mstr(f, "type"))
	if !ok {
		return http.StatusBadRequest, fmt.Errorf("cluster type %q is not catalogued", mstr(f, "type"))
	}
	if body.Type != "" && body.Type != fe.MemberType {
		return http.StatusBadRequest, fmt.Errorf("a %s cluster's members are %s nodes, not %s", fe.Type, fe.MemberType, body.Type)
	}
	me, _ := designCatalog().node(fe.MemberType)
	members := d.members(frameID)
	if fe.MaxMembers > 0 && len(members) >= fe.MaxMembers {
		return http.StatusConflict, fmt.Errorf("a %s cluster takes at most %d members", fe.Type, fe.MaxMembers)
	}
	n := newMember(d, me, f, len(members), len(members)+1, "")
	if err := setLabel(d, n, body.Label); err != nil {
		return http.StatusConflict, err
	}
	if err := applyCatalogOptions(me, n, body.Options); err != nil {
		return http.StatusBadRequest, err
	}
	d.Nodes = append(d.Nodes, n)
	if err := applyRelations(d, me, n, body.Relations); err != nil {
		return http.StatusBadRequest, err
	}
	fitFrame(f, len(members)+1)
	res.Node = n
	return 0, nil
}

// newMember makes member i (of count) of a frame, with the member type's defaults and
// the role compose would give it.
func newMember(d *rawDesign, me *catalogEntry, f map[string]any, i, count int, role string) map[string]any {
	n := map[string]any{"id": d.newID(me.Type), "type": me.Type, "frameId": mstr(f, "id")}
	applyCatalogDefaults(me, n)
	if role == "" {
		role = memberRole(me.Type, i, count)
	}
	if role != "" {
		n["role"] = role
	}
	n["label"] = d.newLabel(mstr(f, "label"))
	n["exportEnabled"], n["exportHostPort"] = false, 0
	n["x"] = mnum(f, "x") + composeMemberIX + float64(i*composeMemberDX)
	n["y"] = mnum(f, "y") + composeMemberIY
	return n
}

// memberRole is the role compose gives member i of a cluster of this type, if any.
func memberRole(memberType string, i, count int) string {
	for _, k := range composeKinds {
		if k.Frame && k.Type == memberType && k.Roles != nil {
			return k.Roles(i, count)
		}
	}
	return ""
}

func fitFrame(f map[string]any, members int) {
	f["w"] = math.Max(mnum(f, "w"), float64(composeMemberIX*2+composeMemberDX*members))
}

// handleDesignAddFrame adds a cluster: the frame and its members.
func (a *App) handleDesignAddFrame(w http.ResponseWriter, r *http.Request) {
	body, err := decodeItemBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.editDesign(w, r, func(d *rawDesign, res *designEditResult) (int, error) {
		fe, ok := designCatalog().frame(body.Type)
		if !ok {
			return http.StatusBadRequest, fmt.Errorf("unknown cluster type %q — GET /api/stacks/design/catalog lists them under frames", body.Type)
		}
		me, ok := designCatalog().node(fe.MemberType)
		if !ok {
			return http.StatusInternalServerError, fmt.Errorf("cluster type %q has no member entry", fe.Type)
		}
		if body.FrameID != "" {
			return http.StatusBadRequest, errors.New("frameId belongs to POST …/design/nodes, which adds a member to an existing cluster")
		}
		f := map[string]any{"id": d.newID("frame"), "type": fe.Type}
		applyCatalogDefaults(fe, f)
		f["label"] = d.newLabel(fe.Type)
		if err := setLabel(d, f, body.Label); err != nil {
			return http.StatusConflict, err
		}
		if err := applyCatalogOptions(fe, f, body.Options); err != nil {
			return http.StatusBadRequest, err
		}
		// A sharded MongoDB cluster has a fixed shape, chosen by psmdbSetup.
		var topology []composeMember
		if fe.Type == "psmdb" {
			if body.Members != 0 {
				return http.StatusBadRequest, errors.New(`a psmdb cluster's members follow its shape: set options.psmdbSetup to "standard" or "minimum" instead of members`)
			}
			topology = psmdbTopology(mstr(f, "psmdbSetup"))
		}
		count := body.Members
		if topology != nil {
			count = len(topology)
		} else if count == 0 {
			count = fe.DefaultMembers
		}
		if topology == nil && (count < fe.MinMembers || fe.MaxMembers > 0 && count > fe.MaxMembers) {
			return http.StatusBadRequest, fmt.Errorf("a %s cluster takes %d–%d members, not %d", fe.Type, fe.MinMembers, fe.MaxMembers, count)
		}
		f["x"], f["y"] = d.rightEdge(), float64(composeFrameY)
		if body.X != nil {
			f["x"] = *body.X
		}
		if body.Y != nil {
			f["y"] = *body.Y
		}
		f["w"], f["h"] = 0.0, float64(composeFrameH)
		fitFrame(f, count)
		f["w"] = math.Max(mnum(f, "w"), 400)
		d.Frames = append(d.Frames, f)
		if err := applyRelations(d, fe, f, body.Relations); err != nil {
			return http.StatusBadRequest, err
		}
		for i := 0; i < count; i++ {
			role := ""
			if topology != nil {
				role = topology[i].Role
			}
			n := newMember(d, me, f, i, count, role)
			if topology != nil {
				n["label"] = d.newLabel(mstr(f, "label") + "-" + topology[i].Suffix)
				n["shard"] = topology[i].Shard
			}
			if err := applyCatalogOptions(me, n, body.MemberOptions); err != nil {
				return http.StatusBadRequest, fmt.Errorf("memberOptions: %w", err)
			}
			d.Nodes = append(d.Nodes, n)
			res.Members = append(res.Members, n)
		}
		res.Frame = f
		return 0, nil
	})
}

// handleDesignUpdateNode changes a node's label, position, options or relations.
func (a *App) handleDesignUpdateNode(w http.ResponseWriter, r *http.Request) {
	body, err := decodeItemBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	nid := r.PathValue("nid")
	a.editDesign(w, r, func(d *rawDesign, res *designEditResult) (int, error) {
		n := d.node(nid)
		if n == nil {
			return http.StatusNotFound, fmt.Errorf("no node %q in this design", nid)
		}
		if body.Type != "" && body.Type != mstr(n, "type") {
			return http.StatusBadRequest, errors.New("a node's type cannot change; remove it and add a new one")
		}
		if body.FrameID != "" || body.Members != 0 || body.MemberOptions != nil {
			return http.StatusBadRequest, errors.New("frameId, members and memberOptions cannot be changed here")
		}
		e, ok := designCatalog().node(mstr(n, "type"))
		if !ok {
			return http.StatusBadRequest, fmt.Errorf("node type %q is not catalogued", mstr(n, "type"))
		}
		if err := setLabel(d, n, body.Label); err != nil {
			return http.StatusConflict, err
		}
		if body.X != nil {
			n["x"] = *body.X
		}
		if body.Y != nil {
			n["y"] = *body.Y
		}
		if err := applyCatalogOptions(e, n, body.Options); err != nil {
			return http.StatusBadRequest, err
		}
		if err := applyRelations(d, e, n, body.Relations); err != nil {
			return http.StatusBadRequest, err
		}
		res.Node = n
		return 0, nil
	})
}

// handleDesignUpdateFrame changes a cluster frame's label, position, options or
// relations. memberOptions applies to every member at once.
func (a *App) handleDesignUpdateFrame(w http.ResponseWriter, r *http.Request) {
	body, err := decodeItemBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	fid := r.PathValue("fid")
	a.editDesign(w, r, func(d *rawDesign, res *designEditResult) (int, error) {
		f := d.frame(fid)
		if f == nil {
			return http.StatusNotFound, fmt.Errorf("no cluster frame %q in this design", fid)
		}
		if body.Type != "" && body.Type != mstr(f, "type") {
			return http.StatusBadRequest, errors.New("a cluster's type cannot change; remove it and add a new one")
		}
		if body.Members != 0 {
			return http.StatusBadRequest, errors.New("add members with POST …/design/nodes and frameId; remove them with DELETE …/design/nodes/{nid}")
		}
		fe, ok := designCatalog().frame(mstr(f, "type"))
		if !ok {
			return http.StatusBadRequest, fmt.Errorf("cluster type %q is not catalogued", mstr(f, "type"))
		}
		if err := setLabel(d, f, body.Label); err != nil {
			return http.StatusConflict, err
		}
		// Moving a frame carries its members with it, as dragging it does.
		dx, dy := 0.0, 0.0
		if body.X != nil {
			dx = *body.X - mnum(f, "x")
			f["x"] = *body.X
		}
		if body.Y != nil {
			dy = *body.Y - mnum(f, "y")
			f["y"] = *body.Y
		}
		members := d.members(fid)
		for _, n := range members {
			n["x"], n["y"] = mnum(n, "x")+dx, mnum(n, "y")+dy
		}
		if err := applyCatalogOptions(fe, f, body.Options); err != nil {
			return http.StatusBadRequest, err
		}
		if err := applyRelations(d, fe, f, body.Relations); err != nil {
			return http.StatusBadRequest, err
		}
		if body.MemberOptions != nil {
			me, _ := designCatalog().node(fe.MemberType)
			for _, n := range members {
				if err := applyCatalogOptions(me, n, body.MemberOptions); err != nil {
					return http.StatusBadRequest, fmt.Errorf("memberOptions: %w", err)
				}
			}
			res.Members = members
		}
		res.Frame = f
		return 0, nil
	})
}

// handleDesignDeleteNode removes a node, the lines drawn to it and every relation that
// pointed at it. A deployed node's container goes with it.
func (a *App) handleDesignDeleteNode(w http.ResponseWriter, r *http.Request) {
	nid := r.PathValue("nid")
	a.editDesign(w, r, func(d *rawDesign, res *designEditResult) (int, error) {
		n := d.node(nid)
		if n == nil {
			return http.StatusNotFound, fmt.Errorf("no node %q in this design", nid)
		}
		if fid := mstr(n, "frameId"); fid != "" {
			if f := d.frame(fid); f != nil {
				if fe, ok := designCatalog().frame(mstr(f, "type")); ok && len(d.members(fid)) <= fe.MinMembers {
					return http.StatusConflict, fmt.Errorf("a %s cluster needs at least %d members; remove the whole cluster with DELETE …/design/frames/%s", fe.Type, fe.MinMembers, fid)
				}
			}
		}
		d.Nodes = slices.DeleteFunc(d.Nodes, func(m map[string]any) bool { return mstr(m, "id") == nid })
		d.dropReferencesTo(map[string]bool{nid: true})
		res.Removed = []string{nid}
		return 0, nil
	})
}

// handleDesignDeleteFrame removes a cluster: the frame, its members, their lines and
// relations.
func (a *App) handleDesignDeleteFrame(w http.ResponseWriter, r *http.Request) {
	fid := r.PathValue("fid")
	a.editDesign(w, r, func(d *rawDesign, res *designEditResult) (int, error) {
		if d.frame(fid) == nil {
			return http.StatusNotFound, fmt.Errorf("no cluster frame %q in this design", fid)
		}
		removed := map[string]bool{fid: true}
		for _, n := range d.members(fid) {
			removed[mstr(n, "id")] = true
		}
		d.Frames = slices.DeleteFunc(d.Frames, func(m map[string]any) bool { return mstr(m, "id") == fid })
		d.Nodes = slices.DeleteFunc(d.Nodes, func(m map[string]any) bool { return removed[mstr(m, "id")] })
		d.dropReferencesTo(removed)
		for id := range removed {
			res.Removed = append(res.Removed, id)
		}
		sort.Strings(res.Removed)
		return 0, nil
	})
}

// handleDesignAddEdge draws an association line between two nodes or frames.
func (a *App) handleDesignAddEdge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
		Type string `json:"type"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Type == "" {
		body.Type = "directional"
	}
	a.editDesign(w, r, func(d *rawDesign, res *designEditResult) (int, error) {
		if body.Type != "directional" && body.Type != "async" && body.Type != "bidir" {
			return http.StatusBadRequest, errors.New(`type is "directional", "async" or "bidir"`)
		}
		if body.From == "" || body.To == "" || body.From == body.To {
			return http.StatusBadRequest, errors.New("from and to name two different node or frame ids")
		}
		typeOf := func(id string) (string, bool, error) {
			if n := d.node(id); n != nil {
				return mstr(n, "type"), false, nil
			}
			if f := d.frame(id); f != nil {
				return mstr(f, "type"), true, nil
			}
			return "", false, fmt.Errorf("no node or frame %q in this design", id)
		}
		ft, fFrame, err := typeOf(body.From)
		if err != nil {
			return http.StatusBadRequest, err
		}
		tt, tFrame, err := typeOf(body.To)
		if err != nil {
			return http.StatusBadRequest, err
		}
		for _, e := range d.Edges {
			from, _ := e["from"].(map[string]any)
			to, _ := e["to"].(map[string]any)
			x, y := mstr(from, "node"), mstr(to, "node")
			if x == body.From && y == body.To || x == body.To && y == body.From {
				return http.StatusConflict, fmt.Errorf("%s and %s are already connected (%s)", body.From, body.To, mstr(e, "id"))
			}
		}
		// The catalogue's edge lists are what the provisioners walk. A pair neither end
		// lists is saved anyway, since the validator has the final word, but it is noted.
		if !catalogAllowsEdge(ft, fFrame, tt, body.Type) && !catalogAllowsEdge(tt, tFrame, ft, body.Type) {
			res.Notes = append(res.Notes, fmt.Sprintf("neither %s nor %s lists a %s line to the other in the catalogue; no provisioner may use it", ft, tt, body.Type))
		}
		e := map[string]any{
			"id":   d.newID("e"),
			"from": map[string]any{"node": body.From, "port": "right"},
			"to":   map[string]any{"node": body.To, "port": "left"},
			"type": body.Type,
		}
		d.Edges = append(d.Edges, e)
		res.Edge = e
		return 0, nil
	})
}

func catalogAllowsEdge(fromType string, fromFrame bool, toType, edgeType string) bool {
	var e *catalogEntry
	var ok bool
	if fromFrame {
		e, ok = designCatalog().frame(fromType)
	} else {
		e, ok = designCatalog().node(fromType)
	}
	if !ok {
		return false
	}
	for _, ed := range e.Edges {
		if ed.Type == edgeType && slices.Contains(ed.To, toType) {
			return true
		}
	}
	return false
}

func (a *App) handleDesignDeleteEdge(w http.ResponseWriter, r *http.Request) {
	eid := r.PathValue("eid")
	a.editDesign(w, r, func(d *rawDesign, res *designEditResult) (int, error) {
		n := len(d.Edges)
		d.Edges = slices.DeleteFunc(d.Edges, func(m map[string]any) bool { return mstr(m, "id") == eid })
		if len(d.Edges) == n {
			return http.StatusNotFound, fmt.Errorf("no line %q in this design", eid)
		}
		res.Removed = []string{eid}
		return 0, nil
	})
}
