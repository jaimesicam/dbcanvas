package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// waitFixture is a stack whose deployments the wait helpers poll.
func waitFixture(t *testing.T) (*App, Stack) {
	t.Helper()
	app := newTestApp(t)
	u, err := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	st, err := app.store.CreateStack("waits", u.ID, ttlInfinity, nil, []byte(defaultDesign))
	if err != nil {
		t.Fatal(err)
	}
	return app, st
}

// A linked node waiting on a ProxySQL whose every member failed must give up at
// once, not hold its own deployment (and the stack's Deploy lock) until the
// deploy timeout.
func TestWaitProxySQLRunningFailsWhenEveryMemberFailed(t *testing.T) {
	app, st := waitFixture(t)
	doc := designDoc{Nodes: []designNode{{ID: "px1", Type: "proxysql", Label: "proxysql-01"}}}
	if err := app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "px1", State: DeployError}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, _, _, _, err := app.waitProxySQLRunning(context.Background(), st.ID, doc, stackHostnames(doc), "example.net", "px1", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "failed to provision") {
		t.Fatalf("err = %v, want a failed-to-provision error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s to give up; want immediately", d)
	}
}

// One failed member of a ProxySQL cluster is not enough to give up: another may
// still come up.
func TestWaitProxySQLRunningWaitsWhileAMemberMayStillComeUp(t *testing.T) {
	app, st := waitFixture(t)
	doc := designDoc{
		Frames: []designFrame{{ID: "pf", Type: "proxysql", Label: "proxysql-cluster-01"}},
		Nodes: []designNode{
			{ID: "px1", Type: "proxysql", FrameID: "pf", Label: "proxysql-1"},
			{ID: "px2", Type: "proxysql", FrameID: "pf", Label: "proxysql-2"},
		},
	}
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "px1", State: DeployError})
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "px2", State: DeployProvisioning})

	_, _, _, _, err := app.waitProxySQLRunning(context.Background(), st.ID, doc, stackHostnames(doc), "example.net", "pf", 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("err = %v, want the timeout error", err)
	}
}

// An asynchronous replication frame whose primary failed has nowhere to write.
func TestWaitMySQLFamilyFrameFailsWhenPrimaryFailed(t *testing.T) {
	app, st := waitFixture(t)
	f := designFrame{ID: "mf", Type: "mariadbrepl", Label: "mariadb-repl-01"}
	doc := designDoc{
		Frames: []designFrame{f},
		Nodes: []designNode{
			{ID: "m1", Type: "mariadbrepl", FrameID: "mf", Label: "mariadb-1", Role: "primary"},
			{ID: "m2", Type: "mariadbrepl", FrameID: "mf", Label: "mariadb-2"},
		},
	}
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "m1", State: DeployError})
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "m2", State: DeployProvisioning})

	start := time.Now()
	_, _, err := app.waitMySQLFamilyFrame(context.Background(), st.ID, f, doc, "example.net", true, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "failed to provision") {
		t.Fatalf("err = %v, want a failed-to-provision error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s to give up; want immediately", d)
	}

	// Without wantPrimary (Galera, Group Replication) the replica is still a
	// candidate, so the same state is only a wait.
	_, _, err = app.waitMySQLFamilyFrame(context.Background(), st.ID, f, doc, "example.net", false, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("err = %v, want the timeout error", err)
	}
}

// A node a previous server process was still provisioning is failed at startup with a
// reason; finished and already-failed nodes are left alone.
func TestFailInterruptedDeployments(t *testing.T) {
	app, st := waitFixture(t)
	for id, state := range map[string]string{"pend": DeployPending, "prov": DeployProvisioning, "run": DeployRunning, "err": DeployError} {
		if err := app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: id, State: state}); err != nil {
			t.Fatal(err)
		}
	}

	app.failInterruptedDeployments()

	for id, want := range map[string]string{"pend": DeployError, "prov": DeployError, "run": DeployRunning, "err": DeployError} {
		d, err := app.store.GetDeployment(st.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.State != want {
			t.Errorf("%s: state = %q, want %q", id, d.State, want)
		}
		interrupted := strings.Contains(string(d.Progress), "server restarted")
		if interrupted != (id == "pend" || id == "prov") {
			t.Errorf("%s: progress = %s", id, d.Progress)
		}
	}
}

