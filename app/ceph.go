package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ceph.go — the Ceph node, and K3D frames whose volumes live on it.
//
// A K3D cluster's volumes are k3s's local-path by default: a directory on one node, which
// Kubernetes cannot resize — so every operator's "grow the storage" is refused before the
// operator is even asked, and a lab cannot reproduce what a customer does when a volume fills.
// A Ceph node gives a frame network block storage that can grow: RBD images, provisioned by
// Ceph CSI, mounted by the kernel's RBD client on the k3s nodes.
//
// The node is one container from Ceph's own image (quay.io/ceph/ceph) running a monitor, a
// manager and one BlueStore OSD on a sparse file, bootstrapped by hand (cephBootstrapScript):
// cephadm wants systemd and a container runtime of its own, and a lab needs neither. It has no
// redundancy (pool size 1) — it is a storage *backend* to test against, not a store to trust.
//
// What making that work on a real Docker host took, each found by trying it (IMPLEMENTATION.md
// §425 has the details):
//
//   - Every key is the classic AES type, and the monitors offer only that cipher. Squid
//     generates aes256k keys by default, and the kernel's RBD client rejects them ("libceph:
//     secret too big 32"). Squid then reports the AES keys as insecure, so those health
//     alerts are muted — deliberately, and only those.
//   - Volumes are mapped with ms_mode=legacy (msgr v1). Over msgr2 the kernel tested
//     (6.18) cannot decode Squid's OSD map ("libceph: corrupt full osdmap").
//   - The k3s nodes get the host's /dev and /lib/modules. A privileged container's /dev is a
//     snapshot taken when it starts, so the /dev/rbdN a map creates later never appears in it
//     ("rbd: mapping succeeded", then the mount fails); and Ceph CSI loads the rbd module.
//   - The frame's k3s runs without its local-path provisioner, so Ceph is the cluster's only
//     storage class and its default: every volume the operator makes — data, backups, logs —
//     is on Ceph, and k3s restarting cannot bring a second default back.

const (
	cephRepo      = "quay.io/ceph/ceph"
	cephTag       = "v19.2.6" // Squid; the release the workarounds above were found against
	cephPool      = "rbd"
	cephUser      = "k8s"
	cephMonPortV1 = 6789

	cephOSDDefaultGB = 20
	cephOSDMinGB     = 5
	cephOSDMaxGB     = 1000

	// Ceph CSI, from Ceph's own chart repository. Pinned: a chart release changes the values
	// it reads, and this is the one the storage class below was written for.
	cephCSIRepo    = "https://ceph.github.io/csi-charts"
	cephCSIChart   = "ceph-csi-rbd"
	cephCSIVersion = "3.18.1"
	cephCSINS      = "ceph-csi-rbd"
	// cephStorageClass is the class every volume of a Ceph-backed frame is made in.
	cephStorageClass = "ceph-rbd"

	cephReadyFile = "/var/lib/ceph/dbcanvas/ready"
	cephScript    = "/usr/local/bin/dbcanvas-ceph"

	k3dStorageMinGB = 1
	k3dStorageMaxGB = 1000
)

// cephConfig is the non-secret profile of a deployed Ceph node.
type cephConfig struct {
	Image    string `json:"image"`
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
	FSID     string `json:"fsid"`
	// Monitor is the monitor's msgr v1 address, ip:6789 — what Ceph CSI is given. An address
	// rather than a name: Ceph CSI's node plugin runs on the k3s node's own network, where the
	// stack's names do not resolve.
	Monitor   string `json:"monitor"`
	Pool      string `json:"pool"`
	OSDSizeGB int    `json:"osdSizeGb"`
	User      string `json:"user"`
}

// cephSecrets is the key Kubernetes authenticates with (client.k8s: RBD on the one pool).
type cephSecrets struct {
	UserKey string `json:"userKey"`
}

func cephOSDSize(n designNode) int {
	if n.CephOSDSizeGB <= 0 {
		return cephOSDDefaultGB
	}
	return n.CephOSDSizeGB
}

