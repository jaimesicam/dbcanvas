# syntax=docker/dockerfile:1
#
# systemd-enabled base image for the RHEL family (Oracle Linux 8/9/10).
# Used by `make images` to produce selectable container-instance bases.
# Run the resulting image with systemd as PID 1, e.g.:
#   docker run -d --privileged \
#     -v /sys/fs/cgroup:/sys/fs/cgroup:ro \
#     <tag>
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

# Tell systemd it is running inside a container.
ENV container=docker

# Force dnf to resolve over IPv4 (ip_resolve=4) so a host without working IPv6
# doesn't stall on AAAA when downloading packages — both during this build and for
# every node instance started from the image. Mirrors Squid's dns_v4_first and
# bind's filter-aaaa.
RUN grep -q '^ip_resolve=' /etc/dnf/dnf.conf 2>/dev/null || echo 'ip_resolve=4' >> /etc/dnf/dnf.conf

# Install systemd (PID 1) plus the required tooling. percona-toolkit is pulled
# from the dedicated Percona Toolkit repo (`percona-release setup pt`), which is
# the only one carrying percona-toolkit on EL10 (the generic "tools" repo lacks
# it there).
#   - net-tools           → ifconfig/netstat/route
#   - openldap-clients    → ldapsearch and friends (OpenLDAP client)
#   - sysstat             → sar/iostat/mpstat
#   - percona-release     → Percona repository manager
#   - percona-toolkit     → pt-* DBA tools
RUN set -eux; \
    yum -y install systemd net-tools openldap-clients sysstat; \
    yum -y install https://repo.percona.com/yum/percona-release-latest.noarch.rpm; \
    percona-release setup pt; \
    yum -y install percona-toolkit; \
    yum clean all; \
    rm -rf /var/cache/yum /var/cache/dnf

# Trim systemd units that are pointless or harmful inside a container. Done
# without `set -e` so a missing unit/dir never fails the build.
RUN cd /lib/systemd/system/sysinit.target.wants/ && \
    (for f in *; do [ "$f" = systemd-tmpfiles-setup.service ] || rm -f "$f"; done) ; \
    rm -f /lib/systemd/system/multi-user.target.wants/* \
          /etc/systemd/system/*.wants/* \
          /lib/systemd/system/local-fs.target.wants/* \
          /lib/systemd/system/sockets.target.wants/*udev* \
          /lib/systemd/system/sockets.target.wants/*initctl* \
          /lib/systemd/system/basic.target.wants/* \
          /lib/systemd/system/anaconda.target.wants/* ; \
    true

STOPSIGNAL SIGRTMIN+3
VOLUME ["/sys/fs/cgroup"]
CMD ["/usr/sbin/init"]
