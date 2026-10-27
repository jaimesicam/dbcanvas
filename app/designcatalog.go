package main

import (
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// designcatalog.go — what a stack design can hold, served to API clients.
//
// A design is one JSON document of nodes, cluster frames and edges, and each node or
// frame carries the fields its type reads: about 335 of them across designNode and
// designFrame, most meaningful to a handful of types. The Stack Designer knows which
// fields belong to which type because its properties panels draw them; an API client
// has no panels, so this file is the same knowledge as data. It drives:
//
//   - GET /api/stacks/design/catalog — every node type, cluster type, option,
//     relationship, association and post-deployment action, for a client to read.
//   - The design editing endpoints (designedit.go), which accept a type's catalogued
//     options and refuse anything else by name, so a misspelt field is an error
//     rather than a key the deploy silently ignores.
//   - compose's "options" (compose.go), checked the same way.
//
// The entries live in one file per family (designcatalog_*.go). Each option names a
// JSON field of designNode (a node) or designFrame (a cluster frame); the field's JSON
// type is read from the struct here rather than written in the table, so the two
// cannot disagree. TestDesignCatalog holds the table to the structs, the designer's
// type list and the route table.

// catalogOption is one deploy option: a field of the node or frame.
type catalogOption struct {
	Field string `json:"field"`
	// Type is the JSON shape, filled in from the struct: string, boolean, integer,
	// number, array or object.
	Type string `json:"type"`
	// Values lists the accepted values when the field takes a fixed set.
	Values []string `json:"values,omitempty"`
	// Default is what the designer gives a new node or frame of this type; nil when
	// the zero value is the default.
	Default any    `json:"default,omitempty"`
	Desc    string `json:"description"`
	// When says what the option depends on, e.g. "usePgBackRest is on", or "".
	When string `json:"when,omitempty"`
}

// catalogEdge is an association line a type draws on the canvas. The provisioners
// resolve these by walking the edge graph (a proxy's backend, a simulator's target),
// so they are part of the design, not decoration.
type catalogEdge struct {
	// To lists the node or frame types at the other end.
	To []string `json:"to"`
	// Type is the edge's type field: "directional", "async" or "bidir".
	Type string `json:"type"`
	Desc string `json:"description"`
	// Required means the type cannot deploy without one.
	Required bool `json:"required,omitempty"`
}

// catalogAction is a post-deployment action: an endpoint that applies to a deployed
// node or frame of this type.
type catalogAction struct {
	Method string `json:"method"`
	// Path is the route pattern exactly as the route table declares it.
	Path string `json:"path"`
	Desc string `json:"description"`
	// Body lists the request body's fields, "" when it takes none.
	Body string `json:"body,omitempty"`
	When string `json:"when,omitempty"`
}

// catalogEntry describes one node type or one cluster frame type.
type catalogEntry struct {
	Type    string `json:"type"`
	Label   string `json:"label"`
	Summary string `json:"summary"`
	// Placement is "standalone" (a node on its own), "member" (a node that lives in a
	// frame of FrameTypes), "standalone,member", or "frame" (a cluster frame).
	Placement string `json:"placement"`
	// FrameTypes are the frames a member node can live in.
	FrameTypes []string `json:"frameTypes,omitempty"`
	Singleton  bool     `json:"singleton,omitempty"`
	// For a frame: the members' node type and how many it takes.
	MemberType     string `json:"memberType,omitempty"`
	DefaultMembers int    `json:"defaultMembers,omitempty"`
	MinMembers     int    `json:"minMembers,omitempty"`
	MaxMembers     int    `json:"maxMembers,omitempty"`
	// Requires lists what must exist in the stack first, in prose.
	Requires []string        `json:"requires,omitempty"`
	Options  []catalogOption `json:"options"`
	// Relations names entries of catalogRelations this type can hold.
	Relations []string        `json:"relations,omitempty"`
	Edges     []catalogEdge   `json:"edges,omitempty"`
	Actions   []catalogAction `json:"actions,omitempty"`
}

// isFrame reports whether the entry is a cluster frame type.
func (e catalogEntry) isFrame() bool { return e.Placement == "frame" }

// catalogRelation is a reference from a node or frame to another node, held in one of
// the design's …NodeId fields. Setting one through the API also sets its Enable flags,
// which is what the designer's picker does.
type catalogRelation struct {
	Name  string `json:"name"`
	Field string `json:"field"`
	// Enable lists boolean fields that must be on for the reference to be used.
	Enable []string `json:"enable,omitempty"`
	// Targets are the node types it may point at.
	Targets []string `json:"targets"`
	Desc    string   `json:"description"`
}

// catalogRelations are the references a design can hold. A type lists the ones it
// offers by name in its entry.
var catalogRelations = []catalogRelation{
	{Name: "monitor", Field: "pmmNodeId", Targets: []string{"pmm"},
		Desc: "A PMM client on every database node sends metrics and query analytics to the PMM server."},
	{Name: "orchestrator", Field: "orchestratorNodeId", Targets: []string{"orchestrator"},
		Desc: "Orchestrator discovers the replication topology and handles failover."},
	{Name: "repository", Field: "repositoryNodeId", Targets: []string{"repository"},
		Desc: "Packages (or, on a K3D frame, images and charts) come from the Repository mirror instead of upstream."},
	{Name: "vault", Field: "openbaoNodeId", Enable: []string{"enableVault"}, Targets: []string{"openbao"},
		Desc: "Data-at-rest encryption keys are kept in OpenBao."},
	{Name: "backup", Field: "seaweedfsNodeId", Targets: []string{"seaweedfs"},
		Desc: "Backups (and WAL, binary log or oplog archives where enabled) go to a SeaweedFS S3 bucket; the backup tool's own switch (usePgBackRest, useBarman, enablePBM, useXtraBackup) turns them on."},
	{Name: "storage", Field: "cephNodeId", Targets: []string{"ceph"},
		Desc: "Kubernetes volumes are RBD images on a Ceph node, through Ceph CSI (k3dStorage \"ceph\")."},
	{Name: "oidc", Field: "keycloakNodeId", Enable: []string{"enableOIDC"}, Targets: []string{"keycloak"},
		Desc: "Single sign-on with OIDC tokens issued by Keycloak."},
	{Name: "ldap", Field: "ldapDirNodeId", Enable: []string{"ldapAuth"}, Targets: []string{"intranet", "sambaad"},
		Desc: "Users authenticate against the Intranet's OpenLDAP or a Samba AD DC directory."},
	{Name: "kerberos", Field: "ldapDirNodeId", Enable: []string{"ldapAuth", "kerberosAuth"}, Targets: []string{"sambaad"},
		Desc: "GSSAPI (Kerberos) sign-in against a Samba AD DC, on top of its LDAP binding."},
	{Name: "upgrade", Field: "watchtowerNodeId", Targets: []string{"watchtower"},
		Desc: "Watchtower lets the node upgrade itself from inside its own UI."},
	{Name: "app", Field: "ssAIONode", Targets: []string{"aio"},
		Desc: "The simulator drives its workload against an instance on an All-in-One node (ssMode \"aio\", ssAIOInstance names the instance)."},
}

// catalogCommonActions apply to every deployed node, whatever its type, and are not
// repeated in each entry's Actions.
var catalogCommonActions = []catalogAction{
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}", Desc: "The node's deployment: state, container, published ports and connection details."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/start", Desc: "Start the node's container."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/stop", Desc: "Stop the node's container."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/restart", Desc: "Restart the node's container."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/rebuild", Desc: "What a rebuild would recreate."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/rebuild", Desc: "Recreate the node from the current design."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/errorlog", Desc: "The tail of the node's main log."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/sshforward", Desc: "The SSH port-forward command for the node's ports, when SSH forwarding is configured."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/term", Desc: "A shell in the node (WebSocket)."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/upload", Desc: "Copy a file into the node (multipart)."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/fs/list", Desc: "Browse the node's file system; the rest of the file manager is under …/fs/."},
}

