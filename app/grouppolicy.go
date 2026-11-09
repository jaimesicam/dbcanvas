package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// grouppolicy.go — limits on what each security group's accounts may deploy, set by an
// administrator next to the features each group may use (secgroups.go).
//
//	maxTtl           the longest lifetime a stack may be given or extended to ("" = any,
//	                 including infinity); "24h" means no stack of the group outlives a day
//	deniedNodeTypes  node and cluster types the group may not deploy ("k3d", "kubeadm", …)
//	maxStacks        how many stacks one account of the group may have (0 = any)
//	maxContainers    how many nodes one account may have deployed across its stacks (0 = any)
//
// Every field defaults to no limit, so an installation that never sets one behaves as before.
// They are enforced on the server: the TTL on create, compose and extend; node types by
// validation, which deploy and compose already obey; the stack count on create and compose; the
// container count on deploy.

const settingGroupPolicies = "groupPolicies"

// GroupPolicy is one group's limits.
type GroupPolicy struct {
	MaxTTL          string   `json:"maxTtl,omitempty"`
	DeniedNodeTypes []string `json:"deniedNodeTypes,omitempty"`
	MaxStacks       int      `json:"maxStacks,omitempty"`
	MaxContainers   int      `json:"maxContainers,omitempty"`
}

// ttlOrder ranks the TTL choices, shortest first.
var ttlOrder = []string{"2h", "4h", "8h", "24h", "2w", ttlInfinity}

func ttlRank(t string) int {
	for i, v := range ttlOrder {
		if v == t {
			return i
		}
	}
	return -1
}

// groupPolicies is the stored configuration; a group with nothing stored has no limits.
func (a *App) groupPolicies() map[string]GroupPolicy {
	out := map[string]GroupPolicy{}
	if v, err := a.store.AppSetting(settingGroupPolicies); err == nil && v != "" {
		json.Unmarshal([]byte(v), &out)
	}
	for _, g := range groupOrder {
		if _, ok := out[g]; !ok {
			out[g] = GroupPolicy{}
		}
	}
	return out
}

// policyFor is the policy of an account's group.
func (a *App) policyFor(u User) GroupPolicy { return a.groupPolicies()[groupOf(u)] }

// ownerPolicy is the policy that governs a stack: its owner's group's.
func (a *App) ownerPolicy(st Stack) (GroupPolicy, User) {
	u, err := a.store.GetUser(st.OwnerID)
	if err != nil {
		return GroupPolicy{}, User{}
	}
	return a.policyFor(u), u
}

// ttlAllowed reports whether a TTL is within the policy, and why not.
func (p GroupPolicy) ttlAllowed(ttl string) (bool, string) {
	if p.MaxTTL == "" || ttlRank(ttl) <= ttlRank(p.MaxTTL) {
		return true, ""
	}
	return false, fmt.Sprintf("your security group allows stacks to live at most %s, not %s", p.MaxTTL, ttl)
}

// expiryAllowed reports whether an absolute expiry (nil = never) is within the policy's TTL.
func (p GroupPolicy) expiryAllowed(exp *string) (bool, string) {
	if p.MaxTTL == "" || p.MaxTTL == ttlInfinity {
		return true, ""
	}
	if exp == nil {
		return false, fmt.Sprintf("your security group allows stacks to live at most %s, so one cannot be kept forever", p.MaxTTL)
	}
	t, err := time.Parse(time.RFC3339, *exp)
	if err != nil || t.After(time.Now().Add(ttlDurations[p.MaxTTL]+time.Minute)) {
		return false, fmt.Sprintf("your security group allows stacks to live at most %s from now", p.MaxTTL)
	}
	return true, ""
}

// nodeTypeIssues are validation errors for the node and cluster types a stack's owner may not
// deploy.
func (a *App) nodeTypeIssues(st Stack, doc designDoc) []issue {
	p, _ := a.ownerPolicy(st)
	if len(p.DeniedNodeTypes) == 0 {
		return nil
	}
	denied := map[string]bool{}
	for _, t := range p.DeniedNodeTypes {
		denied[t] = true
	}
	var out []issue
	seen := map[string]bool{}
	add := func(typ, label string) {
		if denied[typ] && !seen[typ+"/"+label] {
			seen[typ+"/"+label] = true
			out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s (%s): your security group may not deploy this type", label, typ)})
		}
	}
	for _, f := range doc.Frames {
		add(f.Type, f.Label)
	}
	for _, n := range doc.Nodes {
		if n.FrameID == "" {
			add(n.Type, n.Label)
		}
	}
	return out
}

