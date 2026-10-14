package main

import (
	"strings"
	"testing"
)

func TestPGRestoreArgs(t *testing.T) {
	if a, err := pgRestoreArgs(pgRestoreRequest{Mode: "latest"}); err != nil || a != nil {
		t.Errorf("latest = %v, %v", a, err)
	}
	a, err := pgRestoreArgs(pgRestoreRequest{Mode: "time", Time: "2025-10-14T09:30:00+08:00"})
	if err != nil {
		t.Fatal(err)
	}
	// Converted to UTC, in the form recovery_target_time takes, and one argument despite the space.
	if got := strings.Join(a, "|"); got != "--type=time|--target=2025-10-14 01:30:00+00|--target-action=promote" {
		t.Errorf("time args = %s", got)
	}
	a, err = pgRestoreArgs(pgRestoreRequest{Mode: "backup", Set: "20261014-013000F"})
	if err != nil || strings.Join(a, "|") != "--set=20261014-013000F|--type=immediate|--target-action=promote" {
		t.Errorf("backup args = %v, %v", a, err)
	}
	for _, bad := range []pgRestoreRequest{
		{Mode: "time", Time: "yesterday"},
		{Mode: "time", Time: "2999-01-01T00:00:00Z"},
		{Mode: "backup"},
		{Mode: "backup", Set: "x; rm -rf /"},
		{Mode: "drop"},
	} {
		if _, err := pgRestoreArgs(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
