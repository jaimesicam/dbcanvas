package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// k3d_rbac.go — a copyable admin kubeconfig, plus genuine Kubernetes RBAC users for a K3D cluster,
// surfaced in the node's properties panel so a learner can test what a limited identity can and
// can't do against the cluster.
//
// A user is a real Kubernetes `User`: an X.509 client certificate whose CommonName is the username,
// issued by the cluster's own CA via the CertificateSigningRequest API (generated in-process with
// crypto/x509 — the k3s image is busybox and has neither openssl nor a CSR-signing service of its
// own). dbcanvas holds cluster-admin via the server node's kubeconfig, so it submits the CSR AND
// approves it itself — a synchronous round trip, not a manual approval step for the caller.
//
// There is no dbcanvas-side bookkeeping: the cluster is the source of truth, exactly like every
// other K3D feature. Each user's signed cert + private key + role/namespace metadata live as a
// single Kubernetes Secret in a dedicated "dbcanvas-system" namespace, labeled so they can be
// listed and deleted without a second store. Permissions come from binding the built-in `view` /
// `edit` / `admin` ClusterRoles (namespace-scoped, via a RoleBinding) or `cluster-admin`
// (cluster-scoped, via a ClusterRoleBinding) to that username — no custom Role authoring needed.
const k3dUsersNamespace = "dbcanvas-system"

var k3dBuiltinRoles = map[string]bool{"view": true, "edit": true, "admin": true, "cluster-admin": true}

type k3dUserInfo struct {
	Username  string `json:"username"`
	Namespace string `json:"namespace"` // "" for cluster-admin (cluster-scoped)
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
}

// k3dFrameAndServer finds the K3D frame `fid` in a stack's design and its currently running server
// member — the only node kubectl can run against (kubectl ships only inside the k3s images).
func (a *App) k3dFrameAndServer(st Stack, doc designDoc, fid string) (designFrame, Deployment, error) {
	var frame designFrame
	found := false
	for _, f := range doc.Frames {
		if f.ID == fid && f.Type == "k3d" {
			frame, found = f, true
			break
		}
	}
	if !found {
		return designFrame{}, Deployment{}, fmt.Errorf("K3D cluster not found")
	}
	for _, n := range doc.Nodes {
		if n.FrameID != fid || n.Type != "k3d" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		var cfg k3dConfig
		if json.Unmarshal(dep.Config, &cfg) != nil || cfg.Role != "server" {
			continue
		}
		return frame, dep, nil
	}
	return frame, Deployment{}, fmt.Errorf("no running K3D server node found for this cluster")
}

// k3dServerLBAddr is the address a kubeconfig's `server:` field should point at: k3d's own
// serverlb container, the load balancer k3d creates in front of the server node(s) — the intended
// stable entry point, reachable by container name from any other node on the same stack network
// (Docker's embedded DNS resolves container names on a user-defined bridge network) the same way
// MetalLB LoadBalancer IPs already are.
func k3dServerLBAddr(cluster string) string {
	return fmt.Sprintf("https://k3d-%s-serverlb:6443", cluster)
}

var kubeconfigServerLine = regexp.MustCompile(`(?m)^(\s*server:\s*).*$`)

// k3dFetchKubeconfig returns the cluster's admin kubeconfig, with the `server:` field rewritten
// from the API server's untracked loopback/host-port address to the serverlb's stack-network
// address — usable from any other node on the same stack (e.g. the Linux Client node's terminal),
// not from the caller's own machine.
func (a *App) k3dFetchKubeconfig(ctx context.Context, serverID, cluster string) (string, error) {
	raw, err := a.kubectl(ctx, serverID, "config", "view", "--raw")
	if err != nil {
		return "", err
	}
	return kubeconfigServerLine.ReplaceAllString(raw, "${1}"+k3dServerLBAddr(cluster)), nil
}