// stackCountAllowed reports whether an account may create another stack.
func (a *App) stackCountAllowed(u User) (bool, string) {
	p := a.policyFor(u)
	if p.MaxStacks <= 0 {
		return true, ""
	}
	stacks, err := a.store.ListStacks(u.ID, false)
	if err != nil || len(stacks) < p.MaxStacks {
		return true, ""
	}
	return false, fmt.Sprintf("your security group allows %s per account, and you have %d — delete one first", fmt.Sprintf("%d %s", p.MaxStacks, plural(p.MaxStacks, "stack", "stacks")), len(stacks))
}

// containerCountAllowed reports whether deploying a stack keeps its owner within the container
// limit: the nodes deployed in the owner's other stacks plus this stack's design.
func (a *App) containerCountAllowed(st Stack, doc designDoc) (bool, string) {
	p, u := a.ownerPolicy(st)
	if p.MaxContainers <= 0 {
		return true, ""
	}
	others := 0
	stacks, _ := a.store.ListStacks(u.ID, false)
	for _, o := range stacks {
		if o.ID == st.ID {
			continue
		}
		deps, _ := a.store.ListDeployments(o.ID)
		for _, d := range deps {
			if d.ContainerID != "" {
				others++
			}
		}
	}
	mine := len(doc.Nodes)
	if others+mine <= p.MaxContainers {
		return true, ""
	}
	return false, fmt.Sprintf("your security group allows %s per account; your other stacks run %d and this one has %d", fmt.Sprintf("%d %s", p.MaxContainers, plural(p.MaxContainers, "deployed node", "deployed nodes")), others, mine)
}

// handleGetGroupPolicies is GET /api/security/policies.
func (a *App) handleGetGroupPolicies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"policies": a.groupPolicies(), "ttls": ttlOrder})
}

// handleSetGroupPolicies is PUT /api/security/policies: {"policies": {group: GroupPolicy}}. A
// group left out keeps what is stored; a group given is replaced whole.
func (a *App) handleSetGroupPolicies(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Policies map[string]GroupPolicy `json:"policies"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || json.Unmarshal(raw, &in) != nil || in.Policies == nil {
		writeErr(w, http.StatusBadRequest, `body is {"policies": {"admin"|"user"|"guest": {"maxTtl", "deniedNodeTypes", "maxStacks", "maxContainers"}}}`)
		return
	}
	cur := a.groupPolicies()
	var changed []string
	for g, p := range in.Policies {
		if _, ok := groupLabels[g]; !ok {
			writeErr(w, http.StatusBadRequest, "unknown group "+g+": one of admin, user, guest")
			return
		}
		if p.MaxTTL != "" && ttlRank(p.MaxTTL) < 0 {
			writeErr(w, http.StatusBadRequest, "maxTtl must be one of "+strings.Join(ttlOrder, ", ")+", or empty for any")
			return
		}
		if p.MaxStacks < 0 || p.MaxContainers < 0 {
			writeErr(w, http.StatusBadRequest, "maxStacks and maxContainers are counts; 0 means no limit")
			return
		}
		sort.Strings(p.DeniedNodeTypes)
		cur[g] = p
		changed = append(changed, groupLabels[g])
	}
	b, _ := json.Marshal(cur)
	if err := a.store.SetAppSetting(settingGroupPolicies, string(b)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the policies")
		return
	}
	sort.Strings(changed)
	a.notify(Notification{Scope: "admin", Type: "security.policies", Severity: "info",
		Title: "Security group limits changed", Body: "An administrator changed the limits of " + strings.Join(changed, ", ") + "."})
	writeJSON(w, http.StatusOK, map[string]any{"policies": cur, "ttls": ttlOrder})
}
