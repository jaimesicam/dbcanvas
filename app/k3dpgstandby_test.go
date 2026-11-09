package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// pgStandbyDoc is two Percona PostgreSQL operator frames with a link of the given type from the
// first to the second, both backing up to the same SeaweedFS node.
func pgStandbyDoc(linkType string) designDoc {
	doc := designDoc{Frames: []designFrame{
		{ID: "k-src", Type: "k3d", Label: "east", K3DOperator: "pg", SeaweedFSNodeID: "sw"},
		{ID: "k-dst", Type: "k3d", Label: "west", K3DOperator: "pg", SeaweedFSNodeID: "sw"},
	}}
	doc.Edges = []designEdge{{ID: "e1", From: edgeEnd{Node: "k-src"}, To: edgeEnd{Node: "k-dst"}, Type: linkType}}
	return doc
}

func TestPGTransformStandby(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-pg.yaml")
	if err != nil {
		t.Skipf("no PG cr.yaml fixture: %v", err)
	}
	out := pgTransform(string(raw), pgOptions{
		Name:            "west",
		RepoPathCluster: "east",
		S3:              &crS3{Bucket: "backups", Region: "us-east-1", EndpointURL: "https://sw.example.net:8333", Secret: "west-pgbackrest-secrets"},
		Standby:         &pgStandby{Host: "172.20.0.240", Port: 5432, RepoName: "repo1"},
	})
	var cr struct {
		Spec struct {
			Standby struct {
				Enabled  bool   `json:"enabled"`
				Host     string `json:"host"`
				Port     int    `json:"port"`
				RepoName string `json:"repoName"`
			} `json:"standby"`
			Backups struct {
				PGBackRest struct {
					Global map[string]string `json:"global"`
				} `json:"pgbackrest"`
			} `json:"backups"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &cr); err != nil {
		t.Fatalf("the rewritten cr.yaml does not parse: %v", err)
	}
	sb := cr.Spec.Standby
	if !sb.Enabled || sb.Host != "172.20.0.240" || sb.Port != 5432 || sb.RepoName != "repo1" {
		t.Errorf("spec.standby = %+v", sb)
	}
	if got := cr.Spec.Backups.PGBackRest.Global["repo1-path"]; got != "/pgbackrest/east/repo1" {
		t.Errorf("repo1-path = %q, want the source's repository", got)
	}

	// Streaming only: no repoName, and the cluster's own repository path.
	out = pgTransform(string(raw), pgOptions{
		Name:    "west",
		S3:      &crS3{Bucket: "backups", Region: "us-east-1", EndpointURL: "https://sw.example.net:8333", Secret: "s"},
		Standby: &pgStandby{Host: "172.20.0.240", Port: 5432},
	})
	cr.Spec.Standby.RepoName = ""
	if err := yaml.Unmarshal([]byte(out), &cr); err != nil {
		t.Fatalf("the streaming-only cr.yaml does not parse: %v", err)
	}
	if cr.Spec.Standby.RepoName != "" || cr.Spec.Standby.Host != "172.20.0.240" {
		t.Errorf("streaming-only spec.standby = %+v", cr.Spec.Standby)
	}
	if !strings.Contains(out, "repo1-path: /pgbackrest/west/repo1") {
		t.Error("a streaming-only standby lost its own repository path")
	}
	if strings.Contains(pgTransform(string(raw), pgOptions{Name: "east"}), "\n  standby:\n    enabled: true") {
		t.Error("a cluster that is not a standby has spec.standby enabled")
	}
}

func TestK3DPGStandbyLinks(t *testing.T) {
	doc := pgStandbyDoc("async")
	if src, ok := k3dPGStandbySourceOf(doc, "k-dst"); !ok || src.ID != "k-src" {
		t.Errorf("west is not a standby of east: %v %v", src.ID, ok)
	}
	if !k3dPGIsStandbySource(doc, "k-src") || k3dPGIsStandbySource(doc, "k-dst") {
		t.Error("the source is not recognised as one")
	}
	if !k3dPGStandbySharesRepo(doc.Frames[0], doc.Frames[1]) {
		t.Error("two frames on one SeaweedFS node and bucket do not share a repository")
	}
	other := doc.Frames[1]
	other.SeaweedFSBucket = "second"
	if k3dPGStandbySharesRepo(doc.Frames[0], other) {
		t.Error("different buckets are reported as a shared repository")
	}
	// Not a PXC-operator link.
	if len(k3dReplLinks(doc)) != 0 {
		t.Error("a PostgreSQL standby link resolves as a PXC replication link")
	}
}

func TestK3DPGStandbyIssues(t *testing.T) {
	errs := func(doc designDoc) string {
		var out []string
		seen := map[string]bool{}
		for _, e := range doc.Edges {
			if src, dst, ok := k3dPGStandbyEdgeFrames(doc, e); ok {
				for _, is := range k3dPGStandbyIssues(doc, src, dst, e, seen) {
					if is.Level == "error" {
						out = append(out, is.Message)
					}
				}
			}
		}
		return strings.Join(out, " | ")
	}
	if got := errs(pgStandbyDoc("async")); got != "" {
		t.Errorf("a plain standby link has errors: %s", got)
	}
	both := pgStandbyDoc("async")
	for i := range both.Frames {
		both.Frames[i].K3DCertManager, both.Frames[i].K3DCertIssuer = true, "intranet"
	}
	both.Frames[1].K3DCertIssuer = "openbao" // the same root either way
	if got := errs(both); got != "" {
		t.Errorf("both ends on the Intranet CA has errors: %s", got)
	}
	cases := []struct {
		name string
		edit func(*designDoc)
		want string
	}{
		{"bidirectional", func(d *designDoc) { d.Edges[0].Type = "bidir" }, "one way"},
		{"pg_tde", func(d *designDoc) { d.Frames[1].K3DPGTDE = true }, "pg_tde"},
		{"logical replicas", func(d *designDoc) { d.Frames[1].K3DPGLogicalReplicas = 1 }, "logical replicas"},
		{"issuer on the standby only", func(d *designDoc) {
			d.Frames[1].K3DCertManager, d.Frames[1].K3DCertIssuer = true, "intranet"
		}, "same way"},
		{"issuer on the source only", func(d *designDoc) {
			d.Frames[0].K3DCertManager, d.Frames[0].K3DCertIssuer = true, "openbao"
		}, "same way"},
		{"loop", func(d *designDoc) {
			d.Edges = append(d.Edges, designEdge{ID: "e2", From: edgeEnd{Node: "k-dst"}, To: edgeEnd{Node: "k-src"}, Type: "async"})
		}, "loop"},
	}
	for _, c := range cases {
		doc := pgStandbyDoc("async")
		c.edit(&doc)
		if got := errs(doc); !strings.Contains(got, c.want) {
			t.Errorf("%s: errors %q do not mention %q", c.name, got, c.want)
		}
	}
}
