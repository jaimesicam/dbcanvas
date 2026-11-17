package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// stackfile.go — a stack as a file: `dbcanvas export` writes one, `dbcanvas up -f` reads
// one back. The file is YAML (JSON is read too, including a `dbcanvas-cli stack export`
// file from 0.0.x):
//
//	apiVersion: dbcanvas/v1
//	kind: Stack
//	name: pxc-lab
//	ttl: 8h
//	labels: {team: example}
//	design: {nodes: [...], frames: [...], edges: [...]}
//
// design is the canvas document itself, written in the stored key order, so a file
// exported and applied again gives the same stack: everything on the canvas
// round-trips. A hand-written file may give `nodes:` instead, the compose entries
// (`pxc:3,version=8.4,monitor`, or the same as a map), which are expanded the way
// `compose` expands them; export always writes the full design.
//
// Applying a file to a stack that exists is declarative: the file's design is saved
// (a node no longer in it loses its container, as on the canvas), then the caller
// deploys, which builds what is not running. A running node whose settings changed
// keeps running as it is; the plan says so.

const stackFileAPIVersion = "dbcanvas/v1"

// stackFile is a parsed stack file.
type stackFile struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Name       string `json:"name"`
	// TTL and Labels are nil when the file leaves them out: an existing stack keeps
	// its own, a new one gets the default lifetime and no labels.
	TTL    *string           `json:"ttl,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	// LabelsSet is whether the file has a labels key at all (an empty map clears them).
	LabelsSet bool              `json:"-"`
	Design    json.RawMessage   `json:"design,omitempty"`
	Nodes     []composeNodeSpec `json:"nodes,omitempty"`
	// Platform is the architecture (amd64, arm64) of the nodes that do not choose their own
	// (platforms.go, platformFollower); "" leaves them on Automatic.
	Platform string `json:"platform,omitempty"`
}

// parseStackFile reads a stack file, YAML or JSON.
func parseStackFile(raw []byte) (stackFile, error) {
	var sf stackFile
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return sf, fmt.Errorf("not a stack file: %v", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return sf, errors.New("not a stack file: expected a mapping with apiVersion, kind, name and design")
	}
	m := root.Content[0]
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i].Value, m.Content[i+1]
		if seen[key] {
			return sf, fmt.Errorf("line %d: %s is given twice", m.Content[i].Line, key)
		}
		seen[key] = true
		switch key {
		case "apiVersion":
			sf.APIVersion = val.Value
		case "kind":
			sf.Kind = val.Value
		case "name":
			sf.Name = strings.TrimSpace(val.Value)
		case "ttl":
			t := strings.TrimSpace(val.Value)
			sf.TTL = &t
		case "labels":
			sf.LabelsSet = true
			if val.Tag != "!!null" {
				if err := val.Decode(&sf.Labels); err != nil {
					return sf, fmt.Errorf("line %d: labels: %v", val.Line, err)
				}
			}
		case "design":
			j, err := yamlNodeJSON(val)
			if err != nil {
				return sf, fmt.Errorf("design: %v", err)
			}
			sf.Design = j
		case "platform":
			arch, err := normalizePlatform(val.Value)
			if err != nil {
				return sf, fmt.Errorf("line %d: %v", val.Line, err)
			}
			sf.Platform = arch
		case "nodes":
			if val.Kind != yaml.SequenceNode {
				return sf, fmt.Errorf("line %d: nodes is a list", val.Line)
			}
			for _, n := range val.Content {
				spec, err := composeEntry(n)
				if err != nil {
					return sf, fmt.Errorf("line %d: %v", n.Line, err)
				}
				sf.Nodes = append(sf.Nodes, spec)
			}
		default:
			return sf, fmt.Errorf("line %d: unknown key %q (a stack file has apiVersion, kind, name, ttl, labels, platform, and design or nodes)", m.Content[i].Line, key)
		}
	}
	// A 0.0.x export has no apiVersion or kind; anything that names them names these.
	if sf.APIVersion != "" && sf.APIVersion != stackFileAPIVersion {
		return sf, fmt.Errorf("apiVersion %q is not one this DBCanvas reads (%s)", sf.APIVersion, stackFileAPIVersion)
	}
	if sf.Kind != "" && sf.Kind != "Stack" {
		return sf, fmt.Errorf("kind %q: a stack file is kind Stack", sf.Kind)
	}
	if sf.Name == "" {
		return sf, errors.New("a stack file needs a name")
	}
	if len(sf.Design) > 0 && len(sf.Nodes) > 0 {
		return sf, errors.New("give design or nodes, not both")
	}
	if len(sf.Design) == 0 && len(sf.Nodes) == 0 {
		return sf, errors.New("a stack file needs a design (as export writes it) or nodes (compose entries)")
	}
	if len(sf.Design) > 0 {
		var doc designDoc
		if err := json.Unmarshal(sf.Design, &doc); err != nil {
			var te *json.UnmarshalTypeError
			if errors.As(err, &te) && te.Type.Kind() == reflect.String {
				return sf, fmt.Errorf("design: %s is a %s where text is expected; quote it (\"8.0\", not 8.0)", te.Field, te.Value)
			}
			return sf, fmt.Errorf("design: %v", err)
		}
	}
	return sf, nil
}

// composeEntry is one `nodes:` entry: the short form as a string, or the spec as a map.
func composeEntry(n *yaml.Node) (composeNodeSpec, error) {
	var spec composeNodeSpec
	var fields map[string]any
	switch n.Kind {
	case yaml.ScalarNode:
		m, err := parseComposeNode(n.Value)
		if err != nil {
			return spec, err
		}
		fields = m
	case yaml.MappingNode:
		if err := n.Decode(&fields); err != nil {
			return spec, err
		}
	default:
		return spec, errors.New("a nodes entry is a string (pxc:3,version=8.4) or a map")
	}
	j, err := json.Marshal(fields)
	if err != nil {
		return spec, err
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, fmt.Errorf("nodes entry: %v", err)
	}
	return spec, nil
}

// yamlNodeJSON turns a YAML value into JSON.
func yamlNodeJSON(n *yaml.Node) (json.RawMessage, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// encodeStackFile writes a stack as a stack file.
func encodeStackFile(st Stack) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.MappingNode}
	add := func(key string, val *yaml.Node) {
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, val)
	}
	str := func(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }
	add("apiVersion", str(stackFileAPIVersion))
	add("kind", str("Stack"))
	add("name", str(st.Name))
	add("ttl", str(st.TTL))
	if len(st.Labels) > 0 {
		keys := make([]string, 0, len(st.Labels))
		for k := range st.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lm := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range keys {
			lm.Content = append(lm.Content, str(k), str(st.Labels[k]))
		}
		add("labels", lm)
	}
	design := st.Design
	if len(design) == 0 {
		design = json.RawMessage(defaultDesign)
	}
	// JSON is YAML: parsing the stored design as YAML keeps its key order and each
	// value's type; only the flow style and the quoting are dropped, so it is written
	// out as block YAML, with strings quoted only where they would read as something else.
	var d yaml.Node
	if err := yaml.Unmarshal(design, &d); err != nil {
		return nil, fmt.Errorf("the stored design is not readable: %v", err)
	}
	if len(d.Content) != 1 {
		return nil, errors.New("the stored design is empty")
	}
	blockStyle(d.Content[0])
	add("design", d.Content[0])

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	enc.Close()
	return buf.Bytes(), nil
}

// blockStyle clears the JSON flow style and quoting from a parsed tree.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// ------------------------------------------------------------------ plan and apply

// stackPlanItem is one node, cluster frame or association line the apply adds, removes
// or changes.
type stackPlanItem struct {
	What  string `json:"what"` // node | frame | edge
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Type  string `json:"type,omitempty"`
	// Running is set on a removed or changed node or frame that is deployed now.
	Running bool `json:"running,omitempty"`
	// Arch is what an added or changed node or cluster runs as, and Emulated whether that is
	// not the Docker host's own architecture.
	Arch     string `json:"arch,omitempty"`
	Emulated bool   `json:"emulated,omitempty"`
}

// stackPlan is what applying a stack file does, or did.
type stackPlan struct {
	Action  string          `json:"action"` // create | update | unchanged
	StackID int64           `json:"stackId,omitempty"`
	Name    string          `json:"name"`
	Added   []stackPlanItem `json:"added"`
	Removed []stackPlanItem `json:"removed"`
	Changed []stackPlanItem `json:"changed"`
	// TTL is the new lifetime when it changes ("" when it does not); setting one
	// starts it over from now.
	TTL    string            `json:"ttl,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	// LabelsChange is whether the labels are replaced.
	LabelsChange bool     `json:"labelsChange,omitempty"`
	Notes        []string `json:"notes"`
	Issues       []issue  `json:"issues"`
	OK           bool     `json:"ok"`
	Stack        *Stack   `json:"stack,omitempty"`
}

