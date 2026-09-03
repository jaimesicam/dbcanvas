package main

import (
	"net/http"
	"regexp"
	"strings"
)

// apimeta.go — the API describing itself.
//
// Two views over the same slice in api_routes.go. `/api/meta/endpoints` is the one
// the API page renders: grouped, ordered, and carrying the access each endpoint
// needs so the page can say up front what the caller's token will and will not
// reach. `/api/meta/openapi.json` is the same surface as an OpenAPI 3.1 document,
// which costs almost nothing once the table exists and makes the whole thing usable
// from a generated client, Postman or Bruno.
//
// Neither endpoint reveals anything a signed-in account could not discover by
// clicking around the app, but both still require a session: the route list names
// every feature this installation has, and an unauthenticated inventory of that is
// a courtesy to a scanner, not to a user.

// pathParamRe matches a Go 1.22 mux wildcard: {id}, {nid}, {username}.
var pathParamRe = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

// paramDoc explains the wildcards, once, rather than per route. Every route that
// uses {id} means the same thing by it — except the handful listed in
// paramDocByPrefix, where {id} is a capture or a bundle rather than a stack.
var paramDoc = map[string]string{
	"id":       "The stack's numeric id.",
	"nid":      "The node's id within the stack's design (its `id` on the canvas, not its hostname).",
	"fid":      "The frame's id within the stack's design — a cluster or Kubernetes frame.",
	"aid":      "A kept pt-stalk archive's numeric id.",
	"job":      "A Data Generator job id, as returned when the job was started.",
	"inst":     "An instance name inside an All-in-One node.",
	"no":       "A packet's sequence number within its capture.",
	"src":      "A source's index within a log bundle.",
	"user":     "The database username the certificate was issued for.",
	"username": "The Kubernetes RBAC username.",
	"stepId":   "The lab step's id.",
}

// paramDocByPrefix overrides paramDoc for route families where a wildcard name is
// reused for something else. Longest matching prefix wins.
var paramDocByPrefix = map[string]map[string]string{
	"/api/users":                {"id": "The account's numeric id."},
	"/api/templates":            {"id": "The template's id — a number, or `builtin:<name>` for one that ships with the app."},
	"/api/labs":                 {"id": "The lab's id."},
	"/api/queryrun/runs":        {"id": "The query run's id."},
	"/api/benchmark/runs":       {"id": "The benchmark run's id."},
	"/api/pktinspect/captures":  {"id": "The capture's id."},
	"/api/logsummary/bundles":   {"id": "The log bundle's id."},
	"/api/notifications":        {"id": "The notification's numeric id."},
	"/api/stalksummary/archive": {"aid": "A kept pt-stalk archive's numeric id."},
}

// endpointParam is one path wildcard.
type endpointParam struct {
	Name string `json:"name"`
	Help string `json:"help"`
}

// endpointDoc is one route as the API page sees it.
type endpointDoc struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Group    string          `json:"group"`
	Summary  string          `json:"summary"`
	Auth     string          `json:"auth"`  // public | user | admin
	Scope    string          `json:"scope"` // read | write | admin — the least a token needs
	Media    string          `json:"media,omitempty"`
	ReadOnly bool            `json:"readOnly,omitempty"`
	Params   []endpointParam `json:"params,omitempty"`
}

// endpointGroup is one heading on the API page.
type endpointGroup struct {
	Name      string        `json:"name"`
	Endpoints []endpointDoc `json:"endpoints"`
}

// describeRoute turns a route into its documented form.
func describeRoute(rt apiRoute) endpointDoc {
	d := endpointDoc{
		Method:   rt.Method,
		Path:     rt.Path,
		Group:    rt.Group,
		Summary:  rt.Summary,
		Auth:     rt.Auth.String(),
		Scope:    routeScope(rt),
		Media:    string(rt.Media),
		ReadOnly: rt.ReadOnly,
	}
	for _, mt := range pathParamRe.FindAllStringSubmatch(rt.Path, -1) {
		d.Params = append(d.Params, endpointParam{Name: mt[1], Help: paramHelp(rt.Path, mt[1])})
	}
	return d
}

// paramHelp resolves a wildcard's explanation, preferring the most specific
// route-family override.
func paramHelp(path, name string) string {
	best := ""
	for prefix, overrides := range paramDocByPrefix {
		if !strings.HasPrefix(path, prefix) || len(prefix) <= len(best) {
			continue
		}
		if _, ok := overrides[name]; ok {
			best = prefix
		}
	}
	if best != "" {
		return paramDocByPrefix[best][name]
	}
	return paramDoc[name]
}

// groupedEndpoints returns every route grouped and ordered for presentation:
// apiGroupOrder first, then any group nobody thought to add to it.
func groupedEndpoints() []endpointGroup {
	byGroup := map[string][]endpointDoc{}
	var seen []string
	for _, rt := range apiRoutes() {
		if _, ok := byGroup[rt.Group]; !ok {
			seen = append(seen, rt.Group)
		}
		byGroup[rt.Group] = append(byGroup[rt.Group], describeRoute(rt))
	}
	ordered := make([]endpointGroup, 0, len(byGroup))
	taken := map[string]bool{}
	for _, name := range apiGroupOrder {
		if eps, ok := byGroup[name]; ok {
			ordered = append(ordered, endpointGroup{Name: name, Endpoints: eps})
			taken[name] = true
		}
	}
	for _, name := range seen {
		if !taken[name] {
			ordered = append(ordered, endpointGroup{Name: name, Endpoints: byGroup[name]})
		}
	}
	return ordered
}

