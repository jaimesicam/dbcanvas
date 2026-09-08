package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestOrchestratorConfJSONIsValid(t *testing.T) {
	sec := pxcSecrets{OrchestratorUser: "orchestrator", OrchestratorPassword: "s3cret"}
	for _, alert := range []string{"", "admin"} {
		raw := orchestratorConfJSON(sec, alert)
		var v map[string]any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("orchestratorConfJSON(alert=%q) is not valid JSON: %v\n%s", alert, err, raw)
		}
		if v["MySQLTopologyUser"] != "orchestrator" || v["MySQLTopologyPassword"] != "s3cret" {
			t.Errorf("topology credentials not wired through: %+v", v)
		}
		hooks, _ := v["OnFailureDetectionProcesses"].([]any)
		if alert == "" && len(hooks) != 0 {
			t.Errorf("expected no failure-detection hook when alertEmail is empty, got %v", hooks)
		}
		if alert != "" && len(hooks) != 1 {
			t.Errorf("expected exactly one failure-detection hook when alertEmail is set, got %v", hooks)
		}
	}
}

func TestOrchestratorAlertScript(t *testing.T) {
	cases := []struct{ email, domain, wantTo string }{
		{"admin", "example.net", "admin@example.net"},
		{"admin@other.org", "example.net", "admin@other.org"},
	}
	for _, c := range cases {
		script := orchestratorAlertScript(c.email, c.domain)
		if strings.Contains(script, "__TO__") || strings.Contains(script, "__DOMAIN__") {
			t.Errorf("unsubstituted placeholder left in script for %+v:\n%s", c, script)
		}
		if !strings.Contains(script, "TO='"+c.wantTo+"'") {
			t.Errorf("expected TO='%s' in script for %+v, got:\n%s", c.wantTo, c, script)
		}
		if !strings.Contains(script, "RELAY='intranet."+c.domain+"'") {
			t.Errorf("expected relay intranet.%s in script for %+v", c.domain, c)
		}
	}
}

// Orchestrator manages async and semi-sync replication, so nothing on the PXC path may
// still reach for it: not the frame's deploy, not the picker's endpoint, and not the
// topology account the bootstrap used to create for a cluster that could never use it.
func TestPXCHasNoOrchestratorWiring(t *testing.T) {
	if orchestratableFrame("pxc") {
		t.Error("a PXC frame must not be Orchestrator-manageable")
	}
	if strings.Contains(pxcBootstrapScript, "ORCH_USER") {
		t.Error("the PXC bootstrap still creates the orchestrator topology user")
	}
	src, err := os.ReadFile("pxc.go")
	if err != nil {
		t.Skip("pxc.go not readable")
	}
	for _, ref := range []string{"registerOrchestrator", "OrchestratorNodeID", "OrchestratedBy"} {
		if strings.Contains(string(src), ref) {
			t.Errorf("pxc.go still references %s", ref)
		}
	}
	// The replication frames keep both the user and the link — this is a PXC change,
	// not the removal of the feature.
	if !strings.Contains(mysqlBaselineScript, "ORCH_USER") {
		t.Error("the MySQL replication baseline must still create the orchestrator user")
	}
	for _, want := range []string{"mysql", "mariadbrepl", "mysqlcerepl"} {
		if !orchestratableFrame(want) {
			t.Errorf("%s lost its Orchestrator link", want)
		}
	}
}

// The PXC frame form must not offer a picker the backend now rejects.
func TestPXCFrameFormHasNoOrchestratorPicker(t *testing.T) {
	js, err := os.ReadFile("web/src/pages/StackDesigner.jsx")
	if err != nil {
		t.Skip("StackDesigner.jsx not readable")
	}
	i := strings.Index(string(js), "function PXCFrameForm(")
	if i < 0 {
		t.Fatal("PXCFrameForm not found")
	}
	form := string(js)[i:]
	if j := strings.Index(form, "\nfunction "); j > 0 {
		form = form[:j]
	}
	if strings.Contains(form, "setOrchestrator(") || strings.Contains(form, "orchestratorNodeId:") {
		t.Error("the PXC frame form still wires the Orchestrator picker")
	}
}
