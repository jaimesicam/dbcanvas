package main

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// aio_tls.go — per-instance TLS certificates signed by the Intranet CA.
//
// The classic path (pxcApplyCert) writes one certificate into /var/lib/mysql and
// appends ssl-* to the one my.cnf. Neither survives a container holding several
// servers, and the three engines want the material in different shapes, so this
// issues a certificate per instance into its own tree and wires each engine the
// way that engine expects.
//
// TLS is enabled, never *required*. `preferTLS` on MongoDB and plain `ssl=on`
// elsewhere mean existing plaintext connections — including `aioctl connect` and
// the app's own tooling — keep working, while a client that asks for TLS gets it.
// Requiring TLS on a lab node would break every one of those with no warning.

// aioTLSDir is where an instance's certificate material lives.
func aioTLSDir(l instLayout) string { return l.ConfDir + "/tls" }

// aioTLSEngines are the engines this wires up. Valkey and the proxies are not
// included, so they stay listed in aioUnimplementedOptions.
func aioTLSSupported(kind string) bool {
	switch aioEngineForKind(kind) {
	case "mysql", "postgres", "mongodb":
		return true
	}
	return false
}

// aioApplyTLS issues and installs certificates for every instance that asked for
// one. Runs after the databases are up: the certificate is written, the engine's
// config is pointed at it, and the instance is restarted to pick it up.
//
// Best-effort per instance, like PMM — a certificate failure should not fail a
// node whose databases are running, and the reason lands in the deploy log.
func (a *App) aioApplyTLS(ctx context.Context, st Stack, n designNode, doc designDoc, id string, cfg aioConfig, fresh map[string]bool, pr *pxcProg) {
	want := map[string]aioInstance{}
	for _, in := range n.AIOInstances {
		if in.GenerateCert && aioTLSSupported(in.Kind) {
			want[aioSanitizeInst(in.Name)] = in
		}
	}
	if len(want) == 0 {
		return
	}
	intranetID, _, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
	if err != nil {
		pr.logln("certificates skipped: " + err.Error())
		return
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		pr.logln("certificates skipped: " + err.Error())
		return
	}
	caCrt, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		pr.logln("certificates skipped: read CA cert: " + err.Error())
		return
	}
	caKey, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		pr.logln("certificates skipped: read CA key: " + err.Error())
		return
	}
	// Stage the CA once for the whole node rather than per instance.
	if err := a.engCtx(ctx).PutArchive(ctx, id, "/tmp", tarFiles(map[string]fileEntry{
		"dbca-ca.crt": {0o644, 0, caCrt},
		"dbca-ca.key": {0o600, 0, caKey},
	})); err != nil {
		pr.logln("certificates skipped: stage CA: " + err.Error())
		return
	}
	defer a.runStep(ctx, id, "rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/dbca-ca.srl", nil, func(string) {})

	issued := 0
	for _, m := range cfg.Instances {
		key := m.Group
		if key == "" {
			key = m.Inst
		}
		in, ok := want[key]
		if !ok || !fresh[m.Inst] {
			continue
		}
		if err := a.aioIssueCert(ctx, id, in, m, pr); err != nil {
			pr.logln(m.Inst + ": certificate skipped: " + err.Error())
			continue
		}
		issued++
	}
	if issued > 0 {
		pr.logln(fmt.Sprintf("%d instance(s) issued a certificate from the Intranet CA", issued))
	}
}

// aioIssueCert issues one instance's certificate and wires its engine to it.
func (a *App) aioIssueCert(ctx context.Context, id string, in aioInstance, m aioInstanceRuntime, pr *pxcProg) error {
	l := aioLayout(m.Inst, m.Kind, m.Ports)
	user, group := l.userGroup()
	ttlValue, ttlUnit := in.CertTTLValue, in.CertTTLUnit
	if ttlValue <= 0 {
		ttlValue, ttlUnit = 365, "days"
	}
	switch ttlUnit {
	case "minutes", "hours", "days":
	default:
		ttlUnit = "days"
	}
	engine := aioEngineForKind(m.Kind)

	env := []string{
		// The certificate's CN is the instance's own DNS name, not the node's, so
		// a client verifying the hostname it dialled actually matches.
		"FQDN=" + m.FQDN,
		"DIR=" + aioTLSDir(l),
		"OWNER=" + user, "GROUP=" + group,
		"VALUE=" + strconv.Itoa(ttlValue), "TTLUNIT=" + ttlUnit,
		"ENGINE=" + engine,
	}
	if err := a.runStep(ctx, id, aioCertScript, env, pr.logln); err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	// Point the engine at the material. Each wants it differently, and getting
	// this wrong fails at start rather than at connect.
	var wire []string
	switch engine {
	case "mysql":
		wire = []string{"CNF=" + l.ConfPath, "DIR=" + aioTLSDir(l)}
		if err := a.runStep(ctx, id, aioCertWireMySQL, wire, pr.logln); err != nil {
			return fmt.Errorf("configure: %w", err)
		}
	case "postgres":
		wire = []string{"DATADIR=" + l.DataDir, "DIR=" + aioTLSDir(l)}
		if err := a.runStep(ctx, id, aioCertWirePostgres, wire, pr.logln); err != nil {
			return fmt.Errorf("configure: %w", err)
		}
	case "mongodb":
		wire = []string{"CNF=" + l.ConfPath, "DIR=" + aioTLSDir(l), "OWNER=" + user, "GROUP=" + group}
		if err := a.runStep(ctx, id, aioCertWireMongo, wire, pr.logln); err != nil {
			return fmt.Errorf("configure: %w", err)
		}
	}

	if err := a.runStep(ctx, id, aioRestartUnitScript,
		[]string{"UNIT=" + l.Unit, "LOGERR=" + l.LogErr}, pr.logln); err != nil {
		return fmt.Errorf("restart to apply: %w", err)
	}
	pr.logln(fmt.Sprintf("%s: TLS enabled (CN=%s, %d %s)", m.Inst, m.FQDN, ttlValue, ttlUnit))
	return nil
}

