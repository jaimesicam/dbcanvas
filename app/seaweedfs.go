package main

import (
	"archive/tar"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SeaweedFS node — an S3-compatible object store used as a backup target for the
// database nodes (xtrabackup/xbcloud, Percona Backup for MongoDB, pgBackRest).
// Like PMM it ships as a ready-made image (chrislusf/seaweedfs) rather than a
// systemd OS image built by `make images`, so it is pulled at deploy and runs
// the all-in-one `weed server -s3` (master + volume + filer + S3 gateway) in one
// container. A single S3 identity (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY) is
// configured from a staged s3.json, one bucket is created, and the S3 port (8333)
// is published to the host. AWS_DEFAULT_REGION is fixed at us-east-1 (SeaweedFS
// ignores the region but S3 clients require one).

const (
	seaweedRepo       = "chrislusf/seaweedfs"
	seaweedDefaultTag = "latest"
	// The S3 API stays on its 8333 default (used in-network by the database nodes
	// for backups). The port published to the host is the 8080 web interface (the
	// volume-server status UI), not S3.
	seaweedS3Port  = 8333
	seaweedWebPort = 8080
	seaweedRegion  = "us-east-1"
	// In-container paths for the optional S3 TLS material (staged before start when
	// TLS is enabled; weed serves the S3 API over HTTPS via -s3.cert.file/-s3.key.file).
	seaweedTLSCert = "/etc/seaweedfs/tls/s3.crt"
	seaweedTLSKey  = "/etc/seaweedfs/tls/s3.key"
)

// seaweedConfig is the non-secret profile shown for a deployed SeaweedFS node.
type seaweedConfig struct {
	Image            string `json:"image"`
	Hostname         string `json:"hostname"` // unique DNS hostname on the stack
	FQDN             string `json:"fqdn"`     // hostname.<domain>
	Alias            string `json:"alias"`    // network alias (== hostname)
	AccessKey        string `json:"accessKey"`
	Bucket           string `json:"bucket"`
	Region           string `json:"region"`
	WebPort          int    `json:"webPort"`          // host port mapped to container 8080 (web UI)
	InternalEndpoint string `json:"internalEndpoint"` // http(s)://<fqdn>:8333 — S3 endpoint for in-stack DB nodes
	TLS              bool   `json:"tls"`              // S3 endpoint served over HTTPS
	GenerateCert     bool   `json:"generateCert"`     // TLS cert signed by the Intranet CA (else self-signed)
}

// seaweedSecrets holds the S3 secret key (generated or user-supplied).
type seaweedSecrets struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// validBucketName enforces the common S3 bucket-name rules (a strict subset that
// SeaweedFS and AWS both accept): 3–63 chars, lowercase letters / digits / dots /
// hyphens, starting and ending with an alphanumeric.
var bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func validBucketName(s string) bool {
	s = strings.TrimSpace(s)
	if !bucketRe.MatchString(s) {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, ".-") || strings.Contains(s, "-.") {
		return false
	}
	return true
}

// genS3Secret returns a 40-char hex secret for the S3 identity.
func genS3Secret() string {
	b := make([]byte, 20)
	crand.Read(b)
	return hex.EncodeToString(b)
}

// seaweedAccessKey is the access key for a node, defaulting to "seaweedfs".
func seaweedAccessKey(n designNode) string {
	ak := strings.TrimSpace(n.AccessKey)
	if ak == "" {
		ak = "seaweedfs"
	}
	return ak
}

// provisionSeaweedFS records the deployment and starts an async provisioning
// goroutine for a SeaweedFS node.
func (a *App) provisionSeaweedFS(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	ref := seaweedRepo + ":" + seaweedDefaultTag
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	if host == "" {
		host = "seaweedfs"
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the secret across redeploys; otherwise take the user's value or
	// generate one when left empty. The access key + bucket come from the design.
	ak := seaweedAccessKey(n)
	var sec seaweedSecrets
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		json.Unmarshal(dep.Secrets, &sec)
	}
	if sec.SecretKey == "" {
		sk := strings.TrimSpace(n.SecretKey)
		if sk == "" {
			sk = genS3Secret()
		}
		sec.SecretKey = sk
	}
	sec.AccessKey = ak

	scheme := "http"
	if n.TLS {
		scheme = "https"
	}
	cfg := seaweedConfig{
		Image: ref, Hostname: host, FQDN: fqdn, Alias: host,
		AccessKey: ak, Bucket: strings.TrimSpace(n.Bucket), Region: seaweedRegion,
		InternalEndpoint: fmt.Sprintf("%s://%s:%d", scheme, fqdn, seaweedS3Port),
		TLS:              n.TLS, GenerateCert: n.TLS && n.GenerateCert,
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID)
	go func() {
		defer endScope()
		prog := &provProgress{Percent: 0, Phase: "Starting", Log: []string{}}
		save := func() { b, _ := json.Marshal(prog); a.store.SetDeploymentProgress(st.ID, n.ID, b) }
		logln := func(s string) {
			prog.Log = append(prog.Log, s)
			if len(prog.Log) > 200 {
				prog.Log = prog.Log[len(prog.Log)-200:]
			}
			save()
		}
		setPhase := func(p string, pct int) { prog.Phase = p; prog.Percent = pct; save() }
		failNode := func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			log.Printf("stack %d seaweedfs %s: %s", st.ID, n.ID, msg)
			prog.Phase = "failed"
			prog.Message = msg
			save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployError)
		}

		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		setPhase("Pulling image", 5)
		logln("ensuring " + ref + " for " + pullPlatform() + " (this can take a while)")
		if err := a.docker.EnsureImage(ctx, seaweedRepo, seaweedDefaultTag, pullPlatform()); err != nil {
			failNode("pull image %s: %v", ref, err)
			return
		}
		logln("image ready: " + ref)

		// The Intranet is the stack's DNS authority, so the SeaweedFS node must not
		// start until it is up — the DB nodes resolve seaweedfs's FQDN through it.
		setPhase("Waiting for Intranet to be ready", 15)
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			failNode("%v", werr)
			return
		}
		logln("Intranet is running (resolver at " + intranetIP + ")")

		// When TLS is on, issue the S3 server certificate up front (Intranet-CA-signed
		// or self-signed) so it can be staged into the container before start.
		var tlsCert, tlsKey []byte
		if n.TLS {
			setPhase("Issuing TLS certificate", 20)
			var caCrt, caKey []byte
			if n.GenerateCert {
				if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
					failNode("%v", err)
					return
				}
				crt, cerr := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
				if cerr != nil || len(crt) == 0 {
					failNode("read Intranet CA cert: %v", cerr)
					return
				}
				key, kerr := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
				if kerr != nil || len(key) == 0 {
					failNode("read Intranet CA key: %v", kerr)
					return
				}
				caCrt, caKey = crt, key
			}
			c, k, cerr := signTLSCert(caCrt, caKey, fqdn, []string{fqdn, host}, certTTL(n.CertTTLValue, n.CertTTLUnit))
			if cerr != nil {
				failNode("issue TLS certificate: %v", cerr)
				return
			}
			tlsCert, tlsKey = c, k
			if n.GenerateCert {
				logln("S3 TLS certificate signed by the Intranet CA")
			} else {
				logln("S3 TLS certificate self-signed")
			}
		}

		setPhase("Creating container", 25)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		cmd := []string{"server", "-dir=/data", "-s3", "-s3.config=/etc/seaweedfs/s3.json"}
		if n.TLS {
			cmd = append(cmd, "-s3.cert.file="+seaweedTLSCert, "-s3.key.file="+seaweedTLSKey)
		}
		id, err := a.docker.ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: ref, Hostname: host, Platform: pullPlatform(),
			Cmd:     cmd,
			Network: networkName(st.ID), Aliases: []string{host},
			PublishPorts: []int{seaweedWebPort},
			DNS:          []string{intranetIP}, DNSSearch: []string{domain},
		})
		if err != nil {
			failNode("create container: %v", err)
			return
		}

		// Stage the S3 identity config into the (not-yet-started) container so
		// `weed server -s3.config=…` reads it on startup.
		s3cfg := seaweedS3ConfigJSON(ak, sec.SecretKey)
		if err := a.docker.PutArchive(ctx, id, "/etc", seaweedTar("seaweedfs/s3.json", s3cfg)); err != nil {
			failNode("stage s3 config: %v", err)
			return
		}
		// Stage the TLS cert+key (world-readable: weed runs as a non-root user).
		if n.TLS {
			if err := a.docker.PutArchive(ctx, id, "/etc", seaweedTLSTar(tlsCert, tlsKey)); err != nil {
				failNode("stage TLS certificate: %v", err)
				return
			}
		}

		if err := a.docker.ContainerStart(ctx, id); err != nil {
			failNode("start container: %v", err)
			return
		}

		// Record the published host port for the web-interface URL.
		if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", seaweedWebPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.WebPort = p
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
		logln(fmt.Sprintf("container started (web UI host port %d)", cfg.WebPort))
		a.trustIntranetCA(ctx, st, id, n.OS, logln)

		// Create the bucket. The step retries, which also serves as the readiness
		// gate (weed shell only succeeds once the master + filer are up).
		setPhase("Creating bucket", 60)
		if err := a.runShStep(ctx, id, seaweedBucketScript, []string{"BUCKET=" + cfg.Bucket}, logln); err != nil {
			failNode("create bucket %s: %v", cfg.Bucket, err)
			return
		}
		logln("bucket ready: " + cfg.Bucket)

		// Publish this node (and refresh all others) in the Intranet DNS zones.
		a.reconcileStackDNS(ctx, st.ID)

		setPhase("Running", 100)
		prog.Message = "provisioned"
		save()
		a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		log.Printf("stack %d seaweedfs %s: provisioned (bucket %s)", st.ID, n.ID, cfg.Bucket)
	}()
}