// stackFileDesign is the design a file asks for: its own, or its compose entries built.
// The stack's platform goes to the nodes that follow it and have no arch of their own: in a
// compose entry before its versions are resolved, so they come from that architecture's
// catalogue; in a design written into each node, leaving deployed ones as they were built.
func stackFileDesign(sf stackFile, deployed map[string]bool) (json.RawMessage, []string, error) {
	if len(sf.Design) > 0 {
		if sf.Platform == "" {
			return sf.Design, nil, nil
		}
		d, err := stampPlatform(sf.Design, sf.Platform, deployed)
		return d, nil, err
	}
	specs := append([]composeNodeSpec(nil), sf.Nodes...)
	if sf.Platform != "" {
		for i := range specs {
			if k, ok := composeKindByName(specs[i].Kind); ok && specs[i].Arch == "" && platformFollower(k.Type) {
				specs[i].Arch = sf.Platform
			}
		}
	}
	doc, pos, _, added, err := buildCompose(composeSpec{Name: sf.Name, Nodes: specs})
	if err != nil {
		return nil, nil, err
	}
	d, err := designJSON(doc, pos)
	return d, added, err
}

// planStackFile works out what applying sf for ownerID would do, without doing it.
func (a *App) planStackFile(ctx context.Context, ownerID int64, sf stackFile) (stackPlan, json.RawMessage, error) {
	p := stackPlan{Name: sf.Name, Added: []stackPlanItem{}, Removed: []stackPlanItem{},
		Changed: []stackPlanItem{}, Notes: []string{}, Issues: []issue{}}
	if sf.TTL != nil && !validTTL(*sf.TTL) {
		return p, nil, errors.New(ttlRefusal(*sf.TTL))
	}
	if err := validateLabels(sf.Labels); err != nil {
		return p, nil, err
	}
	var old Stack
	var err error
	if id := a.store.StackIDByName(ownerID, sf.Name); id != 0 {
		if old, err = a.store.GetStack(id); err != nil {
			return p, nil, err
		}
		p.Action, p.StackID = "update", id
	} else {
		p.Action = "create"
		old.Design = json.RawMessage(`{"nodes":[],"frames":[],"edges":[]}`)
	}

	running := map[string]bool{}
	if p.StackID != 0 {
		deps, _ := a.store.ListDeployments(p.StackID)
		for _, d := range deps {
			if deployBuilt(d) {
				running[d.NodeID] = true
			}
		}
	}
	design, added, err := stackFileDesign(sf, deployedItems(old.Design, running))
	if err != nil {
		return p, nil, err
	}
	p.Notes = append(p.Notes, added...)
	if err := diffDesigns(old.Design, design, running, &p); err != nil {
		return p, nil, err
	}
	a.annotateArch(design, &p)
	for _, c := range p.Changed {
		if c.Running {
			p.Notes = append(p.Notes, fmt.Sprintf("%s %s is deployed: its new settings are saved, not applied to what is running; remove it and deploy again to rebuild it", c.What, firstNonEmpty(c.Label, c.ID)))
		}
	}

	switch {
	case p.Action == "create":
		p.TTL = "8h"
		if sf.TTL != nil {
			p.TTL = *sf.TTL
		}
	case sf.TTL != nil && *sf.TTL != old.TTL:
		p.TTL = *sf.TTL
	}
	if sf.LabelsSet && !sameLabels(sf.Labels, old.Labels) {
		p.LabelsChange, p.Labels = true, sf.Labels
	}
	if p.Action == "update" && len(p.Added)+len(p.Removed)+len(p.Changed) == 0 && p.TTL == "" && !p.LabelsChange {
		p.Action = "unchanged"
	}

	if issues := a.validateStack(ctx, Stack{ID: p.StackID, Name: sf.Name, OwnerID: ownerID, Design: design}); issues != nil {
		p.Issues = issues
	}
	p.OK = !hasError(p.Issues)
	return p, design, nil
}

