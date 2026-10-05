package main

import (
	"strings"
	"testing"
)

// What `kubectl config view --raw` prints inside a k3s server, server already rewritten.
const k3sKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Q0E=
    server: https://k3d-%s-serverlb:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: default
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
`

func k3s(t *testing.T, cluster, name string) kubeconfig {
	t.Helper()
	kc, err := parseKubeconfig(strings.ReplaceAll(k3sKubeconfig, "%s", cluster))
	if err != nil {
		t.Fatal(err)
	}
	kc, err = renameKubeconfig(kc, name)
	if err != nil {
		t.Fatal(err)
	}
	return kc
}

// Two k3s configs collide on every name ("default"); renamed, they coexist.
func TestMergeKubeconfigsTwoClusters(t *testing.T) {
	a, b := k3s(t, "k3d-01-s5", "k3d-01"), k3s(t, "k3d-02-s5", "k3d-02")
	m := mergeKubeconfigs(kubeconfig{}, []kubeconfig{a, b}, "")
	if len(m.Clusters) != 2 || len(m.Users) != 2 || len(m.Contexts) != 2 {
		t.Fatalf("want 2 of each, got %d clusters %d users %d contexts", len(m.Clusters), len(m.Users), len(m.Contexts))
	}
	if m.CurrentContext != "k3d-01" {
		t.Errorf("current = %q, want the first cluster", m.CurrentContext)
	}
	if c := m.Contexts[1].Context; c.Cluster != "k3d-02" || c.User != "k3d-02" {
		t.Errorf("context k3d-02 points at %+v", c)
	}
	if s := m.Clusters[1].Cluster["server"]; s != "https://k3d-k3d-02-s5-serverlb:6443" {
		t.Errorf("server = %v", s)
	}
	if u := m.Users[0].User["client-key-data"]; u != "S0VZ" {
		t.Error("the user's credentials must survive the rename")
	}
}

// Contexts someone added by hand are kept; ours are replaced, not duplicated; and the current
// context the person chose survives a re-copy.
func TestMergeKubeconfigsKeepsForeignEntries(t *testing.T) {
	existing := mergeKubeconfigs(kubeconfig{}, []kubeconfig{k3s(t, "old", "k3d-01")}, "")
	existing.Clusters = append(existing.Clusters, kcCluster{Name: "prod", Cluster: map[string]any{"server": "https://prod:6443"}})
	existing.Users = append(existing.Users, kcUser{Name: "alice", User: map[string]any{"token": "x"}})
	existing.Contexts = append(existing.Contexts, kcContext{Name: "alice@prod", Context: kcContextBody{Cluster: "prod", User: "alice"}})
	existing.CurrentContext = "alice@prod"

	m := mergeKubeconfigs(existing, []kubeconfig{k3s(t, "new", "k3d-01")}, "")
	if len(m.Contexts) != 2 {
		t.Fatalf("want k3d-01 + alice@prod, got %d contexts", len(m.Contexts))
	}
	if !m.hasContext("alice@prod") || m.CurrentContext != "alice@prod" {
		t.Errorf("the hand-added context and its being current must survive, current=%q", m.CurrentContext)
	}
	for _, c := range m.Clusters {
		if c.Name == "k3d-01" && c.Cluster["server"] != "https://k3d-new-serverlb:6443" {
			t.Error("a re-copy must replace the cluster's entry, not keep the stale one")
		}
	}

	if m2 := mergeKubeconfigs(existing, []kubeconfig{k3s(t, "new", "k3d-01")}, "k3d-01"); m2.CurrentContext != "k3d-01" {
		t.Errorf("an asked-for context must win, got %q", m2.CurrentContext)
	}
}

func TestKubeconfigRoundTrip(t *testing.T) {
	m := mergeKubeconfigs(kubeconfig{}, []kubeconfig{k3s(t, "c", "k3d-01")}, "")
	out, err := m.render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"apiVersion: v1", "kind: Config", "current-context: k3d-01", "certificate-authority-data: Q0E="} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered kubeconfig lacks %q:\n%s", want, out)
		}
	}
	back, err := parseKubeconfig(string(out))
	if err != nil || !back.hasContext("k3d-01") {
		t.Fatalf("the rendered file does not read back: %v", err)
	}
}
