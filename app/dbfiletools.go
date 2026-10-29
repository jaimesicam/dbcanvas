package main

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// dbfiletools.go — innodump, binlogsum and gcache-inspector on the MySQL-family servers.
//
// The Linux Client installs these three at provisioning (linuxclient_files.go); a database server
// is the other place they are wanted, because that is where the files are: the .ibd files, the
// binary logs, the galera.cache. So a standalone Percona Server, MySQL or MariaDB node, and every
// data member of a Percona Server, PXC, MySQL or MariaDB cluster, can ask for them too —
// gcache-inspector only on the two Galera kinds, PXC and MariaDB Galera.
//
// They are installed by a phase of their own at the end of the deploy rather than by each
// provisioner, for the reason reconcileNetem gives: a phase that waits for each server to be
// running and then acts on it covers ten node and frame types without touching ten
// provisioners. A tool already on the node is left alone, so a redeploy costs one version check
// per tool. Installing never fails anything: a server whose tools did not land is still a
// running server, and the stack's notifications say what happened.

// dbFileToolTypes are the server types that can carry the tools, standalone or member.
var dbFileToolTypes = map[string]bool{
	"ps": true, "mysqlce": true, "mariadb": true, // standalone
	"pxc": true, "mysql": true, "innodb": true, "mariadbrepl": true, "mariadbgalera": true,
	"mysqlcerepl": true, "mysqlceinnodb": true, // members
}

// dbFileToolTarget is one server and what it installs, with what the install script needs.
type dbFileToolTarget struct {
	node     designNode
	tools    []lcFileTool
	os, arch string
	useProxy bool
}

// dbFileToolsFor is what a node installs: its own choice when standalone, its frame's when a
// member. An arbitrator (garbd) holds no data, so it gets nothing.
func dbFileToolsFor(n designNode, doc designDoc) dbFileToolTarget {
	t := dbFileToolTarget{node: n, os: n.OS, arch: n.Arch, useProxy: n.UseProxy}
	if !dbFileToolTypes[n.Type] {
		return t
	}
	innodump, binlogsum, gcache := n.InstallInnodump, n.InstallBinlogsum, false
	if n.FrameID != "" {
		f := frameByID(doc, n.FrameID)
		if f.ID == "" || n.Role == "arbitrator" {
			return t
		}
		innodump, binlogsum = f.InstallInnodump, f.InstallBinlogsum
		gcache = f.InstallGcacheInspector && (f.Type == "pxc" || f.Type == "mariadbgalera")
		t.useProxy = t.useProxy || f.UseProxy
		if t.os == "" {
			t.os = f.OS
		}
		if t.arch == "" {
			t.arch = f.Arch
		}
	}
	if innodump {
		t.tools = append(t.tools, lcToolInnodump)
	}
	if binlogsum {
		t.tools = append(t.tools, lcToolBinlogsum)
	}
	if gcache {
		t.tools = append(t.tools, lcToolGcacheInspector)
	}
	return t
}

// reconcileDBFileTools installs what each running server asked for. See the file comment.
func (a *App) reconcileDBFileTools(ctx context.Context, st Stack, doc designDoc) {
	var targets []dbFileToolTarget
	for _, n := range doc.Nodes {
		if t := dbFileToolsFor(n, doc); len(t.tools) > 0 {
			targets = append(targets, t)
		}
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		if !a.waitNodeRunning(st.ID, t.node.ID, deployTimeout()) {
			log.Printf("stack %d file tools: %s not running; nothing installed", st.ID, t.node.Label)
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, t.node.ID)
		if err != nil || dep.ContainerID == "" {
			continue
		}
		logln := func(s string) { log.Printf("stack %d file tools: %s: %s", st.ID, t.node.Label, s) }
		script := lcFileToolsPrelude(t.os) + lcFileToolsBody
		var got, failed []string
		for _, tool := range t.tools {
			if v := a.lcFileToolVersion(ctx, dep.ContainerID, tool); v != "" {
				continue // already there from an earlier deploy
			}
			if err := a.runStep(ctx, dep.ContainerID, script, lcFileToolEnv(tool, t.arch, t.useProxy), logln); err != nil {
				failed = append(failed, tool.ID+" ("+lastLines(err.Error(), 120)+")")
				continue
			}
			if v := a.lcFileToolVersion(ctx, dep.ContainerID, tool); v != "" {
				got = append(got, tool.ID+" "+v)
			} else {
				failed = append(failed, tool.ID)
			}
		}
		if len(got) > 0 {
			a.notifyStack(st.ID, "stack.filetools", "info", "File tools installed",
				fmt.Sprintf("%s now has %s in /usr/local/bin.", t.node.Label, lcJoinAnd(got)), t.node.ID)
		}
		if len(failed) > 0 {
			a.notifyStack(st.ID, "stack.filetools", "warning", "File tools not installed",
				fmt.Sprintf("%s: %s did not install. The server is running; install from its terminal.",
					t.node.Label, strings.Join(failed, ", ")), t.node.ID)
		}
	}
}
