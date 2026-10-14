package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shape `make versions` recorded for Oracle Linux 8 while AppStream's postgresql
// module hid the ppg packages: every PostgreSQL series present, every one empty.
const versionGateYAML = `images:
  - os: oraclelinux
    version: "8"
    platform: linux/amd64
    arch: amd64
    tag: dbcanvas-systemd:oraclelinux-8-amd64
    percona_server:
      "8.0":
        - 8.0.45-36.1
    percona_postgresql:
      "16": []
      "17": []
  - os: oraclelinux
    version: "9"
    platform: linux/amd64
    arch: amd64
    tag: dbcanvas-systemd:oraclelinux-9-amd64
    percona_postgresql:
      "16":
        - 16.11-2
      "17": []
`

func withVersionGateCatalog(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "versions.yaml")
	if err := os.WriteFile(path, []byte(versionGateYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSIONS_FILE", path)
	t.Setenv("IMAGES_FILE", path)
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
}

func TestCatalogVersionIssues(t *testing.T) {
	withVersionGateCatalog(t)
	pg := func(label, osVer, major string) designNode {
		return designNode{ID: label, Label: label, Type: "pg", OS: "oraclelinux", OSVersion: osVer, Arch: "amd64", PGMajor: major}
	}
	cases := []struct {
		name string
		doc  designDoc
		want string // substring of the single expected error; "" → none
	}{
		{"pg on OL8 series is empty", designDoc{Nodes: []designNode{pg("pg1", "8", "16")}},
			"Node pg1: no PostgreSQL 16 builds are available on oraclelinux 8 (amd64)"},
		{"pg on OL8 with no series resolves to the default", designDoc{Nodes: []designNode{pg("pg1", "8", "")}},
			"Node pg1: no PostgreSQL 16 builds are available on oraclelinux 8"},
		{"pg on OL9 series has builds", designDoc{Nodes: []designNode{pg("pg1", "9", "16")}}, ""},
		{"pg on OL9 other series is empty", designDoc{Nodes: []designNode{pg("pg1", "9", "17")}},
			"no PostgreSQL 17 builds"},
		{"default series has builds", designDoc{Nodes: []designNode{pg("pg1", "9", "")}}, ""},
		{"image with no catalogue entry is left to the deploy", designDoc{Nodes: []designNode{pg("pg1", "10", "16")}}, ""},
		{"other engines on OL8 unaffected", designDoc{Nodes: []designNode{{Label: "ps1", Type: "ps", OS: "oraclelinux", OSVersion: "8", PSMajor: "8.0"}}}, ""},
		{"patroni frame with members is checked", designDoc{
			Frames: []designFrame{{ID: "f1", Type: "patroni", Label: "pg-ha", OS: "oraclelinux", OSVersion: "8", Arch: "amd64", PGMajor: "17"}},
			Nodes:  []designNode{{ID: "m1", Label: "m1", Type: "patroni", FrameID: "f1"}},
		}, "Cluster pg-ha: no PostgreSQL 17 builds"},
		{"All-in-One PostgreSQL instances on OL8: one error per major", designDoc{Nodes: []designNode{{
			Label: "aio1", Type: "aio", OS: "oraclelinux", OSVersion: "8", Arch: "amd64",
			AIOPSMajor:   "8.0",
			AIOInstances: []aioInstance{{Kind: "ps"}, {Kind: "pg", PGMajor: "16"}, {Kind: "pg", PGMajor: "16"}},
		}}}, "All-in-One aio1: no PostgreSQL 16 builds"},
		{"All-in-One with installable instances", designDoc{Nodes: []designNode{{
			Label: "aio1", Type: "aio", OS: "oraclelinux", OSVersion: "9", Arch: "amd64",
			AIOInstances: []aioInstance{{Kind: "pg", PGMajor: "16"}, {Kind: "pg"}},
		}}}, ""},
		{"empty frame is not checked", designDoc{
			Frames: []designFrame{{ID: "f1", Type: "patroni", Label: "pg-ha", OS: "oraclelinux", OSVersion: "8", PGMajor: "17"}},
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := catalogVersionIssues(c.doc)
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no issues, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Level != "error" || !strings.Contains(got[0].Message, c.want) {
				t.Fatalf("want one error containing %q, got %+v", c.want, got)
			}
		})
	}
}
