package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Deployment templates — a reusable canvas design, detached from the stack it came
// from. Two populations share one API: the built-in defaults (Go literals in
// templates_builtin.go, ids prefixed "builtin:") and the ones users save (rows in
// stack_templates). The picker treats them identically; only the write paths care,
// and they refuse to touch a built-in.
//
// A template is applied two ways. "New stack from template" copies the design
// verbatim into a fresh stack — node ids are unique per stack, so nothing needs
// rewriting. "Insert template" merges it into an open canvas, which does need id
// remapping, label renumbering and singleton handling; that runs in the designer
// (StackDesigner.jsx), where the label and singleton rules already live.

const builtinPrefix = "builtin:"

func templateIsBuiltin(id string) bool { return strings.HasPrefix(id, builtinPrefix) }

// templateExportVersion is stamped into exported files. It exists so a later format
// change is a clear "this file is from a newer DBCanvas" rather than a confusing
// parse failure against a document that happens to still be valid JSON.
const templateExportVersion = 1

// templateExport is the on-disk shape of an exported template.
type templateExport struct {
	Version     int             `json:"dbcanvasTemplate"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Design      json.RawMessage `json:"design"`
}

// --- sanitization ---

// templateSecretKeys are design fields blanked when a design becomes a template.
// Every one of them either falls back to a value from .env at deploy (so carrying a
// copy achieves nothing and leaks something) or is a generated secret.
//
// This is matched by JSON key name anywhere in the design tree rather than through
// designNode/designFrame, because those structs are ~300 lines across a dozen node
// families and grow with every new one — a typed sanitizer would go stale silently.
// A walk over the raw tree also reaches nested aioInstances[] for free.
// TestTemplateSanitizerCoversEverySecretField keeps this list honest.
var templateSecretKeys = map[string]bool{
	"rootPassword":  true, // MySQL-family root / PostgreSQL superuser / Valkey / PSMDB admin
	"adminPassword": true, // PMM
	"vncPassword":   true, // Ubuntu VNC desktop
	"secretKey":     true, // SeaweedFS S3 AWS_SECRET_ACCESS_KEY
	"ssPassword":    true, // Stock Market Sim, manual connection mode
	"ssDSN":         true, // …and its raw-DSN override, which embeds one
}

// templateHostKeys are fields that name something about *this* Docker host. A
// template is meant to travel — to another stack, another user, another install
// via an export file — and these do not travel with it.
var templateHostKeys = map[string]bool{
	"gdbCoreDir":    true, // host dir, and confined to this install's GDB_MOUNT_ROOT
	"gdbLibDir":     true, // likewise
	"devicePath":    true, // "" → auto-detect the Docker-root block device
	"k3dDevicePath": true, // likewise
}

// templateZeroIntKeys are fields reset to 0. A fixed host port is the one design
// choice that cannot survive being used twice: instantiate a template with a pinned
// exportHostPort into two stacks on one host and the second deploy collides. Zero
// means "auto-assign", which is what a reusable design wants.
var templateZeroIntKeys = map[string]bool{
	"exportHostPort": true,
}

// sanitizeTemplateDesign returns design with the secret, host-specific and
// host-port fields cleared, at any depth. Invalid JSON is returned unchanged —
// the caller validates the design separately and reports a better error than
// this function could.
func sanitizeTemplateDesign(design []byte) []byte {
	var tree any
	if err := json.Unmarshal(design, &tree); err != nil {
		return design
	}
	scrubbed := scrubTemplateTree(tree)
	out, err := json.Marshal(scrubbed)
	if err != nil {
		return design
	}
	return out
}

func scrubTemplateTree(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			switch {
			case templateSecretKeys[k], templateHostKeys[k]:
				t[k] = ""
			case templateZeroIntKeys[k]:
				t[k] = 0
			default:
				t[k] = scrubTemplateTree(child)
			}
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = scrubTemplateTree(child)
		}
		return t
	default:
		return v
	}
}

// --- lookup ---

// templateSummary fills in the node/frame counts the picker shows.
func templateSummary(t *StackTemplate) {
	var doc designDoc
	if json.Unmarshal(t.Design, &doc) != nil {
		return
	}
	t.Nodes = len(doc.Nodes)
	t.Frames = len(doc.Frames)
}

// loadTemplate resolves a wire id to a template — a built-in by slug, or a row by
// decimal id — and enforces read access on the latter (own, shared, or admin).
func (a *App) loadTemplate(id string, u User) (StackTemplate, error) {
	if templateIsBuiltin(id) {
		t, ok := findBuiltinTemplate(id)
		if !ok {
			return StackTemplate{}, sql.ErrNoRows
		}
		return t, nil
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return StackTemplate{}, sql.ErrNoRows
	}
	t, err := a.store.GetStackTemplate(n)
	if err != nil {
		return StackTemplate{}, err
	}
	if t.OwnerID != u.ID && !t.Shared && u.Role != RoleAdmin {
		return StackTemplate{}, errTemplateForbidden
	}
	return t, nil
}

var errTemplateForbidden = errors.New("not your template")

// loadOwnedTemplate is loadTemplate plus write access: a built-in is never
// writable, and a row needs ownership (or admin). It writes the error itself.
func (a *App) loadOwnedTemplate(w http.ResponseWriter, r *http.Request, u User) (StackTemplate, bool) {
	id := r.PathValue("id")
	if templateIsBuiltin(id) {
		writeErr(w, http.StatusBadRequest, "built-in templates cannot be modified — save a copy instead")
		return StackTemplate{}, false
	}
	t, err := a.loadTemplate(id, u)
	if err != nil {
		writeTemplateLoadErr(w, err)
		return StackTemplate{}, false
	}
	if t.OwnerID != u.ID && u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "not your template")
		return StackTemplate{}, false
	}
	return t, true
}

func writeTemplateLoadErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "template not found")
	case errors.Is(err, errTemplateForbidden):
		writeErr(w, http.StatusForbidden, "not your template")
	default:
		writeErr(w, http.StatusInternalServerError, "failed to read template")
	}
}

// --- handlers ---

func (a *App) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	out := []StackTemplate{}
	for _, t := range builtinTemplates() {
		templateSummary(&t)
		t.Design = nil // the list stays light; GET one to fetch a design
		out = append(out, t)
	}
	rows, err := a.store.ListStackTemplates(u.ID, u.Role == RoleAdmin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list templates")
		return
	}
	for _, t := range rows {
		// The list query omits design_json, so the counts need one read each. There
		// are tens of these, not thousands, and the picker wants the size up front.
		if full, err := a.store.GetStackTemplate(mustParseID(t.ID)); err == nil {
			t.Design = full.Design
			templateSummary(&t)
			t.Design = nil
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

func mustParseID(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (a *App) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	t, err := a.loadTemplate(r.PathValue("id"), u)
	if err != nil {
		writeTemplateLoadErr(w, err)
		return
	}
	templateSummary(&t)
	writeJSON(w, http.StatusOK, t)
}

// templateBody is the create/update payload. Design is required on create.
type templateBody struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Design      json.RawMessage `json:"design"`
}

// validTemplateDesign parses a design and rejects an empty one. It deliberately
// does NOT run the deploy-time validation (designIssues): a template is allowed to
// be an incomplete building block — half a topology someone finishes on the canvas
// — and the stack it is applied to is validated before it deploys anyway.
func validTemplateDesign(design []byte) (designDoc, error) {
	var doc designDoc
	if err := json.Unmarshal(design, &doc); err != nil {
		return doc, fmt.Errorf("the design is not valid JSON: %w", err)
	}
	if len(doc.Nodes) == 0 && len(doc.Frames) == 0 {
		return doc, errors.New("the design has no nodes or clusters to save")
	}
	return doc, nil
}

func (a *App) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body templateBody
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Untitled template"
	}
	if _, err := validTemplateDesign(body.Design); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	t, err := a.store.CreateStackTemplate(name, strings.TrimSpace(body.Description),
		strings.TrimSpace(body.Category), u.ID, sanitizeTemplateDesign(body.Design))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save template")
		return
	}
	templateSummary(&t)
	writeJSON(w, http.StatusCreated, t)
}

func (a *App) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	t, ok := a.loadOwnedTemplate(w, r, u)
	if !ok {
		return
	}
	var body templateBody
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Name falls back to the current one because a template cannot be nameless;
	// description and category are replaced outright, blank included, because
	// clearing them is a legitimate edit. Omitting Design keeps the stored one —
	// a metadata edit does not have the canvas to hand.
	name := t.Name
	if s := strings.TrimSpace(body.Name); s != "" {
		name = s
	}
	design := t.Design
	if len(body.Design) > 0 {
		if _, err := validTemplateDesign(body.Design); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		design = sanitizeTemplateDesign(body.Design)
	}
	if err := a.store.UpdateStackTemplate(mustParseID(t.ID), name,
		strings.TrimSpace(body.Description), strings.TrimSpace(body.Category), design); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update template")
		return
	}
	updated, err := a.store.GetStackTemplate(mustParseID(t.ID))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read template")
		return
	}
	templateSummary(&updated)
	writeJSON(w, http.StatusOK, updated)
}

func (a *App) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	t, ok := a.loadOwnedTemplate(w, r, u)
	if !ok {
		return
	}
	if err := a.store.DeleteStackTemplate(mustParseID(t.ID)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete template")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// handleShareTemplate publishes a template instance-wide. Admin-only: a shared
// template shows up in every user's picker, which is an instance-wide change, not
// a personal one.
func (a *App) handleShareTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "only an admin can publish a template instance-wide")
		return
	}
	t, ok := a.loadOwnedTemplate(w, r, u)
	if !ok {
		return
	}
	var body struct {
		Shared bool `json:"shared"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := a.store.SetStackTemplateShared(mustParseID(t.ID), body.Shared); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update template")
		return
	}
	updated, err := a.store.GetStackTemplate(mustParseID(t.ID))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read template")
		return
	}
	templateSummary(&updated)
	writeJSON(w, http.StatusOK, updated)
}