func TestProxySQLBelow26(t *testing.T) {
	for v, want := range map[string]bool{
		"": false, "2.0.13-1.1": true, "2.5.5-1.2": true, "2.6.2-1.1": false,
		"2.7.3-1.2": false, "3.0.9-1.1": false, "1.4.16": true, "junk": false,
	} {
		if got := proxysqlBelow26(v); got != want {
			t.Errorf("proxysqlBelow26(%q) = %v, want %v", v, got, want)
		}
	}
}

// proxysqlBackendIssues across the matrix measured on the catalogue's ProxySQL builds:
// proxysql-admin's client versions (PXC only) and caching_sha2_password (both backends).
func TestProxySQLBackendMatrix(t *testing.T) {
	design := func(backType, series string) designDoc {
		back := designFrame{ID: "b", Type: backType, Label: "backend", PXCMajor: series, PSMajor: series}
		return designDoc{
			Frames: []designFrame{back},
			Nodes:  []designNode{{ID: "px", Type: "proxysql", Label: "proxysql-01"}},
			Edges:  []designEdge{{From: edgeEnd{Node: "b"}, To: edgeEnd{Node: "px"}}},
		}
	}
	ok, refused := false, true
	for _, c := range []struct {
		back, series, major, version string
		want                         bool
	}{
		// PS 5.7 (MySQL replication; there is no PXC 5.7 build): everything works.
		{"mysql", "5.7", "2", "2.0.13-1.1", ok},
		{"mysql", "5.7", "3", "", ok},
		// 8.0: every build — before 2.6 the app user is moved to mysql_native_password at deploy.
		{"pxc", "8.0", "2", "2.0.13-1.1", ok},
		{"pxc", "8.0", "2", "2.5.5-1.2", ok},
		{"pxc", "8.0", "3", "3.0.1-1.1", ok},
		{"mysql", "8.0", "2", "2.0.17-1.1", ok},
		// 8.4 on PXC: proxysql-admin knows the client from 2.7.3; below 2.6, auth fails too.
		{"pxc", "8.4", "2", "2.0.13-1.1", refused},
		{"pxc", "8.4", "2", "2.6.5-1.1", refused},
		{"pxc", "8.4", "2", "2.7.1-1.1", refused},
		{"pxc", "8.4", "2", "2.7.3-1.1", ok},
		{"pxc", "8.4", "2", "", ok}, // newest 2.x is 2.7.3
		{"pxc", "8.4", "3", "3.0.1-1.1", ok},
		// 8.4 on MySQL replication: no proxysql-admin, so only authentication matters.
		{"mysql", "8.4", "2", "2.5.5-1.2", refused},
		{"mysql", "8.4", "2", "2.6.5-1.1", ok},
		// 9.7 on PXC: only ProxySQL 3.0.9's proxysql-admin knows the client.
		{"pxc", "9.7", "2", "", refused},
		{"pxc", "9.7", "2", "2.7.3-1.2", refused},
		{"pxc", "9.7", "3", "3.0.6-1.1", refused},
		{"pxc", "9.7", "3", "3.0.9-1.1", ok},
		{"pxc", "9.7", "3", "", ok},
		// 9.7 on MySQL replication: 2.6+ authenticates; nothing older can.
		{"mysql", "9.7", "2", "2.6.5-1.1", ok},
		{"mysql", "9.7", "2", "2.4.8-1.1", refused},
	} {
		got := proxysqlBackendIssues(design(c.back, c.series), "ProxySQL node proxysql-01", "px", c.major, c.version)
		if (len(got) > 0) != c.want {
			t.Errorf("%s %s behind ProxySQL %s/%q: issues = %v, want refused=%v", c.back, c.series, c.major, c.version, got, c.want)
		}
	}
}

func TestPSClientPackage(t *testing.T) {
	for _, c := range []struct{ os, series, want string }{
		{"oraclelinux", "5.7", "Percona-Server-client-57"},
		{"ubuntu", "5.7", "percona-server-client-5.7"},
		{"oraclelinux", "8.0", "percona-server-client"},
		{"oraclelinux", "8.4", "percona-server-client"},
		{"ubuntu", "9.7", "percona-server-client"},
	} {
		if got := psClientPackage(c.os, c.series); got != c.want {
			t.Errorf("psClientPackage(%s, %s) = %q, want %q", c.os, c.series, got, c.want)
		}
	}
}
