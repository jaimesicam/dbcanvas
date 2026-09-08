# syntax=docker/dockerfile:1
#
# Pre-baked Intranet image — the systemd Oracle Linux 9 base (BASE_IMAGE) plus every
# package the Intranet node's services are built from. Built by `make images` (and
# `make intranet-image`) as dbcanvas-intranet:oraclelinux-9-<arch>; see intranetImage()
# in app/intranet.go, which is what a deployed Intranet node runs.
#
# Only the package installation lives here. Everything the Intranet is *configured*
# with — its CA, the LDAP suffix and admin credentials, the mail domain, the DNS zones
# — depends on the stack being deployed and stays in intranetSteps() in
# app/intranet.go, which runs against this image in about four seconds.
#
# Run with systemd as PID 1, the same way dbcanvas runs the systemd bases.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

# dnf over IPv4: a host without working IPv6 otherwise stalls on every mirror
# lookup. Mirrors dnfIPv4Script in app/intranet.go, which still runs at deploy for
# packages the operator installs later.
RUN grep -q '^ip_resolve=' /etc/dnf/dnf.conf 2>/dev/null || echo 'ip_resolve=4' >> /etc/dnf/dnf.conf

# EPEL carries roundcubemail; CodeReady Builder carries build-time deps some of the
# EPEL packages link against. Then the services themselves: rsyslog, the Squid proxy,
# bind (DNS), postfix + dovecot (mail), OpenLDAP, and the webmail stack. httpd/php-fpm
# arrive with roundcubemail but are never started — dbcanvas-roundcube.service serves
# Roundcube with php's built-in server instead (see "Configure webmail").
RUN set -eux; \
    (dnf -y install oracle-epel-release-el9 || dnf -y install epel-release); \
    (dnf config-manager --set-enabled ol9_codeready_builder || true); \
    dnf -y install \
      rsyslog squid bind bind-utils postfix dovecot \
      openldap-servers openldap-clients \
      httpd php php-fpm roundcubemail mod_ssl \
      openssl net-tools; \
    dnf clean all; \
    rm -rf /var/cache/dnf
