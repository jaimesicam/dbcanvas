package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// k3dstorage.go — a K3D cluster's volumes after deploy, and growing them.
//
// Growing goes through the operator, never around it: the new size is written into the custom
// resource, where the operator reads it, resizes the PersistentVolumeClaims and keeps its
// StatefulSet in step. Patching a PVC directly would work once and leave the operator's own
// record of the size behind — the drift that is itself a classic ticket, but not one to cause
// by accident. So it is offered only where both halves can do it: the volumes are on Ceph (k3s's
// local-path cannot resize anything), and the operator release has volume scaling (ceph.go,
// k3dVolumeScaling). There is no shrinking: Kubernetes refuses a smaller PVC outright.

// k3dCRKind is the kubectl name of each operator's custom resource.
var k3dCRKind = map[string]string{"pxc": "pxc", "ps": "ps", "psmdb": "psmdb", "pg": "pg"}

// k3dVolume is one PVC as the storage panel shows it.
type k3dVolume struct {
	Name          string `json:"name"`
	StorageClass  string `json:"storageClass"`
	Phase         string `json:"phase"`
	Requested     string `json:"requested"`
	Capacity      string `json:"capacity"`
	UsedBytes     int64  `json:"usedBytes,omitempty"`
	CapacityBytes int64  `json:"capacityBytes,omitempty"`
	Pod           string `json:"pod,omitempty"`
	// Resizing is the PVC's resize in progress, in Kubernetes' words ("" when none).
	Resizing string `json:"resizing,omitempty"`
	// Data marks the database volumes — the ones a grow resizes.
	Data bool `json:"data"`
}

// k3dStorageView is what GET returns.
type k3dStorageView struct {
	Storage      string      `json:"storage"` // "ceph" | "local"
	StorageClass string      `json:"storageClass"`
	Operator     string      `json:"operator"`
	OperatorVer  string      `json:"operatorVersion"`
	Scaling      string      `json:"scaling"`
	CanGrow      bool        `json:"canGrow"`
	Why          string      `json:"why,omitempty"` // why it cannot grow
	DataSize     string      `json:"dataSize"`      // the database volume size the CR asks for
	Volumes      []k3dVolume `json:"volumes"`
	MaxGiB       int         `json:"maxGiB"`
	// MonitorMoved is set when the Ceph node's monitor is no longer where Ceph CSI was told it
	// is (the node restarted on another address): new volumes cannot be mapped until it is
	// re-pointed, which a grow does first.
	MonitorMoved string `json:"monitorMoved,omitempty"`
}

// parseQuantity turns a Kubernetes quantity ("6G", "3Gi", "1073741824") into bytes.
func parseQuantity(q string) (int64, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0, false
	}
	units := []struct {
		suffix string
		mult   float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
		{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			mult, q = u.mult, strings.TrimSuffix(q, u.suffix)
			break
		}
	}
	f, err := strconv.ParseFloat(q, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return int64(math.Round(f * mult)), true
}

// k3dStorageContext resolves the frame and its deployed config.
func (a *App) k3dStorageContext(w http.ResponseWriter, r *http.Request) (Stack, designFrame, Deployment, k3dConfig, bool) {
	st, frame, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return Stack{}, designFrame{}, Deployment{}, k3dConfig{}, false
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	return st, frame, dep, cfg, true
}