// cephBootstrapScript brings the node up, and up again after a restart. It is re-entrant:
// the first run creates the cluster, every run starts the daemons and waits for health.
const cephBootstrapScript = `#!/bin/bash
# DBCanvas Ceph node: one monitor, one manager and one BlueStore OSD on a sparse file.
set -euo pipefail
NAME=a
SIZE_GB=${CEPH_OSD_SIZE_GB:-20}
POOL=${CEPH_POOL:-rbd}
IP=$(hostname -i | awk '{print $1}')
STATE=/var/lib/ceph/dbcanvas
mkdir -p /etc/ceph /var/lib/ceph/mon/ceph-$NAME /var/lib/ceph/mgr/ceph-x /var/lib/ceph/osd/ceph-0 /var/log/ceph /var/run/ceph $STATE
rm -f $STATE/ready

# Every key is the classic AES type, and the cluster offers only that cipher: a Linux kernel's
# RBD client (what Ceph CSI maps volumes with) cannot use Squid's default aes256k keys.
genkey() { ceph-authtool --gen-print-key --key-type=aes; }
addr() { echo "[v2:$1:3300,v1:$1:6789]"; }

if [ ! -f $STATE/bootstrapped ]; then
  FSID=$(uuidgen)
  cat >/etc/ceph/ceph.conf <<EOF
[global]
fsid = $FSID
mon_host = $IP
auth_cluster_required = cephx
auth_service_required = cephx
auth_client_required = cephx
auth_allow_insecure_global_id_reclaim = false
osd_pool_default_size = 1
osd_pool_default_min_size = 1
mon_allow_pool_size_one = true
mon_warn_on_pool_no_redundancy = false
osd_crush_chooseleaf_type = 0
[osd]
osd_objectstore = bluestore
bluestore_block_create = true
bluestore_block_size = $((SIZE_GB * 1024 * 1024 * 1024))
osd_memory_target = 1073741824
EOF
  ceph-authtool --create-keyring /tmp/mon.keyring --add-key "$(genkey)" -n mon. --cap mon 'allow *'
  ceph-authtool --create-keyring /etc/ceph/ceph.client.admin.keyring --add-key "$(genkey)" -n client.admin \
    --cap mon 'allow *' --cap osd 'allow *' --cap mds 'allow *' --cap mgr 'allow *'
  ceph-authtool /tmp/mon.keyring --import-keyring /etc/ceph/ceph.client.admin.keyring
  monmaptool --create --addv $NAME "$(addr $IP)" --fsid $FSID \
    --auth-allowed-ciphers aes --auth-service-cipher aes --auth-preferred-cipher aes /tmp/monmap
  ceph-mon --mkfs -i $NAME --monmap /tmp/monmap --keyring /tmp/mon.keyring
  echo $IP > $STATE/ip
fi

# Docker can hand a restarted container another address: move the monitor with it.
if [ "$(cat $STATE/ip)" != "$IP" ]; then
  ceph-mon -i $NAME --extract-monmap /tmp/monmap
  monmaptool --rm $NAME /tmp/monmap
  monmaptool --addv $NAME "$(addr $IP)" /tmp/monmap
  ceph-mon -i $NAME --inject-monmap /tmp/monmap
  sed -i "s/^mon_host = .*/mon_host = $IP/" /etc/ceph/ceph.conf
  echo $IP > $STATE/ip
fi

ceph-mon -i $NAME --public-addr $IP
until ceph -s >/dev/null 2>&1; do sleep 1; done
# Squid reports the classic AES keys as insecure (HEALTH_ERR). They are deliberate — see genkey —
# so the alerts are muted for good, and the health that is left means something.
for c in AUTH_INSECURE_CLIENT_KEY_TYPE AUTH_INSECURE_KEYS_ALLOWED AUTH_INSECURE_KEYS_CREATABLE \
         AUTH_INSECURE_ROTATING_SERVICE_KEY_TYPE AUTH_INSECURE_SERVICE_KEY_TYPE AUTH_INSECURE_SERVICE_TICKETS; do
  ceph health mute $c --sticky >/dev/null 2>&1 || true
done
# Ready means healthy: an OSD the map still shows up from before a restart is not.
healthy() { until ceph health 2>/dev/null | grep -q '^HEALTH_OK'; do sleep 2; done; }

if [ ! -f $STATE/bootstrapped ]; then
  ceph auth get-or-create mgr.x mon 'allow profile mgr' osd 'allow *' mds 'allow *' > /var/lib/ceph/mgr/ceph-x/keyring
  UUID=$(uuidgen)
  SECRET=$(genkey)
  printf '{"cephx_secret": "%s"}' "$SECRET" > /tmp/osd.json
  ID=$(ceph osd new $UUID -i /tmp/osd.json)
  ceph-authtool --create-keyring /var/lib/ceph/osd/ceph-$ID/keyring --name osd.$ID --add-key $SECRET
  ceph-osd -i $ID --mkfs --osd-uuid $UUID
fi

ceph-mgr -i x
ceph-osd -i 0
until ceph osd stat | grep -q " 1 up"; do sleep 1; done
healthy

if [ ! -f $STATE/bootstrapped ]; then
  ceph osd pool create $POOL 32
  ceph osd pool set $POOL size 1 --yes-i-really-mean-it
  rbd pool init $POOL
  # The one identity Kubernetes gets: RBD on this pool, nothing else.
  ceph auth get-or-create client.k8s mon 'profile rbd' osd "profile rbd pool=$POOL" mgr "profile rbd pool=$POOL" >/dev/null
  touch $STATE/bootstrapped
fi
healthy
ceph auth get-key client.k8s > $STATE/k8s.key
ceph fsid > $STATE/fsid
touch $STATE/ready
echo "ceph: ready ($IP)"
# Stay up while the monitor runs; a dead monitor ends the container and Docker restarts it.
while pgrep -x ceph-mon >/dev/null; do sleep 5; done
`

