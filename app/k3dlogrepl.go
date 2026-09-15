package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// k3dlogrepl.go — logical replicas of a Percona Operator for PostgreSQL cluster, as a panel.
//
// The frame's designer knob (k3dpg.go) only decides what `spec.logicalReplicas` says when the
// cluster is first created. Everything interesting about the feature happens afterwards, and the
// operator's documentation is a list of operations rather than settings: add a replica to a
// running cluster, watch it bootstrap, find out why it is stuck, reseed it after a failover broke
// its slot, connect to it, remove it. That is what this file serves.
//
// Three things about the feature shape the whole panel, and all three are documented Operator
// behaviour rather than opinions of ours:
//
//  1. **A REPLICA IS NOT EDITABLE.** "Changing fields on an existing replica does not rebuild it",
//     and "bootstrapMethod is read only during bootstrap". So there is no edit action here — only
//     add, reseed and remove. Reseed is spelled out as a three-step dance (remove it, wait until
//     it leaves status, add it back) precisely because re-applying a changed entry does nothing,
//     and a failed bootstrap never retries on the same volume. k3dLRReseed does those three steps
//     rather than leaving a user to discover that re-applying is a no-op.
//
//  2. **REMOVING A REPLICA ALWAYS DESTROYS ITS DATA.** The operator deletes the PVC "even if the
//     cluster delete-pvc finalizer is off". That is the opposite of the backup panel's rule, where
//     deleting the record and deleting the data are separate acts — so here it is said once, in
//     the response of every destructive call, rather than implied.
//
//  3. **THE REPLICA IS NOT IN ANY OF THE USUAL PLACES.** Not behind pgBouncer, not in the *-ha or
//     *-replicas Services, and not in PMM. The only way to reach it is its own Service,
//     <cluster>-lr-<name>, so the panel computes that address and the psql line for it — otherwise
//     the first thing anybody does with a working replica is connect to the primary by mistake.
//
// The preflight the panel runs before offering "add" is the doc's own requirements list
// (k3dLRPreflight). It is worth running even though the operator would refuse or hang by itself,
// because the operator's refusal is a condition on a custom resource and this one is a sentence.

const (
	// k3dLRNameMax is the documented ceiling on a replica's name. It is not decoration: the name
	// goes into a Service, a Job, a StatefulSet and a replication slot, and the operator builds
	// those by concatenation.
	k3dLRNameMax = 20
	// k3dLRMinPG is the oldest PostgreSQL major the feature supports, and k3dLRMinOperator the
	// oldest operator release that has spec.logicalReplicas at all.
	k3dLRMinPG       = 17
	k3dLRMinOperator = pgFeatures310
	// How long a reseed waits for the operator to drop the replica from status before adding it
	// back. The remove half is the slow one — the operator drops replication slots on the primary
	// first, and keeps the entry with reason AwaitingCleanup while the primary is unreachable.
	k3dLRReseedTimeout = 3 * time.Minute
)

// k3dLRName is what the operator names a replica's objects: <cluster>-lr-<replica>.
func k3dLRName(cluster, replica string) string { return cluster + "-lr-" + replica }