func (a *App) handleAPIEndpoints(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": appVersion,
		// What the caller can reach, so the page can grey out the rest rather than
		// letting somebody build a curl line that was always going to 403.
		"role":   u.Role,
		"scopes": scopesFor(u.Role),
		"total":  len(apiRoutes()),
		"groups": groupedEndpoints(),
	})
}

// ------------------------------------------------------------- OpenAPI

// operationID builds a stable, readable operationId from the method and path:
// GET /api/stacks/{id}/nodes/{nid} -> getStacksByIdNodesByNid.
//
// The wildcards have to be in there. Dropping them reads better in isolation, but
// it collides — GET /api/stacks and GET /api/stacks/{id} both reduce to getStacks,
// and six such pairs exist in this table. A generated client would then have two
// methods of the same name, so TestOpenAPIOperationIDsAreUnique guards it.
func operationID(rt apiRoute) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(rt.Method))
	for _, seg := range strings.Split(strings.TrimPrefix(rt.Path, "/api/"), "/") {
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "{") {
			name := strings.Trim(seg, "{}")
			seg = "by" + strings.ToUpper(name[:1]) + name[1:]
		}
		seg = strings.NewReplacer(".", "", "-", "", "_", "").Replace(seg)
		if seg == "" {
			continue
		}
		b.WriteString(strings.ToUpper(seg[:1]))
		b.WriteString(seg[1:])
	}
	return b.String()
}

// openAPIDocument renders the route table as OpenAPI 3.1.
//
// Request and response bodies stay deliberately untyped. Writing 206 JSON schemas
// by hand would be a second description of the app that could disagree with the
// first, which is exactly what this file exists to avoid; what a generated client
// actually needs from us — the paths, the methods, the parameters, the auth and one
// sentence each — is all here and all true.
func openAPIDocument() map[string]any {
	freeForm := map[string]any{"type": "object", "additionalProperties": true}
	jsonBody := map[string]any{
		"content": map[string]any{"application/json": map[string]any{"schema": freeForm}},
	}

	paths := map[string]any{}
	for _, rt := range apiRoutes() {
		op := map[string]any{
			"tags":        []string{rt.Group},
			"summary":     rt.Summary,
			"operationId": operationID(rt),
			"responses": map[string]any{
				"200": map[string]any{"description": "Success.", "content": jsonBody["content"]},
				"401": map[string]any{"description": "Not signed in, or the token is invalid, expired or revoked."},
				"403": map[string]any{"description": "Signed in, but not permitted — not your stack, not an admin, or the token's scope is too narrow."},
			},
		}
		if rt.Auth == authPublic {
			op["security"] = []any{}
		}
		if rt.ReadOnly {
			op["description"] = rt.Summary + " Changes nothing, so a read-scoped token may call it."
		}
		switch rt.Media {
		case mediaSSE:
			op["responses"].(map[string]any)["200"] = map[string]any{
				"description": "An event stream; each event's data is one JSON notification.",
				"content":     map[string]any{"text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}}},
			}
		case mediaWebSocket:
			op["responses"].(map[string]any)["101"] = map[string]any{"description": "Switching protocols — the connection becomes a WebSocket."}
		case mediaDownload:
			op["responses"].(map[string]any)["200"] = map[string]any{
				"description": "The file, as an attachment.",
				"content":     map[string]any{"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}},
			}
		case mediaMultipart:
			op["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{"multipart/form-data": map[string]any{
					"schema": map[string]any{"type": "object", "additionalProperties": true},
				}},
			}
		}
		if rt.Media == mediaJSON && rt.Method != http.MethodGet {
			op["requestBody"] = jsonBody
		}

		var params []any
		for _, mt := range pathParamRe.FindAllStringSubmatch(rt.Path, -1) {
			params = append(params, map[string]any{
				"name":        mt[1],
				"in":          "path",
				"required":    true,
				"description": paramHelp(rt.Path, mt[1]),
				"schema":      map[string]any{"type": "string"},
			})
		}
		if params != nil {
			op["parameters"] = params
		}

		item, ok := paths[rt.Path].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[rt.Path] = item
		}
		item[strings.ToLower(rt.Method)] = op
	}

	tags := make([]any, 0, len(apiGroupOrder))
	for _, g := range groupedEndpoints() {
		tags = append(tags, map[string]any{"name": g.Name})
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "DBCanvas",
			"version": appVersion,
			"description": "Everything DBCanvas does over HTTP. Authenticate with an API token " +
				"(Settings → API → Tokens) and send it as `Authorization: Bearer dbc_…`. " +
				"A session cookie from the web UI works too.",
		},
		"servers": []any{map[string]any{"url": "/", "description": "This installation."}},
		"tags":    tags,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{
					"type":        "http",
					"scheme":      "bearer",
					"description": "A DBCanvas API token, `dbc_` followed by 43 characters.",
				},
			},
		},
		"security": []any{map[string]any{"bearerAuth": []any{}}},
		"paths":    paths,
	}
}

func (a *App) handleAPIOpenAPI(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, openAPIDocument())
}
