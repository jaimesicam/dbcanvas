package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// k3dcrform.go — the custom resource editor: a form built from the operator's own CRD.
//
// cr.yaml is edited twice in this app. Once before it is applied, by crTransform (k3dcr.go),
// which rewrites the file the operator ships. And once afterwards — here — against the live
// object, because that is where the interesting settings are: the ones you change to see what
// the operator does about it.
//
// The form is generated from the CustomResourceDefinition, not hand-written, for one reason
// that matters more than the effort saved: an operator release adds and removes fields, and a
// hand-written form silently offers the old ones. Whatever `kubectl explain` would tell you is
// what the form shows, for whichever operator version this cluster happens to run.
//
// ------------------------------------------------------------------- what gets sent, and why
//
// The PXC CRD is 1.4 MB, and its v1 `spec` schema alone is ~200 KB. Most of that is Kubernetes
// boilerplate repeated per section — affinity, tolerations, securityContext, sidecars,
// probes — which nobody opens a form to edit and which would make the payload (and the form)
// unusable. So the schema is walked and pruned:
//
//   - a subtree named in crFormOpaque, or deeper than crFormMaxDepth, or with more descendants
//     than crFormMaxProps, is emitted as ONE node marked `raw`;
//   - the editor renders a raw node as a YAML/JSON box holding that subtree's current value.
//
// So nothing is unreachable — every field in the CRD can still be set — but the form is a form
// for the fields people actually turn, and a text box for the ones they paste.
//
// ------------------------------------------------------------------- how a change is applied
//
// A strategic-merge patch, dry-run against the API server first. The dry run is what makes this
// safe to hand to a lab user: the API server validates against the same CRD the form was built
// from, so a value the operator would reject comes back as a message next to the field instead
// of a cluster that stops reconciling. Deletions are sent as JSON null, which is what a merge
// patch means by "remove this key".
//
// The operator is not restarted and nothing else is touched: the object is patched, the
// operator notices, and what it does next is the point of the exercise.

const (
	// crFormMaxDepth is how deep the generated form goes before a subtree becomes a raw box.
	// It is a backstop, not the main pruning rule — crFormOpaque and crFormMaxProps do that
	// work — so it sits well below anything a person navigates to
	// (`backup.storages.<name>.s3.bucket` is five) and only catches genuine recursion.
	crFormMaxDepth = 7
	// crFormMaxProps is the descendant count past which a subtree becomes a raw box, whatever
	// its depth. Affinity is 8 KB of schema; a form with it expanded is not a form.
	crFormMaxProps = 60
	// crFormMaxRawBytes bounds one raw value in the editor, so a pathological object cannot
	// push megabytes through the browser.
	crFormMaxRawBytes = 64 << 10
)

// crFormOpaque are subtrees always sent as a raw box: pure Kubernetes scheduling and security
// plumbing, identical in every section, and never the reason someone opens this editor. They are
// listed by field name because that is how the CRD repeats them.
var crFormOpaque = map[string]bool{
	"affinity":                  true,
	"tolerations":               true,
	"topologySpreadConstraints": true,
	"containerSecurityContext":  true,
	"podSecurityContext":        true,
	"lifecycle":                 true,
	"sidecars":                  true,
	"sidecarVolumes":            true,
	"sidecarPVCs":               true,
	"extraPVCs":                 true,
	"imagePullSecrets":          true,
	"nodeSelector":              true,
	"annotations":               true,
	"labels":                    true,
}

// crFormField is one node of the generated form. It is deliberately close to the JSON Schema it
// came from — the editor decides how to render each kind, and a field it does not recognise
// falls back to a text box rather than disappearing.
type crFormField struct {
	Name     string        `json:"name"`            // the key in the parent object
	Path     string        `json:"path"`            // dotted path from spec, e.g. "pxc.volumeSpec"
	Type     string        `json:"type"`            // object | array | string | integer | number | boolean | raw
	Items    string        `json:"items,omitempty"` // for arrays: the element type
	Enum     []string      `json:"enum,omitempty"`
	Format   string        `json:"format,omitempty"`
	Default  any           `json:"default,omitempty"`
	Min      *float64      `json:"min,omitempty"`
	Max      *float64      `json:"max,omitempty"`
	Desc     string        `json:"desc,omitempty"`   // the CRD's description, when it has one
	Help     string        `json:"help,omitempty"`   // DBCanvas's own, when the CRD has none
	Fields   []crFormField `json:"fields,omitempty"` // object children, in schema order
	Element  []crFormField `json:"element,omitempty"`
	Map      bool          `json:"map,omitempty"`      // an object keyed by names the user chooses
	Raw      bool          `json:"raw,omitempty"`      // edit as YAML/JSON, not as a form
	RawWhy   string        `json:"rawWhy,omitempty"`   // why: "deep" | "large" | "opaque" | "free-form"
	Required bool          `json:"required,omitempty"` //
}

