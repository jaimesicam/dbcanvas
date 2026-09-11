package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// k3dcertmanager.go — cert-manager on a Kubernetes frame, installed before the operator.
//
// It is a design-time checkbox rather than something DBCanvas decides, because what it changes is
// what the *operator* does. The four Percona operators look for cert-manager when they reconcile:
// with it they ask it for the cluster's TLS certificates (a real Issuer, renewal, a CA the pods
// trust); without it they generate a self-signed set themselves and never mention it. Both are
// valid labs — "reproduce the customer's cert-manager setup" and "reproduce it without" are
// different tickets — so the canvas asks, and the answer is visible on the node's panel
// afterwards rather than being something you have to go and discover with kubectl.
//
// ---------------------------------------------------------------------------- the ordering
//
// This installs *before* the operator, and waits. That is the whole point of the option: an
// operator that starts first sees no cert-manager, decides to self-sign, and writes that decision
// into the objects it creates — installing cert-manager a minute later does not undo it. So the
// deploy blocks here until cert-manager can actually serve, and only then applies the bundle.
//
// "Can actually serve" is more than three Available deployments. cert-manager's validating webhook
// is admission control for its own CRDs, and it starts refusing traffic ("failed calling webhook")
// for a few seconds *after* the deployments report ready, while cainjector fills in its CA bundle.
// An operator that applies a Certificate in that window gets an error it does not retry usefully.
// So the last step is a server-side dry-run of a real Certificate: nothing is created, and it
// succeeds only once the webhook is answering. It is what `cmctl check api` does, without adding
// a binary to the image.

const (
	// The cert-manager release installed when the canvas asks for no particular one and the
	// catalog cannot answer either — `make versions` never run, or run without a network.
	//
	// It is a fallback now rather than the pin it used to be: the frame carries a version
	// (K3DCertManagerVer) and an empty one resolves to the newest release in versions.yaml,
	// the same contract the operator picker has. What has not changed is why a version is
	// written down at all — a lab that quietly installs something different next month is not
	// a lab you can compare against last month's — only that the canvas now chooses which.
	certManagerFallbackVersion = "v1.21.1"
	// The single-file install the project publishes — CRDs, namespace, RBAC, and the three
	// deployments, in one manifest.
	certManagerManifestFmt = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"
	// The CRD whose presence means cert-manager is already on this cluster — see
	// certManagerPresent, and installBarmanPlugin in cnpg.go, which installs its own copy by
	// Helm chart and must not do so twice.
	certManagerCRD = "certificates.cert-manager.io"
)

// certManagerNamespace is declared in cnpg.go, which installs cert-manager by Helm chart for the
// barman-cloud plugin. Both paths land in the same namespace on purpose: one cluster has one
// cert-manager, whichever asked for it.

// certManagerWait caps how long each step waits. Deliberately not deployTimeout() (an hour): this
// is three small deployments, and an hour of a deploy spent on an add-on that is not coming up is
// an hour nobody watching the log gets back.
const certManagerWait = 5 * time.Minute

// certManagerDeployments are the three that have to be up: the controller, the webhook that
// admits its CRDs, and the cainjector that keeps the webhook's CA bundle current.
var certManagerDeployments = []string{"cert-manager", "cert-manager-webhook", "cert-manager-cainjector"}

// certManagerManifestURL is the release's manifest.
func certManagerManifestURL(version string) string {
	return fmt.Sprintf(certManagerManifestFmt, version)
}

// certManagerProbe is a Certificate that is never created — it is applied with
// --dry-run=server purely to make the API server call cert-manager's validating webhook. The
// issuerRef names an Issuer that does not exist, which does not matter: admission validates the
// shape of the object, it does not resolve the reference.
var certManagerProbe = []byte(`apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: dbcanvas-webhook-probe
  namespace: ` + certManagerNamespace + `
spec:
  secretName: dbcanvas-webhook-probe
  dnsNames:
    - dbcanvas-webhook-probe.invalid
  issuerRef:
    name: dbcanvas-webhook-probe
    kind: Issuer
`)

