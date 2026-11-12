# syntax=docker/dockerfile:1
#
# The node image of a Kubeadm Cluster frame (app/kubeadm.go): Ubuntu 24.04 with systemd as
# PID 1, containerd configured for the kubelet, and the tools a kubeadm node is administered
# with. kubeadm, kubelet and kubectl are NOT here: the frame picks the Kubernetes version,
# so they are installed at deploy from pkgs.k8s.io for that version.
#
# Built FROM ubuntu:24.04 directly rather than on the dbcanvas-systemd base, for two reasons:
#
#   - It is built for K8S_PLATFORM, not DOCKER_PLATFORM. A Kubernetes node has to run on the
#     host's native architecture (under emulation containerd cannot create a pod sandbox:
#     "seccomp is not supported"), and the systemd bases exist only for DOCKER_PLATFORM.
#   - The systemd base declares VOLUME /sys/fs/cgroup. A kubeadm node runs in a private cgroup
#     namespace (see CgroupnsPrivate in app/docker.go), where Docker mounts the container's own
#     cgroup at /sys/fs/cgroup — and an anonymous volume there hides that mount. kubeadm's
#     preflight then reads the volume first, takes the node for cgroup v1 and reports
#     "missing required cgroups: cpuset memory". This image has no such VOLUME.
#
# Nothing is copied from the build context: the entrypoint is written by a RUN, so DBCanvas can
# build this image from the Dockerfile alone (app/extraimages.go).
#
#   docker run -d --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
#     -v /lib/modules:/lib/modules:ro <tag>
FROM ubuntu:24.04

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive

# systemd, containerd and what a node is administered with. conntrack, socat, ebtables and
# iptables are kubeadm's preflight requirements; openssh lets the control plane reach the
# workers by name; bash-completion, vim, less and jq are for the person at the console.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus ca-certificates curl gpg apt-transport-https \
      containerd conntrack socat ebtables iptables ipset kmod \
      iproute2 iputils-ping net-tools dnsutils \
      openssh-server openssh-client \
      bash-completion vim less jq; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# Trim systemd units that are pointless or harmful inside a container (as the systemd bases
