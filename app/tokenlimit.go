package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// tokenlimit.go — an API token that may touch only some stacks.
//
// A token's scope (read, write, admin) says what kind of call it may make. A limit says which
// stacks: a program that builds and tears down its own stacks gets a token that cannot reach any
// other stack of the account, and cannot reach anything else of the installation either.
//
//	{"ownStacks": true}                       the stacks this token created
//	{"labels": {"tool": "ci", "host": "b2"}}  the stacks carrying all of these labels
//
// Both may be given; a stack must then satisfy both. A limited token may call:
//
//   - any route under /api/stacks/{id}/… for a stack within its limit (nodes, deploy, exec, …);
//   - GET /api/stacks (the list is filtered to its limit), POST /api/stacks and
//     POST /api/stacks/compose;
//   - the read-only discovery routes a client needs to compose: the catalogues, the design
//     catalogue, templates, /api/me and /api/meta.
//
// Everything else is refused with 403, whatever the scope. A stack created with a label-limited
// token gets the token's labels, and such a token cannot set labels that drop them — so it can
// neither create a stack it then cannot see, nor move one out of its own reach.

// tokenLimit is a token's stack limit; nil means none.
type tokenLimit struct {
	OwnStacks bool              `json:"ownStacks,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

func (l *tokenLimit) empty() bool { return l == nil || (!l.OwnStacks && len(l.Labels) == 0) }

// describe is the limit in words, for error messages and the token list.
func (l *tokenLimit) describe() string {
	switch {
	case l.OwnStacks && len(l.Labels) > 0:
		return "the stacks it created labelled " + labelString(l.Labels)
	case l.OwnStacks:
		return "the stacks it created"
	}
	return "stacks labelled " + labelString(l.Labels)
}

func decodeTokenLimit(s string) *tokenLimit {
	if s == "" {
		return nil
	}
	var l tokenLimit
	if json.Unmarshal([]byte(s), &l) != nil || l.empty() {
		return nil
	}
	return &l
}

func encodeTokenLimit(l *tokenLimit) any {
	if l.empty() {
		return nil
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// allowsStack reports whether a stack is within the limit.
func (l *tokenLimit) allowsStack(tokenID int64, st Stack) bool {
	if l.empty() {
		return true
	}
	if l.OwnStacks && st.CreatedByToken != tokenID {
		return false
	}
	for k, v := range l.Labels {
		if st.Labels[k] != v {
			return false
		}
	}
	return true
}

// tokenLimitOpenPaths are the routes a limited token may call that name no stack. Exact
// method+path patterns, as the route table spells them.
var tokenLimitOpenPaths = map[string]bool{
	"GET /api/stacks":                true,
	"GET /api/stacks/events":         true,
	"POST /api/stacks":               true,
	"POST /api/stacks/compose":       true,
	"GET /api/stacks/compose/kinds":  true,
	"GET /api/stacks/design/catalog": true,
	"GET /api/me":                    true,
	"GET /api/templates":             true,
	"GET /api/templates/{id}":        true,
	"GET /api/templates/{id}/export": true,
	"GET /api/meta/endpoints":        true,
	"GET /api/cli/help":              true,
	"GET /api/meta/openapi.json":     true,
	"GET /api/cli/checksums":         true,
	"GET /api/cli/download":          true,
	"GET /api/setup/status":          true,
}

// tokenLimitAllows decides whether a limited token may make this call. It returns "" when it
// may, or the reason it may not.
func (a *App) tokenLimitAllows(rt apiRoute, r *http.Request, tok APIToken) string {
	l := tok.Limit
	if l.empty() {
		return ""
	}
	if strings.HasPrefix(rt.Path, "/api/stacks/{id}") {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return "" // the handler answers a malformed id
		}
		st, err := a.store.GetStack(id)
		if err != nil {
			return "" // and a missing stack
		}
		if !l.allowsStack(tok.ID, st) {
			return fmt.Sprintf("this token is limited to %s; stack %d is not one of them", l.describe(), id)
		}
		return ""
	}
	if tokenLimitOpenPaths[rt.Method+" "+rt.Path] || (rt.Method == "GET" && strings.HasPrefix(rt.Path, "/api/catalog/")) {
		return ""
	}
	return fmt.Sprintf("this token is limited to %s, and %s %s is outside that", l.describe(), rt.Method, rt.Path)
}

// limitOf is the request's token limit, if it came with a limited token.
func limitOf(r *http.Request) (*tokenLimit, int64) {
	p, ok := principalOf(r)
	if !ok || p.Token == nil || p.Token.Limit.empty() {
		return nil, 0
	}
	return p.Token.Limit, p.Token.ID
}

// filterStacksForToken keeps the stacks a limited token may see.
func filterStacksForToken(r *http.Request, stacks []Stack) []Stack {
	l, id := limitOf(r)
	if l == nil {
		return stacks
	}
	out := []Stack{}
	for _, st := range stacks {
		if l.allowsStack(id, st) {
			out = append(out, st)
		}
	}
	return out
}

// labelsForNewStack merges a label-limited token's labels into those asked for, so the token
// can see the stack it creates. A conflicting value is an error rather than silently replaced.
func labelsForNewStack(r *http.Request, asked map[string]string) (map[string]string, error) {
	l, _ := limitOf(r)
	if l == nil || len(l.Labels) == 0 {
		return asked, nil
	}
	out := map[string]string{}
	for k, v := range asked {
		out[k] = v
	}
	for k, v := range l.Labels {
		if have, ok := out[k]; ok && have != v {
			return nil, fmt.Errorf("this token is limited to stacks labelled %s, so label %s cannot be %q", labelString(l.Labels), k, have)
		}
		out[k] = v
	}
	return out, nil
}

// recordStackCreator remembers which token created a stack, for {"ownStacks": true}.
func (a *App) recordStackCreator(r *http.Request, stackID int64) {
	if p, ok := principalOf(r); ok && p.Token != nil {
		a.store.SetStackCreatedByToken(stackID, p.Token.ID)
	}
}
