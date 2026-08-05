package main

import (
	"strings"
	"testing"
)

// The generated manifests were validated with `kubectl apply --dry-run=server` against a live
// CNPG 1.30 + barman-cloud plugin 0.14.0 cluster. These tests lock in the parts that a server
// dry-run would *not* catch: that deprecated fields stay absent, and that an unconfigured
// option produces no stanza at all rather than an empty one.

func TestCNPGClusterManifest(t *testing.T) {
	full := string(cnpgClusterManifest("pg", "ns", 3, 20, "17", "pg-store"))
	for _, want := range []string{
		"apiVersion: postgresql.cnpg.io/v1",
		"kind: Cluster",
		"  instances: 3",
		"  imageName: ghcr.io/cloudnative-pg/postgresql:17",
		"    size: 20Gi",
		"  - name: barman-cloud.cloudnative-pg.io",
		"    isWALArchiver: true",
		"      barmanObjectName: pg-store",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("cluster manifest missing %q:\n%s", want, full)
		}
	}
	// The in-tree backup field is removed in CNPG 1.31 — it must never be emitted.
	if strings.Contains(full, "barmanObjectStore") {
		t.Errorf("cluster manifest uses the removed in-tree barmanObjectStore:\n%s", full)
	}
	// enablePodMonitor is deprecated; the PodMonitor is written as its own resource.
	if strings.Contains(full, "enablePodMonitor") {
		t.Errorf("cluster manifest uses deprecated enablePodMonitor:\n%s", full)
	}

	// An unpinned version and no backup target must leave both stanzas out entirely.
	min := string(cnpgClusterManifest("pg", "ns", 1, 1, "", ""))
	for _, unwanted := range []string{"imageName", "plugins", "barmanObjectName"} {
		if strings.Contains(min, unwanted) {
			t.Errorf("minimal manifest should not mention %q:\n%s", unwanted, min)
		}
	}
}

func TestCNPGObjectStoreManifest(t *testing.T) {
	s3 := &crS3{Bucket: "buck", Region: "us-east-1", EndpointURL: "http://sw:8333", Secret: "sec"}
	got := string(cnpgObjectStoreManifest("pg-store", "ns", s3))
	for _, want := range []string{
		"apiVersion: barmancloud.cnpg.io/v1",
		"kind: ObjectStore",
		"    destinationPath: s3://buck/",
		"    endpointURL: http://sw:8333",
		"        name: sec",
		"        key: AWS_ACCESS_KEY_ID",
		"        key: AWS_SECRET_ACCESS_KEY",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("object store manifest missing %q:\n%s", want, got)
		}
	}
}

func TestCNPGScheduledBackupUsesPlugin(t *testing.T) {
	got := string(cnpgScheduledBackupManifest("pg", "ns"))
	if !strings.Contains(got, "method: plugin") {
		t.Errorf("scheduled backup should use the plugin method:\n%s", got)
	}
	if !strings.Contains(got, "name: barman-cloud.cloudnative-pg.io") {
		t.Errorf("scheduled backup missing the plugin name:\n%s", got)
	}
	// Six-field cron (CNPG includes seconds); a five-field schedule is rejected.
	if !strings.Contains(got, `schedule: "0 0 0 * * *"`) {
		t.Errorf("scheduled backup should carry a six-field cron:\n%s", got)
	}
}

func TestHelmChartManifest(t *testing.T) {
	// Unpinned: no version key at all, so helm resolves the repo's latest.
	plain := string(helmChartManifest("c", "https://r", "chart", "", "ns", ""))
	if strings.Contains(plain, "version:") {
		t.Errorf("blank version should emit no version key:\n%s", plain)
	}
	if !strings.Contains(plain, "  createNamespace: true") {
		t.Errorf("target namespace should be created:\n%s", plain)
	}
	// Values are indented under valuesContent, which is a literal block.
	withVals := string(helmChartManifest("c", "https://r", "chart", "1.2.3", "ns", "a:\n  b: 1\n"))
	for _, want := range []string{"  version: 1.2.3", "  valuesContent: |", "    a:", "      b: 1"} {
		if !strings.Contains(withVals, want) {
			t.Errorf("values manifest missing %q:\n%s", want, withVals)
		}
	}
}

func TestCNPGDefaults(t *testing.T) {
	// A frame designed before these fields existed still deploys, with sane defaults.
	if n := cnpgInstances(designFrame{}); n != 3 {
		t.Errorf("default instances = %d, want 3", n)
	}
	if g := cnpgStorageGB(designFrame{}); g != 1 {
		t.Errorf("default storage = %d, want 1", g)
	}
	// Clamped, so a hand-edited design cannot ask for something absurd.
	if n := cnpgInstances(designFrame{K3DCNPGInstances: 99}); n != 5 {
		t.Errorf("instances clamp = %d, want 5", n)
	}
}

func TestPromStackValuesScrapeAllNamespaces(t *testing.T) {
	// The CNPG PodMonitor lives in the database namespace, not the chart's release namespace.
	// Without these two selectors set false, Prometheus ignores it and the cluster is silently
	// unmonitored.
	got := promStackValues("pw")
	for _, want := range []string{
		"podMonitorSelectorNilUsesHelmValues: false",
		"ruleSelectorNilUsesHelmValues: false",
		`adminPassword: "pw"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prom values missing %q:\n%s", want, got)
		}
	}
}