func catalogRelationByName(name string) (catalogRelation, bool) {
	for _, r := range catalogRelations {
		if r.Name == name {
			return r, true
		}
	}
	return catalogRelation{}, false
}

// catalogFamilies are the per-family tables, in the order the catalogue lists them.
func catalogFamilies() [][]catalogEntry {
	return [][]catalogEntry{
		catalogInfra, catalogMySQLFamily, catalogPostgresFamily, catalogMongoFamily, catalogK3D, catalogApps,
	}
}

// designCatalogIndex is the catalogue with option types filled in, built once.
type designCatalogIndex struct {
	Nodes  []catalogEntry
	Frames []catalogEntry
	byNode map[string]*catalogEntry
	byFrm  map[string]*catalogEntry
}

var (
	designCatalogOnce sync.Once
	designCatalogIdx  designCatalogIndex
)

func designCatalog() *designCatalogIndex {
	designCatalogOnce.Do(func() {
		idx := designCatalogIndex{byNode: map[string]*catalogEntry{}, byFrm: map[string]*catalogEntry{}}
		nodeTypes, frameTypes := jsonFieldTypes(reflect.TypeOf(designNode{})), jsonFieldTypes(reflect.TypeOf(designFrame{}))
		for _, fam := range catalogFamilies() {
			for _, e := range fam {
				types := nodeTypes
				if e.isFrame() {
					types = frameTypes
				}
				e.Options = append([]catalogOption(nil), e.Options...)
				for i := range e.Options {
					e.Options[i].Type = types[e.Options[i].Field]
				}
				if e.isFrame() {
					idx.Frames = append(idx.Frames, e)
				} else {
					idx.Nodes = append(idx.Nodes, e)
				}
			}
		}
		for i := range idx.Nodes {
			idx.byNode[idx.Nodes[i].Type] = &idx.Nodes[i]
		}
		for i := range idx.Frames {
			idx.byFrm[idx.Frames[i].Type] = &idx.Frames[i]
		}
		designCatalogIdx = idx
	})
	return &designCatalogIdx
}

