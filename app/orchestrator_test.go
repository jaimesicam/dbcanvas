package main

import (
	"encoding/json"
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