// ------------------------------------------------------------------ scripts

// aioCertScript issues a server and client certificate for one instance from the
// staged CA. Idempotent in the sense that a re-run simply re-issues.
const aioCertScript = `set -e
case "$TTLUNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
CA=/tmp/dbca-ca.crt; CAKEY=/tmp/dbca-ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "CA material missing"; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl not installed in this image"; exit 1; }
install -d -m 0750 -o "$OWNER" -g "$GROUP" "$DIR"
cp -f "$CA" "$DIR/ca.pem"
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/server-key.pem" -out /tmp/s-$$.csr -subj "/O=DBCanvas/CN=$FQDN" >/dev/null
openssl x509 -req -in /tmp/s-$$.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/server-cert.pem" -not_after "$END" >/dev/null
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/client-key.pem" -out /tmp/c-$$.csr -subj "/O=DBCanvas/CN=$FQDN-client" >/dev/null
openssl x509 -req -in /tmp/c-$$.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/client-cert.pem" -not_after "$END" >/dev/null
# MongoDB wants the key and certificate in ONE file; the others want them apart.
if [ "$ENGINE" = mongodb ]; then
  cat "$DIR/server-key.pem" "$DIR/server-cert.pem" > "$DIR/server.pem"
fi
chown -R "$OWNER:$GROUP" "$DIR"
chmod 600 "$DIR"/*-key.pem
[ -f "$DIR/server.pem" ] && chmod 600 "$DIR/server.pem"
chmod 644 "$DIR/ca.pem" "$DIR/server-cert.pem" "$DIR/client-cert.pem"
rm -f /tmp/s-$$.csr /tmp/c-$$.csr
exit 0`

// aioCertWireMySQL appends the ssl-* block to the instance's own my.cnf. TLS is
// offered, not required: a client that asks for it gets it, and everything that
// already connects in plaintext keeps working.
const aioCertWireMySQL = `set -e
sed -i '/^# --- dbcanvas tls ---$/,$d' "$CNF"
{
  echo "# --- dbcanvas tls ---"
  echo "ssl-ca=$DIR/ca.pem"
  echo "ssl-cert=$DIR/server-cert.pem"
  echo "ssl-key=$DIR/server-key.pem"
} >> "$CNF"
exit 0`

// aioCertWirePostgres appends the ssl settings to the instance's postgresql.conf.
// Written as its own block so a re-issue replaces rather than stacks.
const aioCertWirePostgres = `set -e
CONF="$DATADIR/postgresql.conf"
[ -f "$CONF" ] || { echo "postgresql.conf not found at $CONF"; exit 1; }
sed -i '/^# --- dbcanvas tls ---$/,$d' "$CONF"
{
  echo "# --- dbcanvas tls ---"
  echo "ssl = on"
  echo "ssl_ca_file = '$DIR/ca.pem'"
  echo "ssl_cert_file = '$DIR/server-cert.pem'"
  echo "ssl_key_file = '$DIR/server-key.pem'"
} >> "$CONF"
chown postgres:postgres "$CONF"
exit 0`

// aioCertWireMongo adds a TLS block to the instance's mongod.conf.
//
// preferTLS, not requireTLS: requiring it would immediately break every existing
// plaintext connection — the replica-set members talking to each other, aioctl
// connect, and the app's own tooling — with no way for the user to see why.
const aioCertWireMongo = `set -e
[ -f "$CNF" ] || { echo "mongod.conf not found at $CNF"; exit 1; }
# Strip any previous block, including its indented children.
sed -i '/^# --- dbcanvas tls ---$/,$d' "$CNF"
{
  echo "# --- dbcanvas tls ---"
  echo "# appended last so it wins over anything above"
} >> "$CNF"
python3 - "$CNF" "$DIR" <<'PY'
import sys
conf, d = sys.argv[1], sys.argv[2]
lines = [l for l in open(conf).read().splitlines() if not l.startswith("# --- dbcanvas tls ---") and not l.startswith("# appended last")]
out, in_net = [], False
for l in lines:
    if in_net and l and not l.startswith(" "):
        in_net = False
    if in_net and l.strip().startswith("tls:"):
        continue
    out.append(l)
    if l.startswith("net:"):
        in_net = True
        out.append("  tls:")
        out.append("    mode: preferTLS")
        out.append("    certificateKeyFile: %s/server.pem" % d)
        out.append("    CAFile: %s/ca.pem" % d)
open(conf, "w").write("\n".join(out) + "\n")
PY
exit 0`

// aioRestartUnitScript restarts an instance and waits for it to come back, so a
// config change that breaks startup surfaces here rather than later.
const aioRestartUnitScript = `set -e
systemctl restart "$UNIT" || true
for i in $(seq 1 60); do
  systemctl is-active --quiet "$UNIT" && exit 0
  sleep 1
done
echo "unit $UNIT did not come back after restart:"
systemctl status --no-pager --lines=15 "$UNIT" 2>&1 | tail -20
[ -n "$LOGERR" ] && tail -20 "$LOGERR" 2>/dev/null
exit 1`
