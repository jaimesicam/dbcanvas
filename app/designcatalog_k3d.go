package main

// designcatalog_k3d.go — the catalogue's entries for the K3D Kubernetes frame and its operators. See designcatalog.go.

// k3dExposeValues are the Service types a Percona or Crunchy cr.yaml `expose` section takes.
var k3dExposeValues = []string{"clusterip", "nodeport", "loadbalancer"}

var catalogK3D = []catalogEntry{
	{Type: "k3d", Label: "k3s node", Summary: "One k3s node of a K3D cluster frame: the first member is the server, the rest are agents. k3d creates the containers; every cluster setting lives on the frame.",
		Placement: "member", FrameTypes: []string{"k3d"},
		Options: []catalogOption{},
		Actions: []catalogAction{
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/k8s/pods", Desc: "Every pod in the cluster, with its namespace and containers, read through this k3s node."},
		},
	},
	{Type: "k3d", Label: "K3D Cluster", Summary: "A k3s cluster created by k3d on the stack network, with MetalLB LoadBalancer addresses from the stack subnet, optionally running one database operator: the Percona operators for PXC, Percona Server, MongoDB and PostgreSQL, CloudNativePG, Crunchy PGO, or OpenEverest.",
		Placement: "frame", MemberType: "k3d", DefaultMembers: 1, MinMembers: 1, MaxMembers: 3,
		Requires: []string{
			"an Intranet node (the designer adds a K3D frame only when one exists; pods resolve the stack domain through it)",
			"the k3d binary on the host",
			"a Ceph node when k3dStorage is \"ceph\"",
			"an OpenBao node for k3dVaultEncryption, k3dPgTde or k3dCertIssuer \"openbao\"",
			"a SeaweedFS node (relation backup) for k3dPitr and for both ends of a replication link",
			"for a replication edge: both frames run k3dOperator \"pxc\"",
		},
		Options: []catalogOption{
			// Cluster
			{Field: "k3dK3sVersion", Desc: "rancher/k3s image tag the nodes run, from GET /api/catalog/k3s; \"\" is the catalog's newest."},
			{Field: "k3dNodes", Default: 1, Desc: "Recorded node count, 1–3. The deploy uses the number of member nodes, which is what decides the cluster's size."},
			{Field: "k3dCpus", Default: 4, Desc: "CPUs for the whole cluster, split evenly across its nodes. Below 4 (8 for a sharded PSMDB or async PS cluster, 6 for OpenEverest) validation warns."},
			{Field: "k3dMemoryGb", Default: 8, Desc: "Memory in GiB for the whole cluster, split evenly across its nodes. Below 6 (12 for a sharded PSMDB or async PS cluster, 10 for OpenEverest) validation warns."},
			{Field: "k3dDiskReadMbps", Desc: "Read rate limit per k3s node container, MB/s; 0 is unlimited."},
			{Field: "k3dDiskWriteMbps", Desc: "Write rate limit per k3s node container, MB/s; 0 is unlimited."},
			{Field: "k3dDevicePath", Desc: "Host block device the disk limits apply to; \"\" detects the device backing Docker's data root.", When: "k3dDiskReadMbps or k3dDiskWriteMbps is set"},

			// Add-ons
			{Field: "k3dCertManager", Desc: "Install cert-manager and wait for it before the operator, so the operator issues its certificates through it instead of self-signing. Adds three pods."},
			{Field: "k3dCertManagerVer", Desc: "cert-manager release, from GET /api/catalog/operators (chart:cert-manager); \"\" is the catalog's latest.", When: "k3dCertManager is on"},
			{Field: "k3dCertIssuer", Values: []string{"", "intranet", "openbao"}, Desc: "CA cert-manager issues the database certificates from: \"\" the operator's own self-signed CA, \"intranet\" a CA Issuer holding the Intranet CA, \"openbao\" a Vault Issuer signing with the Intranet CA through OpenBao's PKI (needs an OpenBao node).", When: "k3dCertManager is on and k3dOperator is pxc, ps or psmdb (psmdb 1.23.0 or newer)"},
			{Field: "k3dTlsHosts", Desc: "Put the cluster's endpoint host names in its certificates (tls.SANs; split horizons on PSMDB 1.22.0+) and publish every LoadBalancer Service as <service>.<domain> with its reverse record in the Intranet DNS.", When: "k3dCertManager is on and k3dOperator is pxc, ps or psmdb"},

			// Operator
			{Field: "k3dOperator", Values: []string{"", "pxc", "ps", "psmdb", "pg", "cnpg", "pgo", "everest"}, Desc: "What runs on the cluster: \"\" plain Kubernetes; Percona operators \"pxc\" (XtraDB Cluster), \"ps\" (Percona Server), \"psmdb\" (MongoDB), \"pg\" (PostgreSQL); \"cnpg\" CloudNativePG; \"pgo\" Crunchy Postgres for Kubernetes; \"everest\" OpenEverest, which installs Percona operators and creates no database of its own."},
			{Field: "k3dOperatorVer", Desc: "Operator version (pxc, ps, psmdb, pg) or Helm chart version (cnpg, pgo, everest), from GET /api/catalog/operators; \"\" is the catalog's latest. Several options below are gated on it.", When: "k3dOperator is set"},
			{Field: "k3dNamespace", Default: "default", Desc: "Namespace the operator and its custom resource go into (a DNS-1123 label). The designer sets it to the operator's name when the operator is chosen. cnpg: the Cluster's namespace (the operator runs in cnpg-system); everest: the database namespace (Everest runs in everest-system).", When: "k3dOperator is set"},
			{Field: "k3dEverestOperators", Values: []string{"pxc", "psmdb", "pg"}, Desc: "Operators OpenEverest installs into its database namespace through OLM; empty means all three.", When: "k3dOperator is everest"},

			// PXC
			{Field: "k3dProxy", Values: []string{"haproxy", "proxysql", "router"}, Default: "haproxy", Desc: "Front end in front of the database. pxc: haproxy or proxysql. ps: haproxy or router (MySQL Router, group replication only; an async cluster is always haproxy).", When: "k3dOperator is pxc or ps"},
			{Field: "k3dExposePxc", Values: k3dExposeValues, Default: "clusterip", Desc: "Service type of the per-pod database Services. Held at loadbalancer while the frame is the source of a replication edge.", When: "k3dOperator is pxc"},
			{Field: "k3dExposeHaproxy", Values: k3dExposeValues, Default: "loadbalancer", Desc: "Service type of HAProxy, the cluster's front door.", When: "k3dOperator is pxc or ps, and k3dProxy is haproxy"},
			{Field: "k3dExposeProxysql", Values: k3dExposeValues, Default: "loadbalancer", Desc: "Service type of ProxySQL.", When: "k3dOperator is pxc and k3dProxy is proxysql"},
			{Field: "k3dPitr", Desc: "Point-in-time recovery: the operator's binlog collector (spec.backup.pitr) uploads binary logs continuously to the backup store. On the replica end of a replication edge it deploys off and is turned on once replication runs.", When: "k3dOperator is pxc and a SeaweedFS backup store is set"},
			{Field: "k3dPitrBucket", Desc: "Bucket on the SeaweedFS node for the binary logs; \"\" uses the backup bucket. Two clusters must not share a binlog bucket.", When: "k3dPitr is on"},
			{Field: "k3dPitrSeconds", Desc: "timeBetweenUploads in seconds (10–3600); 0 keeps cr.yaml's 60.", When: "k3dPitr is on"},
			{Field: "k3dReplSsl", Desc: "Replicate over SSL on the channel this frame receives: both clusters take their certificates from the Intranet CA.", When: "k3dOperator is pxc and the frame is the replica end of a replication edge"},
			{Field: "k3dReplSslSkipVerify", Desc: "sslSkipVerify on the channel: the chain is checked but not the host name. Off, the source's certificates also name its LoadBalancer addresses and per-pod DNS names.", When: "k3dReplSsl is on"},

			// PS
			{Field: "k3dClusterType", Values: []string{"group-replication", "async"}, Default: "group-replication", Desc: "Replication: group replication (3 MySQL pods), or async under Orchestrator (3 MySQL + 3 Orchestrator + 3 HAProxy pods).", When: "k3dOperator is ps"},
			{Field: "k3dExposeMysql", Values: k3dExposeValues, Default: "clusterip", Desc: "Service type of the primary MySQL Service.", When: "k3dOperator is ps"},
			{Field: "k3dExposeRouter", Values: k3dExposeValues, Default: "loadbalancer", Desc: "Service type of MySQL Router.", When: "k3dOperator is ps and k3dProxy is router"},

			// PSMDB
			{Field: "k3dSharding", Desc: "Sharded cluster: rs0 plus 3 config servers and 3 mongos routers (9 pods). Off is a 3-pod replica set.", When: "k3dOperator is psmdb"},
			{Field: "k3dExposeReplset", Values: k3dExposeValues, Default: "clusterip", Desc: "Service type of the per-pod replica set Services.", When: "k3dOperator is psmdb"},
			{Field: "k3dExposeMongos", Values: k3dExposeValues, Default: "loadbalancer", Desc: "Service type of the mongos routers.", When: "k3dOperator is psmdb and k3dSharding is on"},
			{Field: "k3dVectorSearch", Desc: "spec.search: Percona Search for MongoDB (mongot), one pod per replica set, and a PSMDB 8.3 server image. Enables $vectorSearch and $search.", When: "k3dOperator is psmdb, operator 1.23.0 or newer"},
			{Field: "k3dClusterSync", Desc: "Run Percona ClusterSync for MongoDB (PerconaServerMongoDBClusterSync) copying k3dClusterSyncSource into this cluster until finalized.", When: "k3dOperator is psmdb, operator 1.23.0 or newer"},
			{Field: "k3dClusterSyncSource", Desc: "Id of the psmrs (replica set) or psmdb (sharded) frame to sync from. A sharded cluster syncs only from a sharded source, a replica set only from a replica set; the source must run PSMDB 8.0 and k3dVectorSearch must be off.", When: "k3dClusterSync is on"},

			// Encryption (PXC, PSMDB)
			{Field: "k3dVaultEncryption", Desc: "Data-at-rest encryption keyed to the stack's OpenBao node (pxc: spec.vaultSecretName / keyring_vault; psmdb: spec.secrets.vault). The cluster gets its own KV v2 mount and token. Cannot be turned on later without re-creating the data.", When: "k3dOperator is pxc, or psmdb 1.13.0 or newer; an OpenBao node exists"},

			// Percona PG
			{Field: "k3dExposePg", Values: k3dExposeValues, Default: "clusterip", Desc: "Service type of the primary PostgreSQL Service (pgo: the HA Service).", When: "k3dOperator is pg or pgo"},
			{Field: "k3dExposePgbouncer", Values: k3dExposeValues, Default: "loadbalancer", Desc: "Service type of the pgBouncer pool.", When: "k3dOperator is pg or pgo"},
			{Field: "k3dPgVector", Desc: "pgvector. pg: spec.extensions.builtin.pgvector; cnpg and pgo: the extension is created in the application database at bootstrap.", When: "k3dOperator is pg (operator 2.6.0 or newer), cnpg or pgo"},
			{Field: "k3dPgLogicalReplicas", Desc: "spec.logicalReplicas: read-only replicas fed by logical replication, each its own pod and PVC (0–3); 0 is none. Cannot be combined with k3dPgTde.", When: "k3dOperator is pg, operator 3.1.0 or newer"},
			{Field: "k3dPgLogicalStorageGb", Default: 1, Desc: "PVC size per logical replica, GiB; 0 is 1.", When: "k3dPgLogicalReplicas > 0"},
			{Field: "k3dPgLogicalBootstrap", Values: []string{"pgbackrest", "pg_basebackup"}, Default: "pgbackrest", Desc: "How a logical replica gets its first copy: from the pgBackRest repository, or pg_basebackup off the primary.", When: "k3dPgLogicalReplicas > 0"},
			{Field: "k3dPgLogicalDatabases", Desc: "Comma-separated databases the replicas subscribe to; \"\" is every non-template database except postgres. Each named database must exist when the replica bootstraps.", When: "k3dPgLogicalReplicas > 0"},
			{Field: "k3dPgNoLogCollector", Desc: "Turn off spec.logcollector (the fluent-bit and logrotate sidecars that keep the server log as files on the data volume); the operator ships it on.", When: "k3dOperator is pg, operator 3.1.0 or newer"},
			{Field: "k3dPgTde", Desc: "Transparent data encryption, spec.extensions.pg_tde, keyed to the stack's OpenBao node. Cannot be turned on later without re-creating the data.", When: "k3dOperator is pg, operator 3.1.0 or newer; an OpenBao node exists"},
			{Field: "k3dPgTdeWal", Desc: "Encrypt the WAL as well (walEncryption).", When: "k3dPgTde is on"},

			// CloudNativePG
			{Field: "k3dCnpgInstances", Default: 3, Desc: "PostgreSQL instances, 1–5; 0 is 3.", When: "k3dOperator is cnpg"},
			{Field: "k3dCnpgStorageGb", Default: 1, Desc: "PVC size per instance, GiB; 0 is 1.", When: "k3dOperator is cnpg"},
			{Field: "k3dCnpgVersion", Desc: "PostgreSQL major (imageName ghcr.io/cloudnative-pg/postgresql:<major>), from GET /api/catalog/operators (image:cnpg-postgresql); \"\" is the chart's default.", When: "k3dOperator is cnpg"},
			{Field: "k3dCnpgExpose", Values: []string{"clusterip", "loadbalancer"}, Default: "clusterip", Desc: "Service type for the primary.", When: "k3dOperator is cnpg"},
			{Field: "k3dCnpgPooler", Desc: "Create a Pooler (PgBouncer) of type rw for the primary.", When: "k3dOperator is cnpg"},
			{Field: "k3dCnpgPoolerInstances", Default: 2, Desc: "PgBouncer pods, 1–5; 0 is 2.", When: "k3dCnpgPooler is on"},
			{Field: "k3dCnpgPoolerMode", Values: []string{"session", "transaction"}, Default: "session", Desc: "PgBouncer pool mode.", When: "k3dCnpgPooler is on"},
			{Field: "k3dCnpgPoolerExpose", Values: []string{"clusterip", "loadbalancer"}, Default: "clusterip", Desc: "Service type for the PgBouncer pool.", When: "k3dCnpgPooler is on"},
			{Field: "k3dCnpgMonitoring", Desc: "Install kube-prometheus-stack into the monitoring namespace with a PodMonitor and CloudNativePG's alerting rules; Grafana gets a LoadBalancer address.", When: "k3dOperator is cnpg"},
			{Field: "k3dCnpgPromVersion", Desc: "kube-prometheus-stack chart version; \"\" is the latest.", When: "k3dCnpgMonitoring is on"},

			// Crunchy PGO
			{Field: "k3dPgoInstances", Default: 2, Desc: "PostgreSQL instances, 1–5; 0 is 2.", When: "k3dOperator is pgo"},
			{Field: "k3dPgoStorageGb", Default: 1, Desc: "PVC size per instance, GiB; 0 is 1.", When: "k3dOperator is pgo"},
			{Field: "k3dPgoVersion", Desc: "PostgreSQL major (spec.postgresVersion), from GET /api/catalog/operators (image:crunchy-postgres); \"\" is the newest the chart ships (17 or older with k3dPgoMonitoring).", When: "k3dOperator is pgo"},
			{Field: "k3dPgoMonitoring", Desc: "spec.monitoring.pgmonitor.exporter sidecars plus kube-prometheus-stack with pgMonitor dashboards; needs PostgreSQL 17 or older.", When: "k3dOperator is pgo"},
			{Field: "k3dPgoPromVersion", Desc: "kube-prometheus-stack chart version; \"\" is the latest.", When: "k3dPgoMonitoring is on"},

			// Legacy expose
			{Field: "k3dExpose", Values: k3dExposeValues, Desc: "Older single Service type, applied to any expose section left empty. Not shown in the designer.", When: "k3dOperator is pxc, ps, psmdb, pg or pgo"},

			// Volumes
			{Field: "k3dStorage", Values: []string{"", "ceph"}, Desc: "Where volumes live: \"\" k3s local-path (a node directory, cannot be resized); \"ceph\" RBD images on the Ceph node (relation storage) through Ceph CSI, which can grow.", When: "k3dOperator is set and not everest"},
			{Field: "k3dStorageGb", Desc: "Database volume per pod, GiB; 0 keeps the operator's cr.yaml default (pxc 6 GB, ps 2 GiB, psmdb 3 GiB, pg 1 GiB).", When: "k3dOperator is pxc, ps, psmdb or pg"},

			// Operator debugging
			{Field: "k3dDebug", Desc: "Build the operator from its source with optimisation off and run it under Delve (--continue); the Operator Debugger attaches over the stack network.", When: "k3dOperator is pxc, ps, psmdb or pg"},
			{Field: "k3dDebugNoPublish", Desc: "Do not publish Delve's port to the host. The in-app debugger does not need the host port.", When: "k3dDebug is on"},
			{Field: "k3dDebugPort", Default: 40000, Desc: "Host port Delve is published on, for an external IDE; 0 is 40000.", When: "k3dDebug is on and k3dDebugNoPublish is off"},

			// Backups and PMM
			{Field: "seaweedfsBucket", Desc: "Bucket on the SeaweedFS node (relation backup) for the operator's S3 backup storage; \"\" is the node's first bucket. pg and pgo need the node's S3 TLS on, or keep a PVC repository.", When: "a SeaweedFS node is set and k3dOperator is not everest"},
			{Field: "k3dPmmTokenTtlValue", Default: 365, Desc: "Lifetime of the PMM service token minted for the pmm-client sidecars; 0 is 365.", When: "a PMM node is set and k3dOperator is pxc, ps, psmdb or pg"},
			{Field: "k3dPmmTokenTtlUnit", Values: []string{"minutes", "hours", "days"}, Default: "days", Desc: "Unit of k3dPmmTokenTtlValue.", When: "a PMM node is set"},
		},
		Relations: []string{"monitor", "repository", "backup", "storage", "vault"},
		Edges: []catalogEdge{
			{To: []string{"k3d"}, Type: "async", Desc: "Cross-cluster replication from this frame (source) to another K3D frame (replica), drawn frame to frame. Both must run k3dOperator \"pxc\" and have a SeaweedFS backup store; the replica is seeded from a backup of the source, and the source's database Services become LoadBalancer."},
			{To: []string{"stocksim", "supportsim", "pgvectorsim", "perfsim"}, Type: "directional", Desc: "Drive a simulator against the database the frame's operator runs (from the frame to the simulator). Performance Sim gives each database pod an address of its own with a companion LoadBalancer Service, removed with the sim."},
		},
		Actions: []catalogAction{
			// Access
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/kubeconfig", Desc: "Admin kubeconfig for the cluster."},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/users", Desc: "RBAC users created on the cluster."},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/users", Desc: "Create an RBAC user bound to a built-in role.", Body: "username, role (view|edit|admin|cluster-admin), namespace (ignored for cluster-admin; \"\" is default)"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/users/delete", Desc: "Delete an RBAC user.", Body: "username"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/users/{username}/kubeconfig", Desc: "Kubeconfig scoped to one RBAC user."},

			// Kubernetes States and objects
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/states", Desc: "One sample of every object in the cluster with its state, summary and warnings."},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/states/logs", Desc: "One container's log. Query: namespace, name (pod), container, tail, previous=1."},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/states/manifest", Desc: "One object's YAML. Query: kind, namespace, name."},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/objects", Desc: "Secrets or ConfigMaps in a namespace, key names and sizes only. Query: kind (secret|configmap), namespace."},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/object", Desc: "One Secret or ConfigMap with its text values decoded. Query: kind, namespace, name."},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/object", Desc: "Set or remove keys of a Secret or ConfigMap, server-side dry-run first.", Body: "kind, namespace, name, set (key → value), remove ([]key), dryRun"},

			// Custom resource
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/cr", Desc: "The operator's CRD as a form model, with the live spec of the cluster's custom resource.", When: "k3dOperator is pxc, ps, psmdb or pg"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/cr", Desc: "Merge-patch the cluster's custom resource, server-side dry-run first.", Body: "patch (object), dryRun", When: "k3dOperator is pxc, ps, psmdb or pg"},

			// Volumes and certificates
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/storage", Desc: "Each PVC with its size, capacity and use, and whether the volumes can grow.", When: "k3dOperator is set and not everest"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/storage", Desc: "Grow the database volumes by writing a larger size into the custom resource; there is no shrink.", Body: "sizeGiB", When: "k3dOperator is pxc, ps, psmdb or pg, k3dStorage is ceph, and the operator version supports volume scaling"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/certs", Desc: "The cluster's TLS Secrets, parsed and checked.", When: "k3dOperator is set and not everest"},

			// Backups
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/backups", Desc: "Backup and restore objects, and the store they go to.", When: "k3dOperator is pxc, ps, psmdb or pg"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/backups", Desc: "Take a backup by applying the operator's backup custom resource; returns the manifest applied.", Body: "name (optional), storage (optional), retain, dryRun", When: "k3dOperator is pxc, ps, psmdb or pg"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/backups/delete", Desc: "Delete a backup object; with data, its files in the object store too.", Body: "name, data", When: "k3dOperator is pxc, ps, psmdb or pg"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/restores", Desc: "Restore by applying the operator's restore custom resource.", Body: "backup (pxc, ps, psmdb), name (optional), storage (pg repository), options ([]pgBackRest option, pg), dryRun", When: "k3dOperator is pxc, ps, psmdb or pg"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/restores/delete", Desc: "Delete a restore object.", Body: "name", When: "k3dOperator is pxc, ps, psmdb or pg"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/backups/manifests", Desc: "Manifests applied by the backup calls, archived under the operator's deploy/backup. Query: name for one.", When: "k3dOperator is pxc, ps, psmdb or pg"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/bucket", Desc: "List one prefix of the backup bucket through a toolbox pod. Query: prefix, after.", When: "a SeaweedFS backup store is set"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/bucket/delete", Desc: "Delete an object, or a whole prefix with recursive, from the backup bucket.", Body: "key, recursive, dryRun", When: "a SeaweedFS backup store is set"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/bucket/download", Desc: "Download one object from the backup bucket. Query: key.", When: "a SeaweedFS backup store is set"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/bucket/toolbox", Desc: "State of the bucket toolbox pod.", When: "a SeaweedFS backup store is set"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/bucket/toolbox", Desc: "Start the bucket toolbox pod, or remove it with action \"stop\".", Body: "action (\"stop\" or empty)", When: "a SeaweedFS backup store is set"},

			// Replication
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/replication", Desc: "State of the cross-cluster replication link this frame is an end of.", When: "k3dOperator is pxc and the frame is on a replication edge"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/replication/reseed", Desc: "Re-seed the replica from a fresh backup of its source, replacing its data, in the background.", When: "the frame is the replica end of a replication edge"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/logicalreplicas", Desc: "Logical replicas, their state, and the databases they can subscribe to.", When: "k3dOperator is pg, operator 3.1.0 or newer"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/logicalreplicas", Desc: "Add a logical replica to spec.logicalReplicas.", Body: "name, databases ([]string), bootstrapMethod (pgbackrest|pg_basebackup), storageGb, expose (ClusterIP|NodePort|LoadBalancer)", When: "k3dOperator is pg, operator 3.1.0 or newer"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/logicalreplicas/remove", Desc: "Remove a logical replica and its volume.", Body: "name", When: "k3dOperator is pg, operator 3.1.0 or newer"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/logicalreplicas/reseed", Desc: "Re-seed a logical replica.", Body: "name", When: "k3dOperator is pg, operator 3.1.0 or newer"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/logicalreplicas/bootstraplog", Desc: "A logical replica's bootstrap log. Query: name.", When: "k3dOperator is pg, operator 3.1.0 or newer"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/clustersync", Desc: "ClusterSync state: support for it in this operator version, and the custom resource's spec and status.", When: "k3dOperator is psmdb"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/clustersync/start", Desc: "Start ClusterSync from a source MongoDB frame.", Body: "source (psmrs or psmdb frame id)", When: "k3dOperator is psmdb, operator 1.23.0 or newer"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/clustersync/stop", Desc: "Stop ClusterSync and remove its custom resource.", When: "k3dOperator is psmdb"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/clustersync/{action}", Desc: "Pause, resume or finalize a running ClusterSync; {action} is pause, resume or finalize.", When: "k3dOperator is psmdb"},

			// Diagnostics
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/collect", Desc: "Whether a pt-k8s-debug-collector capture of the cluster is running."},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/collect", Desc: "Start a pt-k8s-debug-collector capture of the cluster, in the background."},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/collect/dumps", Desc: "Kept pt-k8s-debug-collector captures of the cluster."},
			{Method: "GET", Path: "/api/k8sdumps/{did}/download", Desc: "Download a kept capture; {did} comes from …/k3d/collect/dumps."},
			{Method: "DELETE", Path: "/api/k8sdumps/{did}", Desc: "Delete a kept capture."},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/opsummary", Desc: "Capture the cluster and distil it into workload health, custom resource status and operator errors."},

			// Operator debugger
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/debug/sources", Desc: "The operator's source tree as the debugger sees it.", When: "k3dDebug is on"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/debug/source", Desc: "One source file. Query: path.", When: "k3dDebug is on"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/debug/ws", Desc: "Live debug session (WebSocket): breakpoints, stepping, call stack, locals, evaluation.", When: "k3dDebug is on"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/debug/reconcile", Desc: "Make the operator reconcile the cluster's custom resource now.", When: "k3dDebug is on"},
		},
	},
}