// k3dDataSizes reads the database volume size(s) the custom resource asks for, in spec order.
func (a *App) k3dDataSizes(ctx context.Context, serverID string, cfg k3dConfig) ([]string, error) {
	kind := k3dCRKind[cfg.Operator]
	if kind == "" || cfg.ClusterName == "" {
		return nil, nil
	}
	out, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "get", kind, cfg.ClusterName, "-o", "json")
	if err != nil {
		return nil, err
	}
	type pvc struct {
		PersistentVolumeClaim struct {
			Resources struct {
				Requests struct {
					Storage string `json:"storage"`
				} `json:"requests"`
			} `json:"resources"`
		} `json:"persistentVolumeClaim"`
	}
	var cr struct {
		Spec struct {
			PXC   struct{ VolumeSpec pvc } `json:"pxc"`
			MySQL struct{ VolumeSpec pvc } `json:"mysql"`
			Rs    []struct {
				VolumeSpec pvc `json:"volumeSpec"`
			} `json:"replsets"`
			Instances []struct {
				Data struct {
					Resources struct {
						Requests struct {
							Storage string `json:"storage"`
						} `json:"requests"`
					} `json:"resources"`
				} `json:"dataVolumeClaimSpec"`
			} `json:"instances"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &cr); err != nil {
		return nil, fmt.Errorf("the custom resource did not parse: %w", err)
	}
	var sizes []string
	switch cfg.Operator {
	case "pxc":
		sizes = append(sizes, cr.Spec.PXC.VolumeSpec.PersistentVolumeClaim.Resources.Requests.Storage)
	case "ps":
		sizes = append(sizes, cr.Spec.MySQL.VolumeSpec.PersistentVolumeClaim.Resources.Requests.Storage)
	case "psmdb":
		for _, r := range cr.Spec.Rs {
			sizes = append(sizes, r.VolumeSpec.PersistentVolumeClaim.Resources.Requests.Storage)
		}
	case "pg":
		for _, in := range cr.Spec.Instances {
			sizes = append(sizes, in.Data.Resources.Requests.Storage)
		}
	}
	return sizes, nil
}

// k3dDataPVC tells the database volumes from the rest (backups, logs, config servers), by the
// names each operator gives them.
func k3dDataPVC(op, name string) bool {
	switch op {
	case "pxc":
		return strings.HasPrefix(name, "datadir-") && strings.Contains(name, "-pxc-")
	case "ps":
		return strings.HasPrefix(name, "datadir-") && strings.Contains(name, "-mysql-")
	case "psmdb":
		return strings.HasPrefix(name, "mongod-data-") && !strings.Contains(name, "-cfg-")
	case "pg":
		return strings.HasSuffix(name, "-pgdata")
	}
	return false
}

// k3dVolumes lists the namespace's PVCs, with what kubelet says each holds.
func (a *App) k3dVolumes(ctx context.Context, serverID string, cfg k3dConfig) ([]k3dVolume, error) {
	out, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "get", "pvc", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				StorageClassName string `json:"storageClassName"`
				Resources        struct {
					Requests struct {
						Storage string `json:"storage"`
					} `json:"requests"`
				} `json:"resources"`
			} `json:"spec"`
			Status struct {
				Phase    string `json:"phase"`
				Capacity struct {
					Storage string `json:"storage"`
				} `json:"capacity"`
				Conditions []struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	used := a.k3dVolumeUsage(ctx, serverID, cfg.Namespace)
	vols := []k3dVolume{}
	for _, it := range list.Items {
		v := k3dVolume{Name: it.Metadata.Name, StorageClass: it.Spec.StorageClassName, Phase: it.Status.Phase,
			Requested: it.Spec.Resources.Requests.Storage, Capacity: it.Status.Capacity.Storage,
			Data: k3dDataPVC(cfg.Operator, it.Metadata.Name)}
		for _, c := range it.Status.Conditions {
			if c.Type == "Resizing" || c.Type == "FileSystemResizePending" || c.Type == "ControllerResizeError" || c.Type == "NodeResizeError" {
				v.Resizing = strings.TrimSpace(c.Type + " " + c.Message)
			}
		}
		if u, ok := used[v.Name]; ok {
			v.UsedBytes, v.CapacityBytes, v.Pod = u.used, u.capacity, u.pod
		}
		vols = append(vols, v)
	}
	sort.Slice(vols, func(i, j int) bool {
		if vols[i].Data != vols[j].Data {
			return vols[i].Data
		}
		return vols[i].Name < vols[j].Name
	})
	return vols, nil
}

type k3dVolUse struct {
	used, capacity int64
	pod            string
}

// k3dVolumeUsage asks each node's kubelet what its mounted volumes hold (the summary API —
// what `kubectl top` would need metrics-server for).
func (a *App) k3dVolumeUsage(ctx context.Context, serverID, ns string) map[string]k3dVolUse {
	out := map[string]k3dVolUse{}
	nodes, err := a.kubectl(ctx, serverID, "get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return out
	}
	for _, node := range strings.Fields(nodes) {
		raw, err := a.kubectl(ctx, serverID, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/stats/summary")
		if err != nil {
			continue
		}
		var sum struct {
			Pods []struct {
				PodRef struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"podRef"`
				Volume []struct {
					UsedBytes     int64 `json:"usedBytes"`
					CapacityBytes int64 `json:"capacityBytes"`
					PVCRef        *struct {
						Name string `json:"name"`
					} `json:"pvcRef"`
				} `json:"volume"`
			} `json:"pods"`
		}
		if json.Unmarshal([]byte(raw), &sum) != nil {
			continue
		}
		for _, p := range sum.Pods {
			if p.PodRef.Namespace != ns {
				continue
			}
			for _, v := range p.Volume {
				if v.PVCRef != nil {
					out[v.PVCRef.Name] = k3dVolUse{used: v.UsedBytes, capacity: v.CapacityBytes, pod: p.PodRef.Name}
				}
			}
		}
	}
	return out
}

