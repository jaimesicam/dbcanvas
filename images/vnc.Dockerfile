# syntax=docker/dockerfile:1
#
# Pre-baked Ubuntu VNC image — the systemd Ubuntu 24.04 base (BASE_IMAGE) plus the whole
# desktop: XFCE over TigerVNC + noVNC, Firefox, the OpenSSH client and the Percona
# client tools. Built by `make images` (and `make vnc-image`) as
# dbcanvas-vnc:ubuntu-24.04-<arch>; see vncImage() in app/vnc.go, which is what a
# deployed Ubuntu VNC node runs.
#
# Only the package installation lives here. The desktop user, its VNC password, the
# Intranet CA (system store + Firefox policy) and the two systemd units all depend on
# the stack being deployed and stay in app/vnc.go, which runs against this image in
# about two seconds.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive

# The desktop itself: XFCE, TigerVNC (Xvnc on :1), noVNC + websockify for the browser
# client, and the editors/tools an operator reaches for in a jump box.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      xfce4 xfce4-goodies xfce4-terminal dbus-x11 xterm \
      tigervnc-standalone-server tigervnc-common tigervnc-tools \
      novnc websockify python3 openssh-client \
      wget gnupg2 lsb-release curl ca-certificates sudo net-tools nano vim less procps; \
    [ -f /usr/share/novnc/index.html ] || ln -sf /usr/share/novnc/vnc.html /usr/share/novnc/index.html

# Firefox from Mozilla's own APT repository. Ubuntu's "firefox" package is a snap
# transitional that does not run in a container, and the pin is what keeps apt from
# preferring it on a later `apt-get upgrade`.
RUN set -eux; \
    install -d -m 0755 /etc/apt/keyrings; \
    wget -qO /etc/apt/keyrings/packages.mozilla.org.asc https://packages.mozilla.org/apt/repo-signing-key.gpg; \
    echo "deb [signed-by=/etc/apt/keyrings/packages.mozilla.org.asc] https://packages.mozilla.org/apt mozilla main" \
      > /etc/apt/sources.list.d/mozilla.list; \
    printf 'Package: *\nPin: origin packages.mozilla.org\nPin-Priority: 1000\n' > /etc/apt/preferences.d/mozilla; \
    apt-get update; \
    apt-get install -y firefox; \
    firefox --version

# percona-release plus the client tools: the MySQL/PSMDB/PostgreSQL/Valkey shells,
# percona-toolkit, the OpenLDAP client and the Kerberos client (kinit/klist, for GSSAPI
# logins). Each install is best-effort so one renamed package in a future repository
# refresh never fails the build — the desktop user has sudo — but the build reports what
# actually landed, so a missing client is visible here rather than at deploy.
RUN set -eux; \
    wget -qO /tmp/percona-release.deb https://repo.percona.com/apt/percona-release_latest.generic_all.deb; \
    apt-get install -y /tmp/percona-release.deb; \
    for r in ps-80 psmdb-80 ppg-17 valkey-91 tools; do percona-release enable "$r" || true; done; \
    apt-get update; \
    for p in ldap-utils krb5-user percona-server-client percona-mongodb-mongosh \
             percona-postgresql-client-17 percona-toolkit; do \
      apt-get install -y "$p" || echo "WARN: $p not installed"; \
    done; \
    (apt-get install -y percona-valkey-tools || apt-get install -y valkey-tools || echo "WARN: no valkey client"); \
    rm -f /tmp/percona-release.deb; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*; \
    echo "clients present:"; \
    for c in mysql mongosh psql valkey-cli ldapsearch kinit pt-query-digest firefox; do \
      if command -v "$c" >/dev/null 2>&1; then echo "  $c: $(command -v "$c")"; else echo "  $c: MISSING"; fi; \
    done
