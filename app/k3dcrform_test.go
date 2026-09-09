package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// crdFixture is the real PXC CustomResourceDefinition, when a live cluster has been captured into
// testdata. The pruner's whole job is to survive that file — 1.4 MB, with a decade of served
// versions in it — so the tests that need it skip rather than pretend when it is absent.
func crdFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/pxc-crd.json")
	if err != nil {
		t.Skipf("no CRD fixture: %v", err)
	}
	return b
}

// buildForm runs the same walk k3dCRForm does, without needing a cluster to talk to.
func buildForm(t *testing.T, raw []byte) []crFormField {
	t.Helper()
	var crd crdDoc
	if err := json.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse the CRD: %v", err)
	}
	var specRaw json.RawMessage
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			specRaw = v.Schema.OpenAPIV3Schema.Properties["spec"]
		}
	}
	if len(specRaw) == 0 {
		t.Fatal("the CRD declares no storage version with a spec schema")
	}
	var s jsonSchema
	if err := json.Unmarshal(specRaw, &s); err != nil {
		t.Fatalf("parse the spec schema: %v", err)
	}
	var out []crFormField
	for _, k := range crSortedKeys(s.Properties) {
		out = append(out, crFormBuildIn(k, k, s.Properties[k], 1, false, true))
	}
	return out
}

func findField(fields []crFormField, path string) *crFormField {
	for i := range fields {
		if fields[i].Path == path {
			return &fields[i]
		}
		if f := findField(fields[i].Fields, path); f != nil {
			return f
		}
		if f := findField(fields[i].Element, path); f != nil {
			return f
		}
	}
	return nil
}

// The form model is what crosses the wire to a browser, and the CRD it is built from is 1.4 MB.
// A pruner that let even a fraction of that through would make the editor unusable on the one
// page it exists for.
func TestCRFormPrunesTheCRD(t *testing.T) {
	fields := buildForm(t, crdFixture(t))
	model, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("the form model does not encode: %v", err)
	}
	if len(model) > 120<<10 {
		t.Errorf("the form model is %d KB — too much to send for one panel", len(model)>>10)
	}
	// ...and it must still be the whole document: every top-level section of cr.yaml is there.
	for _, want := range []string{"pxc", "haproxy", "proxysql", "backup", "pmm", "logcollector", "tls", "upgradeOptions", "unsafeFlags"} {
		if findField(fields, want) == nil {
			t.Errorf("section %q is missing from the form", want)
		}
	}
}

// The fields people actually open this editor for have to arrive as controls, not as a JSON box:
// a form that renders `backup.pitr` as raw text is a text editor with extra steps.
func TestCRFormRendersTheFieldsThatMatter(t *testing.T) {
	fields := buildForm(t, crdFixture(t))
	for _, tc := range []struct {
		path, typ string
	}{
		{"pxc.size", "integer"},
		{"pxc.image", "string"},
		{"pxc.configuration", "string"},
		{"pxc.autoRecovery", "boolean"},
		{"backup.pitr.enabled", "boolean"},
		{"backup.pitr.storageName", "string"},
		// The CRD says number, not integer — the form follows the schema, not the field's name.
		{"backup.pitr.timeBetweenUploads", "number"},
		{"haproxy.size", "integer"},
		{"pmm.enabled", "boolean"},
		{"pmm.serverHost", "string"},
		{"tls.enabled", "boolean"},
		{"unsafeFlags.pxcSize", "boolean"},
		{"upgradeOptions.apply", "string"},
	} {
		f := findField(fields, tc.path)
		if f == nil {
			t.Errorf("%s is not in the form", tc.path)
			continue
		}
		if f.Type != tc.typ {
			t.Errorf("%s is %q, want %q", tc.path, f.Type, tc.typ)
		}
		if f.Raw {
			t.Errorf("%s arrived as a raw box; it is exactly the kind of field the form is for", tc.path)
		}
	}
	// The enum from the CRD becomes a picker rather than a text box.
	if f := findField(fields, "pxc.mysqlAllocator"); f == nil || len(f.Enum) < 2 {
		t.Errorf("an enum field must carry its options: %+v", f)
	}
	// backup.storages is a map the user names the keys of — the storage name a schedule and
	// pitr.storageName both refer to. It has to be marked as one, or the editor offers a text box
	// where "add a storage" belongs.
	st := findField(fields, "backup.storages")
	if st == nil || !st.Map || len(st.Element) == 0 {
		t.Fatalf("backup.storages must be a named map with an element schema: %+v", st)
	}
	if findField(st.Element, "backup.storages.*.type") == nil {
		t.Error("the storage element must carry its own fields")
	}
	// An array of objects keeps its element schema, so a schedule entry is a card and not JSON.
	sched := findField(fields, "backup.schedule")
	if sched == nil || sched.Items != "object" || len(sched.Element) == 0 {
		t.Fatalf("backup.schedule must be an object list with an element: %+v", sched)
	}
}

