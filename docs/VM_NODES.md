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
| All in One (SELinux permissive by default), Linux Client (not one set up for core-dump analysis, which reads its files through bind mounts and runs in a container) | 0.1.x |

VMs run Oracle Linux 8, 9 or 10, Ubuntu 22.04 or 24.04, or Debian 12 or 13. A type runs on those
of the operating systems it is offered on as a container: Samba AD DC only on Ubuntu 24.04 (Oracle
Linux's Samba has no AD DC role), OpenBao and Spock only on Oracle Linux. Validation refuses a VM
on any other operating system, such as a Linux Client on CentOS 7, which runs only in a container.

Nodes that arrive as ready-made images — the Intranet, the VNC desktop, PMM, Keycloak, the
simulators — and K3D clusters always run in containers. A Kubeadm node that runs as a VM is
prepared as its image is (containerd, the kernel modules and settings a pod network needs) and gets
its Kubernetes packages at deploy, like a container node. Its pod network carries traffic over the
stack network: Flannel on the interface that reaches the Intranet, Calico on the node's address
there, encapsulated in VXLAN between all nodes when the cluster has VM members. Kubeadm VMs run on
the machine's own architecture.

## Requirements

- `limactl` 2.0 or newer, and `ssh`, on the `PATH` of the dbcanvas service (see below).
  `dbcanvas doctor` reports Lima under *vm nodes*, as a note when it is missing, never as a
  failure.
- QEMU where a VM runs on it: on Linux every VM does (with KVM for the machine's own
  architecture), on macOS only a VM of the other architecture does, such as an amd64 VM on Apple
  Silicon. Validation refuses a VM whose QEMU (`qemu-system-x86_64` for amd64,
  `qemu-system-aarch64` for arm64) is not installed, and doctor lists each one under
  *vm qemu*.
- On Linux, access to `/dev/kvm` (usually membership of the `kvm` group); on a cloud machine,
  nested virtualization. doctor warns when it is missing: VMs would then run under emulation.
- An Intranet node in the stack: it carries the stack network to the VMs.
- Memory: a VM reserves all of its memory while it runs (2 GiB unless the node sets **Memory**).
  Validation warns when a stack's VMs, the VMs already running and, on macOS, the memory given to
  Docker's VM add up to more than the machine has.

### Install Lima

macOS:

```sh
brew install lima        # limactl 2.0 or newer
brew install qemu        # only to run the other architecture (amd64 VMs on Apple Silicon)
```

Linux (Ubuntu or Debian shown):

```sh
VERSION=$(curl -fsSL https://api.github.com/repos/lima-vm/lima/releases/latest | jq -r .tag_name)
curl -fsSL "https://github.com/lima-vm/lima/releases/download/${VERSION}/lima-${VERSION#v}-$(uname -s)-$(uname -m).tar.gz" \
  | sudo tar Cxzm /usr/local
sudo apt install qemu-system-x86 qemu-utils    # add qemu-system-arm qemu-efi-aarch64 for arm64 VMs
sudo usermod -aG kvm $USER                     # then log in again
```

Lima can also be installed with Homebrew on Linux. On other distributions, use their QEMU
packages; a QEMU that runs only the machine's own architecture (such as `qemu-kvm` on Oracle
Linux) runs native VMs only.

### Where dbcanvas finds `limactl`

dbcanvas looks `limactl` up on the `PATH` of its service, when the service starts. The service
inherits the environment of the `dbcanvas` command that started it, so a service started from a
normal shell finds a Homebrew or `/usr/local/bin` install. After installing Lima while the
service runs, run `dbcanvas stop`; the next command starts it again and finds Lima. A service
started where `PATH` lacks Lima (a cron job, a script with a minimal environment) runs every node
in a container, and doctor says Lima is missing.

The Lima that Rancher Desktop bundles inside its application is not on the `PATH` and is not
used. dbcanvas keeps its VMs in a Lima folder of its own (below), so they and your own Lima
instances do not see each other.

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
emulation, which works but boots in minutes and installs packages several times more slowly. A
cluster member runs on its cluster's architecture; set `arch` on the cluster (validation warns
about a member that names another).

The rest of a node's settings work the same way from the command line and the API, without the
editor: `cpus=` and `memoryGb=` size the VM when it is created (2 CPUs and 2 GiB when unset), and
the network conditions (`netLatencyMs=`, `netJitterMs=`, `netLossPct=`, `netRateMbit=`) apply on
the VM's link to the stack network:

```
dbcanvas up pxc-amd64 --node intranet \
  --node 'pxc:3,version=8.4,runtime=vm,arch=amd64,memoryGb=4,netLatencyMs=50' --wait --timeout 90m
```

Through the API, the same stack is `POST /api/stacks/compose` with
`{"name":"pxc-amd64","nodes":[{"kind":"intranet"},{"kind":"pxc","count":3,"version":"8.4","runtime":"vm","arch":"amd64","memoryGb":4,"netLatencyMs":50}]}`,
then `POST /api/stacks/{id}/deploy`; a dry run (`"dryRun":true`) reports a missing QEMU or a
memory shortfall first. See [HTTP API](API.md).

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
  network goes early, so the node's services stop without it, as if the network were cut. Each
  service keeps the stop timeout its own unit sets (All in One's units allow 120 seconds), so a
  database is given the time its package asks for to shut down cleanly; one whose unit would wait
  forever is stopped after 5 minutes, and one that sets nothing gets systemd's default of 90
  seconds.
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

## After a restart

When the dbcanvas service starts, it brings its VMs back as Docker's restart policy brings
containers back: a running VM gets its link to the stack network again, and a VM that stopped
without being stopped from DBCanvas (the machine restarted, or Lima's host agent was killed) is
started again, two at a time, when its node is deployed. A node stopped from DBCanvas stays
stopped. VMs start with the service, so after the machine restarts they come back on the first
`dbcanvas` command.

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