func (c *designCatalogIndex) node(t string) (*catalogEntry, bool) {
	e, ok := c.byNode[t]
	return e, ok
}

func (c *designCatalogIndex) frame(t string) (*catalogEntry, bool) {
	e, ok := c.byFrm[t]
	return e, ok
}

// option finds a catalogued option of an entry.
func (e *catalogEntry) option(field string) (catalogOption, bool) {
	for _, o := range e.Options {
		if o.Field == field {
			return o, true
		}
	}
	return catalogOption{}, false
}

// offers reports whether the entry lists a relation.
func (e *catalogEntry) offers(relation string) bool {
	for _, r := range e.Relations {
		if r == relation {
			return true
		}
	}
	return false
}

// optionNames lists an entry's option fields, sorted, for error messages.
func (e *catalogEntry) optionNames() []string {
	out := make([]string, 0, len(e.Options))
	for _, o := range e.Options {
		out = append(out, o.Field)
	}
	sort.Strings(out)
	return out
}

// jsonFieldTypes maps a struct's JSON field names to their JSON shapes, following
// embedded structs.
func jsonFieldTypes(t reflect.Type) map[string]string {
	out := map[string]string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			for k, v := range jsonFieldTypes(f.Type) {
				out[k] = v
			}
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out[name] = jsonShape(f.Type)
	}
	return out
}

func jsonShape(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	}
	return "object"
}

// ------------------------------------------------------------- handlers

// handleDesignCatalog serves the whole catalogue, or one type's entries with ?type=.
func (a *App) handleDesignCatalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	c := designCatalog()
	if t := strings.TrimSpace(r.URL.Query().Get("type")); t != "" {
		out := map[string]any{}
		if e, ok := c.node(t); ok {
			out["node"] = e
		}
		if e, ok := c.frame(t); ok {
			out["frame"] = e
		}
		if len(out) == 0 {
			writeErr(w, http.StatusNotFound, "no node or cluster type "+t+" — GET /api/stacks/design/catalog lists them")
			return
		}
		out["relations"] = catalogRelations
		out["commonActions"] = catalogCommonActions
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": c.Nodes, "frames": c.Frames, "relations": catalogRelations, "edgeTypes": []string{"directional", "async", "bidir"},
		"commonActions": catalogCommonActions,
	})
}
