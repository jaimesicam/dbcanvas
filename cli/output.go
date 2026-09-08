package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// output.go — tables for people, JSON for scripts.
//
// Every command that prints a table also honours --json, and --json emits the
// server's own response unchanged rather than a re-serialisation of whatever the CLI
// parsed. That distinction matters: a pipeline into jq then depends on the API's
// shape, which is documented and versioned, instead of on this tool's formatting,
// which is neither.

// printJSON writes a value as indented JSON.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printRaw writes bytes straight through, which is what --json does with a response
// the CLI never decoded.
func printRaw(raw []byte) error {
	var buf any
	if json.Unmarshal(raw, &buf) == nil {
		return printJSON(buf)
	}
	_, err := os.Stdout.Write(raw)
	return err
}

// table renders aligned columns, padded to the widest cell. Column widths come from
// the data rather than being fixed, because a stack name can be 4 characters or 40
// and neither should wreck the layout.
type table struct {
	head []string
	rows [][]string
}

func newTable(head ...string) *table { return &table{head: head} }

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) print() {
	if len(t.rows) == 0 {
		return
	}
	// Widths in runes, not bytes. A name truncated with a "…" carries a 3-byte
	// character, and measuring the cell in bytes pads that row two columns short
	// of the rest — which is visible the first time a long stack name appears.
	w := make([]int, len(t.head))
	for i, h := range t.head {
		w[i] = utf8.RuneCountInString(h)
	}
	for _, r := range t.rows {
		for i, c := range r {
			if n := utf8.RuneCountInString(c); i < len(w) && n > w[i] {
				w[i] = n
			}
		}
	}
	var b strings.Builder
	for i, h := range t.head {
		b.WriteString(pad(strings.ToUpper(h), w[i], i == len(t.head)-1))
	}
	fmt.Println(strings.TrimRight(b.String(), " "))
	for _, r := range t.rows {
		b.Reset()
		for i, c := range r {
			if i < len(w) {
				b.WriteString(pad(c, w[i], i == len(r)-1))
			}
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
}

func pad(s string, w int, last bool) string {
	if last {
		return s
	}
	gap := w - utf8.RuneCountInString(s) + 2
	if gap < 1 {
		gap = 1
	}
	return s + strings.Repeat(" ", gap)
}

// empty prints the "nothing here" line, with the suggestion of what to do about it.
func empty(what, suggestion string) {
	fmt.Printf("No %s.\n", what)
	if suggestion != "" {
		fmt.Printf("  %s\n", suggestion)
	}
}

// shortDate renders an RFC3339 stamp as a date, leaving anything unparseable alone
// rather than turning a server value into a confident lie.
func shortDate(s string) string {
	if s == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02")
}

// shortTime is shortDate plus the clock, for things that happen within a day.
func shortTime(s string) string {
	if s == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

// untilText says how long is left, in the unit somebody cares about at that range.
func untilText(s string) string {
	if s == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// jsonUnmarshal is encoding/json's Unmarshal, wrapped so a malformed response reads
// as a server problem rather than a Go type error.
func jsonUnmarshal(raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("the server sent something unexpected: %w", err)
	}
	return nil
}

// apiMessage extracts the server's error field, falling back to the status.
func apiMessage(raw []byte, status int) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	if s := strings.TrimSpace(string(raw)); s != "" {
		return s
	}
	return fmt.Sprintf("request failed (%d)", status)
}
