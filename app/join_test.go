package main

import (
	"strings"
	"testing"
)

func TestFrameSplitAndRefusal(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{{ID: "s", Type: "spock", Label: "sp"}, {ID: "m", Type: "mysql", Label: "repl"}},
		Nodes: []designNode{
			{ID: "s1", Type: "spock", FrameID: "s", Label: "sp-1"},
			{ID: "s2", Type: "spock", FrameID: "s", Label: "sp-2"},
			{ID: "m1", Type: "mysql", FrameID: "m", Label: "db-1"},
			{ID: "m2", Type: "mysql", FrameID: "m", Label: "db-2"},
		},
	}
	existing := map[string]Deployment{
		"s1": {NodeID: "s1", State: DeployRunning, ContainerID: "a"},
		"m1": {NodeID: "m1", State: DeployStopped, ContainerID: "b"},
		"m2": {NodeID: "m2", State: DeployError, ContainerID: "c"}, // failed: built again
	}
	built, fresh := frameSplit(doc.Frames[1], doc, existing)
	if len(built) != 1 || built[0].ID != "m1" || len(fresh) != 1 || fresh[0].ID != "m2" {
		t.Fatalf("built=%v fresh=%v", built, fresh)
	}
	if why := joinRefused(doc.Frames[1], built, fresh); why != "" {
		t.Errorf("replication can take a member: %s", why)
	}
	b, f := frameSplit(doc.Frames[0], doc, existing)
	why := joinRefused(doc.Frames[0], b, f)
	if !strings.Contains(why, "sp-2 cannot join it yet") {
		t.Errorf("spock is refused, not rebuilt: %q", why)
	}
	// Nothing built: a first deploy, not a join.
	if joinRefused(doc.Frames[0], nil, f) != "" {
		t.Error("a first deploy is never refused")
	}
}