// handleExportTemplate serves a template as a downloadable file. Works for
// built-ins too — exporting one is how you get a starting point to edit offline.
func (a *App) handleExportTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	t, err := a.loadTemplate(r.PathValue("id"), u)
	if err != nil {
		writeTemplateLoadErr(w, err)
		return
	}
	doc := templateExport{
		Version:     templateExportVersion,
		Name:        t.Name,
		Description: t.Description,
		Category:    t.Category,
		Design:      t.Design,
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to encode template")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+templateFilename(t.Name)+`"`)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// templateFilename turns a template name into a safe download filename.
func templateFilename(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_':
			b.WriteRune('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "template"
	}
	return slug + ".dbcanvas-template.json"
}

// handleImportTemplate accepts an exported file and saves it as the caller's own
// template. The design is sanitized again on the way in — the file came from
// outside this installation and nothing about it can be assumed.
func (a *App) handleImportTemplate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var doc templateExport
	if err := decode(r, &doc); err != nil {
		writeErr(w, http.StatusBadRequest, "that file is not a DBCanvas template")
		return
	}
	if doc.Version == 0 {
		writeErr(w, http.StatusBadRequest, "that file is not a DBCanvas template (no dbcanvasTemplate version)")
		return
	}
	if doc.Version > templateExportVersion {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"that template was exported by a newer DBCanvas (format %d; this one reads %d)", doc.Version, templateExportVersion))
		return
	}
	if _, err := validTemplateDesign(doc.Design); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(doc.Name)
	if name == "" {
		name = "Imported template"
	}
	t, err := a.store.CreateStackTemplate(name, strings.TrimSpace(doc.Description),
		strings.TrimSpace(doc.Category), u.ID, sanitizeTemplateDesign(doc.Design))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to import template")
		return
	}
	templateSummary(&t)
	writeJSON(w, http.StatusCreated, t)
}
