package main

import (
	"fmt"
	"strings"
)

// cmd_meta.go — `dbcanvas endpoints`, the discovery half of the escape hatch.
//
// It prints what the *server* says it serves, not a list compiled into this binary.
// So a CLI a few releases behind still describes the installation it is pointed at
// correctly, which is the whole reason the catalogue is an endpoint.

type endpointDoc struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Group    string `json:"group"`
	Summary  string `json:"summary"`
	Auth     string `json:"auth"`
	Scope    string `json:"scope"`
	Media    string `json:"media"`
	ReadOnly bool   `json:"readOnly"`
	Params   []struct {
		Name string `json:"name"`
		Help string `json:"help"`
	} `json:"params"`
}

func cmdEndpoints(args []string) error {
	fs := flagsFor("endpoints")
	group := fs.String("group", "", "only this group (e.g. Stacks, Nodes, \"Data Generator\")")
	scope := fs.String("scope", "", "only endpoints needing this scope: read, write or admin")
	q := fs.String("q", "", "match the method, path, summary or group")
	long := fs.Bool("long", false, "include the summary for each endpoint")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/meta/endpoints", &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var cat struct {
		Version string   `json:"version"`
		Total   int      `json:"total"`
		Scopes  []string `json:"scopes"`
		Groups  []struct {
			Name      string        `json:"name"`
			Endpoints []endpointDoc `json:"endpoints"`
		} `json:"groups"`
	}
	if err := jsonUnmarshal(raw, &cat); err != nil {
		return err
	}

	shown := 0
	for _, grp := range cat.Groups {
		if *group != "" && !strings.EqualFold(grp.Name, *group) {
			continue
		}
		var keep []endpointDoc
		for _, ep := range grp.Endpoints {
			if *scope != "" && ep.Scope != *scope {
				continue
			}
			if *q != "" && !endpointMatches(ep, *q) {
				continue
			}
			keep = append(keep, ep)
		}
		if len(keep) == 0 {
			continue
		}
		fmt.Printf("\n%s\n", grp.Name)
		for _, ep := range keep {
			flags := joinNonEmpty(" ", ep.Scope, ep.Media)
			fmt.Printf("  %-6s %-52s %s\n", ep.Method, ep.Path, flags)
			if *long {
				fmt.Printf("         %s\n", ep.Summary)
			}
			shown++
		}
	}
	if shown == 0 {
		fmt.Println("Nothing matches those filters.")
		return nil
	}
	fmt.Printf("\n%d of %d endpoints on %s (DBCanvas %s).\n", shown, cat.Total, c.BaseURL, cat.Version)
	fmt.Printf("Call any of them: dbcanvas api GET /api/stacks\n")
	return nil
}

func endpointMatches(ep endpointDoc, q string) bool {
	hay := strings.ToLower(ep.Method + " " + ep.Path + " " + ep.Summary + " " + ep.Group)
	for _, term := range strings.Fields(strings.ToLower(q)) {
		if !strings.Contains(hay, term) {
			return false
		}
	}
	return true
}
