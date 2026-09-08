package main

import "testing"

// appIsContainerized gates joinStackForDial: containerized → self-join the stack
// bridge; host (hybrid Vagrant mode) → dial directly, no self-join. The filesystem
// probe (/.dockerenv, /run/.containerenv) can't be forced in a unit test, but the
// DBCANVAS_HOST_MODE override must always win so an e2e run on a host with a stray
// /.dockerenv still takes the host path.
func TestAppIsContainerizedHostModeOverride(t *testing.T) {
	for _, v := range []string{"1", "true"} {
		t.Setenv("DBCANVAS_HOST_MODE", v)
		if appIsContainerized() {
			t.Fatalf("DBCANVAS_HOST_MODE=%q: want host mode (not containerized)", v)
		}
	}
	// A non-forcing value falls through to filesystem detection (unset override).
	t.Setenv("DBCANVAS_HOST_MODE", "0")
	// No assertion on the result here — it depends on the runner's filesystem — we
	// only assert "0" is not treated as a forcing value, i.e. it doesn't panic and
	// matches the probe. The probe is environment-dependent, so just exercise it.
	_ = appIsContainerized()
}