// The Kubernetes boilerplate is what makes the CRD enormous, and none of it is why anybody opens
// this editor. It still has to be reachable — as a JSON box — or the editor would quietly cover
// half the custom resource.
func TestCRFormBoilerplateIsRawButPresent(t *testing.T) {
	fields := buildForm(t, crdFixture(t))
	for _, path := range []string{"pxc.affinity", "pxc.tolerations", "pxc.sidecars", "haproxy.affinity", "pxc.podSecurityContext"} {
		f := findField(fields, path)
		if f == nil {
			t.Errorf("%s is missing entirely — the editor must still reach it", path)
			continue
		}
		if !f.Raw {
			t.Errorf("%s should be a raw box, not a generated form", path)
		}
		if f.RawWhy == "" {
			t.Errorf("%s is raw without saying why; the editor tells the user", path)
		}
	}
}

// The help text is the only prose in the whole editor: the Percona CRDs ship no descriptions, so
// a generated form with nothing added is a wall of field names.
func TestCRFormHelpFillsTheCRDsSilence(t *testing.T) {
	fields := buildForm(t, crdFixture(t))
	for _, path := range []string{"pxc.size", "backup.pitr.enabled", "backup.pitr.storageName", "pause", "unsafeFlags.pxcSize", "tls.enabled"} {
		f := findField(fields, path)
		if f == nil {
			t.Fatalf("%s is missing", path)
		}
		if f.Desc == "" && f.Help == "" {
			t.Errorf("%s has no explanation, and the CRD gives none either", path)
		}
	}
	// The leaf table covers the vocabulary repeated in every section, so `haproxy.image` is
	// explained without an entry of its own.
	if f := findField(fields, "haproxy.image"); f == nil || (f.Desc == "" && f.Help == "") {
		t.Error("the repeated vocabulary must be explained wherever it appears")
	}
	// A CRD that does ship descriptions wins: this table is the fallback, not the authority.
	got := crFormBuild("size", "pxc.size", jsonSchema{Type: "integer", Description: "from the CRD"}, 1, false)
	if got.Desc != "from the CRD" || got.Help != "" {
		t.Errorf("the CRD's own description must win: %+v", got)
	}
}

// The groups are what the editor navigates by. A field the grouping table has never heard of —
// which is what a new operator release looks like — must still land somewhere visible.
func TestCRFormGroupsCoverEveryKey(t *testing.T) {
	keys := map[string]bool{"pxc": true, "backup": true, "pmm": true, "pause": true, "somethingNew": true}
	groups := crFormGroupsFor("pxc", keys)
	seen := map[string]bool{}
	for _, g := range groups {
		for _, s := range g.Sections {
			if seen[s] {
				t.Errorf("%s appears in two groups", s)
			}
			seen[s] = true
		}
	}
	for _, want := range []string{"pxc", "backup", "pmm", "pause", "somethingNew"} {
		if !seen[want] {
			t.Errorf("%s is in the CRD but in no group — it would be invisible", want)
		}
	}
	// And the unknown one lands in Other rather than in a section it is not part of.
	for _, g := range groups {
		if g.ID == "other" && (len(g.Sections) != 1 || g.Sections[0] != "somethingNew") {
			t.Errorf("Other should hold exactly the unrecognised key: %v", g.Sections)
		}
	}
}