// crFormSchema is what the editor is handed.
type crFormSchema struct {
	Kind      string        `json:"kind"`      // PerconaXtraDBCluster
	Resource  string        `json:"resource"`  // pxc — what kubectl calls it
	Group     string        `json:"group"`     // pxc.percona.com
	Version   string        `json:"version"`   // v1 — the CRD's storage version
	Cluster   string        `json:"cluster"`   // the object's name
	Namespace string        `json:"namespace"` //
	Operator  string        `json:"operator"`  // the DBCanvas operator key
	Sections  []crFormField `json:"sections"`  // spec's top-level properties, grouped below
	Spec      any           `json:"spec"`      // the live .spec
	Status    string        `json:"status"`    // .status.state, for the header
	// Groups order and name the sections for the editor's navigation. A CRD is a flat bag of
	// properties; a person reading cr.yaml sees chapters, and this is that reading.
	Groups []crFormGroup `json:"groups"`
}

// crFormGroup is one tab of the editor.
type crFormGroup struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Sections []string `json:"sections"` // top-level spec keys in this group, in order
	Note     string   `json:"note,omitempty"`
}

// crFormGroupsFor arranges a CRD's top-level spec keys into the chapters cr.yaml itself reads
// as. Anything the CRD has that is not named here still appears — it lands in "Other", so a
// field added by a new operator release is never hidden by this table being out of date.
func crFormGroupsFor(operator string, keys map[string]bool) []crFormGroup {
	var groups []crFormGroup
	add := func(id, label, note string, want ...string) {
		var have []string
		for _, k := range want {
			if keys[k] {
				have = append(have, k)
				delete(keys, k)
			}
		}
		if len(have) > 0 {
			groups = append(groups, crFormGroup{ID: id, Label: label, Sections: have, Note: note})
		}
	}
	// The cluster-wide settings that are not a section of their own: they sit at the top of
	// cr.yaml, above the first block.
	add("cluster", "Cluster", "Settings that apply to the whole custom resource — the ones at the top of cr.yaml, above the first section.",
		"crVersion", "platform", "pause", "secretsName", "vaultSecretName", "sslSecretName", "sslInternalSecretName",
		"logCollectorSecretName", "initImage", "initContainer", "enableCRValidationWebhook", "enableVolumeExpansion",
		"allowUnsafeConfigurations", "unsafeFlags", "updateStrategy", "upgradeOptions", "ignoreAnnotations", "ignoreLabels",
		"passwordGenerationOptions", "storageScaling", "users", "tls",
		// PS / PSMDB / PG spell some of the same ideas differently.
		"clusterType", "sharding", "unmanaged", "updateOptions", "standby", "openshift", "imagePullPolicy",
	)
	add("database", "Database", "The database pods themselves: size, image, storage and configuration.",
		"pxc", "mysql", "replsets", "instances", "postgresVersion", "dataSource")
	add("proxy", "Proxy", "The front end in front of the database. cr.yaml runs one of these at a time.",
		"haproxy", "proxysql", "proxy", "router", "mongos", "pgBouncer", "sharding")
	add("backup", "Backup", "Backup storages, the schedule, and point-in-time recovery.", "backup", "backups", "restore")
	add("monitor", "Monitoring", "PMM, and the log collector sidecar.", "pmm", "logcollector", "logs", "monitoring")
	// Whatever is left, in CRD order.
	var rest []string
	for k := range keys {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	if len(rest) > 0 {
		groups = append(groups, crFormGroup{ID: "other", Label: "Other",
			Note:     "Everything else this operator's CRD declares. Fields land here when they are newer than DBCanvas's own grouping — they are no less editable for it.",
			Sections: rest})
	}
	return groups
}

// jsonSchema is the slice of an OpenAPI v3 schema this needs. Read as a generic map first and
// then into this, because a CRD's schema is recursive and only some of it is understood.
type jsonSchema struct {
	Type                 string                `json:"type"`
	Description          string                `json:"description"`
	Format               string                `json:"format"`
	Enum                 []any                 `json:"enum"`
	Default              any                   `json:"default"`
	Minimum              *float64              `json:"minimum"`
	Maximum              *float64              `json:"maximum"`
	Required             []string              `json:"required"`
	Properties           map[string]jsonSchema `json:"properties"`
	Items                *jsonSchema           `json:"items"`
	AdditionalProperties json.RawMessage       `json:"additionalProperties"`
	XPreserveUnknown     bool                  `json:"x-kubernetes-preserve-unknown-fields"`
	XIntOrString         bool                  `json:"x-kubernetes-int-or-string"`
}

// countProps is how many descendants a schema has, bounded — it stops counting once past the
// limit, so a huge subtree costs no more to reject than a small one costs to accept.
func countProps(s jsonSchema, limit int) int {
	n := 0
	var walk func(jsonSchema)
	walk = func(x jsonSchema) {
		if n > limit {
			return
		}
		for _, c := range x.Properties {
			n++
			walk(c)
		}
		if x.Items != nil {
			walk(*x.Items)
		}
	}
	walk(s)
	return n
}

// crFormBuild turns one schema node into a form field, pruning as it goes. See the file comment
// for the three reasons a subtree becomes a raw box.
//
// `expand` exempts a node from the size rule, and exists because the rule counts DESCENDANTS: a
// top-level section has thousands of them (the pxc section is 55 KB of schema) and would collapse
// into a raw box entirely — a "form" that is one JSON field per section. The same goes for the
// element schema of a list or a named map, which the user opened deliberately. Their children are
// still pruned one by one, which is where the size actually comes from.
func crFormBuildIn(name, path string, s jsonSchema, depth int, required, expand bool) crFormField {
	f := crFormField{
		Name: name, Path: path, Type: s.Type, Desc: strings.TrimSpace(s.Description),
		Format: s.Format, Default: s.Default, Min: s.Minimum, Max: s.Maximum, Required: required,
	}
	if f.Desc == "" {
		f.Help = crFormHelpFor(path, name)
	}
	for _, e := range s.Enum {
		f.Enum = append(f.Enum, fmt.Sprint(e))
	}
	// An int-or-string field (a Kubernetes quantity like "500m", or `1`) is a string box: the
	// form must not turn "500m" into 500.
	if s.XIntOrString {
		f.Type = "string"
		return f
	}
	raw := func(why string) crFormField {
		f.Type, f.Raw, f.RawWhy, f.Fields, f.Element = "raw", true, why, nil, nil
		return f
	}
	switch {
	case s.XPreserveUnknown && len(s.Properties) == 0:
		// Free-form by declaration: the operator itself does not know what goes in here.
		return raw("free-form")
	case crFormOpaque[name]:
		return raw("opaque")
	case depth >= crFormMaxDepth && (s.Type == "object" || s.Type == "array"):
		return raw("deep")
	}
	switch s.Type {
	case "object":
		if len(s.Properties) == 0 {
			// An object with additionalProperties is a map the user names the keys of —
			// `backup.storages` is the one everybody meets, and `resources.limits` is the one
			// that appears in every section. Sent with its element schema so the editor can
			// offer "add an entry" rather than a text box.
			//
			// XIntOrString counts as an element type: a resource quantity ("500m", "1G") has no
			// `type` in the schema at all, so requiring one turned every limits/requests block
			// in the custom resource into a JSON box.
			var ap jsonSchema
			if len(s.AdditionalProperties) > 0 && json.Unmarshal(s.AdditionalProperties, &ap) == nil &&
				(ap.Type != "" || ap.XIntOrString) {
				f.Map = true
				el := crFormBuildIn("", path+".*", ap, depth+1, false, true)
				f.Element = []crFormField{el}
				return f
			}
			return raw("free-form")
		}
		if !expand && countProps(s, crFormMaxProps) > crFormMaxProps {
			return raw("large")
		}
		req := map[string]bool{}
		for _, r := range s.Required {
			req[r] = true
		}
		for _, k := range crSortedKeys(s.Properties) {
			f.Fields = append(f.Fields, crFormBuildIn(k, path+"."+k, s.Properties[k], depth+1, req[k], false))
		}
		return f
	case "array":
		if s.Items == nil {
			return raw("free-form")
		}
		f.Items = s.Items.Type
		if s.Items.Type == "object" {
			// The element is expanded for the same reason a section is: it is a shape the user
			// asked to see. Its own children are pruned normally.
			el := crFormBuildIn("", path+"[]", *s.Items, depth+1, false, true)
			f.Element = []crFormField{el}
		}
		return f
	}
	return f
}

// crFormBuild is the ordinary entry point: a node pruned by every rule. Sections and element
// schemas go through crFormBuildIn with expand set.
func crFormBuild(name, path string, s jsonSchema, depth int, required bool) crFormField {
	return crFormBuildIn(name, path, s, depth, required, false)
}

func crSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- reading the CRD

// crdDoc is the part of a CustomResourceDefinition this reads.
type crdDoc struct {
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind     string `json:"kind"`
			Plural   string `json:"plural"`
			Singular string `json:"singular"`
		} `json:"names"`
		Versions []struct {
			Name    string `json:"name"`
			Served  bool   `json:"served"`
			Storage bool   `json:"storage"`
			Schema  struct {
				OpenAPIV3Schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

// k3dCRDNames maps a DBCanvas operator key to the CustomResourceDefinition holding its cluster
// object. The same four operators k3dCRState reads a state from — the two Helm-installed
// community PostgreSQL operators are not DBCanvas-managed cr.yaml and are not offered here.
var k3dCRDNames = map[string]string{
	"pxc":   "perconaxtradbclusters.pxc.percona.com",
	"ps":    "perconaservermysqls.ps.percona.com",
	"psmdb": "perconaservermongodbs.psmdb.percona.com",
	"pg":    "perconapgclusters.pgv2.percona.com",
}

// k3dCRForm reads the CRD and the live object, and builds the form model.
func (a *App) k3dCRForm(ctx context.Context, serverID string, cfg k3dConfig) (*crFormSchema, error) {
	crdName, ok := k3dCRDNames[cfg.Operator]
	if !ok {
		return nil, fmt.Errorf("the custom resource editor covers the Percona operators (PXC, PS, PSMDB, PG); this cluster runs %q", orDefault(cfg.Operator, "no operator"))
	}
	out, err := a.kubectl(ctx, serverID, "get", "crd", crdName, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("read the CustomResourceDefinition %s: %w", crdName, err)
	}
	var crd crdDoc
	if err := json.Unmarshal([]byte(out), &crd); err != nil {
		return nil, fmt.Errorf("parse %s: %w", crdName, err)
	}
	// The storage version is the one the object is actually persisted as, and the only one whose
	// schema is guaranteed to describe what a patch has to satisfy. A CRD carries a decade of
	// served versions beside it (v1-2-0 …), and building the form from the wrong one offers
	// fields this operator no longer has.
	var specRaw json.RawMessage
	version := ""
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			version = v.Name
			specRaw = v.Schema.OpenAPIV3Schema.Properties["spec"]
			break
		}
	}
	if version == "" || len(specRaw) == 0 {
		return nil, fmt.Errorf("%s declares no storage version with a spec schema", crdName)
	}
	var specSchema jsonSchema
	if err := json.Unmarshal(specRaw, &specSchema); err != nil {
		return nil, fmt.Errorf("parse the spec schema of %s: %w", crdName, err)
	}

	form := &crFormSchema{
		Kind: crd.Spec.Names.Kind, Resource: cfg.Operator, Group: crd.Spec.Group, Version: version,
		Cluster: cfg.ClusterName, Namespace: cfg.Namespace, Operator: cfg.Operator,
	}
	req := map[string]bool{}
	for _, r := range specSchema.Required {
		req[r] = true
	}
	keys := map[string]bool{}
	for _, k := range crSortedKeys(specSchema.Properties) {
		form.Sections = append(form.Sections, crFormBuildIn(k, k, specSchema.Properties[k], 1, req[k], true))
		keys[k] = true
	}
	form.Groups = crFormGroupsFor(cfg.Operator, keys)

	// The live object: its spec is what the form is populated from, and its state is what the
	// header reports — a patch applied to a cluster that is already in error is worth seeing.
	obj, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "get", cfg.Operator, cfg.ClusterName, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("read %s/%s: %w", cfg.Operator, cfg.ClusterName, err)
	}
	var live struct {
		Spec   any `json:"spec"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(obj), &live); err != nil {
		return nil, fmt.Errorf("parse %s/%s: %w", cfg.Operator, cfg.ClusterName, err)
	}
	form.Spec, form.Status = live.Spec, live.Status.State
	return form, nil
}

// ---------------------------------------------------------------- HTTP

func (a *App) handleK3DCRForm(w http.ResponseWriter, r *http.Request) {
	st, frame, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	_ = st
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	if cfg.ClusterName == "" {
		writeErr(w, http.StatusConflict, "cluster "+frame.Label+" has no custom resource yet — it is still deploying")
		return
	}
	form, err := a.k3dCRForm(r.Context(), dep.ContainerID, cfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, lastLines(err.Error(), 400))
		return
	}
	writeJSON(w, http.StatusOK, form)
}

// crPatchRequest is one edit from the form: a merge patch of `spec`, and whether to only check
// it. A removed field arrives as JSON null, which is what a merge patch means by "delete".
type crPatchRequest struct {
	Patch  map[string]any `json:"patch"`
	DryRun bool           `json:"dryRun"`
}

// handleK3DCRPatch applies (or only validates) an edit.
//
// Every apply is dry-run first, whatever the caller asked for. The API server validates against
// the same CRD the form was generated from, so a value the operator would refuse comes back as
// its own message — before the object is touched. Nothing else is done: the operator watches the
// object and reconciles it, and what it does about the change is what the user came to see.
func (a *App) handleK3DCRPatch(w http.ResponseWriter, r *http.Request) {
	_, frame, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var req crPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Patch) == 0 {
		writeErr(w, http.StatusBadRequest, "nothing to apply — the form has no changes")
		return
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	if _, isPercona := k3dCRDNames[cfg.Operator]; !isPercona {
		writeErr(w, http.StatusConflict, "cluster "+frame.Label+" does not run a Percona operator")
		return
	}
	body, err := json.Marshal(map[string]any{"spec": req.Patch})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "the patch does not encode: "+err.Error())
		return
	}
	patch := func(dry bool) (string, error) {
		args := []string{"-n", cfg.Namespace, "patch", cfg.Operator, cfg.ClusterName, "--type", "merge", "-p", string(body)}
		if dry {
			args = append(args, "--dry-run=server")
		}
		return a.kubectl(r.Context(), dep.ContainerID, args...)
	}
	if out, err := patch(true); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok": false, "dryRun": true,
			"error": lastLines(strings.TrimSpace(err.Error()+"\n"+out), 600),
		})
		return
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dryRun": true,
			"message": "the API server accepts this change — nothing has been applied"})
		return
	}
	if out, err := patch(false); err != nil {
		writeErr(w, http.StatusBadGateway, lastLines(strings.TrimSpace(err.Error()+"\n"+out), 600))
		return
	}
	// Hand back the object as it now stands, so the form reloads from what the API server
	// stored rather than from what the browser believes it sent — defaulting and pruning both
	// happen on the way in.
	form, err := a.k3dCRForm(r.Context(), dep.ContainerID, cfg)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "applied"})
		return
	}
	a.replLogln(dep.StackID, dep.NodeID, "custom resource patched from the editor: "+crPatchSummary(req.Patch))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "applied", "spec": form.Spec, "status": form.Status})
}

// crPatchSummary names the paths an edit touched, for the deployment log. The values are left
// out on purpose: a patch can carry a password (users, secrets), and the log is read by anyone
// who can see the node.
func crPatchSummary(patch map[string]any) string {
	var paths []string
	var walk func(string, map[string]any)
	walk = func(prefix string, m map[string]any) {
		for _, k := range crSortedKeys(m) {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			if child, ok := m[k].(map[string]any); ok && len(child) > 0 {
				walk(p, child)
				continue
			}
			paths = append(paths, p)
		}
	}
	walk("", patch)
	if len(paths) > 12 {
		return fmt.Sprintf("%s and %d more", strings.Join(paths[:12], ", "), len(paths)-12)
	}
	return strings.Join(paths, ", ")
}
