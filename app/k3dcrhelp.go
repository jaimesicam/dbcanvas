package main

import "strings"

// k3dcrhelp.go — what the fields in the custom resource editor mean.
//
// This exists because the Percona CRDs carry no `description` on their properties. `kubectl
// explain pxc.spec.pxc.size` answers with the type and nothing else, and a generated form with
// no prose is a wall of names — the one thing worse than no form. So the fields worth
// understanding get a sentence here, and the editor prefers the CRD's own description whenever a
// release starts shipping them (crFormBuild only falls back to this).
//
// Two lookups, in order: the exact dotted path, then the leaf name. The leaf table is what
// covers `image`, `resources`, `enabled` and the rest of the vocabulary the CRD repeats
// identically in every section — writing those out per section would be twelve copies of one
// sentence, each able to drift.
//
// Kept short. This is a hint under a field, not documentation: it should say what the field does
// and, where it is not obvious, what happens if you change it on a running cluster.

// crFormHelpFor is the description the editor shows under a field.
func crFormHelpFor(path, name string) string {
	if h := crFormHelp[path]; h != "" {
		return h
	}
	// A path inside an array element or a map entry ("backup.storages.*.type") answers to the
	// same help as the shape it is an element of.
	if h := crFormHelp[crFormHelpGeneric(path)]; h != "" {
		return h
	}
	return crFormLeafHelp[name]
}

// crFormHelpGeneric strips the element markers so "backup.schedule[].storageName" can be looked
// up as itself — the tables below are written with those markers, so this is identity today and
// the seam where a positional path (".0.") would be normalised if one ever arrives.
func crFormHelpGeneric(path string) string {
	return strings.ReplaceAll(path, ".*.", ".*.")
}

// crFormLeafHelp is the vocabulary every section repeats.
var crFormLeafHelp = map[string]string{
	"enabled":             "Whether the operator runs this component at all. Turning it off removes its pods.",
	"size":                "How many pods the operator keeps. Scaling down a database tier removes members from the cluster — the operator will refuse a size that breaks quorum unless the matching unsafe flag is set.",
	"image":               "The exact container image. Changing it is how a deliberate version skew is set up — the operator restarts the pods one at a time into the new image.",
	"imagePullPolicy":     "When the kubelet re-pulls the image: Always, IfNotPresent (the default), or Never.",
	"configuration":       "The component's own config file, inline. For a database this is my.cnf / mongod.conf content; the operator writes it into a ConfigMap and restarts the pods.",
	"resources":           "CPU and memory requests and limits. DBCanvas comments these out of the shipped cr.yaml so the pods fit a laptop-sized cluster — setting them here is how you put a limit back to watch what it does.",
	"gracePeriod":         "Seconds Kubernetes waits for a pod to stop cleanly before killing it.",
	"priorityClassName":   "The PriorityClass for these pods — which ones get evicted first when the node runs out.",
	"runtimeClassName":    "A non-default container runtime for these pods (gVisor, Kata).",
	"schedulerName":       "A non-default Kubernetes scheduler for these pods.",
	"serviceAccountName":  "The ServiceAccount the pods run as.",
	"envVarsSecret":       "A Secret whose keys become environment variables in the container.",
	"hookScript":          "A script the operator runs inside the pod at defined points in its lifecycle.",
	"volumeSpec":          "The storage for these pods: the PersistentVolumeClaim's size and class. Growing it works only where the storage class allows expansion.",
	"expose":              "The Service the operator creates for this tier. ClusterIP is reachable only inside Kubernetes; LoadBalancer takes an address from the stack's MetalLB pool, which is what makes it reachable from other nodes on the canvas.",
	"livenessProbes":      "When Kubernetes decides this container is stuck and restarts it.",
	"readinessProbes":     "When Kubernetes decides this pod may take traffic. A readiness probe that is too tight takes a busy database out of the Service.",
	"storageClassName":    "Which StorageClass to claim from. Empty means the cluster default (local-path on k3s).",
	"podDisruptionBudget": "How many of these pods may be unavailable at once during a voluntary disruption — a node drain, or the operator's own rolling update.",
	"sslSecretName":       "The Secret holding this cluster's TLS certificate.",
	"annotations":         "Annotations added to the pods.",
	"labels":              "Labels added to the pods.",
}

