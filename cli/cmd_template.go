package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// cmd_template.go — deployment templates: the shipped ones, your own, and the
// `.json` documents they export to.

type template struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Shared      bool            `json:"shared"`
	Builtin     bool            `json:"builtin"`
	Design      json.RawMessage `json:"design"`
}

func cmdTemplate(args []string) error {
	return sub("template", args, map[string]func([]string) error{
		"list":   templateList,
		"export": templateExport,
		"import": templateImport,
	}, []string{"list", "export", "import"})
}

func templateList(args []string) error {
	fs := flagsFor("template list")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/templates", &raw); err != nil {
		return err
	}
	if g.json {
		return printRaw(raw)
	}
	var tpls []template
	if err := jsonUnmarshal(raw, &tpls); err != nil {
		return err
	}
	if len(tpls) == 0 {
		empty("templates", "")
		return nil
	}
	t := newTable("id", "name", "category", "kind")
	for _, tp := range tpls {
		kind := "yours"
		switch {
		case strings.HasPrefix(tp.ID, "builtin:"):
			kind = "built-in"
		case tp.Shared:
			kind = "published"
		}
		t.add(tp.ID, truncate(tp.Name, 40), tp.Category, kind)
	}
	t.print()
	return nil
}

// resolveTemplate accepts an id or a name, the same way stacks do.
func resolveTemplate(c *Client, ref string) (template, error) {
	var tpls []template
	if err := c.get("/api/templates", &tpls); err != nil {
		return template{}, err
	}
	for _, tp := range tpls {
		if tp.ID == ref {
			return tp, nil
		}
	}
	var hits []template
	for _, tp := range tpls {
		if strings.EqualFold(tp.Name, ref) {
			hits = append(hits, tp)
		}
	}
	// A substring match only when nothing matched exactly — `--template PXC` is a
	// natural thing to type, and there is usually exactly one.
	if len(hits) == 0 {
		for _, tp := range tpls {
			if strings.Contains(strings.ToLower(tp.Name), strings.ToLower(ref)) {
				hits = append(hits, tp)
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return template{}, fmt.Errorf("no template matches %q (try `dbcanvas template list`)", ref)
	default:
		var names []string
		for _, tp := range hits {
			names = append(names, tp.Name)
		}
		return template{}, fmt.Errorf("%q matches several templates: %s", ref, strings.Join(names, "; "))
	}
}

// templateDesign fetches the canvas a template will produce, which is what
// `stack create --template` needs.
func templateDesign(c *Client, ref string) (any, error) {
	tp, err := resolveTemplate(c, ref)
	if err != nil {
		return nil, err
	}
	// The list endpoint may omit the design; fetch the template itself to be sure.
	var full template
	if err := c.get("/api/templates/"+url.PathEscape(tp.ID), &full); err != nil {
		return nil, err
	}
	design := full.Design
	if len(design) == 0 {
		design = tp.Design
	}
	if len(design) == 0 {
		return nil, fmt.Errorf("template %q has no design", tp.Name)
	}
	var doc any
	if err := jsonUnmarshal(design, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func templateExport(args []string) error {
	fs := flagsFor("template export")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a template name or id"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	tp, err := resolveTemplate(c, arg(0))
	if err != nil {
		return err
	}
	var raw []byte
	if err := c.get("/api/templates/"+url.PathEscape(tp.ID)+"/export", &raw); err != nil {
		return err
	}
	return printRaw(raw)
}

func templateImport(args []string) error {
	fs := flagsFor("template import")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a path to an exported template, or - for stdin"); err != nil {
		return err
	}
	raw, err := readFileOrStdin(arg(0))
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", arg(0), err)
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	var created template
	if err := c.post("/api/templates/import", doc, &created); err != nil {
		return err
	}
	if g.json {
		return printJSON(created)
	}
	fmt.Printf("Imported %q (id %s).\n", created.Name, created.ID)
	return nil
}
