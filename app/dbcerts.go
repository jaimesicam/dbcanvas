package main

import (
	"fmt"
	"net/http"
	"strings"
)

// Database client-certificate service on the Intranet node.
//
// The Intranet holds the stack CA (/etc/pki/dbcanvas/ca.{crt,key}). These handlers
// issue CA-signed X.509 *client* certificates — one per username, CN=<username> —
// that a MySQL, PostgreSQL or MongoDB user can present for TLS/mutual-TLS auth. The
// key + cert are stored on the Intranet under dbCertDir and read back for the operator
// to copy; regenerating for an existing username overwrites it. All openssl work runs
// in the container (the image ships openssl); user input reaches the scripts only via
// the exec environment (never string-interpolated) and is validated first.

const dbCertDir = "/etc/pki/dbcanvas/dbcerts"

// dbCertEntry is one issued certificate as listed to the UI.
type dbCertEntry struct {
	Username string `json:"username"`
	NotAfter string `json:"notAfter"`
	Subject  string `json:"subject"`
}

// validCertUser guards the username used as both the certificate CN and the on-disk
// basename. Reuses validName (letters, digits, dot, underscore, hyphen; ≤64), rejects
// the dot-only names, and requires at least one alphanumeric — so it is always a safe,
// non-traversing basename (validName already forbids '/').
func validCertUser(s string) bool {
	if !validName(s) || s == "." || s == ".." {
		return false
	}
	return strings.ContainsFunc(s, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	})
}

// dbCertListScript prints one line per issued cert: username<TAB>notAfter<TAB>subject.
const dbCertListScript = `DIR=` + dbCertDir + `
[ -d "$DIR" ] || exit 0
for f in "$DIR"/*.crt; do
  [ -e "$f" ] || continue
  u=$(basename "$f" .crt)
  end=$(openssl x509 -in "$f" -noout -enddate 2>/dev/null | sed 's/notAfter=//')
  subj=$(openssl x509 -in "$f" -noout -nameopt RFC2253 -subject 2>/dev/null | sed 's/^subject= *//')
  printf '%s\t%s\t%s\n' "$u" "$end" "$subj"
done`

// dbCertGenerateScript issues (overwriting) a CA-signed client certificate for $CN with
// a $VALUE/$UNIT lifetime, storing <CN>.crt/<CN>.key under dbCertDir. Prints notAfter.
// The cert carries clientAuth+serverAuth EKUs so it works for client auth in every
// engine (and, if desired, as a server cert). Signed by the Intranet CA.
const dbCertGenerateScript = `set -e
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
DIR=` + dbCertDir + `
CA=/etc/pki/dbcanvas/ca.crt; CAKEY=/etc/pki/dbcanvas/ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "Intranet CA material missing (is this the Intranet node?)"; exit 1; }
install -d -m 0755 "$DIR"
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/$CN.key" -out /tmp/db-$CN.csr -subj "/O=DBCanvas/CN=$CN" >/dev/null 2>&1
cat >/tmp/db-$CN.ext <<EXT
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=clientAuth,serverAuth
subjectAltName=DNS:$CN
EXT
openssl x509 -req -in /tmp/db-$CN.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial \
  -out "$DIR/$CN.crt" -extfile /tmp/db-$CN.ext -not_after "$END" >/dev/null 2>&1
chmod 600 "$DIR/$CN.key"; chmod 644 "$DIR/$CN.crt"
rm -f /tmp/db-$CN.csr /tmp/db-$CN.ext
openssl x509 -in "$DIR/$CN.crt" -noout -enddate | sed 's/notAfter=//'`

// dbCertInfoScript prints notAfter= and subject= (RFC2253) for the $CN cert.
const dbCertInfoScript = `DIR=` + dbCertDir + `
[ -f "$DIR/$CN.crt" ] || { echo "missing"; exit 1; }
echo "notAfter=$(openssl x509 -in "$DIR/$CN.crt" -noout -enddate | sed 's/notAfter=//')"
echo "subject=$(openssl x509 -in "$DIR/$CN.crt" -noout -nameopt RFC2253 -subject | sed 's/^subject= *//')"`

// dbCertDeleteScript removes the $CN cert + key.
const dbCertDeleteScript = `DIR=` + dbCertDir + `
rm -f "$DIR/$CN.crt" "$DIR/$CN.key"`

func (a *App) handleDBCertList(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, dbCertListScript, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	certs := []dbCertEntry{}
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		p := strings.SplitN(ln, "\t", 3)
		e := dbCertEntry{Username: p[0]}
		if len(p) > 1 {
			e.NotAfter = p[1]
		}
		if len(p) > 2 {
			e.Subject = p[2]
		}
		certs = append(certs, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"certs": certs})
}

func (a *App) handleDBCertGenerate(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Username string `json:"username"`
		Value    int    `json:"value"`
		Unit     string `json:"unit"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	b.Username = strings.TrimSpace(b.Username)
	if !validCertUser(b.Username) {
		writeErr(w, http.StatusBadRequest, "username must be 1–64 chars: letters, digits, dot, underscore, hyphen")
		return
	}
	if b.Value <= 0 {
		b.Value, b.Unit = 365, "days"
	}
	switch b.Unit {
	case "minutes", "hours", "days":
	default:
		b.Unit = "days"
	}
	env := []string{"CN=" + b.Username, fmt.Sprintf("VALUE=%d", b.Value), "UNIT=" + b.Unit}
	if _, err := a.execScript(r.Context(), dep.ContainerID, dbCertGenerateScript, env); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.writeDBCert(w, r, dep, b.Username)
}

func (a *App) handleDBCertGet(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	user := strings.TrimSpace(r.PathValue("user"))
	if !validCertUser(user) {
		writeErr(w, http.StatusBadRequest, "invalid username")
		return
	}
	a.writeDBCert(w, r, dep, user)
}

func (a *App) handleDBCertDelete(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Username string `json:"username"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	b.Username = strings.TrimSpace(b.Username)
	if !validCertUser(b.Username) {
		writeErr(w, http.StatusBadRequest, "invalid username")
		return
	}
	if _, err := a.execScript(r.Context(), dep.ContainerID, dbCertDeleteScript, []string{"CN=" + b.Username}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// writeDBCert reads a user's cert + key (and the CA cert) from the Intranet container
// and returns them, along with the parsed notAfter + subject, for the operator to copy.
func (a *App) writeDBCert(w http.ResponseWriter, r *http.Request, dep Deployment, user string) {
	ctx := r.Context()
	crt, err := a.readContainerFile(ctx, dep.ContainerID, dbCertDir+"/"+user+".crt")
	if err != nil {
		writeErr(w, http.StatusNotFound, "no certificate for "+user)
		return
	}
	key, err := a.readContainerFile(ctx, dep.ContainerID, dbCertDir+"/"+user+".key")
	if err != nil {
		writeErr(w, http.StatusNotFound, "no key for "+user)
		return
	}
	ca, _ := a.readContainerFile(ctx, dep.ContainerID, "/etc/pki/dbcanvas/ca.crt")
	notAfter, subject := "", ""
	if info, e := a.execScript(ctx, dep.ContainerID, dbCertInfoScript, []string{"CN=" + user}); e == nil {
		for _, ln := range strings.Split(strings.TrimSpace(info), "\n") {
			if v, ok := strings.CutPrefix(ln, "notAfter="); ok {
				notAfter = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(ln, "subject="); ok {
				subject = strings.TrimSpace(v)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": user,
		"notAfter": notAfter,
		"subject":  subject,
		"cert":     string(crt),
		"key":      string(key),
		"caCert":   string(ca),
	})
}
