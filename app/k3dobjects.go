package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// k3dobjects.go — the Secrets and ConfigMaps of a Kubernetes frame, as an editor.
//
// The cr.yaml editor next door (k3dcrform.go) covers what the *operator* is told to do. This
// covers the two objects it is told it with, and they are where a surprising amount of a Percona
// cluster actually lives:
//
//   - `<cluster>-secrets` holds root, xtrabackup, monitor, operator, replication and proxyadmin.
//     Changing a password here is how the operator's credential rotation is exercised at all.
//   - `internal-<cluster>` is the operator's own copy of those, and the two disagreeing is a
//     specific, reportable failure people come to a lab to reproduce.
//   - `<cluster>-ssl` / `-ssl-internal` are the TLS chains — the thing to look at when
//     cert-manager is on the cluster (k3dcertmanager.go) and when it is not.
//   - a ConfigMap named `<cluster>-pxc` (or `-haproxy`, `-mongod`, …) is the tuning file. Editing
//     my.cnf in a text box and watching the operator roll it out is the lab.
//
// Everything is read and written through the k3s server node's own kubectl, like the rest of the
// Kubernetes work in this app. Two rules shape the whole file:
//
//  1. VALUES ARE NOT LISTED. Listing many objects returns key names and sizes, never contents;
//     values come only from a read of one named object. A panel that dumps every password in the
//     namespace to build a list is a panel nobody can open in a screen-share.
//  2. BINARY IS NOT ROUND-TRIPPED. A value that is not valid UTF-8 text is reported as binary
//     with its size and nothing else, and the editor never sends one back — a DER blob through a
//     browser text box is corrupted silently, which is far worse than not offering it. A caller
//     that names such a key in `set` explicitly means to replace it, and that is allowed: what is
//     refused is the accident, not the intent.

// k3dObjKinds are the two kinds this editor handles, and the kubectl resource name for each.
var k3dObjKinds = map[string]string{"secret": "secrets", "configmap": "configmaps"}

// A Kubernetes data key: letters, digits, '-', '_' and '.'.
var k3dDataKeyRe = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)

// k3dObjEntry is one key of a Secret or ConfigMap. Value is filled only on a single-object read,
// and only when the value is text: see the two rules above.
type k3dObjEntry struct {
	Key    string `json:"key"`
	Size   int    `json:"size"`
	Binary bool   `json:"binary,omitempty"`
	Value  string `json:"value,omitempty"`
}

// k3dObject is one Secret or ConfigMap, flattened to what an editor needs.
type k3dObject struct {
	Kind      string            `json:"kind"` // secret | configmap
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Type      string            `json:"type,omitempty"` // Secrets only: Opaque, kubernetes.io/tls, …
	Immutable bool              `json:"immutable,omitempty"`
	CreatedAt string            `json:"createdAt,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Entries   []k3dObjEntry     `json:"entries"`
}

// k3dRawObject is the part of a Secret or ConfigMap this file reads. A Secret's `data` values are
// base64; a ConfigMap's are plain text, with anything binary in `binaryData` instead.
type k3dRawObject struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		CreationTimestamp string            `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
	} `json:"metadata"`
	Type       string            `json:"type"`
	Immutable  *bool             `json:"immutable"`
	Data       map[string]string `json:"data"`
	BinaryData map[string]string `json:"binaryData"`
}

// k3dObjTextual reports whether a value can be shown and edited as text. utf8.Valid is not
// enough on its own: a DER certificate is frequently valid UTF-8 by accident, and control
// characters are what give it away.
func k3dObjTextual(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// k3dObjectFrom flattens one decoded object. withValues=false leaves every value out, which is
// what the list uses.
func k3dObjectFrom(kind string, raw k3dRawObject, withValues bool) k3dObject {
	obj := k3dObject{
		Kind: kind, Namespace: raw.Metadata.Namespace, Name: raw.Metadata.Name,
		Type: raw.Type, CreatedAt: raw.Metadata.CreationTimestamp, Labels: raw.Metadata.Labels,
	}
	if raw.Immutable != nil {
		obj.Immutable = *raw.Immutable
	}
	add := func(key string, b []byte, forceBinary bool) {
		e := k3dObjEntry{Key: key, Size: len(b), Binary: forceBinary || !k3dObjTextual(b)}
		if withValues && !e.Binary {
			e.Value = string(b)
		}
		obj.Entries = append(obj.Entries, e)
	}
	for _, k := range sortedKeys(raw.Data) {
		v := raw.Data[k]
		if kind == "secret" {
			// A Secret's data is base64. One that does not decode is not something to guess
			// at — report it as binary of the size it claims and refuse to edit it.
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				add(k, []byte(v), true)
				continue
			}
			add(k, b, false)
			continue
		}
		add(k, []byte(v), false)
	}
	for _, k := range sortedKeys(raw.BinaryData) {
		b, err := base64.StdEncoding.DecodeString(raw.BinaryData[k])
		if err != nil {
			b = []byte(raw.BinaryData[k])
		}
		add(k, b, true) // binaryData is binary by definition, whatever the bytes look like
	}
	sort.SliceStable(obj.Entries, func(i, j int) bool { return obj.Entries[i].Key < obj.Entries[j].Key })
	return obj
}