func (a *App) k3dStorageViewOf(ctx context.Context, st Stack, dep Deployment, cfg k3dConfig) (k3dStorageView, error) {
	v := k3dStorageView{Storage: "local", StorageClass: "local-path", Operator: cfg.Operator, OperatorVer: cfg.OperatorVer, MaxGiB: k3dStorageMaxGB}
	if cfg.Storage == "ceph" {
		v.Storage, v.StorageClass = "ceph", cephStorageClass
	}
	v.Scaling = k3dVolumeScaling(cfg.Operator, cfg.OperatorVer)
	switch {
	case k3dCRKind[cfg.Operator] == "":
		v.Why = "growing volumes is offered for the four Percona operators"
	case v.Storage != "ceph":
		v.Why = "the volumes are k3s's local-path — a directory on a node, which Kubernetes cannot resize. Choose Ceph volumes for the cluster at design time to be able to grow them"
	case v.Scaling == volScaleNone:
		v.Why = "operator " + cfg.OperatorVer + " cannot grow volumes — that came in " + k3dVolumeScalingSince(cfg.Operator)
	default:
		v.CanGrow = true
	}
	vols, err := a.k3dVolumes(ctx, dep.ContainerID, cfg)
	if err != nil {
		return v, err
	}
	v.Volumes = vols
	if sizes, err := a.k3dDataSizes(ctx, dep.ContainerID, cfg); err == nil && len(sizes) > 0 {
		v.DataSize = sizes[0]
	}
	if v.Storage == "ceph" {
		if cdep, err := a.store.GetDeployment(st.ID, cfg.CephNodeID); err == nil {
			if ip := a.cephCurrentIP(ctx, cdep.ContainerID); ip != "" {
				want := fmt.Sprintf("%s:%d", ip, cephMonPortV1)
				if have := a.cephCSIMonitor(ctx, dep.ContainerID); have != "" && have != want {
					v.MonitorMoved = want
				}
			}
		}
	}
	return v, nil
}

