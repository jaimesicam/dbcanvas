package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// spec.search against the operator's real 1.23.0 cr.yaml — the first release that has it.
func TestPSMDBTransformSearch(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-psmdb-1.23.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := psmdbTransform(string(raw), psmdbOptions{Name: "lab", Search: true})

	var doc struct {
		Spec struct {
			Image  string `json:"image"`
			Search struct {
				Enabled       bool   `json:"enabled"`
				Image         string `json:"image"`
				Size          int    `json:"size"`
				Configuration string `json:"configuration"`
			} `json:"search"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("rewritten cr.yaml is not YAML: %v", err)
	}
	if doc.Spec.Image != psmdbSearchServerImage {
		t.Errorf("spec.image = %q, want the 8.3 server %q", doc.Spec.Image, psmdbSearchServerImage)
	}
	var mc struct {
		Metrics struct {
			Enabled bool `json:"enabled"`
		} `json:"metrics"`
	}
	if err := yaml.Unmarshal([]byte(doc.Spec.Search.Configuration), &mc); err != nil || !mc.Metrics.Enabled {
		t.Errorf("spec.search.configuration must turn mongot's metrics on: %q", doc.Spec.Search.Configuration)
	}
	if !doc.Spec.Search.Enabled || doc.Spec.Search.Image != psmdbSearchImage || doc.Spec.Search.Size != 1 {
		t.Errorf("spec.search = %+v", doc.Spec.Search)
	}
	if n := strings.Count(out, "\n  search:\n"); n != 1 {
		t.Errorf("want exactly one active spec.search, got %d", n)
	}
	if !strings.Contains(out, "`search` is already set") {
		t.Error("the shipped commented-out spec.search should be marked as a duplicate")
	}

	// Off: nothing changes about the image or search.
	off := psmdbTransform(string(raw), psmdbOptions{Name: "lab"})
	if strings.Contains(off, psmdbSearchServerImage) || strings.Contains(off, "\n  search:\n") {
		t.Error("search off must leave spec.image and spec.search alone")
	}
}

func TestPSMDBHasSearch(t *testing.T) {
	for v, want := range map[string]bool{"1.22.0": false, "1.23.0": true, "1.23.1": true, "1.24.0": true, "": false} {
		if got := psmdbHasSearch(v); got != want {
			t.Errorf("psmdbHasSearch(%q) = %v, want %v", v, got, want)
		}
	}
}

// mongod.conf takes one setParameter block, so the MClusterAdmin mechanisms and the
// mongot parameters must merge under one header.
func TestMergeSetParams(t *testing.T) {
	merged := mergeSetParams(mongoSCRAMSetParameter(), mongoSearchSetParams("rs-1.example.net:27028"))
	if strings.Count(merged, "setParameter:") != 1 {
		t.Fatalf("want one setParameter header:\n%s", merged)
	}
	conf := mongodConfYAML("rs", "", true, merged, "")
	var c map[string]any
	if err := yaml.Unmarshal([]byte(conf), &c); err != nil {
		t.Fatalf("mongod.conf is not YAML: %v\n%s", err, conf)
	}
	sp := c["setParameter"].(map[string]any)
	for k, want := range map[string]any{
		"mongotHost": "rs-1.example.net:27028", "searchIndexManagementHostAndPort": "rs-1.example.net:27028",
		"useGrpcForSearch": true, "authenticationMechanisms": "SCRAM-SHA-1,SCRAM-SHA-256",
	} {
		if sp[k] != want {
			t.Errorf("setParameter.%s = %v, want %v", k, sp[k], want)
		}
	}
	if mergeSetParams("", "") != "" {
		t.Error("nothing to merge should render nothing")
	}
}

func TestMongotConfigYAML(t *testing.T) {
	var c struct {
		SyncSource struct {
			ReplicaSet struct {
				HostAndPort []string `json:"hostAndPort"`
				ScramAuth   struct {
					Username string `json:"username"`
				} `json:"scramAuth"`
			} `json:"replicaSet"`
			Router *struct {
				HostAndPort []string `json:"hostAndPort"`
			} `json:"router"`
		} `json:"syncSource"`
		Server struct {
			Grpc struct {
				Address string `json:"address"`
			} `json:"grpc"`
		} `json:"server"`
	}
	rs := mongotConfigYAML([]string{"a:27017", "b:27017"}, nil)
	if err := yaml.Unmarshal([]byte(rs), &c); err != nil {
		t.Fatalf("%v\n%s", err, rs)
	}
	if len(c.SyncSource.ReplicaSet.HostAndPort) != 2 || c.SyncSource.ReplicaSet.ScramAuth.Username != mongoSearchUser ||
		c.SyncSource.Router != nil || c.Server.Grpc.Address != "0.0.0.0:27028" {
		t.Errorf("replica-set mongot.yml wrong: %+v", c)
	}
	sh := mongotConfigYAML([]string{"a:27017"}, []string{"mongos:27017"})
	c.SyncSource.Router = nil
	if err := yaml.Unmarshal([]byte(sh), &c); err != nil {
		t.Fatal(err)
	}
	if c.SyncSource.Router == nil || c.SyncSource.Router.HostAndPort[0] != "mongos:27017" {
		t.Errorf("sharded mongot.yml has no router: %s", sh)
	}
}

func TestMongoSearchIssues(t *testing.T) {
	f := designFrame{Type: "psmrs", Label: "rs", VectorSearch: true, PSMDBMajor: "8.0", OS: "oraclelinux", OSVersion: "10"}
	if iss := mongoSearchIssues(f); len(iss) != 2 {
		t.Errorf("8.0 on OL10 should fail twice (series + OS), got %v", iss)
	}
	f.PSMDBMajor, f.OSVersion = "8.3", "9"
	if iss := mongoSearchIssues(f); len(iss) != 0 {
		t.Errorf("8.3 on OL9 should pass, got %v", iss)
	}
	f.VectorSearch = false
	f.PSMDBMajor = "7.0"
	if iss := mongoSearchIssues(f); len(iss) != 0 {
		t.Error("search off validates nothing")
	}
}

// 8.3 is its own repository; falling through to the default once installed 8.0
// on a frame designed for vector search, and mongod refused the search parameters.
func TestPSMDBRepoSeries(t *testing.T) {
	for major, want := range map[string]string{"6.0": "psmdb-60", "7.0": "psmdb-70", "8.0": "psmdb-80", "8.3": "psmdb-83", "": "psmdb-80"} {
		if got := psmdbRepo(major); got != want {
			t.Errorf("psmdbRepo(%q) = %q, want %q", major, got, want)
		}
	}
}

// The status of a live 1.23.1 cluster with search on and rs0 exposed: up, and stuck.
func TestPSMDBSearchStatusStuck(t *testing.T) {
	stuck := `{"state":"initializing","ready":3,"size":3,"replsets":{"rs0":{"initialized":true,"ready":3,"size":3,"status":"ready"}},"search":{"rs0":{"ready":1,"size":1,"status":"ready"}}}`
	if !psmdbSearchStatusStuck([]byte(stuck)) {
		t.Error("the operator bug's status should count as up")
	}
	for name, st := range map[string]string{
		"still starting": `{"state":"initializing","replsets":{"rs0":{"status":"initializing"}},"search":{"rs0":{"status":"ready"}}}`,
		"mongot not up":  `{"state":"initializing","replsets":{"rs0":{"status":"ready"}},"search":{"rs0":{"status":"initializing"}}}`,
		"has a host":     `{"state":"initializing","host":"x","replsets":{"rs0":{"status":"ready"}},"search":{"rs0":{"status":"ready"}}}`,
		"no search":      `{"state":"initializing","replsets":{"rs0":{"status":"ready"}}}`,
		"error":          `{"state":"error","replsets":{"rs0":{"status":"ready"}},"search":{"rs0":{"status":"ready"}}}`,
	} {
		if psmdbSearchStatusStuck([]byte(st)) {
			t.Errorf("%s: must not count as up", name)
		}
	}
}

func TestPSMDBSearchExposeMode(t *testing.T) {
	for _, c := range []struct {
		search, sharded bool
		typ, want       string
	}{
		{true, false, "LoadBalancer", "nodeport"}, {true, false, "ClusterIP", "none"}, {true, false, "NodePort", ""},
		{true, true, "LoadBalancer", ""}, {false, false, "LoadBalancer", ""},
	} {
		if got := psmdbSearchExposeMode(c.search, c.sharded, c.typ); got != c.want {
			t.Errorf("search=%v sharded=%v %s: %q, want %q", c.search, c.sharded, c.typ, got, c.want)
		}
	}
}

func TestPSMDBSearchServicesYAML(t *testing.T) {
	y := psmdbSearchServicesYAML("lab", "rs0", 3)
	docs := strings.Split(strings.TrimPrefix(y, "---\n"), "---\n")
	if len(docs) != 4 {
		t.Fatalf("want 3 member Services + mongot's, got %d", len(docs))
	}
	for i, d := range docs {
		var svc struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Type     string            `json:"type"`
				Selector map[string]string `json:"selector"`
				Ports    []struct {
					Port int `json:"port"`
				} `json:"ports"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(d), &svc); err != nil {
			t.Fatalf("doc %d is not YAML: %v\n%s", i, err, d)
		}
		if svc.Spec.Type != "LoadBalancer" || svc.Metadata.Labels["app.kubernetes.io/managed-by"] != "dbcanvas" {
			t.Errorf("doc %d: %+v", i, svc)
		}
		// The operator adopts Services by its own names and deletes ones with its labels; ours must have neither.
		if !strings.HasSuffix(svc.Metadata.Name, "-ext") {
			t.Errorf("doc %d: %q must not use an operator Service name", i, svc.Metadata.Name)
		}
		if _, ok := svc.Metadata.Labels["app.kubernetes.io/component"]; ok {
			t.Errorf("doc %d carries an operator label", i)
		}
	}
	if !strings.Contains(y, "name: lab-rs0-1-ext\n") || !strings.Contains(y, "statefulset.kubernetes.io/pod-name: lab-rs0-search-0") || !strings.Contains(y, "port: 9946") {
		t.Errorf("names/selectors:\n%s", y)
	}
}
