package main

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

// stackfile_api.go — the stack file endpoints (stackfile.go): export one stack as a file,
// and apply a file, which creates the stack or brings an existing one to the file's
// design. Deploying stays a separate request to the deploy endpoint.

// handleExportStackFile answers with the stack as a stack file (YAML).
func (a *App) handleExportStackFile(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	b, err := encodeStackFile(st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.yaml"`, stackFileName(st.Name)))
	w.Write(b)
}

var stackFileNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// stackFileName is a stack name made safe for a file name.
func stackFileName(name string) string {
	s := stackFileNameRe.ReplaceAllString(name, "-")
	if s == "" || s == "-" {
		return "stack"
	}
	return s
}

// handleApplyStackFile takes {"file": "<stack file text>", "dryRun": bool} and answers
// with the plan: what is (or would be) created, added, removed and changed.
func (a *App) handleApplyStackFile(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		File   string `json:"file"`
		DryRun bool   `json:"dryRun"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sf, err := parseStackFile([]byte(body.File))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if code, why := a.tokenMayApply(r, u, &sf); why != "" {
		writeErr(w, code, why)
		return
	}
	if body.DryRun {
		p, _, err := a.planStackFile(r.Context(), u.ID, sf)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p)
		return
	}
	p, err := a.applyStackFile(r.Context(), u, sf)
	switch {
	case errors.Is(err, errStackBusy):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, ErrStackNameTaken):
		a.writeStackNameTaken(w, u.ID, sf.Name)
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.Action == "create" {
		a.recordStackCreator(r, p.StackID)
	}
	writeJSON(w, http.StatusOK, p)
}

// tokenMayApply holds a stack file to the request's token limits (tokenlimit.go): it may
// update only a stack within the token's reach, every stack keeps the token's labels, and
// the lifetime and node types must be ones the token may give. It answers 0 and "" when
// the file may be applied, with the token's labels merged into sf.
func (a *App) tokenMayApply(r *http.Request, u User, sf *stackFile) (int, string) {
	l, tokID := limitOf(r)
	if l == nil {
		return 0, ""
	}
	p, design, err := a.planStackFile(r.Context(), u.ID, *sf)
	if err != nil {
		return http.StatusBadRequest, err.Error()
	}
	if p.Action != "create" {
		st, err := a.store.GetStack(p.StackID)
		if err == nil && !l.allowsStack(tokID, st) {
			return http.StatusForbidden, fmt.Sprintf("this token is limited to %s, and the stack called %q is not one of them", l.describeStacks(), sf.Name)
		}
	}
	if p.TTL != "" {
		if why := tokenTTLRefusal(r, p.TTL); why != "" {
			return http.StatusForbidden, why
		}
	}
	if why := tokenDesignRefusal(r, design); why != "" {
		return http.StatusForbidden, why
	}
	if p.Action == "create" || sf.LabelsSet {
		labels, err := labelsForNewStack(r, sf.Labels)
		if err != nil {
			return http.StatusForbidden, err.Error()
		}
		if len(labels) > 0 {
			sf.Labels, sf.LabelsSet = labels, true
		}
	}
	return 0, ""
}
