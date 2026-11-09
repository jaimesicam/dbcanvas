package main

import (
	"strings"
	"testing"
)

func TestValidateLabels(t *testing.T) {
	ok := []map[string]string{nil, {}, {"tool": "ci", "run": "12", "team.example/owner": "db", "a": ""}}
	for _, m := range ok {
		if err := validateLabels(m); err != nil {
			t.Errorf("%v: %v", m, err)
		}
	}
	bad := map[string]map[string]string{
		"upper case key":   {"Tool": "ci"},
		"leading dash":     {"-x": "1"},
		"trailing dot":     {"x.": "1"},
		"space in key":     {"a b": "1"},
		"long value":       {"k": strings.Repeat("v", 129)},
		"control in value": {"k": "a\nb"},
	}
	for name, m := range bad {
		if err := validateLabels(m); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := map[string]string{}
	for i := 0; i < maxStackLabels+1; i++ {
		many[string(rune('a'+i%26))+strings.Repeat("x", i/26+1)] = "v"
	}
	if validateLabels(many) == nil {
		t.Error("33 labels accepted")
	}
}

func TestLabelSelector(t *testing.T) {
	sel, err := parseLabelSelector([]string{"tool=ci", "run", "team=db,env=lab"})
	if err != nil || len(sel) != 4 {
		t.Fatalf("%v %v", sel, err)
	}
	stacks := []Stack{
		{ID: 1, Labels: map[string]string{"tool": "ci", "run": "7", "team": "db", "env": "lab"}},
		{ID: 2, Labels: map[string]string{"tool": "ci", "team": "db", "env": "lab"}},                // no run
		{ID: 3, Labels: map[string]string{"tool": "other", "run": "7", "team": "db", "env": "lab"}}, // wrong tool
		{ID: 4},
	}
	got := filterStacksByLabel(stacks, sel)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("matched %v", got)
	}
	if len(filterStacksByLabel(stacks, nil)) != 4 {
		t.Error("an empty selector filtered")
	}
	if _, err := parseLabelSelector([]string{"Bad Key=1"}); err == nil {
		t.Error("a bad key was accepted in a selector")
	}
}

func TestStackLabelsRoundTrip(t *testing.T) {
	st, alice, _ := stackNameStore(t)
	s, _ := st.CreateStack("lab", alice.ID, "4h", nil, []byte(`{}`))
	if err := st.SetStackLabels(s.ID, map[string]string{"tool": "ci", "run": "12"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetStack(s.ID)
	if labelString(got.Labels) != "run=12,tool=ci" {
		t.Fatalf("GetStack labels %v", got.Labels)
	}
	list, _ := st.ListStacks(alice.ID, false)
	if len(list) != 1 || list[0].Labels["run"] != "12" {
		t.Fatalf("ListStacks labels %v", list)
	}
	st.SetStackLabels(s.ID, nil)
	if got, _ := st.GetStack(s.ID); got.Labels != nil {
		t.Fatalf("cleared labels came back: %v", got.Labels)
	}
}
