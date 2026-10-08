package main

import "testing"

func TestPreviewDeploy(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{{ID: "f", Type: "mysql", Label: "repl"}, {ID: "g", Type: "spock", Label: "sp"}, {ID: "q", Type: "proxysql", Label: "pq"}},
		Nodes: []designNode{
			{ID: "i", Type: "intranet", Label: "intranet-01"},
			{ID: "p", Type: "pmm", Label: "pmm-01"},
			{ID: "a", Type: "mysql", FrameID: "f", Label: "db-1"},
			{ID: "b", Type: "mysql", FrameID: "f", Label: "db-2"},
			{ID: "c", Type: "mysql", FrameID: "f", Label: "db-3"}, // added after deploy: joins
			{ID: "x", Type: "spock", FrameID: "g", Label: "sp-1"},
			{ID: "y", Type: "spock", FrameID: "g", Label: "sp-2"}, // added after deploy: refused for now
			{ID: "q1", Type: "proxysql", FrameID: "q", Label: "pq-1"},
			{ID: "q2", Type: "proxysql", FrameID: "q", Label: "pq-2"}, // configuration only: rebuilt whole
		},
	}
	deps := []Deployment{
		{NodeID: "i", State: DeployRunning, ContainerID: "c1"}, {NodeID: "p", State: DeployError, ContainerID: "c2"},
		{NodeID: "a", State: DeployRunning, ContainerID: "c3"}, {NodeID: "b", State: DeployStopped, ContainerID: "c4"},
		{NodeID: "x", State: DeployRunning, ContainerID: "c5"}, {NodeID: "gone", State: DeployRunning, ContainerID: "c6"},
		{NodeID: "q1", State: DeployRunning, ContainerID: "c7"},
	}
	p := previewDeploy(doc, deps)
	if len(p.Remove) != 1 || p.Remove[0].NodeID != "gone" {
		t.Errorf("remove = %+v", p.Remove)
	}
	if len(p.Join) != 1 || p.Join[0].NodeID != "c" {
		t.Errorf("join = %+v", p.Join)
	}
	if len(p.Recreate) != 1 || p.Recreate[0].NodeID != "p" {
		t.Errorf("recreate = %+v", p.Recreate)
	}
	if len(p.Create) != 1 || p.Create[0].NodeID != "q2" {
		t.Errorf("create = %+v", p.Create)
	}
	if len(p.Rebuild) != 1 || p.Rebuild[0].NodeID != "q1" {
		t.Errorf("rebuild = %+v", p.Rebuild)
	}
	if len(p.Refused) != 1 {
		t.Errorf("refused = %+v", p.Refused)
	}
	if p.Unchanged != 4 { // intranet, db-1, db-2 (stopped), sp-1
		t.Errorf("unchanged = %d", p.Unchanged)
	}
}

// A node stopped on purpose keeps its container and data: a deploy leaves it, and its cluster,
// alone.
func TestStoppedIsNotRebuilt(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{{ID: "f", Type: "mysql", Label: "repl"}},
		Nodes: []designNode{
			{ID: "s", Type: "ps", Label: "ps-01"},
			{ID: "a", Type: "mysql", FrameID: "f", Label: "db-1"},
			{ID: "b", Type: "mysql", FrameID: "f", Label: "db-2"},
		},
	}
	deps := []Deployment{
		{NodeID: "s", State: DeployStopped, ContainerID: "c1"},
		{NodeID: "a", State: DeployRunning, ContainerID: "c2"},
		{NodeID: "b", State: DeployStopped, ContainerID: "c3"},
	}
	p := previewDeploy(doc, deps)
	if len(p.Recreate)+len(p.Rebuild)+len(p.Create) != 0 || p.Unchanged != 3 {
		t.Fatalf("%+v", p)
	}
	if deployBuilt(Deployment{State: DeployStopped}) {
		t.Error("a stopped deployment with no container is not built")
	}
}