// k3dLRNamePattern is the character set a name may use. It ends up in a Service name, so it is a
// DNS-1123 label — and the operator builds longer names by concatenating it, hence the ceiling.
var k3dLRNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// k3dLRSpec is one entry of spec.logicalReplicas, in the shape the CR carries it. Only the fields
// the panel offers are modelled: an entry read from a live cluster keeps everything else through
// Extra, so a replica somebody added by hand with an affinity or a probe is not silently stripped
// when this file rewrites the array around it.
type k3dLRSpec struct {
	Name            string                     `json:"name"`
	Databases       []string                   `json:"databases"`
	BootstrapMethod string                     `json:"bootstrapMethod,omitempty"`
	DataVolume      json.RawMessage            `json:"dataVolumeClaimSpec,omitempty"`
	Expose          json.RawMessage            `json:"expose,omitempty"`
	Extra           map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the fields this file does not model, so a rewrite of the array preserves
// them. The array is replaced wholesale on every change (a JSON merge patch cannot edit one
// element of a list), which makes "what did we drop" the central risk of the whole file.
func (s *k3dLRSpec) UnmarshalJSON(b []byte) error {
	type plain k3dLRSpec
	if err := json.Unmarshal(b, (*plain)(s)); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	known := map[string]bool{"name": true, "databases": true, "bootstrapMethod": true,
		"dataVolumeClaimSpec": true, "expose": true}
	s.Extra = map[string]json.RawMessage{}
	for k, v := range all {
		if !known[k] {
			s.Extra[k] = v
		}
	}
	return nil
}

// MarshalJSON writes the modelled fields plus whatever Extra carried.
//
// `databases` is always emitted, and emitted as an ARRAY even when empty: [] is the CRD's own
// "every non-template database except postgres", and omitting the key is not the same statement.
func (s k3dLRSpec) MarshalJSON() ([]byte, error) {
	out := map[string]json.RawMessage{}
	for k, v := range s.Extra {
		out[k] = v
	}
	put := func(k string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out[k] = b
		return nil
	}
	if err := put("name", s.Name); err != nil {
		return nil, err
	}
	dbs := s.Databases
	if dbs == nil {
		dbs = []string{}
	}
	if err := put("databases", dbs); err != nil {
		return nil, err
	}
	if s.BootstrapMethod != "" {
		if err := put("bootstrapMethod", s.BootstrapMethod); err != nil {
			return nil, err
		}
	}
	if len(s.DataVolume) > 0 {
		out["dataVolumeClaimSpec"] = s.DataVolume
	}
	if len(s.Expose) > 0 {
		out["expose"] = s.Expose
	}
	return json.Marshal(out)
}

// k3dLRStatus is one entry of status.logicalReplicas.
type k3dLRStatus struct {
	Name      string   `json:"name"`
	State     string   `json:"state"`
	Reason    string   `json:"reason"`
	Message   string   `json:"message"`
	Databases []string `json:"databases"`
	SeededAt  string   `json:"seededAt"`
}

// k3dLRRow is one replica as the panel shows it: what the spec asks for, what the status reports,
// and the address to reach it on. A row can exist with only one of the two halves — a replica just
// added has no status yet, and one mid-removal is in status but no longer in spec.
type k3dLRRow struct {
	Name            string   `json:"name"`
	InSpec          bool     `json:"inSpec"`
	InStatus        bool     `json:"inStatus"`
	State           string   `json:"state"`
	Reason          string   `json:"reason"`
	Message         string   `json:"message"`
	Databases       []string `json:"databases"`     // resolved, from status
	SpecDatabases   []string `json:"specDatabases"` // asked for; empty = all
	SeededAt        string   `json:"seededAt"`
	BootstrapMethod string   `json:"bootstrapMethod"`
	Storage         string   `json:"storage"`
	Service         string   `json:"service"`
	Endpoint        string   `json:"endpoint"`
	BootstrapJob    string   `json:"bootstrapJob"`
}

// k3dLRCheck is one preflight requirement: whether it holds, and what to do when it does not.
type k3dLRCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Blocks bool   `json:"blocks"` // false = a warning; the add is still offered
	Detail string `json:"detail"`
}