// certManagerResolveVersion turns the frame's choice into the release to install: what the
// canvas asked for, or the newest the catalog knows, or the built-in fallback.
//
// The catalog entry is the Helm chart's, from charts.jetstack.io (images/versions.sh), and it is
// used here for a manifest URL instead — which works because cert-manager's chart version IS its
// app version, tag for tag. That is true of cert-manager and not of charts in general: the
// CloudNativePG chart's 0.29.0 carries operator 1.30.x, which is why that one has a picker of its
// own that says "chart version" rather than pretending the two are the same thing.
func certManagerResolveVersion(want string) string {
	if v, ok := loadChartCatalog().resolveChartVersion(certManagerChart, want); ok && v != "" {
		return v
	}
	// The catalog refused it, which for a chart means it knows the chart and not that version.
	// Install it anyway: k3dFrameIssues already fails a deploy that asks for one, so reaching
	// here means somebody bypassed the canvas, and a 404 from GitHub says more than a silent
	// downgrade to a release they did not choose.
	if v := strings.TrimSpace(want); v != "" {
		return v
	}
	return certManagerFallbackVersion
}

// installCertManager applies the release and returns once cert-manager can serve. The version it
// installed is returned so the node's panel can say which one is on the cluster — which is not
// always the one asked for, since an empty request resolves to the catalog's latest.
func (a *App) installCertManager(ctx context.Context, serverID, want string, logln func(string)) (string, error) {
	version := certManagerResolveVersion(want)
	url := certManagerManifestURL(version)
	manifest, err := httpGetBytes(ctx, url)
	if err != nil {
		return "", fmt.Errorf("fetch the cert-manager manifest: %w", err)
	}
	logln(fmt.Sprintf("cert-manager %s — applying %s (%d KiB)", version, url, len(manifest)/1024))
	// Server-side apply: the CRDs in this manifest are far past the 256KiB ceiling on the
	// last-applied-configuration annotation that a client-side apply would try to write.
	if err := a.kubectlApplyServerSide(ctx, serverID, "", manifest); err != nil {
		return "", err
	}
	if err := a.waitForCRD(ctx, serverID, certManagerCRD, certManagerWait); err != nil {
		return "", fmt.Errorf("the cert-manager CRDs never became established: %w", err)
	}
	for _, d := range certManagerDeployments {
		if err := a.waitForDeployment(ctx, serverID, certManagerNamespace, d, certManagerWait); err != nil {
			return "", fmt.Errorf("%s did not become ready: %w", d, err)
		}
		logln("cert-manager: " + d + " ready")
	}
	if err := a.waitCertManagerWebhook(ctx, serverID); err != nil {
		return "", err
	}
	logln("cert-manager: the webhook is admitting Certificates — the operator will use it for TLS")
	return version, nil
}

// waitCertManagerWebhook dry-runs a Certificate until the webhook answers, or gives up.
func (a *App) waitCertManagerWebhook(ctx context.Context, serverID string) error {
	var last error
	for i := 0; i < 24; i++ { // ~2 minutes
		res, err := a.engCtx(ctx).ExecInput(ctx, serverID, "",
			[]string{"kubectl", "apply", "--dry-run=server", "-f", "-"},
			[]string{"KUBECONFIG=" + k3dKubeconfig}, certManagerProbe)
		switch {
		case err != nil:
			last = err
		case res.Code == 0:
			return nil
		default:
			last = fmt.Errorf("%s", strings.TrimSpace(res.Stderr+res.Stdout))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("the cert-manager webhook never admitted a Certificate: %w", last)
}

// certManagerPresent reports whether cert-manager is already installed on this cluster, however
// it got there — the frame's checkbox, an earlier deploy, or a user with kubectl. The CRD is the
// test rather than the namespace: a namespace outlives an uninstall, a CRD is what anything
// asking cert-manager for a certificate actually needs.
func (a *App) certManagerPresent(ctx context.Context, serverID string) bool {
	out, err := a.kubectl(ctx, serverID, "get", "crd", certManagerCRD, "-o", "jsonpath={.metadata.name}")
	return err == nil && strings.TrimSpace(out) == certManagerCRD
}
