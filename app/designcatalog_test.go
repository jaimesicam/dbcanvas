package main

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// designerTypes reads the Stack Designer's node and frame type lists, so the catalogue
// cannot fall behind a type added to the canvas.
func designerTypes(t *testing.T) (nodes, frames []string) {
	t.Helper()
	src, err := os.ReadFile("web/src/pages/StackDesigner.jsx")
	if err != nil {
		t.Skip("designer source not present:", err)
	}
	s := string(src)
	start := strings.Index(s, "export const NODE_TYPES = {")
	if start < 0 {
		t.Fatal("NODE_TYPES not found in StackDesigner.jsx")
	}
	depth, end := 0, start
	for i := start + len("export const NODE_TYPES = "); i < len(s); i++ {
		if s[i] == '{' {
			depth++
		} else if s[i] == '}' {
			depth--
			if depth == 0 {
				end = i
				break
			}
		}
	}
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z0-9]+): \{`).FindAllStringSubmatch(s[start:end], -1) {
		nodes = append(nodes, m[1])
	}
	fc := regexp.MustCompile(`const FRAME_COLORS = \{([^}]*)\}`).FindStringSubmatch(s)
	if fc == nil {
		t.Fatal("FRAME_COLORS not found in StackDesigner.jsx")
	}
	for _, m := range regexp.MustCompile(`([a-z0-9]+):`).FindAllStringSubmatch(fc[1], -1) {
		frames = append(frames, m[1])
	}
	return nodes, frames
}

func TestDesignCatalog(t *testing.T) {
	c := designCatalog()
	nodeFields := jsonFieldTypes(reflect.TypeOf(designNode{}))
	frameFields := jsonFieldTypes(reflect.TypeOf(designFrame{}))
	routes := map[string]bool{}
	for _, rt := range apiRoutes() {
		routes[rt.Method+" "+rt.Path] = true
	}

	check := func(e catalogEntry) {
		where := fmt.Sprintf("%s %q", e.Placement, e.Type)
		if e.Label == "" || e.Summary == "" {
			t.Errorf("%s: needs a label and a summary", where)
		}
		switch e.Placement {
		case "standalone", "member", "standalone,member", "frame":
		default:
			t.Errorf("%s: placement must be standalone, member, standalone,member or frame", where)
		}
		fields := nodeFields
		if e.isFrame() {
			fields = frameFields
			if e.MemberType == "" {
				t.Errorf("%s: a frame names its members' type", where)
			} else if m, ok := c.node(e.MemberType); !ok {
				t.Errorf("%s: member type %q has no node entry", where, e.MemberType)
			} else if !strings.Contains(m.Placement, "member") || !contains(m.FrameTypes, e.Type) {
				t.Errorf("%s: node %q must be placement member with frameTypes including %q", where, e.MemberType, e.Type)
			}
			if e.MinMembers < 1 || e.MaxMembers < e.MinMembers || e.DefaultMembers < e.MinMembers || e.DefaultMembers > e.MaxMembers {
				t.Errorf("%s: members %d (min %d, max %d) do not add up", where, e.DefaultMembers, e.MinMembers, e.MaxMembers)
			}
		}
		seen := map[string]bool{}
		for _, o := range e.Options {
			if seen[o.Field] {
				t.Errorf("%s: option %q listed twice", where, o.Field)
			}
			seen[o.Field] = true
			if _, ok := fields[o.Field]; !ok {
				t.Errorf("%s: option %q is not a JSON field of the %s struct", where, o.Field, map[bool]string{true: "designFrame", false: "designNode"}[e.isFrame()])
			}
			if o.Desc == "" {
				t.Errorf("%s: option %q needs a description", where, o.Field)
			}
			if o.Default != nil && len(o.Values) > 0 {
				if s, ok := o.Default.(string); ok && !contains(o.Values, s) {
					t.Errorf("%s: option %q default %q is not one of its values %v", where, o.Field, s, o.Values)
				}
			}
			for _, rel := range catalogRelations {
				if rel.Field == o.Field {
					t.Errorf("%s: %q is a relation (%s); list it under relations, not options", where, o.Field, rel.Name)
				}
			}
		}
		for _, name := range e.Relations {
			rel, ok := catalogRelationByName(name)
			if !ok {
				t.Errorf("%s: unknown relation %q", where, name)
				continue
			}
			if _, ok := fields[rel.Field]; !ok {
				t.Errorf("%s: relation %q's field %q is not on the struct", where, name, rel.Field)
			}
		}
		for _, ed := range e.Edges {
			if ed.Type != "directional" && ed.Type != "async" && ed.Type != "bidir" {
				t.Errorf("%s: edge type %q", where, ed.Type)
			}
			for _, to := range ed.To {
				_, n := c.node(to)
				_, f := c.frame(to)
				if !n && !f {
					t.Errorf("%s: edge to unknown type %q", where, to)
				}
			}
		}
		for _, ac := range e.Actions {
			if !routes[ac.Method+" "+ac.Path] {
				t.Errorf("%s: action %s %s is not a route", where, ac.Method, ac.Path)
			}
			if ac.Desc == "" {
				t.Errorf("%s: action %s %s needs a description", where, ac.Method, ac.Path)
			}
		}
	}

	types := map[string]bool{}
	for _, e := range c.Nodes {
		if types["n:"+e.Type] {
			t.Errorf("node type %q catalogued twice", e.Type)
		}
		types["n:"+e.Type] = true
		check(e)
	}
	for _, e := range c.Frames {
		if types["f:"+e.Type] {
			t.Errorf("frame type %q catalogued twice", e.Type)
		}
		types["f:"+e.Type] = true
		check(e)
	}
	for _, ac := range catalogCommonActions {
		if !routes[ac.Method+" "+ac.Path] {
			t.Errorf("common action %s %s is not a route", ac.Method, ac.Path)
		}
	}
	for _, r := range catalogRelations {
		for _, tgt := range r.Targets {
			if _, ok := c.node(tgt); !ok {
				t.Errorf("relation %q targets uncatalogued type %q", r.Name, tgt)
			}
		}
	}

	nodes, frames := designerTypes(t)
	for _, n := range nodes {
		if _, ok := c.node(n); !ok {
			t.Errorf("the designer's node type %q has no catalogue entry", n)
		}
	}
	for _, f := range frames {
		if _, ok := c.frame(f); !ok {
			t.Errorf("the designer's cluster type %q has no catalogue entry", f)
		}
	}
}
