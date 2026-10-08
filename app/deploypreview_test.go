package main

import "testing"

func TestPreviewDeploy(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{{ID: "f", Type: "mysql", Label: "repl"}, {ID: "g", Type: "pxc", Label: "galera"}},
		Nodes: []designNode{
			{ID: "i", Type: "intranet", Label: "intranet-01"},
			{ID: "p", Type: "pmm", Label: "pmm-01"},
			{ID: "a", Type: "mysql", FrameID: "f", Label: "db-1"},
			{ID: "b", Type: "mysql", FrameID: "f", Label: "db-2"},
			{ID: "c", Type: "mysql", FrameID: "f", Label: "db-3"}, // added after deploy
			{ID: "x", Type: "pxc", FrameID: "g", Label: "gx-1"},
		},
	}
	deps := []Deployment{
		{NodeID: "i", State: DeployRunning}, {NodeID: "p", State: DeployStopped},
		{NodeID: "a", State: DeployRunning}, {NodeID: "b", State: DeployRunning},
		{NodeID: "x", State: DeployRunning}, {NodeID: "gone", State: DeployRunning},
	}
	p := previewDeploy(doc, deps)
	if len(p.Remove) != 1 || p.Remove[0].NodeID != "gone" {
		t.Errorf("remove = %+v", p.Remove)
	}
	if len(p.Create) != 1 || p.Create[0].NodeID != "c" {
		t.Errorf("create = %+v", p.Create)
	}
	if len(p.Recreate) != 1 || p.Recreate[0].NodeID != "p" {
		t.Errorf("recreate = %+v", p.Recreate)
	}
	if len(p.Rebuild) != 2 || p.Rebuild[0].Label != "db-1" || p.Rebuild[1].Label != "db-2" {
		t.Errorf("rebuild = %+v", p.Rebuild)
	}
	if p.Unchanged != 2 { // the intranet and the whole running galera frame
		t.Errorf("unchanged = %d", p.Unchanged)
	}
}