// k3dClusterCA returns the cluster CA certificate, base64-encoded exactly as a kubeconfig's
// `certificate-authority-data` field expects it — read straight off the admin kubeconfig rather
// than a second file on disk, so it always matches what `k3dFetchKubeconfig` embeds.
func (a *App) k3dClusterCA(ctx context.Context, serverID string) (string, error) {
	out, err := a.kubectl(ctx, serverID, "config", "view", "--raw",
		"-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}")
	if err != nil {
		return "", err
	}
	ca := strings.TrimSpace(out)
	if ca == "" {
		return "", fmt.Errorf("cluster CA not found in the admin kubeconfig")
	}
	return ca, nil
}

// buildCSR generates an RSA key and a client-auth CSR whose CommonName is the username — the
// identity Kubernetes RBAC binds a User's permissions to.
func buildCSR(username string) (keyPEM, csrPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: username}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, nil, err
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	return keyPEM, csrPEM, nil
}

// k3dCreateUser mints a client-certificate Kubernetes User and binds it to a built-in ClusterRole:
// `view`/`edit`/`admin` scoped to one namespace via a RoleBinding, or `cluster-admin` scoped to the
// whole cluster via a ClusterRoleBinding. The signed cert + key are stored as a Secret so the
// kubeconfig can be re-downloaded later without re-issuing the certificate.
func (a *App) k3dCreateUser(ctx context.Context, serverID, username, namespace, role string) error {
	if !k3dBuiltinRoles[role] {
		return fmt.Errorf("unknown role %q", role)
	}
	keyPEM, csrPEM, err := buildCSR(username)
	if err != nil {
		return fmt.Errorf("generate key/CSR: %w", err)
	}

	csrName := fmt.Sprintf("dbcanvas-user-%s-%d", username, time.Now().Unix())
	csrManifest := fmt.Sprintf(`apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata:
  name: %s
spec:
  request: %s
  signerName: kubernetes.io/kube-apiserver-client
  expirationSeconds: 31536000
  usages:
  - client auth
`, csrName, base64.StdEncoding.EncodeToString(csrPEM))
	if err := a.kubectlApply(ctx, serverID, "", []byte(csrManifest)); err != nil {
		return fmt.Errorf("submit CSR: %w", err)
	}
	if _, err := a.kubectl(ctx, serverID, "certificate", "approve", csrName); err != nil {
		a.kubectl(ctx, serverID, "delete", "csr", csrName)
		return fmt.Errorf("approve CSR: %w", err)
	}

	var certB64 string
	for i := 0; i < 10; i++ {
		out, err := a.kubectl(ctx, serverID, "get", "csr", csrName, "-o", "jsonpath={.status.certificate}")
		if err == nil && strings.TrimSpace(out) != "" {
			certB64 = strings.TrimSpace(out)
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	a.kubectl(ctx, serverID, "delete", "csr", csrName) // cleanup either way — the cert is saved into the Secret below
	if certB64 == "" {
		return fmt.Errorf("certificate was not issued after approval")
	}
	certPEM, err := base64.StdEncoding.DecodeString(certB64)
	if err != nil {
		return fmt.Errorf("decode signed certificate: %w", err)
	}

	ns := namespace
	if role == "cluster-admin" {
		ns = ""
	}
	manifest := k3dUserManifest(username, ns, role, certPEM, keyPEM, time.Now().UTC().Format(time.RFC3339))
	return a.kubectlApply(ctx, serverID, "", manifest)
}

// k3dUserManifest builds the multi-document YAML applied to record a user: the management
// namespace (idempotent to reapply), a Secret carrying the cert/key + metadata, and the
// RoleBinding/ClusterRoleBinding that actually grants the role.
func k3dUserManifest(username, namespace, role string, certPEM, keyPEM []byte, createdAt string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\n", k3dUsersNamespace)
	fmt.Fprintf(&b, `apiVersion: v1
kind: Secret
metadata:
  name: dbcanvas-user-%s
  namespace: %s
  labels:
    dbcanvas.io/user: %q
  annotations:
    dbcanvas.io/role: %q
    dbcanvas.io/namespace: %q
    dbcanvas.io/created-at: %q
type: Opaque
data:
  tls.crt: %s
  tls.key: %s
---
`, username, k3dUsersNamespace, username, role, namespace, createdAt,
		base64.StdEncoding.EncodeToString(certPEM), base64.StdEncoding.EncodeToString(keyPEM))

	if role == "cluster-admin" {
		fmt.Fprintf(&b, `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: dbcanvas-user-%s
  labels:
    dbcanvas.io/user: %q
subjects:
- kind: User
  name: %s
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: cluster-admin
  apiGroup: rbac.authorization.k8s.io
`, username, username, username)
	} else {
		fmt.Fprintf(&b, `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: dbcanvas-user-%s
  namespace: %s
  labels:
    dbcanvas.io/user: %q
subjects:
- kind: User
  name: %s
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: %s
  apiGroup: rbac.authorization.k8s.io
`, username, namespace, username, username, role)
	}
	return []byte(b.String())
}

// k3dListUsers reads every user Secret back into a listing. An empty/missing management namespace
// (no user ever created) is not an error — it just means there is nothing to list yet.
func (a *App) k3dListUsers(ctx context.Context, serverID string) ([]k3dUserInfo, error) {
	out, err := a.kubectl(ctx, serverID, "get", "secrets", "-n", k3dUsersNamespace, "-l", "dbcanvas.io/user", "-o", "json")
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found") {
			return nil, nil
		}
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	users := make([]k3dUserInfo, 0, len(list.Items))
	for _, it := range list.Items {
		u := it.Metadata.Labels["dbcanvas.io/user"]
		if u == "" {
			continue
		}
		users = append(users, k3dUserInfo{
			Username:  u,
			Namespace: it.Metadata.Annotations["dbcanvas.io/namespace"],
			Role:      it.Metadata.Annotations["dbcanvas.io/role"],
			CreatedAt: it.Metadata.Annotations["dbcanvas.io/created-at"],
		})
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	return users, nil
}

// k3dDeleteUser removes a user's Secret and its RoleBinding/ClusterRoleBinding, read back from the
// Secret's own annotations so the caller does not need to know the role/namespace up front. Kubernetes
// has no live client-certificate revocation, so this removes the *authorization* to act — the
// practical effect a learner cares about — even though the certificate itself remains
// cryptographically valid until it expires.
func (a *App) k3dDeleteUser(ctx context.Context, serverID, username string) error {
	out, err := a.kubectl(ctx, serverID, "get", "secret", "dbcanvas-user-"+username, "-n", k3dUsersNamespace, "-o", "json")
	if err != nil {
		return fmt.Errorf("user %q not found", username)
	}
	var sec struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	json.Unmarshal([]byte(out), &sec)
	role := sec.Metadata.Annotations["dbcanvas.io/role"]
	ns := sec.Metadata.Annotations["dbcanvas.io/namespace"]

	a.kubectl(ctx, serverID, "delete", "secret", "dbcanvas-user-"+username, "-n", k3dUsersNamespace)
	if role == "cluster-admin" {
		a.kubectl(ctx, serverID, "delete", "clusterrolebinding", "dbcanvas-user-"+username)
	} else if ns != "" {
		a.kubectl(ctx, serverID, "delete", "rolebinding", "dbcanvas-user-"+username, "-n", ns)
	}
	return nil
}

// k3dUserKubeconfig rebuilds a single-user kubeconfig from the stored Secret — the cert/key never
// leave the cluster except through this call.
func (a *App) k3dUserKubeconfig(ctx context.Context, serverID, cluster, username string) (string, error) {
	out, err := a.kubectl(ctx, serverID, "get", "secret", "dbcanvas-user-"+username, "-n", k3dUsersNamespace, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("user %q not found", username)
	}
	var sec struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &sec); err != nil {
		return "", err
	}
	certB64, keyB64 := sec.Data["tls.crt"], sec.Data["tls.key"]
	if certB64 == "" || keyB64 == "" {
		return "", fmt.Errorf("user %q has no stored credentials", username)
	}
	caB64, err := a.k3dClusterCA(ctx, serverID)
	if err != nil {
		return "", err
	}
	certPEM, err := base64.StdEncoding.DecodeString(certB64)
	if err != nil {
		return "", err
	}
	keyPEM, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return "", err
	}
	return buildUserKubeconfig(cluster, k3dServerLBAddr(cluster), caB64, username, certPEM, keyPEM), nil
}

