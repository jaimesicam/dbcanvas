package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// startingKeycloak answers the kcadm login with a failure until it has been asked
// failFirst times, the way a Keycloak container does while it is still starting.
type startingKeycloak struct {
	Engine
	failFirst int
	calls     int
}

func (k *startingKeycloak) Exec(ctx context.Context, id string, cmd []string, env []string) (ExecResult, error) {
	k.calls++
	if k.calls <= k.failFirst {
		return ExecResult{Code: 1, Stderr: "Failed to send request - Connect to localhost:8080 failed: Connection refused"}, nil
	}
	return ExecResult{}, nil
}

// A Keycloak deployment is what every OIDC integration waits on before logging in with
// kcadm, so it must not count as up until that login works.
func TestWaitKeycloakLoginWaitsForTheLogin(t *testing.T) {
	old := keycloakLoginPoll
	keycloakLoginPoll = time.Millisecond
	t.Cleanup(func() { keycloakLoginPoll = old })
	app := newTestApp(t)

	kc := &startingKeycloak{failFirst: 2}
	if err := app.waitKeycloakLogin(withEngine(context.Background(), kc), "kc", "pw", time.Minute); err != nil {
		t.Fatalf("a Keycloak that comes up after two refusals: %v", err)
	}
	if kc.calls != 3 {
		t.Errorf("tried the login %d times, want 3", kc.calls)
	}

	never := &startingKeycloak{failFirst: 1 << 30}
	err := app.waitKeycloakLogin(withEngine(context.Background(), never), "kc", "pw", 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not accept the admin login") {
		t.Errorf("a Keycloak that never answers gave %v", err)
	}
}

// Every Keycloak script signs in through the retrying kc_login, not a single kcadm call.
func TestKeycloakScriptsRetryTheLogin(t *testing.T) {
	for name, script := range map[string]string{
		"keycloakClientScript": keycloakClientScript,
		"keycloakSetupScript":  keycloakSetupScript,
	} {
		if !strings.Contains(script, "kc_login\n") {
			t.Errorf("%s does not sign in through kc_login", name)
		}
	}
}
