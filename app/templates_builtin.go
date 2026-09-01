package main

import "encoding/json"

// The built-in deployment templates — the defaults every installation ships with,
// so the template picker is useful before anyone has saved one of their own.
//
// They are Go literals rather than seeded database rows for the same reason lab
// designs are: a row would have to be migrated, could be edited into something
// that no longer deploys, and would go stale against a schema that keeps growing.
// A literal is re-read from the binary on every start and covered by tests.
//
// Two rules every template here follows, both learned from the lab designs:
//
//   - No "arch". An installation builds images for one DOCKER_PLATFORM and the
//     server resolves it (archOr); a pinned amd64 is an image an arm64 install
//     never built.
//   - No pinned minor version. Majors ("8.0", "16") are the supported series and
//     are always in the catalog; a minor comes from whatever `make versions`
//     probed on this host, and pinning one ships a template that fails to deploy
//     on an installation that probed a different set. Every *Version field is
//     left "" — the catalog default.
//
// Each also carries the Intranet node, which every stack requires (it is the
// stack's DNS authority — see dns.go).

// builtinTemplateDefs pairs the metadata with its design. The order here is the
// order the picker shows them in: the starter first, then by engine family.
var builtinTemplateDefs = []struct {
	Slug        string
	Name        string
	Description string
	Category    string
	Design      json.RawMessage
}{
	{
		Slug:        "starter-percona-server",
		Name:        "Starter — Percona Server",
		Description: "One standalone Percona Server plus a desktop to reach it from. The smallest stack that is still worth deploying, and the place to start if you have not deployed one before.",
		Category:    "Getting started",
		Design:      tplStarterPSDesign,
	},
	{
		Slug:        "pxc-proxysql-pmm",
		Name:        "PXC + ProxySQL + PMM",
		Description: "A 3-node Percona XtraDB Cluster behind a ProxySQL router, with a PMM server monitoring both. The reference MySQL high-availability stack.",
		Category:    "MySQL",
		Design:      tplPXCProxySQLPMMDesign,
	},
	{
		Slug:        "ps-replication-orchestrator",
		Name:        "Percona Server replication + Orchestrator",
		Description: "Asynchronous replication with GTID — one primary, two replicas — with a Percona Orchestrator node discovering the topology and detecting failures.",
		Category:    "MySQL",
		Design:      tplPSReplOrchestratorDesign,
	},
	{
		Slug:        "innodb-cluster",
		Name:        "InnoDB Cluster",
		Description: "Three Percona Server members in an InnoDB Cluster, each running MySQL Router. Group Replication underneath, with the router handling failover for clients.",
		Category:    "MySQL",
		Design:      tplInnoDBClusterDesign,
	},
	{
		Slug:        "patroni-haproxy",
		Name:        "Patroni + HAProxy",
		Description: "A 3-node Patroni PostgreSQL cluster — the minimum for etcd quorum — fronted by HAProxy, which follows the leader through any switchover on its own health checks.",
		Category:    "PostgreSQL",
		Design:      tplPatroniHAProxyDesign,
	},
	{
		Slug:        "pg-pgbackrest",
		Name:        "PostgreSQL + pgBackRest",
		Description: "A standalone PostgreSQL node backing up to S3 object storage with pgBackRest, against a SeaweedFS node with TLS on (pgBackRest's S3 client requires HTTPS).",
		Category:    "PostgreSQL",
		Design:      tplPGBackRestDesign,
	},
	{
		Slug:        "psmdb-replica-set-pbm",
		Name:        "PSMDB replica set + PBM",
		Description: "A 3-member Percona Server for MongoDB replica set with Percona Backup for MongoDB wired to SeaweedFS S3 storage, configured at deploy.",
		Category:    "MongoDB",
		Design:      tplPSMRSPBMDesign,
	},
	{
		Slug:        "psmdb-sharded",
		Name:        "PSMDB sharded cluster",
		Description: "A sharded Percona Server for MongoDB cluster in its minimum shape — one mongos, one config server, three single-member shards. Five nodes instead of the standard thirteen.",
		Category:    "MongoDB",
		Design:      tplPSMDBShardedDesign,
	},
	{
		Slug:        "valkey-cluster",
		Name:        "Valkey Cluster",
		Description: "A 3-node Valkey Cluster — the minimum for gossip-based quorum — with slots distributed across the members.",
		Category:    "Valkey",
		Design:      tplValkeyClusterDesign,
	},
	{
		Slug:        "k3d-pxc-operator",
		Name:        "Kubernetes — Percona Operator for MySQL (PXC)",
		Description: "A single-node k3s cluster running the Percona Operator for MySQL (PXC), with HAProxy fronting the cluster and a LoadBalancer address on the host.",
		Category:    "Kubernetes",
		Design:      tplK3DPXCOperatorDesign,
	},
	{
		Slug:        "aio-playground",
		Name:        "All-in-One playground",
		Description: "One container running a Percona Server, a PostgreSQL, a PSMDB and a Valkey instance side by side. The cheapest way to have four engines up at once.",
		Category:    "All in One",
		Design:      tplAIOPlaygroundDesign,
	},
}