// provisionCeph records the deployment and brings the node up in the background.
func (a *App) provisionCeph(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	ref := cephRepo + ":" + cephTag
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	if host == "" {
		host = "ceph"
	}
	cfg := cephConfig{Image: ref, Hostname: host, FQDN: fqdnOf(host, domain), Pool: cephPool,
		OSDSizeGB: cephOSDSize(n), User: cephUser}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		prog := &provProgress{Percent: 0, Phase: "Starting", Log: []string{}}
		save := func() { b, _ := json.Marshal(prog); a.store.SetDeploymentProgress(st.ID, n.ID, b) }
		logln := func(s string) {
			prog.Log = append(prog.Log, s)
			if len(prog.Log) > 200 {
				prog.Log = prog.Log[len(prog.Log)-200:]
			}
			save()
		}
		setPhase := func(p string, pct int) { prog.Phase = p; prog.Percent = pct; save() }
		failNode := func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			log.Printf("stack %d ceph %s: %s", st.ID, n.ID, msg)
			prog.Phase = "failed"
			prog.Message = msg
			save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployError)
		}
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		// The node serves the K3D frames, so it runs on their platform (K3D_PLATFORM), natively,
		// rather than on DOCKER_PLATFORM: an OSD under emulation is too slow to be worth having.
		setPhase("Pulling image", 5)
		logln("ensuring " + ref + " for " + k3dPlatform() + " (about 1.5 GB, the first time)")
		if err := a.engCtx(ctx).EnsureImage(ctx, cephRepo, cephTag, k3dPlatform()); err != nil {
			failNode("pull image %s: %v", ref, err)
			return
		}

		setPhase("Waiting for Intranet to be ready", 15)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			failNode("%v", werr)
			return
		}

		setPhase("Creating container", 25)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: ref, Hostname: host, Platform: k3dPlatform(),
			Cmd:     []string{"bash", cephScript},
			Env:     []string{"CEPH_OSD_SIZE_GB=" + strconv.Itoa(cfg.OSDSizeGB), "CEPH_POOL=" + cephPool},
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		})
		if err != nil {
			failNode("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).CopyFile(ctx, id, "/usr/local/bin", "dbcanvas-ceph", 0o755, []byte(cephBootstrapScript)); err != nil {
			failNode("stage the bootstrap script: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			failNode("start container: %v", err)
			return
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON})
		logln(fmt.Sprintf("container started: a monitor, a manager and one OSD on a %d GB sparse file", cfg.OSDSizeGB))

		setPhase("Bootstrapping the cluster", 50)
		deadline := time.Now().Add(5 * time.Minute)
		for {
			if res, err := a.engCtx(ctx).Exec(ctx, id, []string{"test", "-f", cephReadyFile}, nil); err == nil && res.Code == 0 {
				break
			}
			if time.Now().After(deadline) {
				res, _ := a.engCtx(ctx).Exec(ctx, id, []string{"sh", "-c", "ceph health detail 2>&1 | head -15"}, nil)
				failNode("the cluster did not become healthy within 5 minutes:\n%s", strings.TrimSpace(res.Stdout))
				return
			}
			select {
			case <-ctx.Done():
				failNode("cancelled")
				return
			case <-time.After(2 * time.Second):
			}
		}
		read := func(p string) string {
			res, err := a.engCtx(ctx).Exec(ctx, id, []string{"cat", p}, nil)
			if err != nil || res.Code != 0 {
				return ""
			}
			return strings.TrimSpace(res.Stdout)
		}
		cfg.FSID = read("/var/lib/ceph/dbcanvas/fsid")
		ip := read("/var/lib/ceph/dbcanvas/ip")
		sec := cephSecrets{UserKey: read("/var/lib/ceph/dbcanvas/k8s.key")}
		if cfg.FSID == "" || ip == "" || sec.UserKey == "" {
			failNode("the cluster came up but did not report its fsid, address and key")
			return
		}
		cfg.Monitor = fmt.Sprintf("%s:%d", ip, cephMonPortV1)
		cfgJSON, _ = json.Marshal(cfg)
		secJSON, _ := json.Marshal(sec)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
		logln("cluster " + cfg.FSID + " healthy; monitor " + cfg.Monitor + ", pool " + cephPool)

		a.reconcileStackDNS(ctx, st.ID)
		setPhase("Running", 100)
		prog.Message = "provisioned"
		save()
		a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
	}()
}

