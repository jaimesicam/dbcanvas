package main

import (
	"sort"
	"strings"
)

// issuesubject.go — which node or frame a validation issue is about.
//
// Validation messages name what they are about by label ("Percona Server node ps84: …", "PXC
// cluster pxc-cluster-01 needs …"); the designer shows each issue on that item's form as you
// edit, so the server's rules are the only ones and the form never needs a copy of them. Rather
// than thread an id through every one of the hundreds of checks, the label is found in the
// message here: as a whole word, the longest label that matches winning, so "ps-repl-1" is not
// mistaken for "ps-repl-1x". Labels are unique in a valid design; when two items share the
// matched label the issue stays unattributed rather than land on the wrong one.

// issueSubjects fills in Subject on every issue whose message names exactly one node or frame.
func issueSubjects(doc designDoc, issues []issue) []issue {
	type cand struct{ label, id string }
	var cands []cand
	count := map[string]int{}
	for _, n := range doc.Nodes {
		if l := strings.TrimSpace(n.Label); l != "" {
			cands = append(cands, cand{l, n.ID})
			count[l]++
		}
	}
	for _, f := range doc.Frames {
		if l := strings.TrimSpace(f.Label); l != "" {
			cands = append(cands, cand{l, f.ID})
			count[l]++
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return len(cands[i].label) > len(cands[j].label) })
	for i := range issues {
		if issues[i].Subject != "" {
			continue
		}
		for _, c := range cands {
			if containsWord(issues[i].Message, c.label) {
				if count[c.label] == 1 {
					issues[i].Subject = c.id
				}
				break
			}
		}
	}
	return issues
}

// containsWord reports whether word occurs in s with no label character on either side.
func containsWord(s, word string) bool {
	for from := 0; ; {
		k := strings.Index(s[from:], word)
		if k < 0 {
			return false
		}
		k += from
		end := k + len(word)
		if (k == 0 || !labelByte(s[k-1])) && (end == len(s) || !labelByte(s[end])) {
			return true
		}
		from = k + 1
	}
}

// labelByte is a byte that can continue a label: letters, digits, '-' and '_'. A '.' ends one,
// since messages end sentences with it.
func labelByte(b byte) bool {
	return b == '-' || b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}