// builtinTemplates materializes the definitions as StackTemplates. Rebuilt per
// call so a caller that mutates one (templateSummary, clearing Design for the
// list response) cannot affect the next.
func builtinTemplates() []StackTemplate {
	out := make([]StackTemplate, 0, len(builtinTemplateDefs))
	for _, d := range builtinTemplateDefs {
		out = append(out, StackTemplate{
			ID:          builtinPrefix + d.Slug,
			Name:        d.Name,
			Description: d.Description,
			Category:    d.Category,
			Builtin:     true,
			Design:      d.Design,
		})
	}
	return out
}

// findBuiltinTemplate resolves a "builtin:<slug>" id.
func findBuiltinTemplate(id string) (StackTemplate, bool) {
	for _, t := range builtinTemplates() {
		if t.ID == id {
			return t, true
		}
	}
	return StackTemplate{}, false
}

// --- the designs ---

var tplStarterPSDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-ps","type":"ps","label":"ps-01","os":"oraclelinux","osVersion":"9","psMajor":"8.0","psVersion":"","gtid":true,"exportEnabled":false,"exportHostPort":0,"x":300,"y":40},
    {"id":"tpl-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","x":40,"y":220}
  ],
  "frames": [],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)

var tplPXCProxySQLPMMDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-pmm","type":"pmm","label":"pmm-01","version":"","generateCert":false,"x":40,"y":220},
    {"id":"tpl-proxysql","type":"proxysql","label":"proxysql-01","os":"oraclelinux","osVersion":"9","proxysqlMajor":"2","proxysqlVersion":"","mode":"singlewrite","pmmNodeId":"tpl-pmm","exportEnabled":false,"exportHostPort":0,"x":300,"y":40},
    {"id":"tpl-pxc-1","type":"pxc","label":"pxc-1","role":"regular","frameId":"tpl-pxc","exportEnabled":false,"exportHostPort":0,"x":574,"y":66},
    {"id":"tpl-pxc-2","type":"pxc","label":"pxc-2","role":"regular","frameId":"tpl-pxc","exportEnabled":false,"exportHostPort":0,"x":702,"y":66},
    {"id":"tpl-pxc-3","type":"pxc","label":"pxc-3","role":"regular","frameId":"tpl-pxc","exportEnabled":false,"exportHostPort":0,"x":830,"y":66}
  ],
  "frames": [
    {"id":"tpl-pxc","type":"pxc","label":"pxc-cluster-01","os":"oraclelinux","osVersion":"9","pxcMajor":"8.0","pxcVersion":"","gtid":true,"pmmNodeId":"tpl-pmm","rootPassword":"","useProxy":false,"generateCert":false,"certTtlValue":365,"certTtlUnit":"days","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"tpl-edge-proxysql","from":{"node":"tpl-proxysql","port":"right"},"to":{"node":"tpl-pxc","port":"left"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

var tplPSReplOrchestratorDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-orch","type":"orchestrator","label":"orchestrator-01","os":"oraclelinux","osVersion":"9","orchestratorVersion":"","exportEnabled":false,"exportHostPort":0,"x":300,"y":40},
    {"id":"tpl-mysql-1","type":"mysql","label":"mysql-1","role":"primary","frameId":"tpl-mysql-repl","exportEnabled":false,"exportHostPort":0,"x":574,"y":66},
    {"id":"tpl-mysql-2","type":"mysql","label":"mysql-2","role":"secondary","frameId":"tpl-mysql-repl","exportEnabled":false,"exportHostPort":0,"x":702,"y":66},
    {"id":"tpl-mysql-3","type":"mysql","label":"mysql-3","role":"secondary","frameId":"tpl-mysql-repl","exportEnabled":false,"exportHostPort":0,"x":830,"y":66}
  ],
  "frames": [
    {"id":"tpl-mysql-repl","type":"mysql","label":"mysql-repl-01","os":"oraclelinux","osVersion":"9","psMajor":"8.0","psVersion":"","gtid":true,"replMode":"async","orchestratorNodeId":"tpl-orch","rootPassword":"","pmmNodeId":"","useProxy":false,"generateCert":false,"certTtlValue":365,"certTtlUnit":"days","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)

var tplInnoDBClusterDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-innodb-1","type":"innodb","label":"innodb-1","frameId":"tpl-innodb","exportEnabled":false,"exportHostPort":0,"x":574,"y":66},
    {"id":"tpl-innodb-2","type":"innodb","label":"innodb-2","frameId":"tpl-innodb","exportEnabled":false,"exportHostPort":0,"x":702,"y":66},
    {"id":"tpl-innodb-3","type":"innodb","label":"innodb-3","frameId":"tpl-innodb","exportEnabled":false,"exportHostPort":0,"x":830,"y":66}
  ],
  "frames": [
    {"id":"tpl-innodb","type":"innodb","label":"innodb-cluster-01","os":"oraclelinux","osVersion":"9","pdpsRepo":"","replMode":"innodbcluster","mysqlRouter":true,"rootPassword":"","pmmNodeId":"","useProxy":false,"generateCert":false,"certTtlValue":365,"certTtlUnit":"days","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)

var tplPatroniHAProxyDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-haproxy","type":"haproxy","label":"haproxy-01","os":"oraclelinux","osVersion":"9","x":300,"y":40},
    {"id":"tpl-pg-1","type":"patroni","label":"patroni-1","frameId":"tpl-patroni","exportEnabled":false,"exportHostPort":0,"x":574,"y":66},
    {"id":"tpl-pg-2","type":"patroni","label":"patroni-2","frameId":"tpl-patroni","exportEnabled":false,"exportHostPort":0,"x":702,"y":66},
    {"id":"tpl-pg-3","type":"patroni","label":"patroni-3","frameId":"tpl-patroni","exportEnabled":false,"exportHostPort":0,"x":830,"y":66}
  ],
  "frames": [
    {"id":"tpl-patroni","type":"patroni","label":"patroni-cluster-01","os":"oraclelinux","osVersion":"9","pgMajor":"16","pgVersion":"","rootPassword":"","pmmNodeId":"","useProxy":false,"generateCert":false,"certTtlValue":365,"certTtlUnit":"days","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"tpl-edge-haproxy","from":{"node":"tpl-haproxy","port":"right"},"to":{"node":"tpl-patroni","port":"left"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// pgBackRest talks S3 over HTTPS only, so the SeaweedFS node must have TLS on and
// at least one bucket named up front — SeaweedFS nodes get no default bucket.
// See pgBackRestSeaweedIssues in pg.go, which refuses to deploy without both.
var tplPGBackRestDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-pg","type":"pg","label":"pg-01","os":"oraclelinux","osVersion":"9","pgMajor":"16","pgVersion":"","usePgBackRest":true,"seaweedfsNodeId":"tpl-seaweed","seaweedfsBucket":"backups","exportEnabled":false,"exportHostPort":0,"x":300,"y":40},
    {"id":"tpl-seaweed","type":"seaweedfs","label":"seaweedfs-01","bucket":"backups","buckets":["backups"],"tls":true,"generateCert":true,"certTtlValue":365,"certTtlUnit":"days","x":300,"y":220}
  ],
  "frames": [],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)

var tplPSMRSPBMDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-rs-1","type":"psmrs","label":"rs-1","frameId":"tpl-psmrs","exportEnabled":false,"exportHostPort":0,"x":574,"y":66},
    {"id":"tpl-rs-2","type":"psmrs","label":"rs-2","frameId":"tpl-psmrs","exportEnabled":false,"exportHostPort":0,"x":702,"y":66},
    {"id":"tpl-rs-3","type":"psmrs","label":"rs-3","frameId":"tpl-psmrs","exportEnabled":false,"exportHostPort":0,"x":830,"y":66},
    {"id":"tpl-seaweed","type":"seaweedfs","label":"seaweedfs-01","bucket":"backups","buckets":["backups"],"tls":false,"x":700,"y":220}
  ],
  "frames": [
    {"id":"tpl-psmrs","type":"psmrs","label":"psmrs-01","os":"oraclelinux","osVersion":"9","psmdbMajor":"8.0","psmdbVersion":"","rootPassword":"","pmmNodeId":"","useProxy":false,"enablePBM":true,"seaweedfsNodeId":"tpl-seaweed","seaweedfsBucket":"backups","generateCert":false,"certTtlValue":365,"certTtlUnit":"days","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)

var tplPSMDBShardedDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-mongos","type":"psmdb","label":"mongos","frameId":"tpl-psmdb","role":"mongos","exportEnabled":false,"exportHostPort":0,"x":560,"y":20},
    {"id":"tpl-cfg1","type":"psmdb","label":"cfg1","frameId":"tpl-psmdb","role":"config","exportEnabled":false,"exportHostPort":0,"x":680,"y":20},
    {"id":"tpl-s0r1","type":"psmdb","label":"s0r1","frameId":"tpl-psmdb","role":"shard","shard":0,"exportEnabled":false,"exportHostPort":0,"x":560,"y":110},
    {"id":"tpl-s1r1","type":"psmdb","label":"s1r1","frameId":"tpl-psmdb","role":"shard","shard":1,"exportEnabled":false,"exportHostPort":0,"x":680,"y":110},
    {"id":"tpl-s2r1","type":"psmdb","label":"s2r1","frameId":"tpl-psmdb","role":"shard","shard":2,"exportEnabled":false,"exportHostPort":0,"x":800,"y":110}
  ],
  "frames": [
    {"id":"tpl-psmdb","type":"psmdb","label":"psmdb-sharded-01","os":"oraclelinux","osVersion":"9","psmdbMajor":"8.0","psmdbVersion":"","psmdbSetup":"minimum","rootPassword":"","pmmNodeId":"","useProxy":false,"enablePBM":false,"seaweedfsNodeId":"","generateCert":false,"certTtlValue":365,"certTtlUnit":"days","x":540,"y":0,"w":300,"h":150}
  ],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)

var tplValkeyClusterDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-vk-1","type":"valkeycluster","label":"valkey-1","frameId":"tpl-valkey","exportEnabled":false,"exportHostPort":0,"x":574,"y":66},
    {"id":"tpl-vk-2","type":"valkeycluster","label":"valkey-2","frameId":"tpl-valkey","exportEnabled":false,"exportHostPort":0,"x":702,"y":66},
    {"id":"tpl-vk-3","type":"valkeycluster","label":"valkey-3","frameId":"tpl-valkey","exportEnabled":false,"exportHostPort":0,"x":830,"y":66}
  ],
  "frames": [
    {"id":"tpl-valkey","type":"valkeycluster","label":"valkey-cluster-01","os":"oraclelinux","osVersion":"9","valkeyMajor":"9.1","valkeyVersion":"","rootPassword":"","pmmNodeId":"","useProxy":false,"x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)

// A single k3s node is enough for an operator — k3dNodes is 1 by default and the
// operator's own CR asks for the database members. k3dOperatorVer is left "" so
// the catalog's current operator version is used.
var tplK3DPXCOperatorDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-k3s-1","type":"k3d","label":"k3s-1","frameId":"tpl-k3d","x":574,"y":66}
  ],
  "frames": [
    {"id":"tpl-k3d","type":"k3d","label":"k3d-01","k3dNodes":1,"k3dCpus":4,"k3dMemoryGb":8,"k3dK3sVersion":"",
     "k3dOperator":"pxc","k3dOperatorVer":"","k3dNamespace":"default",
     "k3dProxy":"haproxy","k3dExposePxc":"clusterip","k3dExposeHaproxy":"loadbalancer","k3dExposeProxysql":"loadbalancer",
     "k3dSharding":false,"k3dExposeReplset":"clusterip","k3dExposeMongos":"loadbalancer",
     "k3dExposePg":"clusterip","k3dExposePgbouncer":"loadbalancer",
     "k3dPgoInstances":2,"k3dPgoStorageGb":1,"k3dPgoVersion":"",
     "k3dClusterType":"group-replication","k3dExposeMysql":"clusterip","k3dExposeRouter":"loadbalancer",
     "k3dPmmTokenTtlValue":365,"k3dPmmTokenTtlUnit":"days",
     "k3dDebug":false,"k3dDebugPort":40000,"k3dDebugNoPublish":false,
     "pmmNodeId":"","seaweedfsNodeId":"","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)

// Four engines in one container. Instance ids are node-local (aio.go), so they
// need no "tpl-" prefixing the way node ids do.
var tplAIOPlaygroundDesign = json.RawMessage(`{
  "nodes": [
    {"id":"tpl-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"tpl-aio","type":"aio","label":"aio-01","os":"oraclelinux","osVersion":"9","useProxy":false,
     "aioPsMajor":"8.0","aioPsVersion":"","aioPxcMajor":"8.0","aioPxcVersion":"",
     "aioPsmdbMajor":"8.0","aioValkeyMajor":"9.1","aioProxysqlMajor":"2",
     "aioInstances":[
       {"id":"inst-ps","kind":"ps","name":"ps01","members":1,"rootPassword":"","exportEnabled":false,"exportHostPort":0,"certTtlValue":365,"certTtlUnit":"days"},
       {"id":"inst-pg","kind":"pg","name":"pg01","members":1,"pgMajor":"16","pgVersion":"","rootPassword":"","exportEnabled":false,"exportHostPort":0,"certTtlValue":365,"certTtlUnit":"days"},
       {"id":"inst-psmdb","kind":"psmdb","name":"psmdb01","members":1,"rootPassword":"","exportEnabled":false,"exportHostPort":0,"certTtlValue":365,"certTtlUnit":"days"},
       {"id":"inst-valkey","kind":"valkey","name":"valkey01","members":1,"rootPassword":"","exportEnabled":false,"exportHostPort":0,"certTtlValue":365,"certTtlUnit":"days"}
     ],"x":300,"y":40}
  ],
  "frames": [],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)