# do), then enable what a node runs. systemd-nologin.conf is masked: it writes /run/nologin at
# boot for systemd-user-sessions to remove, which the trimmed boot never runs, and every ssh to
# the node then printed "System is booting up".
RUN cd /lib/systemd/system/sysinit.target.wants/ && \
    (for f in *; do [ "$f" = systemd-tmpfiles-setup.service ] || rm -f "$f"; done) ; \
    rm -f /lib/systemd/system/multi-user.target.wants/* \
          /etc/systemd/system/*.wants/* \
          /lib/systemd/system/local-fs.target.wants/* \
          /lib/systemd/system/sockets.target.wants/*udev* \
          /lib/systemd/system/sockets.target.wants/*initctl* \
          /lib/systemd/system/basic.target.wants/* ; \
    systemctl enable containerd.service ssh.service ; \
    ln -sf /dev/null /etc/tmpfiles.d/systemd-nologin.conf ; \
    true

# containerd for the kubelet: the systemd cgroup driver (the kubelet's default since 1.22),
# and crictl pointed at it. The kubelet waits for containerd: after a container restart it
# otherwise starts first, fails its CRI check once and only recovers on systemd's retry.
RUN set -eux; \
    mkdir -p /etc/containerd /etc/systemd/system/kubelet.service.d; \
    containerd config default > /etc/containerd/config.toml; \
    sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml; \
    grep -q 'SystemdCgroup = true' /etc/containerd/config.toml; \
    printf 'runtime-endpoint: unix:///run/containerd/containerd.sock\nimage-endpoint: unix:///run/containerd/containerd.sock\n' > /etc/crictl.yaml; \
    printf '[Unit]\nWants=containerd.service\nAfter=containerd.service\n' > /etc/systemd/system/kubelet.service.d/05-containerd.conf; \
    printf 'net.ipv4.ip_forward = 1\nnet.bridge.bridge-nf-call-iptables = 1\nnet.bridge.bridge-nf-call-ip6tables = 1\n' > /etc/sysctl.d/90-kubernetes.conf

# The entrypoint runs before systemd, on every start:
#
#   - Cgroup delegation. In a private cgroup namespace the container's cgroup is the root, and
#     every `docker exec` (DBCanvas's provisioning steps, kubeadm itself) lands in it. cgroup v2
#     will not enable a controller for children while processes sit in their parent, so whether
#     systemd could hand pids, memory and cpu down to kubepods.slice came down to timing — and
#     when it lost, the kubelet exited on "openat2 /sys/fs/cgroup/kubepods.slice/pids.max: no
#     such file or directory" until kubeadm init timed out. Moving everything into /init and
#     enabling every controller on the root first makes it deterministic: systemd starts in
#     /init and treats it as its root, and later execs land in a leaf below it. (kind's
#     entrypoint does the same.) It is retried until the root reports every controller
#     delegated: one attempt lost the race on a WSL2 Docker host when three nodes booted at
#     once — a process reached the root between the move and the enable, the enable failed
#     with EBUSY, and the node ran with nothing delegated. Every provisioning exec then
#     landed in the root (runc can join a cgroup with no controllers enabled) and kept
#     delegation off for as long as it ran, so the kubelet failed on "missing controllers:
#     cpu, cpuset, hugetlb, memory, pids" until kubeadm join gave up. Where the first attempt
#     succeeds — as it did on Rancher Desktop — the loop runs once.
#   - Shared mount propagation. Docker makes a container's mounts private; a CNI plugin that
#     mounts with propagation (Calico's bpffs) then fails with "path /sys/fs is mounted on /sys
#     but it is not a shared mount". It has to be redone on every start, not once.
#   - A kernel config where kubeadm's SystemVerification preflight looks for one. A container
#     has no /boot of its own, and a host kernel that publishes /proc/config.gz is the
#     exception, so without this every `kubeadm init`, `join` and `upgrade` would need
#     --ignore-preflight-errors=SystemVerification. The real /proc/config.gz is used when the
#     kernel has one; otherwise the file lists the options the check requires, which the
#     running kernel is known to have — the nodes depend on every one of them and could not
#     run a pod without them.
RUN set -eux; \
    { \
      echo '#!/bin/sh'; \
      echo '# Written by images/kubeadm.Dockerfile. See the comment above it there.'; \
      echo 'if [ -f /sys/fs/cgroup/cgroup.controllers ]; then'; \
      echo '  mkdir -p /sys/fs/cgroup/init'; \
      echo '  want="$(cat /sys/fs/cgroup/cgroup.controllers)"'; \
      echo '  i=0'; \
      echo '  while [ "$(cat /sys/fs/cgroup/cgroup.subtree_control)" != "$want" ] && [ $i -lt 100 ]; do'; \
      echo '    for p in $(cat /sys/fs/cgroup/cgroup.procs); do echo "$p" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || :; done'; \
      echo '    sed -e "s/ / +/g" -e "s/^/+/" < /sys/fs/cgroup/cgroup.controllers > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || :'; \
      echo '    i=$((i+1)); [ "$(cat /sys/fs/cgroup/cgroup.subtree_control)" = "$want" ] || sleep 0.05'; \
      echo '  done'; \
      echo 'fi'; \
      echo 'mount --make-rshared /'; \
      echo 'k="/boot/config-$(uname -r)"'; \
      echo 'if [ ! -e "$k" ]; then'; \
      echo '  mkdir -p /boot'; \
      echo '  if [ -r /proc/config.gz ]; then zcat /proc/config.gz > "$k"; else'; \
      echo '    { echo "# Written at container start: the host kernel publishes no config of its own."; \'; \
      echo '      echo "# These are the options kubeadm'"'"'s preflight check requires."; \'; \
      echo '      for o in NAMESPACES NET_NS PID_NS IPC_NS UTS_NS CGROUPS CGROUP_CPUACCT CGROUP_DEVICE \'; \
      echo '        CGROUP_FREEZER CGROUP_PIDS CGROUP_SCHED CGROUP_BPF CGROUP_HUGETLB CPUSETS MEMCG \'; \
      echo '        FAIR_GROUP_SCHED CFS_BANDWIDTH INET EXT4_FS PROC_FS OVERLAY_FS BLK_DEV_DM \'; \
      echo '        NETFILTER_XT_TARGET_REDIRECT NETFILTER_XT_MATCH_COMMENT SECCOMP SECCOMP_FILTER \'; \
      echo '        BPF BPF_SYSCALL; do echo "CONFIG_$o=y"; done; } > "$k"'; \
      echo '  fi'; \
      echo 'fi'; \
      echo 'exec /sbin/init "$@"'; \
    } > /usr/local/sbin/dbcanvas-node-init; \
    chmod 0755 /usr/local/sbin/dbcanvas-node-init; \
    sh -n /usr/local/sbin/dbcanvas-node-init

# containerd's images and container layers. A volume, so they are not overlayfs on top of the
# container's own overlayfs; anonymous, so it goes with the container (DBCanvas removes nodes
# with their volumes) and survives a restart.
VOLUME ["/var/lib/containerd"]

STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/sbin/dbcanvas-node-init"]
