package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every built-in template has to survive the same two rules the lab designs learned
// the hard way (see labs_design_test.go): no pinned architecture, because an
// installation only builds images for its own DOCKER_PLATFORM, and no pinned minor
// version, because minors come from whatever `make versions` probed on this host.
func TestBuiltinTemplatesAreDeployableDesigns(t *testing.T) {
	tpls := builtinTemplates()
	if len(tpls) == 0 {
		t.Fatal("no built-in templates")
	}
	seen := map[string]bool{}
	for _, tpl := range tpls {
		if seen[tpl.ID] {
			t.Errorf("%s: duplicate template id", tpl.ID)
		}
		seen[tpl.ID] = true
		if tpl.Name == "" || tpl.Description == "" || tpl.Category == "" {
			t.Errorf("%s: needs a name, description and category — the picker shows all three", tpl.ID)
		}
		if !tpl.Builtin {
			t.Errorf("%s: not flagged builtin", tpl.ID)
		}

		body := string(tpl.Design)
		if strings.Contains(body, `"arch"`) {
			t.Errorf("%s: pins an architecture", tpl.ID)
		}
		for _, pinned := range pinnedVersionKeys {
			if regexp.MustCompile(`"` + pinned + `"\s*:\s*"[^"]+"`).MatchString(body) {
				t.Errorf("%s: pins a minor version in %q — leave it \"\" for the catalog default", tpl.ID, pinned)
			}
		}

		var doc designDoc
		if err := json.Unmarshal(tpl.Design, &doc); err != nil {
			t.Errorf("%s: design does not parse: %v", tpl.ID, err)
			continue
		}
		if len(doc.Nodes) == 0 {
			t.Errorf("%s: design has no nodes", tpl.ID)
		}

		// Every stack needs an Intranet — it is the stack's DNS authority, and
		// validation refuses to deploy without one.
		if !hasNodeType(doc, "intranet") {
			t.Errorf("%s: no Intranet node, so the design can never deploy", tpl.ID)
		}
		assertTemplateRefsResolve(t, tpl.ID, doc, tpl.Design)
	}
}

// pinnedVersionKeys are the "minor version" fields a template must leave empty.
// Major/series fields (pxcMajor, pgMajor…) are deliberately not here: those are the
// supported series, always present in the catalog.
var pinnedVersionKeys = []string{
	"pxcVersion", "psVersion", "pgVersion", "psmdbVersion", "valkeyVersion",
	"proxysqlVersion", "mariadbVersion", "mysqlceVersion", "orchestratorVersion",
	"version", "k3dK3sVersion", "k3dOperatorVer", "k3dPgoVersion",
	"aioPsVersion", "aioPxcVersion", "aioMariadbVersion", "aioMysqlceVersion",
	"aioPsmdbVersion", "aioValkeyVersion", "aioProxysqlVersion", "aioOrchestratorVersion",
}

func hasNodeType(doc designDoc, typ string) bool {
	for _, n := range doc.Nodes {
		if n.Type == typ {
			return true
		}
	}
	return false
}

// assertTemplateRefsResolve checks that every id a design points at — a member's
// frameId, an edge endpoint, and each of the cross-node picker fields — names
// something in the same design. A dangling reference is the failure mode a
// hand-written design literal actually has, and it surfaces at deploy, not at parse.
func assertTemplateRefsResolve(t *testing.T, id string, doc designDoc, raw json.RawMessage) {
	t.Helper()
	known := map[string]bool{}
	for _, n := range doc.Nodes {
		known[n.ID] = true
	}
	for _, f := range doc.Frames {
		known[f.ID] = true
	}
	for _, n := range doc.Nodes {
		if n.FrameID != "" && !known[n.FrameID] {
			t.Errorf("%s: node %s belongs to unknown frame %q", id, n.ID, n.FrameID)
		}
	}
	for _, e := range doc.Edges {
		for _, end := range []string{e.From.Node, e.To.Node} {
			if !known[end] {
				t.Errorf("%s: edge %s points at unknown %q", id, e.ID, end)
			}
		}
	}
	// The picker fields are matched by key name over the raw tree, so a template
	// that uses one this test has never heard of is still covered.
	for key, val := range collectRefValues(raw) {
		for _, v := range val {
			if v != "" && !known[v] {
				t.Errorf("%s: %s references unknown node/frame %q", id, key, v)
			}
		}
	}
}

// refKeySuffixes identify design fields whose value is another node's or frame's id.
var refKeySuffixes = []string{"NodeId", "NodeID", "frameId", "ssAIONode"}