// crFormHelp is the per-field table, by dotted path from `spec`.
var crFormHelp = map[string]string{
	// --- the top of cr.yaml
	"crVersion":                             "The operator API version this object is written for. It is set from the operator that created it; changing it by hand is a migration, not a setting.",
	"pause":                                 "Stop the cluster without deleting it: the operator scales every workload to zero and keeps the volumes. Unpausing brings it back with its data.",
	"secretsName":                           "The Secret holding the system users' passwords (root, monitor, xtrabackup, …).",
	"vaultSecretName":                       "The Secret with the HashiCorp Vault / OpenBao configuration for data-at-rest encryption.",
	"allowUnsafeConfigurations":             "Deprecated in favour of unsafeFlags. Let the operator create a cluster that cannot survive a node loss — one database pod, no TLS, a proxy with a single replica.",
	"unsafeFlags":                           "Permission to run a cluster that would normally be refused: fewer database pods than a quorum needs, a single-replica proxy, TLS off. This is what makes a one-node lab cluster legal.",
	"unsafeFlags.pxcSize":                   "Allow fewer than three PXC pods. A one- or two-node Galera cluster cannot form a quorum on its own and is a lab arrangement only.",
	"unsafeFlags.proxySize":                 "Allow a single proxy pod, so a failure of it is a failure of the whole front door.",
	"unsafeFlags.tls":                       "Allow the cluster to run without TLS between its members.",
	"unsafeFlags.backupIfUnhealthy":         "Allow a backup to start while the cluster is not healthy.",
	"updateStrategy":                        "How the operator rolls out a change: SmartUpdate (it picks the order, replicas before the primary), RollingUpdate (Kubernetes does it), or OnDelete (nothing moves until you delete a pod).",
	"upgradeOptions":                        "Whether the operator upgrades the database image by itself, from Percona's version service.",
	"upgradeOptions.apply":                  "\"disabled\" leaves the image alone. A version like \"8.0-recommended\", or \"latest\", lets the operator upgrade the cluster on the schedule below — which is worth watching once, and rarely what you want in a lab you are measuring.",
	"upgradeOptions.schedule":               "Cron expression for when the version check runs.",
	"upgradeOptions.versionServiceEndpoint": "The version service asked what to upgrade to.",
	"tls":                                   "The cluster's certificates: who issues them and how long they last.",
	"tls.enabled":                           "Turn TLS off for the whole cluster. The pods then talk in plaintext, which is what makes a packet capture readable — and is why this exists in a lab.",
	"tls.SANs":                              "Extra subject alternative names on the generated certificate, for reaching the cluster by a name it does not know about.",
	"tls.certValidityDuration":              "How long an issued certificate is valid — set it short to watch the operator rotate it.",
	"pmm":                                   "The PMM client sidecar in every pod.",
	"pmm.enabled":                           "Run the pmm-client sidecar. It needs a serverHost and a token in the cluster's Secret.",
	"pmm.serverHost":                        "The PMM server the sidecars report to.",
	"pmm.pxcParams":                         "Extra flags passed to pmm-admin when it registers the database — this is where a query-source or a slow-log setting goes.",
	"pmm.customClusterName":                 "The cluster name PMM groups these services under.",
	"logcollector.enabled":                  "Run the Fluent Bit sidecar that ships each pod's logs.",
	"logcollector.logRotate":                "How the collector rotates what it has shipped.",

	// --- the database tier
	"pxc":                     "The Percona XtraDB Cluster pods — the database itself.",
	"pxc.size":                "How many Galera members. Three is a quorum; one or two need unsafeFlags.pxcSize, and are a lab arrangement.",
	"pxc.autoRecovery":        "Let the operator recover a cluster that lost quorum by bootstrapping from the most advanced member. Turning it off is how you get to do that by hand.",
	"pxc.configuration":       "my.cnf for every member, inline. The operator writes it to a ConfigMap and restarts the pods — this is the fastest way to change a server variable that needs a restart.",
	"pxc.replicationChannels": "Cross-cluster replication. A channel with isSource true makes this cluster a source; one with a sourcesList makes it a replica of those addresses. DBCanvas writes these from the canvas's replication links.",
	"pxc.expose":              "Per-pod Services for the database. LoadBalancer is what a replica in another Kubernetes cluster dials — a ClusterIP resolves inside this cluster only.",
	"pxc.sstRetryCount":       "How many times a joining member retries state transfer before giving up.",
	"pxc.mysqlAllocator":      "The memory allocator mysqld links against, jemalloc or tcmalloc.",
	"pxc.livenessDelaySec":    "Seconds before the liveness probe starts. Too short and a slow-starting member is killed while it is recovering.",
	"pxc.readinessDelaySec":   "Seconds before the readiness probe starts.",
	"pxc.volumeSpec":          "The data volume for each member.",

	// --- proxies
	"haproxy":                "HAProxy in front of the cluster: a primary Service that follows whichever member takes writes, and a replicas Service for reads.",
	"haproxy.size":           "How many HAProxy pods. One needs unsafeFlags.proxySize.",
	"haproxy.exposePrimary":  "The Service carrying writes. This is the cluster's front door — a LoadBalancer here is what an application outside Kubernetes connects to.",
	"haproxy.exposeReplicas": "The read Service, across the non-primary members.",
	"proxysql":               "ProxySQL as the front end instead of HAProxy. The two are mutually exclusive — the operator runs one.",
	"proxysql.size":          "How many ProxySQL pods.",
	"proxysql.expose":        "The Service in front of ProxySQL.",

	// --- backups
	"backup":                         "Backups: where they are stored, when they run, and whether binary logs are collected for point-in-time recovery.",
	"backup.storages":                "The named storages a backup or the binlog collector can write to. A name here is what schedule[].storageName and pitr.storageName refer to.",
	"backup.storages.*.type":         "s3, azure, or filesystem (a PVC).",
	"backup.storages.*.verifyTLS":    "Verify the object store's certificate. DBCanvas sets this false for its SeaweedFS node, whose certificate the backup image does not trust.",
	"backup.storages.*.s3":           "The bucket, endpoint, region and credentials Secret for an S3 storage.",
	"backup.schedule":                "Scheduled backups: a name, a cron expression, a storage, and how many to keep.",
	"backup.schedule[].retention":    "How many backups to keep, and whether deleting the object also deletes what is in the bucket.",
	"backup.schedule[].storageName":  "Which of the storages above this schedule writes to.",
	"backup.pitr":                    "Point-in-time recovery: the binlog collector Deployment. It uploads binary logs continuously, so a restore can land at any moment between backups rather than only on a backup.",
	"backup.pitr.enabled":            "Run the collector. On a cluster about to be restored — the replica end of a replication link — this must be off: the restore replaces the GTID history the collector is uploading, and a stream that spans both cannot be replayed.",
	"backup.pitr.storageName":        "Which storage the binary logs go to. Give them one of their own: two clusters uploading binlogs into one bucket interleave two streams, and neither replays.",
	"backup.pitr.timeBetweenUploads": "Seconds between uploads — at worst, how much of the most recent history a restore can be missing.",
	"backup.pitr.timeoutSeconds":     "How long an upload may take before the collector gives up on it.",
	"backup.allowParallel":           "Allow more than one backup to run at once.",
	"backup.startingDeadlineSeconds": "How long a scheduled backup may wait to start before it is skipped.",

	// --- the other operators, where a field means something different enough to say so
	"mysql.size":         "How many MySQL pods (Percona Server operator).",
	"mysql.clusterType":  "group-replication, or async replication managed by Orchestrator. Async needs the Orchestrator pods to be enabled as well.",
	"replsets":           "The MongoDB replica sets. Each has its own size, storage and expose settings.",
	"sharding.enabled":   "Run this MongoDB deployment sharded: config servers and mongos routers on top of the shards.",
	"instances":          "The PostgreSQL instance sets — each is a group of pods with its own storage and count.",
	"proxy.pgBouncer":    "The PgBouncer connection pool in front of PostgreSQL.",
	"backups.pgbackrest": "pgBackRest: the repositories this cluster archives WAL to and takes base backups into.",
}
