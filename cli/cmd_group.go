package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// cmd_group.go — security groups (app/secgroups.go): which pages each group may use.
//
//	dbcanvas-cli group list
//	dbcanvas-cli group set guest kanban=on api=on log-summary=off
//
// Administrators only; the server enforces the result on every request, this CLI's
// included.

type groupsView struct {
	Features []struct {
		ID       string `json:"id"`
		Label    string `json:"label"`
		Requires string `json:"requires"`
	} `json:"features"`
	Groups []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"groups"`
	Allowed map[string]map[string]bool `json:"allowed"`
	Locked  map[string]map[string]bool `json:"locked"`
}

func cmdGroup(args []string) error {
	return sub("group", args, map[string]func([]string) error{
		"list":   groupList,
		"set":    groupSet,
		"policy": groupPolicyCmd,
	}, []string{"list", "set", "policy"})
}

func groupList(args []string) error {
	fs := flagsFor("group list")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/security/groups", &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var v groupsView
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	printGroups(v)
	return nil
}

func printGroups(v groupsView) {
	head := []string{"feature", "page"}
	for _, gr := range v.Groups {
		head = append(head, gr.ID)
	}
	t := newTable(head...)
	for _, f := range v.Features {
		row := []string{f.ID, f.Label}
		for _, gr := range v.Groups {
			cell := "-"
			if v.Allowed[gr.ID][f.ID] {
				cell = "yes"
			}
			if v.Locked[gr.ID][f.ID] {
				cell += " (fixed)"
			}
			row = append(row, cell)
		}
		t.add(row...)
	}
	t.print()
}

func groupSet(args []string) error {
	fs := flagsFor("group set")
	if err := parse(fs, args); err != nil {
		return err
	}
	rest := positional
	if len(rest) < 2 {
		return fmt.Errorf("usage: dbcanvas-cli group set <admin|user|guest> <feature>=on|off ...  (features: dbcanvas-cli group list)")
	}
	group := rest[0]
	cells := map[string]bool{}
	for _, kv := range rest[1:] {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("%q: write <feature>=on or <feature>=off", kv)
		}
		switch strings.ToLower(val) {
		case "on", "yes", "true", "1":
			cells[k] = true
		case "off", "no", "false", "0":
			cells[k] = false
		default:
			return fmt.Errorf("%q: the value is on or off", kv)
		}
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	body := map[string]any{"allowed": map[string]any{group: cells}}
	if err := c.request("PUT", "/api/security/groups", body, &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var v groupsView
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	printGroups(v)
	return nil
}