// runShStep runs a single /bin/sh script in the container with up to 10 retries.
// SeaweedFS uses a minimal (alpine) image without bash, so it can't use runStep.
func (a *App) runShStep(ctx context.Context, id, script string, env []string, logln func(string)) error {
	var lastErr string
	for attempt := 1; attempt <= 10; attempt++ {
		res, err := a.docker.Exec(ctx, id, []string{"sh", "-c", script}, env)
		if err == nil && res.Code == 0 {
			return nil
		}
		if err != nil {
			lastErr = err.Error()
		} else if lastErr = strings.TrimSpace(res.Stderr); lastErr == "" {
			lastErr = strings.TrimSpace(res.Stdout)
		}
		logln(fmt.Sprintf("attempt %d/10 failed: %s", attempt, lastLines(lastErr, 160)))
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("%s", lastLines(lastErr, 160))
}

// certTTL converts a TTL value/unit (as carried on the node) into a duration,
// defaulting to 365 days when unset/invalid.
func certTTL(value int, unit string) time.Duration {
	if value <= 0 {
		return 365 * 24 * time.Hour
	}
	switch unit {
	case "minutes":
		return time.Duration(value) * time.Minute
	case "hours":
		return time.Duration(value) * time.Hour
	default: // "days"
		return time.Duration(value) * 24 * time.Hour
	}
}

// seaweedTLSTar builds an uncompressed tar carrying the S3 cert + key under
// /etc/seaweedfs/tls/ (extracted at /etc), with explicit parent-dir entries so the
// directories are created. World-readable (0644): weed runs as a non-root user, so
// it must be able to read the key; the container is the trust boundary.
func seaweedTLSTar(certPEM, keyPEM []byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, dir := range []string{"seaweedfs/", "seaweedfs/tls/"} {
		tw.WriteHeader(&tar.Header{Name: dir, Mode: 0755, Typeflag: tar.TypeDir, ModTime: time.Now()})
	}
	for _, f := range []struct {
		name    string
		content []byte
	}{{"seaweedfs/tls/s3.crt", certPEM}, {"seaweedfs/tls/s3.key", keyPEM}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0644, ModTime: time.Now(), Size: int64(len(f.content))})
		tw.Write(f.content)
	}
	tw.Close()
	return buf.Bytes()
}

