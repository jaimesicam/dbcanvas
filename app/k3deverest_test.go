package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// An empty selection is the chart's default (all three), and whatever is chosen comes back in
// the chart's order with unknown keys dropped — the values file must never name an operator
// the chart does not have.
func TestEverestChosen(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, []string{"pxc", "psmdb", "pg"}},
		{[]string{}, []string{"pxc", "psmdb", "pg"}},
		{[]string{"pg", "pxc"}, []string{"pxc", "pg"}},
		{[]string{" PSMDB "}, []string{"psmdb"}},
		{[]string{"ps", "pg"}, []string{"pg"}},
		{[]string{"cnpg"}, []string{"pxc", "psmdb", "pg"}},
	}
	for _, c := range cases {
		if got := everestChosen(designFrame{K3DEverestOperators: c.in}); !reflect.DeepEqual(got, c.want) {
			t.Errorf("everestChosen(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The values are the chart's own spelling: `postgresql`, not `pg`, and every operator is
// stated either way, so an unticked one is off rather than left to the chart's default (on).
func TestEverestValues(t *testing.T) {
	v := everestValues("dbs", []string{"pxc", "pg"}, "s3cret")
	for _, want := range []string{
		"telemetry: false\n",
		`  initialAdminPassword: "s3cret"` + "\n",
		"    type: LoadBalancer\n",
		"  namespaceOverride: dbs\n",
		"  pxc: true\n",
		"  psmdb: false\n",
		"  postgresql: true\n",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("values missing %q:\n%s", want, v)
		}
	}
	// A password with YAML in it stays one scalar.
	if v := everestValues("dbs", nil, `a: b # c`); !strings.Contains(v, `initialAdminPassword: "a: b # c"`) {
		t.Errorf("password not quoted:\n%s", v)
	}
}

func TestEverestDBNamespace(t *testing.T) {
	for in, want := range map[string]string{"": "everest", "default": "everest", "dbs": "dbs", " x ": "x"} {
		if got := everestDBNamespace(designFrame{K3DNamespace: in}); got != want {
			t.Errorf("everestDBNamespace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEverestFrameIssues(t *testing.T) {
	levels := func(is []issue) map[string]int {
		m := map[string]int{}
		for _, i := range is {
			m[i.Level]++
		}
		return m
	}
	ok := designFrame{Label: "k", K3DOperator: "everest", K3DCPUs: 8, K3DMemoryGB: 16}
	if is := everestFrameIssues(ok); len(is) != 0 {
		t.Fatalf("a plain Everest frame should be clean, got %+v", is)
	}
	if is := everestFrameIssues(designFrame{K3DOperator: "pxc", PMMNodeID: "p"}); len(is) != 0 {
		t.Fatalf("not an Everest frame, got %+v", is)
	}

	bad := ok
	bad.K3DEverestOperators = []string{"pxc", "mariadb"}
	bad.K3DNamespace = "everest-system"
	if got := levels(everestFrameIssues(bad)); got["error"] != 2 {
		t.Errorf("unknown operator + reserved namespace: want 2 errors, got %v", got)
	}

	wired := ok
	wired.PMMNodeID, wired.SeaweedFSNodeID = "pmm", "sw"
	wired.K3DCPUs, wired.K3DMemoryGB = 4, 8
	if got := levels(everestFrameIssues(wired)); got["warning"] != 3 || got["error"] != 0 {
		t.Errorf("PMM + SeaweedFS + small budget: want 3 warnings, got %v", got)
	}
}

// Everest is chart-installed, so its version resolves against the chart catalog — a frame that
// resolved it against the Percona operator catalog would find no such product and silently
// deploy without it.
func TestEverestIsAChartOperator(t *testing.T) {
	if !k3dDeployableOperator["everest"] || k3dChartOperator["everest"] != everestChart {
		t.Fatal("everest must be a deployable, chart-installed operator")
	}
}

// The HelmChart is named after the release, which is what helm-controller installs it as and
// what waitHelmInstall's Job name is derived from.
func TestEverestHelmChartManifest(t *testing.T) {
	m := string(helmChartManifest(everestRelease, everestChartRepo, everestChart, "1.16.2", everestNamespace,
		everestValues("everest", everestChosen(designFrame{}), "pw")))
	for _, want := range []string{
		"  name: everest-core\n", "  chart: openeverest\n", "  repo: https://openeverest.github.io/helm-charts\n",
		"  version: 1.16.2\n", "  targetNamespace: everest-system\n", "      namespaceOverride: everest\n",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}

// The host port lands on the load balancer and forwards to the NodePort the everest-host
// Service fixes — the two numbers have to agree, since one is set before the other exists.
func TestEverestHostPublish(t *testing.T) {
	t.Setenv("CONTAINER_BIND_IP", "127.0.0.1")
	if got := strings.Join(everestCreateArgs(41234), " "); got != "--port 127.0.0.1:41234:30808/tcp@loadbalancer" {
		t.Errorf("create args = %q", got)
	}
	m := string(everestHostServiceManifest())
	for _, want := range []string{"name: everest-host", "namespace: everest-system", "type: NodePort", "nodePort: 30808", "app.kubernetes.io/name: everest-server"} {
		if !strings.Contains(m, want) {
			t.Errorf("host Service missing %q:\n%s", want, m)
		}
	}
}

// A design pinned to a release whose server is unusable is told why, rather than getting the
// generic "unknown chart version" (it is left out of the catalog, so it is unknown too).
func TestEverestBrokenVersionIsExplained(t *testing.T) {
	// A dead socket keeps this hermetic, as in TestK3DFrameIssuesCNPGVersion.
	a := &App{docker: NewDocker(filepath.Join(t.TempDir(), "absent.sock"))}
	f := designFrame{Label: "k", K3DOperator: "everest", K3DOperatorVer: "1.15.0", K3DCPUs: 8, K3DMemoryGB: 16}
	var msgs []string
	for _, is := range a.k3dFrameIssues(t.Context(), f, 1, OperatorCatalog{}) {
		if is.Level == "error" && strings.Contains(is.Message, "1.15.0") {
			msgs = append(msgs, is.Message)
		}
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "blank page") {
		t.Fatalf("want one error explaining 1.15.0, got %q", msgs)
	}
}
