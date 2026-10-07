package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ceph_test.go — a Ceph-backed K3D frame: the volume size lands where each operator reads it,
// growth is switched on the way each release spells it, and the grow API never shrinks.

// valueAt returns the uncommented values at a cr.yaml path matched by want.
func valueAt(src string, want func(string) bool) []string {
	var out []string
	path := newYPath()
	for _, ln := range strings.Split(src, "\n") {
		ind, commented, body := crLine(ln)
		key := path.update(ind, commented, body)
		if key != "" && want(path.String()) {
			_, v, _ := strings.Cut(body, ":")
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

func TestDataVolumeSizeLandsWhereEachOperatorReadsIt(t *testing.T) {
	for _, c := range []struct {
		op, file string
		want     int // how many database volume sizes the file has
	}{
		{"pxc", "testdata/cr.yaml", 1},
		{"psmdb", "testdata/cr-psmdb.yaml", 3}, // the replica set, and its hidden and non-voting members
		{"pg", "testdata/cr-pg.yaml", 1},
		{"ps", "testdata/cr-ps.yaml", 1},
	} {
		raw, err := os.ReadFile(c.file)
		if err != nil {
			t.Logf("%s: no fixture (%v), skipped", c.op, err)
			continue
		}
		out := crSetDataStorage(string(raw), c.op, "25Gi")
		got := valueAt(out, func(p string) bool { return k3dDataVolumePath(c.op, p) })
		if len(got) != c.want {
			t.Errorf("%s: %d database volume sizes, want %d", c.op, len(got), c.want)
		}
		for _, v := range got {
			if v != "25Gi" {
				t.Errorf("%s: database volume is %q, want 25Gi", c.op, v)
			}
		}
		// Nothing else changes size: backups, the proxy, config servers keep the shipped value.
		before := valueAt(string(raw), func(p string) bool { return strings.HasSuffix(p, ".storage") && !k3dDataVolumePath(c.op, p) })
		after := valueAt(out, func(p string) bool { return strings.HasSuffix(p, ".storage") && !k3dDataVolumePath(c.op, p) })
		if strings.Join(before, ",") != strings.Join(after, ",") {
			t.Errorf("%s: other volumes changed: %v → %v", c.op, before, after)
		}
	}
}

func TestVolumeGrowthIsSpelledPerRelease(t *testing.T) {
	for _, c := range []struct{ op, ver, want string }{
		{"pxc", "1.15.1", volScaleNone}, {"pxc", "1.16.0", volScaleExpansion}, {"pxc", "1.19.1", volScaleExpansion}, {"pxc", "1.20.0", volScaleScaling},
		{"psmdb", "1.17.0", volScaleNone}, {"psmdb", "1.18.0", volScaleExpansion}, {"psmdb", "1.21.2", volScaleExpansion}, {"psmdb", "1.22.0", volScaleScaling},
		{"ps", "0.10.0", volScaleNone}, {"ps", "0.11.0", volScaleExpansion}, {"ps", "1.1.0", volScaleExpansion}, {"ps", "1.2.0", volScaleScaling},
		{"pg", "2.7.0", volScalePVC}, {"pg", "3.1.0", volScalePVC},
		{"cnpg", "0.26.0", volScaleNone}, {"pxc", "", volScaleNone},
	} {
		if got := k3dVolumeScaling(c.op, c.ver); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.op, c.ver, got, c.want)
		}
	}
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skip(err)
	}
	ceph := designFrame{K3DStorage: "ceph", K3DStorageGB: 10}
	newer := k3dStorageCR(string(raw), ceph, "pxc", "1.20.0", func(string) {})
	if got := valueAt(newer, func(p string) bool { return p == "spec.storageScaling.enableVolumeScaling" }); len(got) != 1 || got[0] != "true" {
		t.Errorf("1.20.0: storageScaling.enableVolumeScaling = %v", got)
	}
	older := k3dStorageCR(string(raw), ceph, "pxc", "1.16.0", func(string) {})
	if got := valueAt(older, func(p string) bool { return p == "spec.enableVolumeExpansion" }); len(got) != 1 || got[0] != "true" {
		t.Errorf("1.16.0: enableVolumeExpansion = %v", got)
	}
	// On local-path nothing can grow, so nothing is switched on; the size still applies.
	local := k3dStorageCR(string(raw), designFrame{K3DStorageGB: 10}, "pxc", "1.20.0", func(string) {})
	if strings.Contains(local, "enableVolumeScaling: true") || strings.Contains(local, "enableVolumeExpansion: true") {
		t.Error("volume growth switched on for a local-path cluster")
	}
	if got := valueAt(local, func(p string) bool { return k3dDataVolumePath("pxc", p) }); len(got) != 1 || got[0] != "10Gi" {
		t.Errorf("local-path cluster's volume size: %v", got)
	}
	// Untouched when the frame asks for nothing.
	if k3dStorageCR(string(raw), designFrame{}, "pxc", "1.20.0", func(string) {}) != string(raw) {
		t.Error("a frame with no storage choices changed cr.yaml")
	}
}