// parseK3DObjectList turns `kubectl get secrets -o json` into the list the picker is built from.
// Pure, so the decoding is testable without a cluster.
func parseK3DObjectList(kind string, out []byte) ([]k3dObject, error) {
	var doc struct {
		Items []k3dRawObject `json:"items"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("kubectl returned something that is not a %s list: %w", kind, err)
	}
	objs := make([]k3dObject, 0, len(doc.Items))
	for _, it := range doc.Items {
		objs = append(objs, k3dObjectFrom(kind, it, false))
	}
	sort.SliceStable(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })
	return objs, nil
}

// parseK3DObject turns `kubectl get secret <name> -o json` into the object being edited, values
// included.
func parseK3DObject(kind string, out []byte) (k3dObject, error) {
	var raw k3dRawObject
	if err := json.Unmarshal(out, &raw); err != nil {
		return k3dObject{}, fmt.Errorf("kubectl returned something that is not a %s: %w", kind, err)
	}
	if raw.Metadata.Name == "" {
		return k3dObject{}, fmt.Errorf("no such %s", kind)
	}
	return k3dObjectFrom(kind, raw, true), nil
}

// k3dObjPatch is the merge patch for one edit: the changed keys, encoded the way the kind
// requires, and a JSON null for each key being removed. Pure — the encoding is the part worth
// testing, since a Secret's values are base64 and a ConfigMap's are not.
func k3dObjPatch(kind string, set map[string]string, remove []string) ([]byte, error) {
	data := map[string]any{}
	for _, k := range sortedKeys(set) {
		if !k3dDataKeyRe.MatchString(k) {
			return nil, fmt.Errorf("%q is not a valid key", k)
		}
		if kind == "secret" {
			data[k] = base64.StdEncoding.EncodeToString([]byte(set[k]))
			continue
		}
		data[k] = set[k]
	}
	for _, k := range remove {
		if !k3dDataKeyRe.MatchString(k) {
			return nil, fmt.Errorf("%q is not a valid key", k)
		}
		data[k] = nil
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("nothing to apply — no key changed")
	}
	return json.Marshal(map[string]any{"data": data})
}

// kubectlQuiet runs kubectl and reports a failure with the command's OUTPUT ONLY, never its
// arguments. That is not tidiness: the patch is an argument, half of what this file patches is a
// password, and a.kubectl puts the whole argv in its error — which would put a new credential
// (base64, which is not a secret) into an error box, a deployment log, and whatever the browser
// does with them. The API server's own message is the useful half anyway.
func (a *App) kubectlQuiet(ctx context.Context, serverID string, args ...string) (string, error) {
	res, err := a.engCtx(ctx).Exec(ctx, serverID, append([]string{"kubectl"}, args...),
		[]string{"KUBECONFIG=" + k3dKubeconfig})
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(res.Stdout + res.Stderr)
	if res.Code != 0 {
		if out == "" {
			out = fmt.Sprintf("kubectl exited %d", res.Code)
		}
		return out, fmt.Errorf("%s", out)
	}
	return out, nil
}

// ------------------------------------------------------------------------ handlers

// k3dObjRequestKind reads and checks the ?kind= parameter.
func k3dObjRequestKind(r *http.Request) (kind, resource string, err error) {
	kind = strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind == "" {
		kind = "secret"
	}
	resource, ok := k3dObjKinds[kind]
	if !ok {
		return "", "", fmt.Errorf("kind must be secret or configmap")
	}
	return kind, resource, nil
}

// k3dObjNamespace is the namespace to work in: the one asked for, or the cluster's own.
func k3dObjNamespace(r *http.Request, cfg k3dConfig) (string, error) {
	ns := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if ns == "" {
		ns = cfg.Namespace
	}
	if ns == "" {
		ns = "default"
	}
	if !k3dDNSSubdomain.MatchString(ns) {
		return "", fmt.Errorf("namespace %q is not a Kubernetes name", ns)
	}
	return ns, nil
}

// k3dNamespaces lists the cluster's namespaces, for the editor's picker.
func (a *App) k3dNamespaces(ctx context.Context, serverID string) []string {
	out, err := a.kubectl(ctx, serverID, "get", "namespaces", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	sort.Strings(names)
	return names
}

// handleK3DObjects lists the Secrets or ConfigMaps of one namespace — names, types and key
// names, never values.
func (a *App) handleK3DObjects(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	kind, resource, err := k3dObjRequestKind(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ns, err := k3dObjNamespace(r, cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := a.kubectl(r.Context(), dep.ContainerID, "-n", ns, "get", resource, "-o", "json")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list "+resource+": "+lastLines(err.Error(), 200))
		return
	}
	objs, err := parseK3DObjectList(kind, []byte(out))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": kind, "namespace": ns, "namespaces": a.k3dNamespaces(r.Context(), dep.ContainerID),
		"clusterNamespace": cfg.Namespace, "objects": objs,
	})
}

// handleK3DObject reads one Secret or ConfigMap, with its text values.
func (a *App) handleK3DObject(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	kind, resource, err := k3dObjRequestKind(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ns, err := k3dObjNamespace(r, cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if !k3dDNSSubdomain.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "name is not a Kubernetes name")
		return
	}
	out, err := a.kubectl(r.Context(), dep.ContainerID, "-n", ns, "get", resource, name, "-o", "json")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "read "+kind+" "+name+": "+lastLines(err.Error(), 200))
		return
	}
	obj, err := parseK3DObject(kind, []byte(out))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

// k3dObjPatchRequest is one edit: the keys to write, the keys to drop, and whether to stop after
// the dry-run.
type k3dObjPatchRequest struct {
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Set       map[string]string `json:"set"`
	Remove    []string          `json:"remove"`
	DryRun    bool              `json:"dryRun"`
}

// handleK3DObjectPatch applies (or only validates) an edit to a Secret or ConfigMap.
//
// Every apply is dry-run against the API server first, whatever the caller asked for — the same
// contract the cr.yaml editor has, and it catches the two things that actually happen here: an
// immutable object, and a key the object's own type will not accept (a kubernetes.io/tls Secret
// insists on tls.crt and tls.key).
//
// Nothing is restarted afterwards. What a changed Secret or ConfigMap *does* is the operator's
// business — a rolling restart, a credential rotation, or nothing at all — and watching which is
// usually the reason for the edit.
func (a *App) handleK3DObjectPatch(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	var req k3dObjPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resource, known := k3dObjKinds[req.Kind]
	if !known {
		writeErr(w, http.StatusBadRequest, "kind must be secret or configmap")
		return
	}
	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = cfg.Namespace
	}
	if !k3dDNSSubdomain.MatchString(ns) || !k3dDNSSubdomain.MatchString(strings.TrimSpace(req.Name)) {
		writeErr(w, http.StatusBadRequest, "namespace and name must be Kubernetes names")
		return
	}
	name := strings.TrimSpace(req.Name)
	body, err := k3dObjPatch(req.Kind, req.Set, req.Remove)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	patch := func(dry bool) (string, error) {
		args := []string{"-n", ns, "patch", resource, name, "--type", "merge", "-p", string(body)}
		if dry {
			args = append(args, "--dry-run=server")
		}
		return a.kubectlQuiet(r.Context(), dep.ContainerID, args...)
	}
	if out, err := patch(true); err != nil {
		_ = err // the message is the output; the argv carries the value being written
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok": false, "dryRun": true, "error": lastLines(out, 600),
		})
		return
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dryRun": true,
			"message": "the API server accepts this change — nothing has been applied"})
		return
	}
	if out, err := patch(false); err != nil {
		_ = err
		writeErr(w, http.StatusBadGateway, lastLines(out, 600))
		return
	}
	// The keys, never the values: this log is read by anyone who can see the node, and half of
	// what is edited here is a password.
	a.replLogln(dep.StackID, dep.NodeID, fmt.Sprintf("%s %s/%s edited from the panel: %s",
		req.Kind, ns, name, k3dObjChangeSummary(req.Set, req.Remove)))

	out, err := a.kubectl(r.Context(), dep.ContainerID, "-n", ns, "get", resource, name, "-o", "json")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "applied"})
		return
	}
	obj, perr := parseK3DObject(req.Kind, []byte(out))
	if perr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "applied"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "applied", "object": obj})
}

// k3dObjChangeSummary names what an edit touched, for the deployment log.
func k3dObjChangeSummary(set map[string]string, remove []string) string {
	var parts []string
	if keys := sortedKeys(set); len(keys) > 0 {
		parts = append(parts, "set "+strings.Join(keys, ", "))
	}
	if len(remove) > 0 {
		rm := append([]string(nil), remove...)
		sort.Strings(rm)
		parts = append(parts, "removed "+strings.Join(rm, ", "))
	}
	return strings.Join(parts, "; ")
}