func buildUserKubeconfig(cluster, server, caB64, username string, certPEM, keyPEM []byte) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %s
  cluster:
    server: %s
    certificate-authority-data: %s
contexts:
- name: %s@%s
  context:
    cluster: %s
    user: %s
current-context: %s@%s
users:
- name: %s
  user:
    client-certificate-data: %s
    client-key-data: %s
`, cluster, server, caB64,
		username, cluster, cluster, username,
		username, cluster,
		username, base64.StdEncoding.EncodeToString(certPEM), base64.StdEncoding.EncodeToString(keyPEM))
}

// ---------------------------------------------------------------- HTTP handlers

// k3dRBACContext loads the owner-scoped stack, its design, and the K3D frame `fid`'s running
// server member — the shared setup every handler below needs.
func (a *App) k3dRBACContext(w http.ResponseWriter, r *http.Request) (Stack, designFrame, Deployment, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Stack{}, designFrame{}, Deployment{}, false
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return Stack{}, designFrame{}, Deployment{}, false
	}
	frame, dep, err := a.k3dFrameAndServer(st, doc, r.PathValue("fid"))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return Stack{}, designFrame{}, Deployment{}, false
	}
	return st, frame, dep, true
}

func (a *App) handleK3DKubeconfig(w http.ResponseWriter, r *http.Request) {
	st, frame, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	cluster := k3dClusterName(st.ID, frame)
	yaml, err := a.k3dFetchKubeconfig(r.Context(), dep.ContainerID, cluster)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "fetch kubeconfig: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"kubeconfig": yaml, "cluster": cluster})
}

func (a *App) handleK3DUsers(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	users, err := a.k3dListUsers(r.Context(), dep.ContainerID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list users: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (a *App) handleK3DUserCreate(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var b struct{ Username, Namespace, Role string }
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	b.Username = strings.TrimSpace(b.Username)
	b.Role = strings.TrimSpace(b.Role)
	b.Namespace = strings.TrimSpace(b.Namespace)
	if !validNamespace(b.Username) {
		writeErr(w, http.StatusBadRequest, "username must be lowercase letters, digits and '-' (a DNS-1123 label)")
		return
	}
	if !k3dBuiltinRoles[b.Role] {
		writeErr(w, http.StatusBadRequest, "role must be one of view, edit, admin, cluster-admin")
		return
	}
	if b.Role == "cluster-admin" {
		b.Namespace = ""
	} else {
		if b.Namespace == "" {
			b.Namespace = "default"
		}
		if !validNamespace(b.Namespace) {
			writeErr(w, http.StatusBadRequest, "namespace must be lowercase letters, digits and '-' (a DNS-1123 label)")
			return
		}
	}
	if err := a.k3dCreateUser(r.Context(), dep.ContainerID, b.Username, b.Namespace, b.Role); err != nil {
		writeErr(w, http.StatusBadGateway, "create user: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleK3DUserDelete(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var b struct{ Username string }
	if err := decode(r, &b); err != nil || strings.TrimSpace(b.Username) == "" {
		writeErr(w, http.StatusBadRequest, "username is required")
		return
	}
	if err := a.k3dDeleteUser(r.Context(), dep.ContainerID, strings.TrimSpace(b.Username)); err != nil {
		writeErr(w, http.StatusBadGateway, "delete user: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleK3DUserKubeconfig(w http.ResponseWriter, r *http.Request) {
	st, frame, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	username := strings.TrimSpace(r.PathValue("username"))
	if username == "" {
		writeErr(w, http.StatusBadRequest, "username is required")
		return
	}
	cluster := k3dClusterName(st.ID, frame)
	yaml, err := a.k3dUserKubeconfig(r.Context(), dep.ContainerID, cluster, username)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user kubeconfig: "+lastLines(err.Error(), 200))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"kubeconfig": yaml, "cluster": cluster})
}
