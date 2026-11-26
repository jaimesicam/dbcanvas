# Nodes that run as virtual machines

A node normally runs in a container, sharing the Docker host's kernel. A node can run as a
virtual machine instead, with a kernel of its own: SELinux or AppArmor as the operating system
ships them, its own sysctls, transparent huge pages, I/O stack and systemd, and real reboots.
DBCanvas runs these VMs with [Lima](https://lima-vm.io) and puts them on the stack's network next
to its containers, so a cluster can mix the two: one Percona XtraDB Cluster member in a VM, the
rest in containers.

Lima is optional. Without it, every node runs in a container as before.

## What can run as a VM

Nodes that DBCanvas installs from packages on its operating-system images. A type becomes
available once its provisioning is verified on a VM (Oracle Linux with SELinux enforcing and
Ubuntu with AppArmor, alone and in a cluster with containers, and across a reboot):

| Type | Since |
|---|---|
| Percona XtraDB Cluster (cluster and members) | 0.1.x |
| Percona Server for MySQL, standalone and asynchronous replication | 0.1.x |
| Percona Distribution for PostgreSQL, standalone | 0.1.x |
| Patroni with etcd (cluster and members) | 0.1.x |
| Percona Server for MongoDB: standalone, replica set and sharded cluster | 0.1.x |
| ProxySQL, standalone and cluster | 0.1.x |
| Percona Orchestrator | 0.1.x |
| HAProxy | 0.1.x |
| PgBouncer | 0.1.x |
| repmgr (cluster and members) | 0.1.x |
| Valkey: standalone, cluster and Sentinel (SELinux permissive by default) | 0.1.x |
| MariaDB: standalone, Galera Cluster and asynchronous replication | 0.1.x |
| MySQL Community: standalone, asynchronous replication and InnoDB Cluster | 0.1.x |
| Percona InnoDB Cluster | 0.1.x |
| PostgreSQL streaming replication, and PostgreSQL with Spock | 0.1.x |
| PostgreSQL and Patroni from PGDG, MongoDB Community Server | 0.1.x |
| OpenBao, Samba AD DC | 0.1.x |
| Kubeadm Cluster (the control plane and workers, alone or beside container nodes) | 0.1.x |
| All in One (SELinux permissive by default), Linux Client | 0.1.x |

A type runs on the operating systems it is offered on as a container: Samba AD DC only on Ubuntu
24.04 (Oracle Linux's Samba has no AD DC role), OpenBao and Spock only on Oracle Linux.

Nodes that arrive as ready-made images — the Intranet, the VNC desktop, PMM, Keycloak, the
simulators — and K3D clusters always run in containers. A Kubeadm node that runs as a VM is
prepared as its image is (containerd, the kernel modules and settings a pod network needs) and gets
its Kubernetes packages at deploy, like a container node. Its pod network carries traffic over the
stack network: Flannel on the interface that reaches the Intranet, Calico on the node's address
there, encapsulated in VXLAN between all nodes when the cluster has VM members. Kubeadm VMs run on
the machine's own architecture.

## Requirements

- `limactl` 2.0 or newer on the machine dbcanvas runs on. `dbcanvas doctor` reports it under
  *vm nodes*, as a note when it is missing, never as a failure.
- On Linux, access to `/dev/kvm` (usually membership of the `kvm` group); on a cloud machine,
  nested virtualization. doctor warns when it is missing: VMs would then run under emulation.
- On macOS, nothing more: Lima uses Apple's Virtualization framework.
- An Intranet node in the stack: it carries the stack network to the VMs.
- Memory: a VM reserves all of its memory (2 GiB unless the node sets **Memory**).

## Choose where a node runs

In the editor, a node or cluster of a type that can run as a VM has **Runs on** under
Architecture: *Container (Docker)*, the default, or *Virtual machine (Lima)*. A cluster member
follows its cluster unless it picks its own. It cannot change while the node is deployed.

In a stack file, set `runtime: vm` on a cluster or node (`runtime: docker` on a member keeps it in
a container in a cluster of VMs). With `dbcanvas up --node`, add `runtime=vm`:

```
dbcanvas up pxc-lab --node intranet --node 'pxc:3,version=8.4,runtime=vm' --wait
```

**Architecture** applies too: the machine's own runs natively; the other one runs under QEMU's
emulation, which works but boots in minutes and installs packages several times more slowly.

## What a VM node is like

- **Operating system defaults are kept.** Oracle Linux runs SELinux enforcing (see below for the
  exceptions); Ubuntu and Debian run AppArmor with the profiles the packages install (Percona XtraDB Cluster installs its
  `mysqld` profile in complain mode, as it does on any Ubuntu server). firewalld runs, with the
  stack network's interface in its trusted zone, so the stack's nodes reach each other as
  containers do.
- **The same tooling as a container node**: percona-release, Percona Toolkit, `tc`, sysstat,
  and the node's terminal, files, logs, live stats and network conditions.
- **Its own boot.** A reboot from inside the VM is a real one; the node's services wait for the
  stack network at boot and start once it is back. When the VM shuts down, its link to the stack
  network goes early, so the node's services stop without it, as if the network were cut; each
  has 30 seconds to stop, as a container has when it is stopped.
- **Its ports on the host, as a container's.** A node that publishes ports (Export, or a type
  that always does) is reached from your machine the same way: Lima forwards exactly those ports,
  on host ports DBCanvas picks and keeps, at `CONTAINER_BIND_IP`. Every other port the VM listens
  on stays inside.
- Not available on a VM: disk read/write limits (validation warns when they are set).

## SELinux mode

On Oracle Linux a VM node runs SELinux **enforcing**, except the types that have no SELinux policy
that fits them, which run **permissive** by default: Valkey (it runs under the redis policy, which
keeps a cluster node from opening its `nodes.conf`) and All in One (its many instances use ports
and folders no policy knows). Any VM node can be set either way with **SELinux** under Runs on in
the editor, `selinux: enforcing|permissive` in a stack file, or `selinux=` in a compose entry; a
cluster member follows its cluster unless it says otherwise. Ubuntu and Debian have no SELinux,
so the setting does nothing there.

## The network

A VM gets one of the 16 addresses at the top of the half of the stack subnet Docker assigns
containers from (networks created since this feature keep them out of Docker's hands). The
Intranet answers for that address on the Docker network and passes its traffic to the VM over a
link dbcanvas carries: one end runs in the Intranet container, the other in the VM over ssh. The
VM's name is in the Intranet's DNS like any node's.

Nothing changes on the host: no routes, firewall rules or listening ports beyond `127.0.0.1`.
dbcanvas reaches each VM over ssh with the key Lima made for its folder, and checks the VM's host
key against the one Lima pinned when the VM was created.

The link runs while dbcanvas runs. `dbcanvas stop` says how many nodes are VMs; until dbcanvas
starts again their link is down. On macOS they are then cut off from the stack; on Linux a VM can
still open connections to the containers through the host, but nothing reaches the VM. When
dbcanvas starts, when the Intranet restarts, or when the VM reboots, the link comes back by itself.

## Where the VMs are

In `~/.dbcanvas/lima` (or `DBCANVAS_LIMA_HOME`), apart from your own Lima instances, named
`dbc-` and a hash of the node's container name. To look at them with Lima itself:

```
LIMA_HOME=~/.dbcanvas/lima limactl list
```

Removing or expiring a stack deletes its VMs with it.

| Setting | Purpose |
|---|---|
| `DBCANVAS_LIMA_HOME` | Where Lima keeps dbcanvas's VMs; keep it short, Lima's socket paths have a length limit. |
| `DBCANVAS_LIMA_TEMPLATE_<OS>_<VER>` | A Lima template of your own for one OS, e.g. `DBCANVAS_LIMA_TEMPLATE_UBUNTU_24_04=/srv/ubuntu.yaml`. |
| `DBCANVAS_VM_PREVIEW` | Node types to allow as VMs before their support is verified, comma-separated. |