// errStackBusy is a node added or removed while a deployment of the stack is running.
var errStackBusy = errors.New("cannot add or remove nodes while a deployment is in progress")

// applyStackFile creates or updates the owner's stack from sf. Deploying is the
// caller's next step, through the deploy endpoint, as after compose.
func (a *App) applyStackFile(ctx context.Context, owner User, sf stackFile) (stackPlan, error) {
	p, design, err := a.planStackFile(ctx, owner.ID, sf)
	if err != nil {
		return p, err
	}
	switch p.Action {
	case "create":
		st, err := a.store.CreateStack(sf.Name, owner.ID, p.TTL, expiryFor(p.TTL), design)
		if err != nil {
			return p, err
		}
		if len(sf.Labels) > 0 {
			a.store.SetStackLabels(st.ID, sf.Labels)
		}
		p.StackID = st.ID
		a.stackEventByID(st.ID, "created")
	case "update":
		unlock := lockStackEdits(p.StackID)
		st, err := a.store.GetStack(p.StackID)
		if err == nil && !sameNodeSet(st.Design, design) && a.deployInProgress(st.ID) {
			err = errStackBusy
		}
		if err == nil {
			err = a.store.UpdateStack(st.ID, st.Name, design)
		}
		if err == nil {
			a.cleanupRemovedNodes(st.ID, design)
			if p.TTL != "" {
				err = a.store.SetStackExpiry(st.ID, p.TTL, expiryFor(p.TTL))
			}
		}
		if err == nil && p.LabelsChange {
			err = a.store.SetStackLabels(st.ID, sf.Labels)
		}
		unlock()
		if err != nil {
			return p, err
		}
		a.stackEvent(st, "updated", "stack file", owner.Username)
	}
	if st, err := a.store.GetStack(p.StackID); err == nil {
		p.Stack = &st
	}
	return p, nil
}