// A patch is logged so the deployment log says what was changed. It must name the paths and never
// the values: a custom resource carries passwords (users, secrets), and the log is readable by
// anyone who can open the node.
func TestCRPatchSummaryNamesPathsNotValues(t *testing.T) {
	got := crPatchSummary(map[string]any{
		"backup": map[string]any{"pitr": map[string]any{"enabled": true, "storageName": "seaweedfs-binlog"}},
		"pxc":    map[string]any{"size": 5},
		"users":  []any{map[string]any{"name": "app", "password": "hunter2"}},
	})
	for _, want := range []string{"backup.pitr.enabled", "backup.pitr.storageName", "pxc.size", "users"} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary must name %s: %q", want, got)
		}
	}
	for _, leak := range []string{"hunter2", "seaweedfs-binlog", "true", "5"} {
		if strings.Contains(got, leak) {
			t.Errorf("the summary leaked a value (%s): %q", leak, got)
		}
	}
	// A wide patch is truncated rather than filling the log with one line.
	wide := map[string]any{}
	for i := 0; i < 30; i++ {
		wide[string(rune('a'+i))] = i
	}
	if s := crPatchSummary(wide); !strings.Contains(s, "and 18 more") {
		t.Errorf("a wide patch must be summarised: %q", s)
	}
}

// An int-or-string (a Kubernetes quantity: "500m", or 1) must stay a text box. Rendering it as a
// number turns "500m" into 500 the moment the field is touched, which the API server accepts and
// which means something else entirely.
func TestCRFormQuantityStaysAString(t *testing.T) {
	got := crFormBuild("cpu", "pxc.resources.limits.cpu", jsonSchema{XIntOrString: true}, 3, false)
	if got.Type != "string" {
		t.Errorf("an int-or-string field is a text box, got %q", got.Type)
	}
	if got.Raw {
		t.Error("it is a scalar, not a raw box")
	}
}

// A number worth having in front of you when the pruning rules are next changed: what the browser
// is actually handed, and how much of the CRD it is.
func TestCRFormModelSizeIsReported(t *testing.T) {
	raw := crdFixture(t)
	fields := buildForm(t, raw)
	model, _ := json.Marshal(fields)
	t.Logf("CRD %d KB → form model %d KB (%.1f%%), %d sections",
		len(raw)>>10, len(model)>>10, 100*float64(len(model))/float64(len(raw)), len(fields))
}

// `resources.limits` and `resources.requests` are maps of Kubernetes quantities — {cpu: 500m,
// memory: 1G} — and a quantity has no `type` in the schema at all, because it is int-or-string.
// Requiring a type turned every resources block in the custom resource into a JSON box, which is
// the field people reach for most: it is in every section, and it is what you change to watch a
// pod get OOM-killed.
func TestCRFormQuantityMapsAreKeyValue(t *testing.T) {
	fields := buildForm(t, crdFixture(t))
	for _, path := range []string{"pxc.resources.limits", "pxc.resources.requests", "haproxy.resources.limits"} {
		f := findField(fields, path)
		if f == nil {
			t.Errorf("%s is missing", path)
			continue
		}
		if f.Raw {
			t.Errorf("%s is a JSON box; it is a map of quantities and should be key/value rows", path)
		}
		if !f.Map || len(f.Element) == 0 {
			t.Errorf("%s must be a named map with an element: %+v", path, f)
			continue
		}
		// The element is the value control: a text box, because "500m" is not a number.
		if el := f.Element[0]; el.Type != "string" {
			t.Errorf("%s values are quantities, so the control is a string, got %q", path, el.Type)
		}
	}
}
