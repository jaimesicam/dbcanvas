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
#
# The `tools` repo is enabled alongside it for exactly one package:
# perl-DBD-MySQL, which percona-toolkit hard-requires as perl(DBD::mysql) and
# which the `pt` repo does not build for EL10. Left to the distro, dnf takes
# perl-DBD-MySQL-0:5.007-4.el10 from ol10_appstream, and that one links
# libmysqlclient.so.24 — so it drags mysql8.4-libs + mysql8.4-common into the
# image. Nothing in Percona's repositories Obsoletes mysql8.4-libs
# (percona-server-shared covers only the unversioned mysql-libs, the el8/el9
# spelling), while percona-server-server Obsoletes mysql8.4-common < 99, and
# mysql8.4-libs Requires mysql8.4-common at its exact version. The result is a
# deadlock at install time — "cannot install the best candidate for the job" —
# and every Percona Server / PXC 8.4 and 9.7 node on EL10 fails to provision.
#
# Percona's own perl-DBD-MySQL-1:5.013-3 has no libmysqlclient dependency at all
# (statically linked) and its epoch 1 beats the distro's 0, so enabling `tools`
# is enough to keep the distro MySQL out of the image entirely. This is also why
# EL9 was never affected: there the `pt` repo carries perl-DBD-MySQL itself.
# EL8's `tools` repo has no build, so this line is a no-op there.
#   - net-tools           → ifconfig/netstat/route
#   - openldap-clients    → ldapsearch and friends (OpenLDAP client)
#   - sysstat             → sar/iostat/mpstat
#   - percona-release     → Percona repository manager
#   - percona-toolkit     → pt-* DBA tools
#   - git                 → clone diagnostic tooling (e.g. pg_gather) on DB nodes
#   - iproute-tc          → tc, for the per-node network conditions (see netem.go)
#
# iproute-tc is its own package on RHEL: `iproute` is installed already as a
# dependency and provides `ip`, but *not* `tc`, which is what the network
# shaping needs. Installing iproute and expecting tc is the trap here.
RUN set -eux; \
    yum -y install systemd net-tools openldap-clients sysstat git iproute-tc; \
    yum -y install https://repo.percona.com/yum/percona-release-latest.noarch.rpm; \
    percona-release setup pt; \
    percona-release enable tools release; \
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
