package main

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// stacklabels.go — key=value labels on a stack, and selecting stacks by them.
//
// A label says what a stack is for in terms a program can match: the tool that made it, the run
// it belongs to, the machine that drives it (tool=ci, run=1234, host=builder-2). Listing filters on
// them (GET /api/stacks?label=run=1234), so whatever made a set of stacks can find exactly those
// again — to watch them or to tear them down — without depending on how they were named.
//
// Keys look like Kubernetes label keys: lowercase letters, digits and . _ / -, starting and ending
// alphanumeric, up to 63 characters. Values are up to 128 printable characters. A stack carries at
// most 32. Labels are not secret and are not sealed.

const maxStackLabels = 32

var labelKeyRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]{0,61}[a-z0-9])?$`)

// validateLabels checks a whole label set.
func validateLabels(labels map[string]string) error {
	if len(labels) > maxStackLabels {
		return fmt.Errorf("a stack carries at most %d labels, not %d", maxStackLabels, len(labels))
	}
	for k, v := range labels {
		if !labelKeyRe.MatchString(k) {
			return fmt.Errorf("label key %q: lowercase letters, digits and . _ / -, starting and ending with a letter or digit, at most 63 characters", k)
		}
		if len(v) > 128 {
			return fmt.Errorf("label %s: the value is longer than 128 characters", k)
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("label %s: the value has a control character", k)
			}
		}
	}
	return nil
}

// labelReq is one selector term: key=value, or key alone (the stack has the label at all).
type labelReq struct {
	Key, Value string
	HasValue   bool
}

// parseLabelSelector reads ?label= terms ("tool=ci", "run").
func parseLabelSelector(terms []string) ([]labelReq, error) {
	var out []labelReq
	for _, t := range terms {
		for _, part := range strings.Split(t, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			k, v, has := strings.Cut(part, "=")
			k = strings.TrimSpace(k)
			if !labelKeyRe.MatchString(k) {
				return nil, fmt.Errorf("label selector %q: %q is not a label key", part, k)
			}
			out = append(out, labelReq{Key: k, Value: v, HasValue: has})
		}
	}
	return out, nil
}

// matchesLabels reports whether a stack carries every selector term.
func matchesLabels(labels map[string]string, sel []labelReq) bool {
	for _, r := range sel {
		v, ok := labels[r.Key]
		if !ok || (r.HasValue && v != r.Value) {
			return false
		}
	}
	return true
}

// filterStacksByLabel keeps the stacks matching every term.
func filterStacksByLabel(stacks []Stack, sel []labelReq) []Stack {
	if len(sel) == 0 {
		return stacks
	}
	out := []Stack{}
	for _, st := range stacks {
		if matchesLabels(st.Labels, sel) {
			out = append(out, st)
		}
	}
	return out
}

// labelString renders labels sorted, k=v,k2=v2 — for logs and tables.
func labelString(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ",")
}

// handleSetStackLabels replaces a stack's labels: PUT /api/stacks/{id}/labels {"labels": {...}}.
func (a *App) handleSetStackLabels(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		Labels map[string]string `json:"labels"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateLabels(body.Labels); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A label-limited token cannot drop the labels that put the stack within its reach.
	if l, _ := limitOf(r); l != nil && !matchesLabels(body.Labels, selectorOf(l.Labels)) {
		writeErr(w, http.StatusForbidden, "this token is limited to stacks labelled "+labelString(l.Labels)+", and these labels would take the stack out of its reach")
		return
	}
	if err := a.store.SetStackLabels(st.ID, body.Labels); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the labels")
		return
	}
	if body.Labels == nil {
		body.Labels = map[string]string{}
	}
	a.stackEventByID(st.ID, "labels")
	writeJSON(w, http.StatusOK, map[string]any{"id": st.ID, "labels": body.Labels})
}

// selectorOf turns a label set into the selector requiring all of it.
func selectorOf(labels map[string]string) []labelReq {
	out := make([]labelReq, 0, len(labels))
	for k, v := range labels {
		out = append(out, labelReq{Key: k, Value: v, HasValue: true})
	}
	return out
}
