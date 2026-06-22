# syntax=docker/dockerfile:1
#
# systemd-enabled base image for the Debian family (Ubuntu 22.04/24.04).
# Used by `make images` to produce selectable container-instance bases.
# Run the resulting image with systemd as PID 1, e.g.:
#   docker run -d --privileged \
#     -v /sys/fs/cgroup:/sys/fs/cgroup:ro \
#     <tag>
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive

# Install systemd (PID 1) plus the required tooling. percona-toolkit is pulled
# from the dedicated Percona Toolkit repo (`percona-release setup pt`).
#   - net-tools  → ifconfig/netstat/route
#   - ldap-utils → ldapsearch and friends (OpenLDAP client)
#   - sysstat    → sar/iostat/mpstat
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      systemd systemd-sysv net-tools ldap-utils sysstat \
      wget gnupg2 lsb-release ca-certificates; \
    wget -qO /tmp/percona-release.deb \
      "https://repo.percona.com/apt/percona-release_latest.$(lsb_release -sc)_all.deb"; \
    apt-get install -y /tmp/percona-release.deb; \
    percona-release setup pt; \
    apt-get update; \
    apt-get install -y --no-install-recommends percona-toolkit; \
    rm -f /tmp/percona-release.deb; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# Trim systemd units that are pointless or harmful inside a container. Done
# without `set -e` so a missing unit/dir never fails the build.
RUN cd /lib/systemd/system/sysinit.target.wants/ && \
    (for f in *; do [ "$f" = systemd-tmpfiles-setup.service ] || rm -f "$f"; done) ; \
    rm -f /lib/systemd/system/multi-user.target.wants/* \
          /etc/systemd/system/*.wants/* \
          /lib/systemd/system/local-fs.target.wants/* \
          /lib/systemd/system/sockets.target.wants/*udev* \
          /lib/systemd/system/sockets.target.wants/*initctl* \
          /lib/systemd/system/basic.target.wants/* ; \
    true

STOPSIGNAL SIGRTMIN+3
VOLUME ["/sys/fs/cgroup"]
CMD ["/sbin/init"]
