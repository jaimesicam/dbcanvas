package main

import (
	"context"
	"time"
)

// catrust.go — install the Intranet CA into a node's system trust store so the node trusts every
// certificate the Intranet CA issues: LDAPS to Samba AD DC / OpenLDAP, HTTPS to Keycloak & PMM,
// TLS-enabled databases, SeaweedFS S3 over TLS, and so on. Called from each node's provisioner.

// trustIntranetCA adds the Intranet CA to containerID's system trust store. Best-effort and
// idempotent: a stack without an Intranet is a no-op, and any failure is logged (not fatal).
// Handles both RHEL-family (update-ca-trust) and Debian/Ubuntu (update-ca-certificates) nodes.
// The cert file is written by the Docker daemon (root) and the refresh runs via `ExecAs root`,
// so it also works on images whose default user is unprivileged (PMM, Keycloak).
func (a *App) trustIntranetCA(ctx context.Context, st Stack, containerID, nodeOS string, logln func(string)) {
	intranetID := a.intranetContainerID(ctx, st)
	if intranetID == "" {
		return // no Intranet → no CA to trust
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		logln("trust Intranet CA skipped: " + err.Error())
		return
	}
	ca, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil || len(ca) == 0 {
		logln("trust Intranet CA skipped: CA not readable")
		return
	}
	dir, refresh := "/etc/pki/ca-trust/source/anchors", "update-ca-trust"
	if isDebianOS(nodeOS) {
		dir, refresh = "/usr/local/share/ca-certificates", "update-ca-certificates"
	}
	if err := a.engCtx(ctx).CopyFile(ctx, containerID, dir, "dbcanvas-ca.crt", 0o644, ca); err != nil {
		logln("trust Intranet CA skipped: " + err.Error())
		return
	}
	if _, err := a.engCtx(ctx).ExecAs(ctx, containerID, "root", []string{"bash", "-lc", refresh + " >/dev/null 2>&1 || true"}, nil); err != nil {
		logln("trust Intranet CA: refresh failed: " + err.Error())
		return
	}
	logln("Intranet CA added to the system trust store")
}
