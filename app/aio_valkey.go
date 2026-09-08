package main

import (
	"context"
	"fmt"
	"strings"
)

// aio_valkey.go — Valkey instances inside an All-in-One node.
//
// Kinds: valkey (standalone) and valkeycluster (an all-master shard set).
//
// Valkey is the easiest family to co-tenant — one unversioned package set, N
// config files — but it carries the sharpest port trap in the whole node type.
// A clustered Valkey's *bus* port defaults to `port + 10000`. With the family
// based at 19000 that would put the bus at 29000+, straight out of the family's
// reserved range and into whatever happens to be there. Every instance therefore
// sets `cluster-port` explicitly to the slot's second port, and announces it, so
// the bus stays inside the slot this instance owns.
//
// `supervised systemd` is equally load-bearing and equally non-obvious: without
// it the Type=notify unit never reports active and sits in "activating" forever
// even though Valkey is up and serving (the same footgun valkey.go documents).

// aioProvisionValkey installs the Valkey packages once, then brings up every
// Valkey instance and forms any clusters.
func (a *App) aioProvisionValkey(ctx context.Context, st Stack, n designNode, doc designDoc, id string, cfg aioConfig, fresh map[string]bool, pr *pxcProg, base, span int) error {
	pr.phase("Installing Valkey", base)
	instScript := valkeyInstallRHEL
	if isDebianOS(n.OS) {
		instScript = valkeyInstallDebian
	}
	if err := a.runStep(ctx, id, instScript,
		[]string{"PKGS=" + valkeyPackages(n.OS), "VER=" + n.AIOValkeyVer}, pr.logln); err != nil {
		return fmt.Errorf("install Valkey: %w", err)
	}
	pr.logln("Valkey installed")
	if err := a.runStep(ctx, id, aioMaskVendorUnits, []string{"UNITS=valkey valkey-server redis"}, pr.logln); err != nil {
		return fmt.Errorf("mask vendor valkey unit: %w", err)
	}
	pr.logln("vendor valkey unit masked — instances own their ports")

	domain := envOr("DOMAIN", "example.net")
	baseDN := domainToDN(domain)
	members := aioMembersOfFamily(cfg, famValkey)
	declBy := map[string]aioInstance{}
	for _, in := range n.AIOInstances {
		if aioFamilyOf(in.Kind) == famValkey {
			declBy[aioSanitizeInst(in.Name)] = in
		}
	}

	for i, m := range members {
		key := m.Group
		if key == "" {
			key = m.Inst
		}
		in, ok := declBy[key]
		if !ok {
			continue
		}
		if !fresh[m.Inst] {
			continue
		}
		pr.phase(fmt.Sprintf("Preparing Valkey instance %s (%d/%d)", m.Inst, i+1, len(members)), base+span/2)
		if err := a.aioValkeyPrepare(ctx, id, n, in, m, domain, baseDN, pr); err != nil {
			return err
		}
	}

	// Form each valkeycluster instance once all its members are up.
	for _, in := range n.AIOInstances {
		if in.Kind != "valkeycluster" {
			continue
		}
		if err := a.aioValkeyFormCluster(ctx, id, in, cfg, pr, base+(span*3)/4); err != nil {
			return err
		}
	}
	return nil
}

// aioValkeyPassword is an instance's default-user password (requirepass).
func aioValkeyPassword(in aioInstance) string {
	if pw := strings.TrimSpace(in.RootPassword); pw != "" {
		return pw
	}
	return envOr("VALKEY_PASSWORD", "valkey_password")
}

// aioValkeyPrepare writes one instance's config and unit and starts it.
func (a *App) aioValkeyPrepare(ctx context.Context, id string, n designNode, in aioInstance, m aioInstanceRuntime, domain, baseDN string, pr *pxcProg) error {
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	if err := a.runStep(ctx, id, l.mkdirScript(), nil, pr.logln); err != nil {
		return fmt.Errorf("%s: create directories: %w", m.Inst, err)
	}
	conf := aioValkeyConf(l, m, in, domain, baseDN, n.OS)
	if err := a.engCtx(ctx).CopyFile(ctx, id, l.ConfDir, "valkey.conf", 0o640, []byte(conf)); err != nil {
		return fmt.Errorf("%s: write valkey.conf: %w", m.Inst, err)
	}
	if err := a.runStep(ctx, id, aioValkeyOwnConfScript, []string{"CONF=" + l.ConfPath}, pr.logln); err != nil {
		return fmt.Errorf("%s: own valkey.conf: %w", m.Inst, err)
	}
	spec := aioUnitSpec{
		Description: fmt.Sprintf("dbcanvas All-in-One Valkey instance %s (port %d)", m.Inst, m.Ports.Client),
		ExecStart:   fmt.Sprintf("/usr/bin/valkey-server %s", l.ConfPath),
		Type:        "notify", // matches `supervised systemd` in the config
		TimeoutSec:  120,
		// AIO_VALKEY_PW lets `aioctl connect` authenticate. The file is root-only
		// (0640) and sits beside valkey.conf, which already holds the same
		// requirepass — the MySQL equivalent is /root/.my.cnf.
		EnvFile: fmt.Sprintf("AIO_INST=%s\nAIO_KIND=%s\nAIO_PORT=%d\nAIO_SOCKET=%s\nAIO_DATADIR=%s\nAIO_CNF=%s\nAIO_VALKEY_PW=%s\n",
			m.Inst, m.Kind, m.Ports.Client, l.Sock, l.DataDir, l.ConfPath, aioValkeyPassword(in)),
	}
	if err := a.aioWriteUnit(ctx, id, l, spec); err != nil {
		return err
	}
	if err := a.runStep(ctx, id, aioStartUnitScript, []string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
		return fmt.Errorf("%s: start: %w", m.Inst, err)
	}
	pr.logln(fmt.Sprintf("%s running on port %d", m.Inst, m.Ports.Client))
	return nil
}