// diffDesigns lists the nodes, frames and edges that differ between two designs, by id.
// Canvas geometry alone (a node moved, a frame resized) is not a change worth listing.
func diffDesigns(oldDesign, newDesign json.RawMessage, running map[string]bool, p *stackPlan) error {
	type doc struct {
		Nodes  []map[string]any `json:"nodes"`
		Frames []map[string]any `json:"frames"`
		Edges  []map[string]any `json:"edges"`
	}
	var o, n doc
	if err := json.Unmarshal(oldDesign, &o); err != nil {
		return fmt.Errorf("the stored design is not readable: %v", err)
	}
	if err := json.Unmarshal(newDesign, &n); err != nil {
		return fmt.Errorf("design: %v", err)
	}
	// A frame counts as running when any of its members does.
	frameRunning := map[string]bool{}
	for _, m := range o.Nodes {
		if f, _ := m["frameId"].(string); f != "" && running[planStr(m["id"])] {
			frameRunning[f] = true
		}
	}
	cmp := func(what string, old, cur []map[string]any, isRunning func(string) bool) {
		byID := map[string]map[string]any{}
		for _, m := range old {
			byID[planStr(m["id"])] = m
		}
		item := func(m map[string]any) stackPlanItem {
			id := planStr(m["id"])
			return stackPlanItem{What: what, ID: id, Label: planStr(m["label"]), Type: planStr(m["type"]), Running: isRunning(id)}
		}
		seen := map[string]bool{}
		for _, m := range cur {
			id := planStr(m["id"])
			seen[id] = true
			prev, ok := byID[id]
			switch {
			case !ok:
				it := item(m)
				it.Running = false
				p.Added = append(p.Added, it)
			case !reflect.DeepEqual(withoutGeometry(prev), withoutGeometry(m)):
				p.Changed = append(p.Changed, item(m))
			}
		}
		for _, m := range old {
			if !seen[planStr(m["id"])] {
				p.Removed = append(p.Removed, item(m))
			}
		}
	}
	cmp("node", o.Nodes, n.Nodes, func(id string) bool { return running[id] })
	cmp("frame", o.Frames, n.Frames, func(id string) bool { return frameRunning[id] })
	cmp("edge", o.Edges, n.Edges, func(string) bool { return false })
	return nil
}

// withoutGeometry is a design item without its place on the canvas.
func withoutGeometry(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch k {
		case "x", "y", "w", "h":
			continue
		}
		out[k] = v
	}
	return out
}

func planStr(v any) string {
	s, _ := v.(string)
	return s
}

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// deployedItems is the ids of the nodes that are built and the clusters with a built member,
// whose architecture a stack platform does not change.
func deployedItems(design json.RawMessage, running map[string]bool) map[string]bool {
	out := map[string]bool{}
	var doc designDoc
	json.Unmarshal(design, &doc)
	for _, n := range doc.Nodes {
		if running[n.ID] {
			out[n.ID] = true
			if n.FrameID != "" {
				out[n.FrameID] = true
			}
		}
	}
	return out
}

// annotateArch fills in what each added or changed node and cluster runs as.
func (a *App) annotateArch(design json.RawMessage, p *stackPlan) {
	var doc designDoc
	if json.Unmarshal(design, &doc) != nil {
		return
	}
	arch := map[string]string{}
	for _, f := range doc.Frames {
		arch["frame/"+f.ID] = effectiveArch(f.Type, f.Arch, "")
	}
	for _, n := range doc.Nodes {
		frameArch := ""
		if n.FrameID != "" {
			frameArch = arch["frame/"+n.FrameID]
		}
		arch["node/"+n.ID] = effectiveArch(n.Type, n.Arch, frameArch)
		if n.Type == "pmm" { // its release decides (pmmarch.go)
			if v, _, err := a.pmmArch(context.Background(), p.StackID, n); err == nil {
				arch["node/"+n.ID] = v
			}
		}
	}
	fill := func(items []stackPlanItem) {
		for i := range items {
			if v := arch[items[i].What+"/"+items[i].ID]; v != "" {
				items[i].Arch = v
				items[i].Emulated = a.hostArch != "" && v != a.hostArch
			}
		}
	}
	fill(p.Added)
	fill(p.Changed)
}