// k3dLRCluster is the slice of the custom resource this file reads.
type k3dLRCluster struct {
	Spec struct {
		PostgresVersion int `json:"postgresVersion"`
		Instances       []struct {
			Name string `json:"name"`
		} `json:"instances"`
		LogicalReplicas []k3dLRSpec `json:"logicalReplicas"`
		Backups         struct {
			Enabled *bool `json:"enabled"`
		} `json:"backups"`
		Extensions struct {
			PGTDE struct {
				Enabled bool `json:"enabled"`
			} `json:"pg_tde"`
		} `json:"extensions"`
		Users []struct {
			Name string `json:"name"`
		} `json:"users"`
	} `json:"spec"`
	Status struct {
		State           string        `json:"state"`
		LogicalReplicas []k3dLRStatus `json:"logicalReplicas"`
		Conditions      []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// k3dLRContext resolves the frame and refuses every cluster that is not a Percona PostgreSQL
// operator new enough to have the feature. The refusal is a sentence rather than an empty tab:
// "this cluster cannot do logical replicas" and "this cluster has none" are different answers.
func (a *App) k3dLRContext(w http.ResponseWriter, r *http.Request) (Stack, Deployment, k3dConfig, bool) {
	st, frame, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return Stack{}, Deployment{}, k3dConfig{}, false
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	if cfg.Operator != "pg" {
		writeErr(w, http.StatusConflict, "cluster "+frame.Label+" does not run the Percona Operator for PostgreSQL — "+
			"logical replicas are that operator's feature")
		return Stack{}, Deployment{}, k3dConfig{}, false
	}
	if !pgHasClusterFeatures(cfg.OperatorVer) {
		writeErr(w, http.StatusConflict, "cluster "+frame.Label+" runs operator "+orDefault(cfg.OperatorVer, "of an unknown version")+
			" — logical replicas need "+k3dLRMinOperator+" or newer")
		return Stack{}, Deployment{}, k3dConfig{}, false
	}
	return st, dep, cfg, true
}

// k3dLRRead fetches the custom resource.
func (a *App) k3dLRRead(ctx context.Context, dep Deployment, cfg k3dConfig) (*k3dLRCluster, error) {
	out, err := a.kubectl(ctx, dep.ContainerID, "-n", cfg.Namespace, "get", "pg", cfg.ClusterName, "-o", "json")
	if err != nil {
		return nil, err
	}
	var cr k3dLRCluster
	if err := json.Unmarshal([]byte(out), &cr); err != nil {
		return nil, fmt.Errorf("the custom resource did not parse: %w", err)
	}
	return &cr, nil
}

// k3dLRStorageOf renders a replica's requested volume size for display.
func k3dLRStorageOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		Resources struct {
			Requests struct {
				Storage string `json:"storage"`
			} `json:"requests"`
		} `json:"resources"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.Resources.Requests.Storage
}

// k3dLRRows merges spec and status into the table the panel renders, and resolves each replica's
// Service address. A replica present in one half only still gets a row: that is exactly the state
// a half-finished add or remove is in, and hiding it would make the table lie during the two
// operations most likely to go wrong.
func (a *App) k3dLRRows(ctx context.Context, dep Deployment, cfg k3dConfig, cr *k3dLRCluster) []k3dLRRow {
	byName := map[string]*k3dLRRow{}
	var order []string
	get := func(name string) *k3dLRRow {
		if row, ok := byName[name]; ok {
			return row
		}
		row := &k3dLRRow{
			Name: name, Service: k3dLRName(cfg.ClusterName, name),
			BootstrapJob: k3dLRName(cfg.ClusterName, name) + "-bootstrap",
		}
		byName[name] = row
		order = append(order, name)
		return row
	}
	for _, s := range cr.Spec.LogicalReplicas {
		row := get(s.Name)
		row.InSpec = true
		row.SpecDatabases = s.Databases
		row.BootstrapMethod = orDefault(s.BootstrapMethod, "pgbackrest")
		row.Storage = k3dLRStorageOf(s.DataVolume)
	}
	for _, s := range cr.Status.LogicalReplicas {
		row := get(s.Name)
		row.InStatus = true
		row.State, row.Reason, row.Message = s.State, s.Reason, s.Message
		row.Databases, row.SeededAt = s.Databases, s.SeededAt
	}
	// The Service address, per replica. One `kubectl get svc` for all of them.
	if out, err := a.kubectl(ctx, dep.ContainerID, "-n", cfg.Namespace, "get", "svc", "-o", "json"); err == nil {
		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					Type      string `json:"type"`
					ClusterIP string `json:"clusterIP"`
					Ports     []struct {
						Port     int `json:"port"`
						NodePort int `json:"nodePort"`
					} `json:"ports"`
				} `json:"spec"`
				Status struct {
					LoadBalancer struct {
						Ingress []struct {
							IP       string `json:"ip"`
							Hostname string `json:"hostname"`
						} `json:"ingress"`
					} `json:"loadBalancer"`
				} `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal([]byte(out), &list) == nil {
			for _, svc := range list.Items {
				for _, name := range order {
					row := byName[name]
					if svc.Metadata.Name != row.Service {
						continue
					}
					port := pgoPostgresPort
					if len(svc.Spec.Ports) > 0 && svc.Spec.Ports[0].Port > 0 {
						port = svc.Spec.Ports[0].Port
					}
					host := row.Service + "." + cfg.Namespace + ".svc.cluster.local"
					if len(svc.Status.LoadBalancer.Ingress) > 0 {
						if ip := svc.Status.LoadBalancer.Ingress[0].IP; ip != "" {
							host = ip
						} else if h := svc.Status.LoadBalancer.Ingress[0].Hostname; h != "" {
							host = h
						}
					} else if svc.Spec.Type == "NodePort" && len(svc.Spec.Ports) > 0 && svc.Spec.Ports[0].NodePort > 0 {
						host, port = cfg.FQDN, svc.Spec.Ports[0].NodePort
					}
					row.Endpoint = fmt.Sprintf("%s:%d", host, port)
				}
			}
		}
	}
	out := make([]k3dLRRow, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// k3dLRPreflight is the documented requirements list, evaluated against this cluster.
//
// Only the things a wrong answer makes expensive are checked. The operator itself reports the rest
// through its ReadyForLogicalReplication condition (wal_level, the reserved logicalrepl user, the
// pg_hba rules), and re-deriving those here would mean running SQL on the primary to say something
// the cluster already says — so that condition is surfaced instead, by the caller.
func k3dLRPreflight(cfg k3dConfig, cr *k3dLRCluster, backups k3dLRBackups) []k3dLRCheck {
	var out []k3dLRCheck
	add := func(name string, ok, blocks bool, detail string) {
		out = append(out, k3dLRCheck{Name: name, OK: ok, Blocks: blocks, Detail: detail})
	}

	add("Operator "+k3dLRMinOperator+" or newer", true, true, "running "+cfg.OperatorVer)

	pgOK := cr.Spec.PostgresVersion >= k3dLRMinPG
	add(fmt.Sprintf("PostgreSQL %d or newer", k3dLRMinPG), pgOK, true,
		fmt.Sprintf("this cluster runs PostgreSQL %d", cr.Spec.PostgresVersion))

	// Documented, and the thing this app found the hard way before the doc was read: the operator
	// does not project the pg_tde credentials into the bootstrap Job, so the restored replica dies
	// on `could not open file "/pgconf/tde/token"` and is marked broken.
	tde := cr.Spec.Extensions.PGTDE.Enabled
	add("Transparent data encryption off", !tde, true,
		"logical replication is not supported when TDE is enabled in the cluster — Percona documents this as a "+
			"limitation to be removed in a future release; today the bootstrap job has no access to the pg_tde key "+
			"provider and the replica fails to start")

	ready := cr.Status.State == "ready"
	add("Cluster ready", ready, false,
		"the operator starts bootstrapping a replica only once the cluster is ready — this one is "+
			orDefault(cr.Status.State, "in an unknown state"))

	// The backup requirement applies to the default bootstrap method only, which is why it does
	// not block: pg_basebackup is a supported answer to it rather than a workaround.
	backupsOff := cr.Spec.Backups.Enabled != nil && !*cr.Spec.Backups.Enabled
	switch {
	case backupsOff:
		add("A backup to seed from", false, false,
			"backups are disabled on this cluster, so a replica must use bootstrapMethod pg_basebackup")
	case backups.Succeeded > 0:
		when := "an unknown time"
		if backups.Completed != "" {
			when = backups.Completed
		}
		add("A backup to seed from", true, false,
			fmt.Sprintf("%d successful backup(s); the newest finished at %s. A pgbackrest bootstrap restores that "+
				"backup, so every database the replica follows must have existed by then — if you created one since, "+
				"take a new backup first or the replica fails with `database \"...\" does not exist`",
				backups.Succeeded, when))
	default:
		add("A backup to seed from", false, false,
			"no successful backup in the repository yet — pgbackrest bootstrap has nothing to restore. "+
				"Take a backup first, or use pg_basebackup")
	}

	// logicalrepl is reserved; a user of that name in spec.users is a documented way to break this.
	clash := ""
	for _, u := range cr.Spec.Users {
		if u.Name == "logicalrepl" {
			clash = "spec.users defines logicalrepl, which the operator reserves for logical replication"
		}
	}
	add("logicalrepl not redefined", clash == "", true,
		orDefault(clash, "the operator's reserved replication user is untouched"))
	return out
}

// k3dLRNameIssue validates a proposed replica name against the documented rules.
func k3dLRNameIssue(name string, cr *k3dLRCluster) string {
	switch {
	case name == "":
		return "a replica needs a name"
	case len(name) > k3dLRNameMax:
		return fmt.Sprintf("the name is %d characters; the operator allows at most %d, because it builds "+
			"Service, Job and replication-slot names by appending to it", len(name), k3dLRNameMax)
	case !k3dLRNamePattern.MatchString(name):
		return "the name becomes a Service name, so it must be a DNS-1123 label: lowercase letters, digits and '-'"
	}
	for _, s := range cr.Spec.LogicalReplicas {
		if s.Name == name {
			return "this cluster already has a logical replica called " + name
		}
	}
	// The doc is explicit that the name must not collide with an instance set's.
	for _, i := range cr.Spec.Instances {
		if i.Name == name {
			return "spec.instances already has a set called " + name + ", and a replica may not share its name"
		}
	}
	return ""
}

// k3dLRApply writes a whole logicalReplicas array back, server-dry-run first.
//
// The array is replaced wholesale because a JSON merge patch — the only patch type a custom
// resource takes here — cannot address one element of a list. That is why k3dLRSpec keeps its
// unmodelled fields: the rewrite has to hand back everything it was given.
func (a *App) k3dLRApply(ctx context.Context, dep Deployment, cfg k3dConfig, list []k3dLRSpec) error {
	if list == nil {
		list = []k3dLRSpec{}
	}
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"logicalReplicas": list}})
	if err != nil {
		return fmt.Errorf("the change does not encode: %w", err)
	}
	patch := func(dry bool) (string, error) {
		args := []string{"-n", cfg.Namespace, "patch", "pg", cfg.ClusterName, "--type", "merge", "-p", string(body)}
		if dry {
			args = append(args, "--dry-run=server")
		}
		return a.kubectl(ctx, dep.ContainerID, args...)
	}
	if out, err := patch(true); err != nil {
		return fmt.Errorf("%s", lastLines(strings.TrimSpace(err.Error()+"\n"+out), 600))
	}
	if out, err := patch(false); err != nil {
		return fmt.Errorf("%s", lastLines(strings.TrimSpace(err.Error()+"\n"+out), 600))
	}
	return nil
}

