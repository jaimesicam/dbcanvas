package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// cmd_policy.go — `group policy`: each security group's limits (the server's grouppolicy.go).
//
//	dbcanvas-cli group policy                         show every group's limits
//	dbcanvas-cli group policy guest maxTtl=24h maxStacks=3 maxContainers=12 deny=k3d,kubeadm
//	dbcanvas-cli group policy guest maxTtl= deny=     clear them (empty = no limit)

type groupPolicy struct {
	MaxTTL          string   `json:"maxTtl,omitempty"`
	DeniedNodeTypes []string `json:"deniedNodeTypes,omitempty"`
	MaxStacks       int      `json:"maxStacks,omitempty"`
	MaxContainers   int      `json:"maxContainers,omitempty"`
}

func groupPolicyCmd(args []string) error {
	fs := flagsFor("group policy")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var cur struct {
		Policies map[string]groupPolicy `json:"policies"`
	}
	if err := c.get("/api/security/policies", &cur); err != nil {
		return err
	}
	if nargs() == 0 {
		if g.json {
			return printJSON(cur)
		}
		printPolicies(cur.Policies)
		return nil
	}
	group := arg(0)
	p := cur.Policies[group]
	if nargs() == 1 {
		return fmt.Errorf("give the limits to set, e.g. maxTtl=24h maxStacks=3 maxContainers=12 deny=k3d (empty clears)")
	}
	for _, kv := range positional[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("%q: want key=value (maxTtl, maxStacks, maxContainers, deny)", kv)
		}
		switch strings.ToLower(k) {
		case "maxttl":
			p.MaxTTL = v
		case "maxstacks", "maxcontainers":
			n := 0
			if v != "" {
				if n, err = strconv.Atoi(v); err != nil || n < 0 {
					return fmt.Errorf("%s=%s: want a count (0 or empty = no limit)", k, v)
				}
			}
			if strings.EqualFold(k, "maxstacks") {
				p.MaxStacks = n
			} else {
				p.MaxContainers = n
			}
		case "deny", "deniednodetypes":
			p.DeniedNodeTypes = nil
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					p.DeniedNodeTypes = append(p.DeniedNodeTypes, t)
				}
			}
		default:
			return fmt.Errorf("unknown limit %q: maxTtl, maxStacks, maxContainers or deny", k)
		}
	}
	var out struct {
		Policies map[string]groupPolicy `json:"policies"`
	}
	if err := c.put("/api/security/policies", map[string]any{"policies": map[string]groupPolicy{group: p}}, &out); err != nil {
		return err
	}
	if g.json {
		return printJSON(out)
	}
	printPolicies(out.Policies)
	return nil
}

func printPolicies(ps map[string]groupPolicy) {
	groups := make([]string, 0, len(ps))
	for k := range ps {
		groups = append(groups, k)
	}
	sort.Strings(groups)
	t := newTable("group", "max ttl", "max stacks", "max nodes", "denied types")
	none := func(s string) string {
		if s == "" || s == "0" {
			return "-"
		}
		return s
	}
	for _, gname := range groups {
		p := ps[gname]
		t.add(gname, none(p.MaxTTL), none(strconv.Itoa(p.MaxStacks)), none(strconv.Itoa(p.MaxContainers)), none(strings.Join(p.DeniedNodeTypes, ",")))
	}
	t.print()
}