func collectRefValues(raw json.RawMessage) map[string][]string {
	out := map[string][]string{}
	var tree any
	if json.Unmarshal(raw, &tree) != nil {
		return out
	}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if s, ok := child.(string); ok && isRefKey(k) {
					out[k] = append(out[k], s)
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(tree)
	return out
}

func isRefKey(k string) bool {
	for _, suf := range refKeySuffixes {
		if k == suf || strings.HasSuffix(k, suf) {
			return true
		}
	}
	return false
}

// --- sanitization ---

func TestSanitizeTemplateDesignClearsSecretsAtEveryDepth(t *testing.T) {
	in := []byte(`{
	  "nodes": [
	    {"id":"n1","type":"pmm","adminPassword":"hunter2","exportHostPort":33306,"gdbCoreDir":"/srv/cores"},
	    {"id":"n2","type":"aio","aioInstances":[
	      {"id":"i1","kind":"ps","rootPassword":"nested-secret","exportHostPort":3307}
	    ]},
	    {"id":"n3","type":"stocksim","ssPassword":"pw","ssDSN":"postgres://u:p@h/db","ssUser":"keepme"}
	  ],
	  "frames": [{"id":"f1","type":"pxc","rootPassword":"frame-secret","devicePath":"/dev/sda"}],
	  "edges": [], "view": {"x":0,"y":0,"z":1}
	}`)
	out := string(sanitizeTemplateDesign(in))

	for _, leaked := range []string{"hunter2", "nested-secret", "frame-secret", "postgres://u:p@h/db", "/srv/cores", "/dev/sda"} {
		if strings.Contains(out, leaked) {
			t.Errorf("sanitized design still contains %q:\n%s", leaked, out)
		}
	}
	if strings.Contains(out, "33306") || strings.Contains(out, "3307") {
		t.Errorf("a fixed host port survived sanitization — two stacks from one template would collide:\n%s", out)
	}
	// Non-secret fields must survive: the template is still a design.
	for _, kept := range []string{`"keepme"`, `"pmm"`, `"aioInstances"`, `"f1"`} {
		if !strings.Contains(out, kept) {
			t.Errorf("sanitization dropped %s:\n%s", kept, out)
		}
	}
}

func TestSanitizeTemplateDesignPassesInvalidJSONThrough(t *testing.T) {
	in := []byte(`{"nodes": [`)
	if got := string(sanitizeTemplateDesign(in)); got != string(in) {
		t.Errorf("invalid JSON should come back unchanged, got %q", got)
	}
}

// TestTemplateSanitizerCoversEverySecretField is the guard that keeps
// templateSecretKeys honest. The design structs grow a new node family every few
// sessions; when one of them adds a password field, this fails until the field is
// either blanked by the sanitizer or explicitly declared safe to travel — rather
// than silently shipping inside every exported template from then on.
func TestTemplateSanitizerCoversEverySecretField(t *testing.T) {
	// Design-carried fields that look like secrets but are not, with the reason.
	safe := map[string]string{
		// The Intranet's own service passwords are fixed, well-known lab credentials
		// baked into the node's provisioning — not per-stack secrets. They are part
		// of what the template describes.
		"ldapAdminPassword": "documented Intranet lab credential",
		"mailAdminPassword": "documented Intranet lab credential",
	}
	tagRE := regexp.MustCompile("json:\"([a-zA-Z]+)\"")
	for _, file := range []string{"intranet.go", "aio.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, m := range tagRE.FindAllStringSubmatch(string(src), -1) {
			key := m[1]
			lower := strings.ToLower(key)
			if !strings.Contains(lower, "password") && !strings.Contains(lower, "secret") {
				continue
			}
			if templateSecretKeys[key] {
				continue
			}
			if reason, ok := safe[key]; ok {
				t.Logf("%s: %q not sanitized (%s)", file, key, reason)
				continue
			}
			t.Errorf("%s: design field %q looks like a secret but templateSecretKeys does not clear it — "+
				"add it there, or add it to this test's safe list with a reason", file, key)
		}
	}
}

func TestTemplateFilenameIsSafe(t *testing.T) {
	cases := map[string]string{
		"PXC + ProxySQL + PMM":     "pxc--proxysql--pmm.dbcanvas-template.json",
		"../../etc/passwd":         "etcpasswd.dbcanvas-template.json",
		"  ":                       "template.dbcanvas-template.json",
		"Starter — Percona Server": "starter--percona-server.dbcanvas-template.json",
	}
	for in, want := range cases {
		if got := templateFilename(in); got != want {
			t.Errorf("templateFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidTemplateDesignRejectsEmptyAndBroken(t *testing.T) {
	if _, err := validTemplateDesign([]byte(`{"nodes":[],"frames":[],"edges":[]}`)); err == nil {
		t.Error("an empty design should not be saveable as a template")
	}
	if _, err := validTemplateDesign([]byte(`not json`)); err == nil {
		t.Error("invalid JSON should not be saveable as a template")
	}
	if _, err := validTemplateDesign(tplStarterPSDesign); err != nil {
		t.Errorf("a built-in design should be valid: %v", err)
	}
}

func TestTemplateIsBuiltin(t *testing.T) {
	if !templateIsBuiltin("builtin:pxc-proxysql-pmm") {
		t.Error("builtin: prefix not recognized")
	}
	if templateIsBuiltin("42") {
		t.Error("a row id must not read as builtin")
	}
	if _, ok := findBuiltinTemplate("builtin:nope"); ok {
		t.Error("findBuiltinTemplate invented a template")
	}
}