// seaweedS3ConfigJSON builds the SeaweedFS S3 identities config granting the
// single user full access (Admin covers Read/Write/List/Tagging).
func seaweedS3ConfigJSON(accessKey, secretKey string) []byte {
	cfg := map[string]any{
		"identities": []map[string]any{{
			"name": accessKey,
			"credentials": []map[string]any{{
				"accessKey": accessKey,
				"secretKey": secretKey,
			}},
			"actions": []string{"Admin", "Read", "Write", "List", "Tagging"},
		}},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return b
}

// seaweedTar builds an uncompressed tar carrying one file, including an explicit
// parent-directory entry so the target dir is created on extraction. The file is
// world-readable (0644): the seaweedfs image runs `weed` as a non-root user, so a
// root-owned 0600 config is unreadable to it (the container is the trust
// boundary, so this is fine for the S3 credentials).
func seaweedTar(name string, content []byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if dir := strings.TrimSuffix(name[:strings.LastIndex(name, "/")+1], "/"); dir != "" {
		tw.WriteHeader(&tar.Header{Name: dir + "/", Mode: 0755, Typeflag: tar.TypeDir, ModTime: time.Now()})
	}
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, ModTime: time.Now(), Size: int64(len(content))})
	tw.Write(content)
	tw.Close()
	return buf.Bytes()
}

// seaweedBucketScript creates the bucket via `weed shell` (idempotent) and
// verifies it now exists. weed shell talks to the master (localhost:9333) which
// `weed server` runs, so it succeeds only once the server is up — making this
// step double as the readiness gate.
const seaweedBucketScript = `
printf 's3.bucket.create -name %s\n' "$BUCKET" | weed shell >/dev/null 2>&1 || true
printf 's3.bucket.list\n' | weed shell 2>/dev/null | grep -qw "$BUCKET"`
