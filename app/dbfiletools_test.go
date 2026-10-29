package main

import (
	"strings"
	"testing"
)

func dbToolIDs(t dbFileToolTarget) string {
	var ids []string
	for _, x := range t.tools {
		ids = append(ids, x.ID)
	}
	return strings.Join(ids, ",")
}

// A standalone server installs its own choice; a member installs its frame's, and only the Galera
// kinds get gcache-inspector.
func TestDBFileToolsFor(t *testing.T) {
	doc := designDoc{
		Nodes: []designNode{
			{ID: "ps1", Type: "ps", OS: "ubuntu", InstallInnodump: true, InstallBinlogsum: true},
			{ID: "ma1", Type: "mariadb", InstallBinlogsum: true},
			{ID: "x1", Type: "pxc", FrameID: "fx", Role: "regular"},
			{ID: "x2", Type: "pxc", FrameID: "fx", Role: "arbitrator"},
			{ID: "r1", Type: "mysql", FrameID: "fr", Role: "primary"},
			{ID: "g1", Type: "mariadbgalera", FrameID: "fg"},
			{ID: "pg1", Type: "pg", InstallInnodump: true},
		},
		Frames: []designFrame{
			{ID: "fx", Type: "pxc", OS: "oraclelinux", UseProxy: true, InstallInnodump: true, InstallGcacheInspector: true},
			// gcache-inspector on a frame with no galera.cache is ignored.
			{ID: "fr", Type: "mysql", InstallBinlogsum: true, InstallGcacheInspector: true},
			{ID: "fg", Type: "mariadbgalera", InstallInnodump: true, InstallBinlogsum: true, InstallGcacheInspector: true},
		},
	}
	want := map[string]string{
		"ps1": "innodump,binlogsum",
		"ma1": "binlogsum",
		"x1":  "innodump,gcache-inspector",
		"x2":  "", // garbd holds no data
		"r1":  "binlogsum",
		"g1":  "innodump,binlogsum,gcache-inspector",
		"pg1": "", // not a MySQL-family server
	}
	for _, n := range doc.Nodes {
		got := dbFileToolsFor(n, doc)
		if ids := dbToolIDs(got); ids != want[n.ID] {
			t.Errorf("%s installs %q, want %q", n.ID, ids, want[n.ID])
		}
	}
	// A member takes its OS and proxy from the frame when it has none of its own.
	x1 := dbFileToolsFor(doc.Nodes[2], doc)
	if x1.os != "oraclelinux" || !x1.useProxy {
		t.Errorf("member target = %+v, want the frame's OS and proxy", x1)
	}
	if ps := dbFileToolsFor(doc.Nodes[0], doc); ps.os != "ubuntu" {
		t.Errorf("standalone OS = %q, want its own", ps.os)
	}
}

// The binlogsum asset carries its version like innodump's, so the shared checksum match covers it.
func TestLCFileToolsAssetPattern(t *testing.T) {
	if !strings.Contains(lcFileToolsBody, `${TOOL}_([0-9][^_]*_)?linux_${ARCH}\.tar\.gz`) {
		t.Fatal("the asset match must allow a version in the name (innodump, binlogsum) and none (gcache-inspector)")
	}
	if !strings.HasSuffix(lcToolBinlogsum.Repo, "/binlogsum/releases") {
		t.Errorf("binlogsum repo = %q", lcToolBinlogsum.Repo)
	}
}
