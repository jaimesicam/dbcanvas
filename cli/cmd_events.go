package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// cmd_events.go — `stack events`: follow the server's stack lifecycle stream
// (GET /api/stacks/events) until interrupted. One line per event, or with the global --json one
// JSON object per line.

func stackEventsCmd(args []string) error {
	fs := flagsFor("stack events")
	var sel multiFlag
	fs.Var(&sel, "label", "only stacks carrying this label (key=value or key) — repeatable")
	all := fs.Bool("all", false, "every account's stacks (administrators)")
	if err := parse(fs, args); err != nil {
		return err
	}
	c, err := mustClient()
	if err != nil {
		return err
	}
	q := url.Values{}
	for _, s := range sel {
		q.Add("label", s)
	}
	if *all {
		q.Set("all", "1")
	}
	path := "/api/stacks/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := http.NewRequest("GET", c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "text/event-stream")
	// No client timeout: the stream is meant to stay open.
	resp, err := (&http.Client{Transport: c.HTTP.Transport}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: %s", resp.Status, e.Error)
	}
	fmt.Fprintln(os.Stderr, "Following stack events; Ctrl-C to stop.")
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if g.json {
			fmt.Println(data)
			continue
		}
		var e struct {
			Type, Name, Reason, By, At string
			StackID                    int64             `json:"stackId"`
			Labels                     map[string]string `json:"labels"`
		}
		if json.Unmarshal([]byte(data), &e) != nil {
			continue
		}
		line := fmt.Sprintf("%s  %-10s %s (id %d)", e.At, e.Type, e.Name, e.StackID)
		if e.Reason != "" {
			line += " reason=" + e.Reason
		}
		if e.By != "" {
			line += " by=" + e.By
		}
		if len(e.Labels) > 0 {
			line += "  " + labelText(e.Labels)
		}
		fmt.Println(line)
	}
	return sc.Err()
}
