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

// The Patroni restore was worked out by hand on a live cluster; these are the parts of it that
// look like they could be simplified and cannot.
func TestPatroniRestoreSteps(t *testing.T) {
	// patronictl remove's exit status is not proof: the cluster must be seen gone from etcd, or
	// the restored leader is told to rejoin as a replica.
	if !strings.Contains(patroniForgetScript, "(uninitialized)") {
		t.Error("forgetting the cluster must check etcd shows it uninitialized")
	}
	// Recovery runs outside Patroni and must finish (promote) before Patroni starts on it.
	if !strings.Contains(patroniRestoreLeaderScript, "pg_is_in_recovery()") ||
		!strings.Contains(patroniRestoreLeaderScript, "-m fast -w stop") {
		t.Error("the leader must recover to the end, then be stopped for Patroni to start")
	}
	// A replica's data directory is emptied, not removed: Patroni (and initdb's permissions)
	// expect the directory itself to be there.
	if !strings.Contains(patroniRebuildReplicaScript, `find "$DATA" -mindepth 1 -delete`) {
		t.Error("a replica's data directory must be emptied with -mindepth 1, never removed")
	}
}