func TestQuantities(t *testing.T) {
	for in, want := range map[string]int64{"6G": 6e9, "3Gi": 3 << 30, "1073741824": 1 << 30, "512Mi": 512 << 20, "1.5Gi": 3 << 29} {
		if got, ok := parseQuantity(in); !ok || got != want {
			t.Errorf("parseQuantity(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	if _, ok := parseQuantity("lots"); ok {
		t.Error("parsed junk")
	}
}

func TestGrowPatchCoversEveryDataVolume(t *testing.T) {
	var ops []struct{ Op, Path, Value string }
	p, _ := k3dGrowPatch("psmdb", 2, "8Gi")
	json.Unmarshal([]byte(p), &ops)
	if len(ops) != 2 || ops[1].Path != "/spec/replsets/1/volumeSpec/persistentVolumeClaim/resources/requests/storage" || ops[0].Value != "8Gi" {
		t.Errorf("psmdb patch: %s", p)
	}
	p, _ = k3dGrowPatch("pg", 1, "8Gi")
	if !strings.Contains(p, "/spec/instances/0/dataVolumeClaimSpec/resources/requests/storage") {
		t.Errorf("pg patch: %s", p)
	}
	if _, err := k3dGrowPatch("pg", 0, "8Gi"); err == nil {
		t.Error("a patch with nothing to grow was built")
	}
	for name, want := range map[string]bool{
		"datadir-k3d-01-pxc-0": true, "datadir-k3d-01-haproxy-0": false,
		"mongod-data-k3d-01-rs0-0": true, "mongod-data-k3d-01-cfg-0": false,
	} {
		op := "pxc"
		if strings.HasPrefix(name, "mongod") {
			op = "psmdb"
		}
		if k3dDataPVC(op, name) != want {
			t.Errorf("%s: data volume = %v", name, !want)
		}
	}
}

func TestCephFrameChecks(t *testing.T) {
	doc := designDoc{Nodes: []designNode{{ID: "c1", Type: "ceph", Label: "ceph-01", CephOSDSizeGB: 20}}}
	has := func(is []issue, level, part string) bool {
		for _, i := range is {
			if i.Level == level && strings.Contains(i.Message, part) {
				return true
			}
		}
		return false
	}
	f := designFrame{Label: "k3d-01", K3DOperator: "pxc", K3DOperatorVer: "1.20.0", K3DStorage: "ceph"}
	if is := k3dStorageIssues(f, doc); !has(is, "error", "no Ceph node") {
		t.Errorf("Ceph without a node: %+v", is)
	}
	f.CephNodeID = "c1"
	if is := k3dStorageIssues(f, doc); len(is) != 0 {
		t.Errorf("a good Ceph frame: %+v", is)
	}
	f.K3DStorageGB = 50
	if is := k3dStorageIssues(f, doc); !has(is, "warning", "more than Ceph node ceph-01 holds") {
		t.Errorf("volumes larger than the OSD: %+v", is)
	}
	f.K3DStorageGB, f.K3DOperatorVer = 0, "1.15.1"
	if is := k3dStorageIssues(f, doc); !has(is, "warning", "cannot grow volumes (that came in 1.16.0)") {
		t.Errorf("an operator that cannot grow: %+v", is)
	}
	f.K3DOperator = "everest"
	if is := k3dStorageIssues(f, doc); !has(is, "error", "OpenEverest") {
		t.Errorf("Everest on Ceph: %+v", is)
	}
	if is := cephNodeIssues(designNode{Label: "ceph-01", CephOSDSizeGB: 2}); !has(is, "error", "5 to 1000") {
		t.Errorf("a 2 GB OSD: %+v", is)
	}
}

func TestCephCSIMapsTheWayTheKernelCan(t *testing.T) {
	v := cephCSIValues(cephConfig{FSID: "f-1", Monitor: "172.19.0.5:6789", Pool: "rbd", User: "k8s"}, "AQ==")
	for _, want := range []string{`ms_mode=legacy`, `allowVolumeExpansion: true`, `is-default-class: "true"`, `monitors: ["172.19.0.5:6789"]`, `clusterID: "f-1"`} {
		if !strings.Contains(v, want) {
			t.Errorf("chart values lack %s:\n%s", want, v)
		}
	}
	args := strings.Join(k3dCephCreateArgs(), " ")
	for _, want := range []string{"/dev:/dev@all", "/lib/modules:/lib/modules:ro@all", "--disable=local-storage@server:*"} {
		if !strings.Contains(args, want) {
			t.Errorf("k3d create args lack %s: %s", want, args)
		}
	}
	// The bootstrap makes every key the kernel can use, and offers only that cipher.
	for _, want := range []string{"--key-type=aes", "--auth-allowed-ciphers aes", "AUTH_INSECURE_SERVICE_TICKETS", "--inject-monmap"} {
		if !strings.Contains(cephBootstrapScript, want) {
			t.Errorf("bootstrap lacks %s", want)
		}
	}
}