func (a *App) handleK3DStorage(w http.ResponseWriter, r *http.Request) {
	st, _, dep, cfg, ok := a.k3dStorageContext(w, r)
	if !ok {
		return
	}
	v, err := a.k3dStorageViewOf(r.Context(), st, dep, cfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "read the volumes: "+lastLines(err.Error(), 300))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleK3DStorageGrow writes a larger database volume size into the custom resource:
// {"sizeGiB": 10}. The operator does the rest; GET shows it happen.
func (a *App) handleK3DStorageGrow(w http.ResponseWriter, r *http.Request) {
	st, frame, dep, cfg, ok := a.k3dStorageContext(w, r)
	if !ok {
		return
	}
	var in struct {
		SizeGiB int `json:"sizeGiB"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx := r.Context()
	v, err := a.k3dStorageViewOf(ctx, st, dep, cfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "read the volumes: "+lastLines(err.Error(), 300))
		return
	}
	if !v.CanGrow {
		writeErr(w, http.StatusConflict, "cluster "+frame.Label+" cannot grow its volumes: "+v.Why)
		return
	}
	if in.SizeGiB < k3dStorageMinGB || in.SizeGiB > k3dStorageMaxGB {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("a database volume is %d to %d GiB", k3dStorageMinGB, k3dStorageMaxGB))
		return
	}
	want := int64(in.SizeGiB) << 30
	// Larger than what the custom resource asks for and than every database volume already is
	// — a request at or below either is a shrink, which nothing will do.
	floor := int64(0)
	if b, ok := parseQuantity(v.DataSize); ok {
		floor = b
	}
	for _, vol := range v.Volumes {
		if !vol.Data {
			continue
		}
		for _, q := range []string{vol.Capacity, vol.Requested} {
			if b, ok := parseQuantity(q); ok && b > floor {
				floor = b
			}
		}
	}
	if want <= floor {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("the volumes are %.1f GiB already — a new size has to be larger (Kubernetes cannot shrink a volume)",
			float64(floor)/(1<<30)))
		return
	}
	// A Ceph node that moved since the cluster was told where it is: re-point Ceph CSI first, or
	// the resize cannot reach the image.
	if v.MonitorMoved != "" {
		if err := a.cephRepointCSI(ctx, st, dep, cfg); err != nil {
			writeErr(w, http.StatusBadGateway, "re-point Ceph CSI at the moved monitor: "+lastLines(err.Error(), 300))
			return
		}
	}
	size := fmt.Sprintf("%dGi", in.SizeGiB)
	kind := k3dCRKind[cfg.Operator]
	sizes, err := a.k3dDataSizes(ctx, dep.ContainerID, cfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "read the custom resource: "+lastLines(err.Error(), 300))
		return
	}
	patch, err := k3dGrowPatch(cfg.Operator, len(sizes), size)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if _, err := a.kubectl(ctx, dep.ContainerID, "-n", cfg.Namespace, "patch", kind, cfg.ClusterName, "--type", "json", "-p", patch); err != nil {
		writeErr(w, http.StatusBadGateway, "patch the custom resource: "+lastLines(err.Error(), 400))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"size": size, "patched": kind + "/" + cfg.ClusterName})
}

// k3dGrowPatch is the JSON patch that sets every database volume's size in the custom resource.
// n is how many replica sets (PSMDB) or instance sets (PG) the spec has.
func k3dGrowPatch(op string, n int, size string) (string, error) {
	type opn struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}
	var ops []opn
	switch op {
	case "pxc":
		ops = append(ops, opn{"replace", "/spec/pxc/volumeSpec/persistentVolumeClaim/resources/requests/storage", size})
	case "ps":
		ops = append(ops, opn{"replace", "/spec/mysql/volumeSpec/persistentVolumeClaim/resources/requests/storage", size})
	case "psmdb":
		for i := 0; i < n; i++ {
			ops = append(ops, opn{"replace", fmt.Sprintf("/spec/replsets/%d/volumeSpec/persistentVolumeClaim/resources/requests/storage", i), size})
		}
	case "pg":
		for i := 0; i < n; i++ {
			ops = append(ops, opn{"replace", fmt.Sprintf("/spec/instances/%d/dataVolumeClaimSpec/resources/requests/storage", i), size})
		}
	}
	if len(ops) == 0 {
		return "", fmt.Errorf("the custom resource has no database volume to grow")
	}
	b, _ := json.Marshal(ops)
	return string(b), nil
}

// cephRepointCSI gives a cluster's Ceph CSI the Ceph node's current monitor address. The chart
// is re-applied with it; the node plugins read the config map as it changes.
func (a *App) cephRepointCSI(ctx context.Context, st Stack, dep Deployment, cfg k3dConfig) error {
	cdep, err := a.store.GetDeployment(st.ID, cfg.CephNodeID)
	if err != nil {
		return fmt.Errorf("the Ceph node is not deployed")
	}
	var ccfg cephConfig
	var sec cephSecrets
	json.Unmarshal(cdep.Config, &ccfg)
	json.Unmarshal(cdep.Secrets, &sec)
	ip := a.cephCurrentIP(ctx, cdep.ContainerID)
	if ip == "" || ccfg.FSID == "" || sec.UserKey == "" {
		return fmt.Errorf("the Ceph node is not running")
	}
	ccfg.Monitor = fmt.Sprintf("%s:%d", ip, cephMonPortV1)
	manifest := helmChartManifest(cephCSIChart, cephCSIRepo, cephCSIChart, cephCSIVersion, cephCSINS, cephCSIValues(ccfg, sec.UserKey))
	if err := a.kubectlApply(ctx, dep.ContainerID, "", manifest); err != nil {
		return err
	}
	return a.waitHelmInstall(ctx, dep.ContainerID, cephCSIChart, 5*time.Minute)
}