// aioValkeyConf renders one instance's valkey.conf.
func aioValkeyConf(l instLayout, m aioInstanceRuntime, in aioInstance, domain, baseDN, os string) string {
	pw := aioValkeyPassword(in)
	var b strings.Builder
	fmt.Fprintf(&b, "# dbcanvas All-in-One — instance %s (%s). Generated; edits are lost on redeploy.\n", m.Inst, m.Kind)
	if in.UseLDAP {
		// The module must load before its ldap.* directives are parsed.
		b.WriteString("loadmodule " + valkeyLdapModulePath(os) + "\n")
	}
	fmt.Fprintf(&b, "port %d\n", m.Ports.Client)
	b.WriteString("bind 0.0.0.0\nprotected-mode no\n")
	// Required for the Type=notify unit to ever report active — see valkey.go.
	b.WriteString("supervised systemd\n")
	fmt.Fprintf(&b, "requirepass %s\nmasterauth %s\n", pw, pw)
	fmt.Fprintf(&b, "dir %s\n", l.DataDir)
	fmt.Fprintf(&b, "unixsocket %s\nunixsocketperm 770\n", l.Sock)
	fmt.Fprintf(&b, "logfile %s\n", l.LogErr)
	fmt.Fprintf(&b, "pidfile %s/valkey.pid\n", l.RunDir)
	b.WriteString("appendonly yes\n")
	if in.UseLDAP {
		fmt.Fprintf(&b, "ldap.servers ldap://intranet.%s:389\n", domain)
		b.WriteString("ldap.auth_mode bind\nldap.bind_dn_prefix uid=\n")
		fmt.Fprintf(&b, "ldap.bind_dn_suffix ,ou=People,%s\n", baseDN)
		fmt.Fprintf(&b, "ldap.search_base ou=People,%s\n", baseDN)
		b.WriteString("ldap.search_attribute uid\n")
	}
	if in.Kind == "valkeycluster" {
		b.WriteString("cluster-enabled yes\n")
		fmt.Fprintf(&b, "cluster-config-file %s/nodes.conf\n", l.DataDir)
		b.WriteString("cluster-node-timeout 5000\n")
		// THE trap: the bus port defaults to port+10000, which would leave this
		// family's reserved range entirely. Pin it inside the slot and announce
		// both, so peers learn the right bus port rather than inferring it.
		fmt.Fprintf(&b, "cluster-port %d\n", m.Ports.Group)
		fmt.Fprintf(&b, "cluster-announce-ip 127.0.0.1\n")
		fmt.Fprintf(&b, "cluster-announce-port %d\n", m.Ports.Client)
		fmt.Fprintf(&b, "cluster-announce-bus-port %d\n", m.Ports.Group)
	}
	return b.String()
}

// aioValkeyFormCluster runs `valkey-cli --cluster create` across an instance's
// members. All members live in this container, so they are addressed as
// 127.0.0.1:<port> — the ports are the only thing distinguishing them.
func (a *App) aioValkeyFormCluster(ctx context.Context, id string, in aioInstance, cfg aioConfig, pr *pxcProg, pct int) error {
	group := aioSanitizeInst(in.Name)
	var addrs []string
	for _, m := range cfg.Instances {
		if m.Group == group && m.Family == famValkey {
			addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", m.Ports.Client))
		}
	}
	if len(addrs) < 3 {
		return fmt.Errorf("valkey cluster %s needs at least 3 members, has %d", in.Name, len(addrs))
	}
	pr.phase("Forming Valkey cluster "+in.Name, pct)
	if err := a.runStep(ctx, id, aioValkeyClusterCreateScript, []string{
		"ADDRS=" + strings.Join(addrs, " "),
		"PW=" + aioValkeyPassword(in),
	}, pr.logln); err != nil {
		return fmt.Errorf("form valkey cluster %s: %w", in.Name, err)
	}
	pr.logln(fmt.Sprintf("valkey cluster %s formed across %d shard(s)", in.Name, len(addrs)))
	return nil
}

// ------------------------------------------------------------------ scripts

// aioValkeyOwnConfScript hands the generated config to the valkey user — it
// contains requirepass, so it must not be world-readable.
const aioValkeyOwnConfScript = `set -e
chown valkey:valkey "$CONF"
chmod 640 "$CONF"
exit 0`

// aioValkeyClusterCreateScript forms an all-master cluster. Idempotent: if the
// first node already reports cluster_state:ok the create is skipped, so a
// redeploy does not fail against an already-formed cluster.
const aioValkeyClusterCreateScript = `set -e
FIRST=$(echo $ADDRS | awk '{print $1}')
HOST=${FIRST%:*}; PORT=${FIRST##*:}
if valkey-cli -h "$HOST" -p "$PORT" -a "$PW" --no-auth-warning cluster info 2>/dev/null | grep -q "cluster_state:ok"; then
  echo "cluster already formed"
  exit 0
fi
yes yes | valkey-cli -a "$PW" --no-auth-warning --cluster create $ADDRS --cluster-replicas 0 >/dev/null
for i in $(seq 1 30); do
  valkey-cli -h "$HOST" -p "$PORT" -a "$PW" --no-auth-warning cluster info 2>/dev/null | grep -q "cluster_state:ok" && exit 0
  sleep 2
done
echo "cluster did not reach state ok:"
valkey-cli -h "$HOST" -p "$PORT" -a "$PW" --no-auth-warning cluster info 2>/dev/null | head -5
exit 1`