// k3dLRBackups is what the repository has to seed a replica from: how many successful backups,
// and WHEN the newest of them finished.
//
// The timestamp is the load-bearing part. Percona's requirement is not "a backup exists", it is
// "a backup taken AFTER you created the databases the replica will follow" — restore an older one
// and pg_createsubscriber stops with `database "x" does not exist`, which is a broken replica and
// a deleted volume rather than an error you can retry. Nothing in Kubernetes or PostgreSQL will
// tell us when a database was created, so the panel cannot decide this on the user's behalf; what
// it can do is put the backup's own clock on the screen next to the choice.
type k3dLRBackups struct {
	Succeeded int    `json:"succeeded"`
	Latest    string `json:"latest"`    // the newest successful backup's object name
	Completed string `json:"completed"` // …and when it finished (RFC3339)
}

func (a *App) k3dLRBackups(ctx context.Context, dep Deployment, cfg k3dConfig) k3dLRBackups {
	var res k3dLRBackups
	out, err := a.kubectl(ctx, dep.ContainerID, "-n", cfg.Namespace, "get", "pg-backup", "-o", "json")
	if err != nil {
		return res
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				State     string `json:"state"`
				Completed string `json:"completed"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return res
	}
	for _, it := range list.Items {
		if !strings.EqualFold(it.Status.State, "Succeeded") {
			continue
		}
		res.Succeeded++
		if it.Status.Completed > res.Completed {
			res.Completed, res.Latest = it.Status.Completed, it.Metadata.Name
		}
	}
	return res
}

// k3dLRPrimaryRoles are the label values the operator puts on the primary instance Pod. Percona's
// build of the Crunchy operator labels it `primary`; the upstream lineage used `master`, and a
// cluster mid-upgrade can be either — so both are tried rather than one being assumed.
var k3dLRPrimaryRoles = []string{"primary", "master"}

// k3dLRPrimaryDatabases lists the non-template databases on the primary.
//
// It exists to catch the other half of the "database does not exist" failure: a name that is
// simply wrong. The Operator's documented behaviour for a database it cannot find is to WAIT —
// "if a named database does not exist yet, it waits" — so a typo does not fail, it hangs, and the
// replica sits in bootstrapping with nothing to read. Offering the real list turns that into a
// pick rather than a spelling test.
//
// Best-effort: on any error the panel simply has no list, and the add is not blocked by it.
func (a *App) k3dLRPrimaryDatabases(ctx context.Context, dep Deployment, cfg k3dConfig) []string {
	pod := ""
	for _, role := range k3dLRPrimaryRoles {
		out, err := a.kubectl(ctx, dep.ContainerID, "-n", cfg.Namespace, "get", "pods",
			"-l", "postgres-operator.crunchydata.com/role="+role,
			"-o", "jsonpath={.items[0].metadata.name}")
		if err == nil && strings.TrimSpace(out) != "" {
			pod = strings.TrimSpace(out)
			break
		}
	}
	if pod == "" {
		return nil
	}
	out, err := a.kubectl(ctx, dep.ContainerID, "-n", cfg.Namespace, "exec", pod, "-c", "database", "--",
		"psql", "-tAc", "SELECT datname FROM pg_database WHERE datistemplate = false AND datname <> 'postgres' ORDER BY 1")
	if err != nil {
		return nil
	}
	var dbs []string
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			dbs = append(dbs, ln)
		}
	}
	return dbs
}

// ------------------------------------------------------------------ handlers

func (a *App) handleK3DLogicalReplicas(w http.ResponseWriter, r *http.Request) {
	_, dep, cfg, ok := a.k3dLRContext(w, r)
	if !ok {
		return
	}
	cr, err := a.k3dLRRead(r.Context(), dep, cfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, lastLines(err.Error(), 400))
		return
	}
	backups := a.k3dLRBackups(r.Context(), dep, cfg)
	resp := map[string]any{
		"namespace": cfg.Namespace, "cluster": cfg.ClusterName,
		"operatorVersion": cfg.OperatorVer, "postgresVersion": cr.Spec.PostgresVersion,
		"clusterState": cr.Status.State,
		"replicas":     a.k3dLRRows(r.Context(), dep, cfg, cr),
		"checks":       k3dLRPreflight(cfg, cr, backups),
		"nameMax":      k3dLRNameMax,
		"appUser":      cfg.ClusterName,
		"userSecret":   cfg.ClusterName + "-pguser-" + cfg.ClusterName,
		"backups":      backups,
		// The databases a replica may actually be pointed at. The Operator waits rather than
		// failing on a name it cannot find, so offering the list is what keeps a typo from
		// becoming a replica that bootstraps forever.
		"availableDatabases": a.k3dLRPrimaryDatabases(r.Context(), dep, cfg),
	}
	// The operator's own verdict on whether the primary can feed a replica at all. It is the
	// answer to "why is it still bootstrapping", so it is surfaced verbatim rather than summarised.
	for _, c := range cr.Status.Conditions {
		if c.Type == "ReadyForLogicalReplication" {
			resp["ready"] = map[string]any{"status": c.Status, "reason": c.Reason, "message": c.Message}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// k3dLRAddRequest is the add form.
type k3dLRAddRequest struct {
	Name            string   `json:"name"`
	Databases       []string `json:"databases"`
	BootstrapMethod string   `json:"bootstrapMethod"`
	StorageGB       int      `json:"storageGb"`
	Expose          string   `json:"expose"` // "" | ClusterIP | NodePort | LoadBalancer
}

// k3dLRSpecFrom builds a spec entry from the add form.
func k3dLRSpecFrom(req k3dLRAddRequest) (k3dLRSpec, error) {
	method := strings.TrimSpace(req.BootstrapMethod)
	if method == "" {
		method = "pgbackrest"
	}
	if method != "pgbackrest" && method != "pg_basebackup" {
		return k3dLRSpec{}, fmt.Errorf("bootstrapMethod must be pgbackrest or pg_basebackup")
	}
	gb := req.StorageGB
	if gb <= 0 {
		gb = 1
	}
	gb = clampInt(gb, 1, 512)
	// dataVolumeClaimSpec is required by the CRD — there is no operator-side default — so it is
	// always written, even when the form left the size blank.
	vol, err := json.Marshal(map[string]any{
		"accessModes": []string{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]any{"storage": fmt.Sprintf("%dGi", gb)}},
	})
	if err != nil {
		return k3dLRSpec{}, err
	}
	s := k3dLRSpec{Name: strings.TrimSpace(req.Name), BootstrapMethod: method, DataVolume: vol}
	seen := map[string]bool{}
	for _, db := range req.Databases {
		db = strings.TrimSpace(db)
		if db == "" || seen[db] {
			continue
		}
		seen[db] = true
		s.Databases = append(s.Databases, db)
	}
	switch e := strings.TrimSpace(req.Expose); e {
	case "", "ClusterIP":
		// The operator's default; no expose block, so the CR stays as small as what was asked for.
	case "NodePort", "LoadBalancer":
		b, err := json.Marshal(map[string]any{"type": e})
		if err != nil {
			return k3dLRSpec{}, err
		}
		s.Expose = b
	default:
		return k3dLRSpec{}, fmt.Errorf("expose must be ClusterIP, NodePort or LoadBalancer")
	}
	return s, nil
}

func (a *App) handleK3DLogicalReplicaAdd(w http.ResponseWriter, r *http.Request) {
	_, dep, cfg, ok := a.k3dLRContext(w, r)
	if !ok {
		return
	}
	var req k3dLRAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cr, err := a.k3dLRRead(r.Context(), dep, cfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, lastLines(err.Error(), 400))
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if msg := k3dLRNameIssue(req.Name, cr); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	// The blocking preflight, refused here rather than left to the operator: every one of these
	// produces a replica that sits broken or bootstrapping forever rather than an error.
	backups := a.k3dLRBackups(r.Context(), dep, cfg)
	for _, c := range k3dLRPreflight(cfg, cr, backups) {
		if c.Blocks && !c.OK {
			writeErr(w, http.StatusConflict, c.Name+": "+c.Detail)
			return
		}
	}
	// A database the primary does not have. The Operator's response to this is to WAIT, so the
	// replica would bootstrap forever with nothing on screen to say why — which makes refusing it
	// here worth more than the round trip. Skipped entirely when the primary could not be read:
	// a best-effort check must not become a blocker on its own failure.
	if have := a.k3dLRPrimaryDatabases(r.Context(), dep, cfg); len(have) > 0 {
		known := map[string]bool{}
		for _, db := range have {
			known[db] = true
		}
		for _, db := range req.Databases {
			if db = strings.TrimSpace(db); db != "" && !known[db] {
				writeErr(w, http.StatusConflict, "this cluster has no database called "+db+
					" — the operator waits indefinitely for one it cannot find rather than failing. It has: "+
					strings.Join(have, ", "))
				return
			}
		}
	}
	entry, err := k3dLRSpecFrom(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.k3dLRApply(r.Context(), dep, cfg, append(cr.Spec.LogicalReplicas, entry)); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.replLogln(dep.StackID, dep.NodeID, "logical replica "+entry.Name+" added ("+entry.BootstrapMethod+")")
	msg := "added — the operator bootstraps it once the cluster is ready; watch the state on this tab"
	// The one caveat that only bites this combination: a pgbackrest seed restores the newest
	// backup, so a database created after it is not in the restored copy and pg_createsubscriber
	// stops with `database "..." does not exist`.
	if entry.BootstrapMethod == "pgbackrest" && len(entry.Databases) > 0 && backups.Completed != "" {
		msg += ". It will be seeded from the backup that finished at " + backups.Completed +
			" — if " + strings.Join(entry.Databases, " or ") + " was created after that, take a new backup and reseed"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": entry.Name, "message": msg})
}

// k3dLRRemove drops one replica from the array and applies the result. Returns the entry it
// removed, so a reseed can put the same one back.
func (a *App) k3dLRRemove(ctx context.Context, dep Deployment, cfg k3dConfig, name string) (k3dLRSpec, error) {
	cr, err := a.k3dLRRead(ctx, dep, cfg)
	if err != nil {
		return k3dLRSpec{}, err
	}
	var kept []k3dLRSpec
	var found *k3dLRSpec
	for i, s := range cr.Spec.LogicalReplicas {
		if s.Name == name {
			found = &cr.Spec.LogicalReplicas[i]
			continue
		}
		kept = append(kept, s)
	}
	if found == nil {
		return k3dLRSpec{}, fmt.Errorf("cluster %s has no logical replica called %s in its spec", cfg.ClusterName, name)
	}
	if err := a.k3dLRApply(ctx, dep, cfg, kept); err != nil {
		return k3dLRSpec{}, err
	}
	return *found, nil
}

func (a *App) handleK3DLogicalReplicaRemove(w http.ResponseWriter, r *http.Request) {
	_, dep, cfg, ok := a.k3dLRContext(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	name := strings.TrimSpace(req.Name)
	if _, err := a.k3dLRRemove(r.Context(), dep, cfg, name); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.replLogln(dep.StackID, dep.NodeID, "logical replica "+name+" removed (its volume is deleted with it)")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"message": "removed. The operator always deletes the replica's volume with it, whatever the cluster's " +
			"delete-pvc finalizer says — the data is gone. If the primary is down the entry stays in status with " +
			"reason AwaitingCleanup until the replication slots can be dropped",
	})
}

// handleK3DLogicalReplicaReseed performs the documented reseed: remove the replica, wait until the
// operator has dropped it from status, then add the same entry back.
//
// It is three steps rather than one because the operator does not rebuild a replica in place —
// "changing fields on an existing replica does not rebuild it", and "a failed bootstrap does not
// retry on the same volume". Adding the entry back before the removal has finished re-uses the old
// state, which is the failure this exists to avoid, so the wait is not optional.
func (a *App) handleK3DLogicalReplicaReseed(w http.ResponseWriter, r *http.Request) {
	_, dep, cfg, ok := a.k3dLRContext(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	name := strings.TrimSpace(req.Name)
	entry, err := a.k3dLRRemove(r.Context(), dep, cfg, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// Wait for it to leave status. On timeout the replica is left REMOVED rather than added back
	// half-cleaned: that is the recoverable end of the two, and the panel says so.
	deadline := time.Now().Add(k3dLRReseedTimeout)
	for {
		cr, rerr := a.k3dLRRead(r.Context(), dep, cfg)
		if rerr == nil {
			gone := true
			for _, s := range cr.Status.LogicalReplicas {
				if s.Name == name {
					gone = false
					break
				}
			}
			if gone {
				break
			}
		}
		if time.Now().After(deadline) {
			a.replLogln(dep.StackID, dep.NodeID, "logical replica "+name+" reseed: removal did not finish in time")
			writeJSON(w, http.StatusAccepted, map[string]any{
				"ok": false, "removed": true,
				"message": "the replica was removed but the operator has not finished dropping it from status " +
					"(it keeps the entry with reason AwaitingCleanup while the primary is unreachable). " +
					"Nothing was added back — add it again from this tab once the entry is gone",
			})
			return
		}
		select {
		case <-r.Context().Done():
			writeErr(w, http.StatusRequestTimeout, "the request was cancelled; the replica has been removed but not added back")
			return
		case <-time.After(3 * time.Second):
		}
	}
	cr, err := a.k3dLRRead(r.Context(), dep, cfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, lastLines(err.Error(), 400))
		return
	}
	if err := a.k3dLRApply(r.Context(), dep, cfg, append(cr.Spec.LogicalReplicas, entry)); err != nil {
		writeErr(w, http.StatusBadGateway, "the replica was removed but adding it back failed: "+err.Error())
		return
	}
	a.replLogln(dep.StackID, dep.NodeID, "logical replica "+name+" reseeded (removed, waited for cleanup, added back)")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "reseeded — the replica is bootstrapping again from a fresh copy",
	})
}

// handleK3DLogicalReplicaBootstrapLog returns the bootstrap Job's log, which is where a replica
// stuck in `bootstrapping` explains itself. The Job is what the doc tells you to look at.
func (a *App) handleK3DLogicalReplicaBootstrapLog(w http.ResponseWriter, r *http.Request) {
	_, dep, cfg, ok := a.k3dLRContext(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	job := k3dLRName(cfg.ClusterName, name) + "-bootstrap"
	resp := map[string]any{"job": job, "namespace": cfg.Namespace}
	if out, err := a.kubectl(r.Context(), dep.ContainerID, "-n", cfg.Namespace, "get", "job", job,
		"-o", "jsonpath={.status.succeeded}/{.status.failed}"); err == nil {
		resp["status"] = strings.TrimSpace(out)
	}
	// --all-containers so the restore step and pg_createsubscriber both appear; a bootstrap that
	// failed in an init container is otherwise an empty log.
	out, err := a.kubectl(r.Context(), dep.ContainerID, "-n", cfg.Namespace, "logs", "job/"+job,
		"--all-containers", "--tail", "400")
	if err != nil {
		resp["error"] = lastLines(strings.TrimSpace(err.Error()+"\n"+out), 400)
	} else {
		resp["log"] = out
	}
	writeJSON(w, http.StatusOK, resp)
}