// waitCephReady waits for a Ceph node to be running and returns what a K3D frame needs from it.
// The monitor address is read from the container as it is now, not from the config: a node
// restarted since it was provisioned may have a new one (its bootstrap moves the monitor).
func (a *App) waitCephReady(ctx context.Context, stackID int64, nodeID string, timeout time.Duration) (cephConfig, string, error) {
	if nodeID == "" {
		return cephConfig{}, "", fmt.Errorf("no Ceph node is selected")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := a.store.GetDeployment(stackID, nodeID)
		if err == nil {
			if dep.State == DeployError {
				return cephConfig{}, "", fmt.Errorf("the Ceph node failed to provision")
			}
			var cfg cephConfig
			var sec cephSecrets
			json.Unmarshal(dep.Secrets, &sec)
			if dep.State == DeployRunning && json.Unmarshal(dep.Config, &cfg) == nil && cfg.FSID != "" && sec.UserKey != "" {
				if ip := a.cephCurrentIP(ctx, dep.ContainerID); ip != "" {
					cfg.Monitor = fmt.Sprintf("%s:%d", ip, cephMonPortV1)
				}
				return cfg, sec.UserKey, nil
			}
		}
		select {
		case <-ctx.Done():
			return cephConfig{}, "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return cephConfig{}, "", fmt.Errorf("timed out waiting for the Ceph node to be ready")
}

// cephCurrentIP is the address the node's monitor is on now ("" if it cannot be read).
func (a *App) cephCurrentIP(ctx context.Context, containerID string) string {
	if containerID == "" {
		return ""
	}
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"cat", "/var/lib/ceph/dbcanvas/ip"}, nil)
	if err != nil || res.Code != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// ---------------------------------------------------------------- K3D frames on Ceph

// k3dOnCeph reports whether a frame's volumes are on a Ceph node.
func k3dOnCeph(f designFrame) bool { return f.K3DStorage == "ceph" }

// k3dCephCreateArgs are what `k3d cluster create` needs for a Ceph-backed frame: the host's
// /dev and kernel modules in every k3s node, and no local-path provisioner.
func k3dCephCreateArgs() []string {
	return []string{
		"--volume", "/dev:/dev@all",
		"--volume", "/lib/modules:/lib/modules:ro@all",
		"--k3s-arg", "--disable=local-storage@server:*",
	}
}

// cephCSIValues are the chart's values: the one cluster, and a default storage class that can
// grow, mapped over msgr v1 (see the file comment).
func cephCSIValues(cfg cephConfig, key string) string {
	return fmt.Sprintf(`csiConfig:
  - clusterID: %q
    monitors: [%q]
provisioner:
  replicaCount: 1
storageClass:
  create: true
  name: %s
  annotations:
    storageclass.kubernetes.io/is-default-class: "true"
  clusterID: %q
  pool: %s
  imageFeatures: layering
  mapOptions: "ms_mode=legacy"
  allowVolumeExpansion: true
  reclaimPolicy: Delete
secret:
  create: true
  userID: %s
  userKey: %q
`, cfg.FSID, cfg.Monitor, cephStorageClass, cfg.FSID, orDefault(cfg.Pool, cephPool), orDefault(cfg.User, cephUser), key)
}

// installCephCSI installs Ceph CSI into a frame's cluster and waits for its storage class and
// node plugins. Before the operator, so the first volume the operator asks for is already on Ceph.
func (a *App) installCephCSI(ctx context.Context, serverID string, cfg cephConfig, key string, logln func(string)) error {
	manifest := helmChartManifest(cephCSIChart, cephCSIRepo, cephCSIChart, cephCSIVersion, cephCSINS, cephCSIValues(cfg, key))
	if err := a.kubectlApply(ctx, serverID, "", manifest); err != nil {
		return fmt.Errorf("apply the Ceph CSI chart: %w", err)
	}
	if err := a.waitHelmInstall(ctx, serverID, cephCSIChart, 8*time.Minute); err != nil {
		return err
	}
	if err := a.waitForDeployment(ctx, serverID, cephCSINS, cephCSIChart+"-provisioner", 5*time.Minute); err != nil {
		return fmt.Errorf("the Ceph CSI provisioner did not become ready: %w", err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		out, err := a.kubectl(ctx, serverID, "-n", cephCSINS, "get", "ds", cephCSIChart+"-nodeplugin",
			"-o", "jsonpath={.status.desiredNumberScheduled}/{.status.numberReady}")
		if parts := strings.SplitN(strings.TrimSpace(out), "/", 2); err == nil && len(parts) == 2 && parts[0] != "" && parts[0] != "0" && parts[0] == parts[1] {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Ceph CSI node plugins did not become ready (%s)", strings.TrimSpace(out))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	logln("Ceph CSI " + cephCSIVersion + ": storage class " + cephStorageClass + " (the default, and the only one) on pool " +
		orDefault(cfg.Pool, cephPool) + " of " + cfg.Monitor + ", expandable")
	return nil
}

// cephCSIMonitor is the monitor address a cluster's Ceph CSI was last given ("" if none).
func (a *App) cephCSIMonitor(ctx context.Context, serverID string) string {
	out, err := a.kubectl(ctx, serverID, "-n", cephCSINS, "get", "configmap", "ceph-csi-config", "-o", "jsonpath={.data.config\\.json}")
	if err != nil {
		return ""
	}
	var clusters []struct {
		Monitors []string `json:"monitors"`
	}
	if json.Unmarshal([]byte(out), &clusters) != nil || len(clusters) == 0 || len(clusters[0].Monitors) == 0 {
		return ""
	}
	return clusters[0].Monitors[0]
}

// ---------------------------------------------------------------- growing volumes

// Volume scaling, as each operator release spells it. Found by reading every offered release's
// CRD (deploy/crd.yaml): enableVolumeExpansion arrived in PXC 1.16.0, PSMDB 1.18.0 and PS 0.11.0;
// storageScaling (enableVolumeScaling, and automatic growth) superseded it in PXC 1.20.0,
// PSMDB 1.22.0 and PS 1.2.0 — the older field is still in those CRDs but no longer in cr.yaml.
// The PostgreSQL operator has no switch: like Crunchy PGO it was forked from, it resizes a
// volume when the instance's dataVolumeClaimSpec asks for more.
const (
	volScaleNone      = ""
	volScaleExpansion = "enableVolumeExpansion"
	volScaleScaling   = "storageScaling"
	volScalePVC       = "dataVolumeClaimSpec"
)

var volScaleSince = map[string][2]string{ // operator → {enableVolumeExpansion, storageScaling}
	"pxc":   {"1.16.0", "1.20.0"},
	"psmdb": {"1.18.0", "1.22.0"},
	"ps":    {"0.11.0", "1.2.0"},
}

// k3dVolumeScaling is how an operator release grows a volume, or volScaleNone when it cannot.
func k3dVolumeScaling(op, ver string) string {
	if op == "pg" {
		return volScalePVC
	}
	since, ok := volScaleSince[op]
	if !ok || ver == "" {
		return volScaleNone
	}
	switch {
	case compareVersions(ver, since[1]) >= 0:
		return volScaleScaling
	case compareVersions(ver, since[0]) >= 0:
		return volScaleExpansion
	}
	return volScaleNone
}

// k3dVolumeScalingSince is the first release of an operator that can grow a volume.
func k3dVolumeScalingSince(op string) string { return volScaleSince[op][0] }

// k3dDataVolumePath is the cr.yaml path of the database volume's size, per operator. PSMDB's
// matches every replica set member kind (replsets, and their hidden / non-voting members).
func k3dDataVolumePath(op, path string) bool {
	const pvc = ".volumeSpec.persistentVolumeClaim.resources.requests.storage"
	switch op {
	case "pxc":
		return path == "spec.pxc"+pvc
	case "ps":
		return path == "spec.mysql"+pvc
	case "psmdb":
		return strings.HasPrefix(path, "spec.replsets.") && strings.HasSuffix(path, pvc)
	case "pg":
		return path == "spec.instances.dataVolumeClaimSpec.resources.requests.storage"
	}
	return false
}

// k3dStorageCR applies a frame's storage choices to the operator's cr.yaml, after the
// operator's own transform: the database volume's size, and — on Ceph, where a volume can
// grow — the release's switch for letting it.
func k3dStorageCR(src string, frame designFrame, op, ver string, logln func(string)) string {
	out := src
	if frame.K3DStorageGB > 0 {
		out = crSetDataStorage(out, op, fmt.Sprintf("%dGi", frame.K3DStorageGB))
		logln(fmt.Sprintf("database volumes: %d GiB each", frame.K3DStorageGB))
	}
	if !k3dOnCeph(frame) {
		return out
	}
	switch mode := k3dVolumeScaling(op, ver); mode {
	case volScaleExpansion, volScaleScaling:
		out = crSetVolumeScaling(out, mode)
		logln("volume growth on: spec." + map[string]string{volScaleExpansion: "enableVolumeExpansion", volScaleScaling: "storageScaling.enableVolumeScaling"}[mode] + " = true")
	case volScalePVC:
		logln("volume growth: the PostgreSQL operator resizes a volume when the instance asks for more")
	default:
		logln("operator " + ver + " cannot grow volumes (that came in " + k3dVolumeScalingSince(op) + ") — they are on Ceph, at a fixed size")
	}
	return out
}

// crSetDataStorage sets the database volume's size wherever the operator's cr.yaml has it.
func crSetDataStorage(src, op, size string) string {
	lines := strings.Split(src, "\n")
	path := newYPath()
	for i, ln := range lines {
		ind, commented, body := crLine(ln)
		key := path.update(ind, commented, body)
		if key == "storage" && k3dDataVolumePath(op, path.String()) {
			lines[i] = strings.Repeat(" ", ind) + "storage: " + size
		}
	}
	return strings.Join(lines, "\n")
}

// crSetVolumeScaling turns on the release's volume growth, as the first key of spec. Any
// shipped (commented) block stays as the example it is.
func crSetVolumeScaling(src, mode string) string {
	block := "  enableVolumeExpansion: true"
	if mode == volScaleScaling {
		block = "  storageScaling:\n    enableVolumeScaling: true"
	}
	lines := strings.Split(src, "\n")
	for i, ln := range lines {
		if strings.TrimRight(ln, " ") == "spec:" {
			out := append([]string{}, lines[:i+1]...)
			out = append(out, "  # DBCanvas: the volumes are on Ceph, which can grow them.")
			out = append(out, strings.Split(block, "\n")...)
			return strings.Join(append(out, lines[i+1:]...), "\n")
		}
	}
	return src
}

// ---------------------------------------------------------------- validation

func cephNodeIssues(n designNode) []issue {
	var out []issue
	if n.CephOSDSizeGB != 0 && (n.CephOSDSizeGB < cephOSDMinGB || n.CephOSDSizeGB > cephOSDMaxGB) {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf("Ceph node %s: the OSD is %d to %d GB", n.Label, cephOSDMinGB, cephOSDMaxGB)})
	}
	return out
}

// k3dStorageIssues checks a frame's storage choices against the design.
func k3dStorageIssues(f designFrame, doc designDoc) []issue {
	var out []issue
	who := "K3D cluster " + f.Label
	op := f.K3DOperator
	if f.K3DStorageGB != 0 {
		switch {
		case op != "pxc" && op != "ps" && op != "psmdb" && op != "pg":
			out = append(out, issue{Level: "warning", Message: who + " sets a database volume size, which applies to the four Percona operators only — ignored"})
		case f.K3DStorageGB < k3dStorageMinGB || f.K3DStorageGB > k3dStorageMaxGB:
			out = append(out, issue{Level: "error", Message: fmt.Sprintf("%s: a database volume is %d to %d GiB", who, k3dStorageMinGB, k3dStorageMaxGB)})
		}
	}
	switch f.K3DStorage {
	case "", "local":
		return out
	case "ceph":
	default:
		return append(out, issue{Level: "error", Message: who + ": unknown storage " + strconv.Quote(f.K3DStorage) + " — local or ceph"})
	}
	if op == "everest" {
		out = append(out, issue{Level: "error", Message: who + ": OpenEverest creates its own clusters and storage — Ceph volumes are for the operators DBCanvas installs itself"})
	}
	var ceph *designNode
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == f.CephNodeID && doc.Nodes[i].Type == "ceph" {
			ceph = &doc.Nodes[i]
		}
	}
	if ceph == nil {
		return append(out, issue{Level: "error", Message: who + " keeps its volumes on Ceph but no Ceph node is selected — add one to the stack and pick it"})
	}
	if f.K3DStorageGB > 0 && f.K3DStorageGB > cephOSDSize(*ceph) {
		out = append(out, issue{Level: "warning", Message: fmt.Sprintf("%s asks for %d GiB volumes, more than Ceph node %s holds (%d GB) — they are thin, so they are created, and writes fail when it fills",
			who, f.K3DStorageGB, ceph.Label, cephOSDSize(*ceph))})
	}
	if ver, ok := loadOperatorCatalog().resolveOperatorVersion(op, f.K3DOperatorVer); ok && (op == "pxc" || op == "ps" || op == "psmdb") && k3dVolumeScaling(op, ver) == volScaleNone {
		out = append(out, issue{Level: "warning", Message: fmt.Sprintf("%s runs operator %s, which cannot grow volumes (that came in %s) — they will be on Ceph at a fixed size",
			who, ver, k3dVolumeScalingSince(op))})
	}
	return out
}

