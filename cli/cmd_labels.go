package main

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// cmd_labels.go — stack labels from the CLI: --label on compose, create and list, `stack label`
// to change them, and --label on destroy and delete to act on every stack that matches. The
// server's rules for keys and values apply (app/stacklabels.go); it is the one that refuses.

// labelsFromFlags turns repeated --label k=v into a map. A key without "=" is refused here:
// on a create it would be an empty value nobody meant.
func labelsFromFlags(flags multiFlag) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range flags {
		for _, part := range strings.Split(f, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("--label %q: want key=value", part)
			}
			out[strings.TrimSpace(k)] = v
		}
	}
	return out, nil
}

// labelQuery is the ?label=… query for a selector (key=value or key).
func labelQuery(sel multiFlag) string {
	if len(sel) == 0 {
		return ""
	}
	q := url.Values{}
	for _, s := range sel {
		q.Add("label", s)
	}
	return "?" + q.Encode()
}

// labelText renders labels sorted, for a table column.
func labelText(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

// stacksByLabel is every stack carrying all of the selector's terms.
func stacksByLabel(c *Client, sel multiFlag) ([]Stack, error) {
	var stacks []Stack
	err := c.get("/api/stacks"+labelQuery(sel), &stacks)
	return stacks, err
}

// stackLabel is `stack label <stack> key=value … key-`: set or (with a trailing -) remove labels.
func stackLabel(args []string) error {
	fs := flagsFor("stack label")
	if err := parse(fs, args); err != nil {
		return err
	}
	if err := need(1, "a stack name or id, then key=value or key- (remove)"); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	st, err := resolveStack(c, arg(0))
	if err != nil {
		return err
	}
	labels := map[string]string{}
	for k, v := range st.Labels {
		labels[k] = v
	}
	for _, a := range positional[1:] {
		if k, ok := strings.CutSuffix(a, "-"); ok && !strings.Contains(a, "=") {
			delete(labels, k)
			continue
		}
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return fmt.Errorf("%q: want key=value to set, or key- to remove", a)
		}
		labels[k] = v
	}
	var out struct {
		Labels map[string]string `json:"labels"`
	}
	if err := c.put(fmt.Sprintf("/api/stacks/%d/labels", st.ID), map[string]any{"labels": labels}, &out); err != nil {
		return err
	}
	if g.json {
		return printJSON(out)
	}
	if len(out.Labels) == 0 {
		fmt.Printf("%s (id %d) has no labels.\n", st.Name, st.ID)
	} else {
		fmt.Printf("%s (id %d): %s\n", st.Name, st.ID, labelText(out.Labels))
	}
	return nil
}

// stacksForBulk resolves what destroy or delete acts on: the one stack named, or every stack
// matching --label. Both at once, or neither, is a usage error.
func stacksForBulk(c *Client, sel multiFlag, verb string) ([]Stack, error) {
	switch {
	case len(sel) > 0 && nargs() > 0:
		return nil, fmt.Errorf("give a stack or --label, not both")
	case len(sel) > 0:
		stacks, err := stacksByLabel(c, sel)
		if err == nil && len(stacks) == 0 {
			fmt.Printf("No stack carries %s; nothing to %s.\n", strings.Join(sel, " and "), verb)
		}
		return stacks, err
	case nargs() == 0:
		return nil, fmt.Errorf("give a stack name or id, or --label key=value to %s every stack that carries it", verb)
	}
	st, err := resolveStack(c, arg(0))
	if err != nil {
		return nil, err
	}
	return []Stack{st}, nil
}

// stackNames lists stacks for a confirmation prompt.
func stackNames(stacks []Stack) string {
	parts := make([]string, len(stacks))
	for i, s := range stacks {
		parts[i] = s.Name + " (id " + strconv.FormatInt(s.ID, 10) + ")"
	}
	return strings.Join(parts, ", ")
}