// ---------------------------------------------------------------- the node's panel

// cephStatusScript prints the node's state as one JSON document: health, raw capacity, and every
// RBD image with what it is provisioned at, what it holds, and the PVC Ceph CSI made it for.
const cephStatusScript = `import json, subprocess
def run(*a):
    return subprocess.run(a, capture_output=True, text=True, timeout=30).stdout
def js(*a):
    try:
        return json.loads(run(*a) or "null")
    except Exception:
        return None
pool = "%s"
# rbd takes --format; its -f is not the format flag, as ceph's is
health = js("ceph", "health", "-f", "json") or {}
df = js("ceph", "df", "-f", "json") or {}
du = js("rbd", "du", "-p", pool, "--format", "json") or {}
images = []
for im in du.get("images", []):
    meta = js("rbd", "image-meta", "list", pool + "/" + im["name"], "--format", "json") or {}
    images.append({"name": im["name"], "provisionedBytes": im.get("provisioned_size", 0), "usedBytes": im.get("used_size", 0),
                   "pvc": meta.get("csi.storage.k8s.io/pvc/name", ""), "namespace": meta.get("csi.storage.k8s.io/pvc/namespace", "")})
st = df.get("stats", {})
print(json.dumps({"health": health.get("status", ""), "checks": sorted((health.get("checks") or {}).keys()),
                  "totalBytes": st.get("total_bytes", 0), "usedBytes": st.get("total_used_raw_bytes", 0),
                  "provisionedBytes": du.get("total_provisioned_size", 0), "images": images,
                  "status": run("ceph", "-s")}))
`

// handleCephStatus is a running Ceph node's state, for its panel.
func (a *App) handleCephStatus(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	var cfg cephConfig
	json.Unmarshal(dep.Config, &cfg)
	res, err := a.engCtx(r.Context()).Exec(r.Context(), dep.ContainerID,
		[]string{"python3", "-c", fmt.Sprintf(cephStatusScript, orDefault(cfg.Pool, cephPool))}, nil)
	if err != nil || res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if err != nil {
			msg = err.Error()
		}
		writeErr(w, http.StatusBadGateway, "read the Ceph node's state: "+lastLines(msg, 300))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(res.Stdout))
}
