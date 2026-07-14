package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// nodeversion.go — the *deployed* version of a node's engine.
//
// A node's design only carries what was asked for ("8.0", or "" for latest), which is not what
// the operator wants to see on a running node: they want the version that actually got installed
// (PS 8.4.10-10, PSMDB 8.0.26-11, PMM 3.3.1). So each running node is probed once — its engine is
// asked for its version banner — and the answer is persisted into the deployment's config under
// "serverVersion", where the canvas and the node's properties read it.
//
// The probe is lazy (it runs off handleGetStack, in the background, at most once per node per
// process) rather than another step in every provisioner: it is one integration point instead of
// fifteen, and it also fills in stacks that were deployed before this existed. A node whose probe
// fails simply keeps showing its design-time label.

// nodeVersionScripts maps a node type to the command that prints its engine's version banner.
// Types not listed here (intranet — it runs a dozen services, watchtower, vnc) are not probed;
// pulled-image nodes fall back to their image tag (see probeVersion).
var nodeVersionScripts = map[string]string{
	// MySQL family — "…/mysqld  Ver 8.4.10-10 for Linux…"
	"pxc":    "mysqld --version 2>/dev/null",
	"ps":     "mysqld --version 2>/dev/null",
	"mysql":  "mysqld --version 2>/dev/null",
	"innodb": "mysqld --version 2>/dev/null",
	// MongoDB family — "db version v8.0.26-11". A mongos router has no mongod.
	"psm":   "(mongod --version 2>/dev/null || mongos --version 2>/dev/null) | head -1",
	"psmdb": "(mongod --version 2>/dev/null || mongos --version 2>/dev/null) | head -1",
	"psmrs": "(mongod --version 2>/dev/null || mongos --version 2>/dev/null) | head -1",
	// PostgreSQL family — "psql (PostgreSQL) 16.10 - Percona Distribution"
	"pg":      "psql --version 2>/dev/null || postgres --version 2>/dev/null",
	"patroni": "psql --version 2>/dev/null || postgres --version 2>/dev/null",
	"repmgr":  "psql --version 2>/dev/null || postgres --version 2>/dev/null",
	"spock":   "psql --version 2>/dev/null || postgres --version 2>/dev/null",
	// The rest.
	"proxysql":      "proxysql --version 2>/dev/null | head -1",
	"haproxy":       "haproxy -v 2>/dev/null | head -1",
	"valkey":        "valkey-server --version 2>/dev/null | head -1",
	"valkeycluster": "valkey-server --version 2>/dev/null | head -1",
	"openbao":       "bao version 2>/dev/null | head -1",
	"sambaad":       "samba --version 2>/dev/null | head -1",
	"pmm":           "pmm-admin --version 2>/dev/null | head -1",
	"seaweedfs":     "weed version 2>/dev/null | head -1",
	// A k3s node: "k3s version v1.31.5+k3s1 (…)".
	"k3d": "k3s --version 2>/dev/null | head -1",
}

// versionProbeCooldown is how long a fruitless probe waits before it is retried (an engine that
// is still starting up has no version to give yet).
const versionProbeCooldown = 2 * time.Minute

// versionRe pulls the version out of a banner: the first dotted-numeric token, plus whatever
// build/revision suffix hangs off it (8.4.10-10, 2.5.5-1.el9, 16.10).
var versionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z._]+)*`)

// parseVersionBanner extracts a display version from an engine's version output. Returns "" when
// the output has nothing version-shaped in it.
func parseVersionBanner(out string) string {
	// Take the first line that actually mentions a version — some banners lead with a path.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if m := versionRe.FindString(line); m != "" {
			return strings.Trim(m, ".,;:")
		}
	}
	return ""
}

// imageTagVersion returns the tag of an image reference when it looks like a version
// ("quay.io/keycloak/keycloak:26.5.5" → "26.5.5"). A floating tag ("latest") is not a version.
func imageTagVersion(image string) string {
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i+1:], "/") {
		return ""
	}
	tag := image[i+1:]
	if versionRe.FindString(tag) != tag {
		return ""
	}
	return tag
}

// ensureNodeVersions probes, in the background, every running node whose deployed version is not
// recorded yet. Each node is attempted once per process; the result lands in the node's config as
// "serverVersion" and shows up on the next poll. Never blocks the caller.
func (a *App) ensureNodeVersions(st Stack, deps []Deployment) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return
	}
	types := map[string]string{}
	for _, n := range doc.Nodes {
		types[n.ID] = n.Type
	}
	for _, dep := range deps {
		if dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		nodeType := types[dep.NodeID]
		if nodeType == "" || nodeType == "intranet" {
			continue
		}
		var cfg map[string]any
		json.Unmarshal(dep.Config, &cfg)
		if v, _ := cfg["serverVersion"].(string); v != "" {
			continue
		}
		// One probe per node at a time, and — when a probe finds nothing (an engine still
		// starting, a container without the CLI) — no more than one attempt per cooldown, so a
		// polling UI cannot turn a failing probe into an exec every second.
		key := fmt.Sprintf("%d:%s", st.ID, dep.NodeID)
		if prev, loaded := a.versionProbes.LoadOrStore(key, time.Now()); loaded {
			if last, _ := prev.(time.Time); time.Since(last) < versionProbeCooldown {
				continue
			}
			a.versionProbes.Store(key, time.Now())
		}
		go func(dep Deployment, nodeType, image string) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if v := a.probeVersion(ctx, dep.ContainerID, nodeType, image); v != "" {
				a.persistConfigKey(st, dep.NodeID, "serverVersion", v)
				a.versionProbes.Delete(key) // done: a redeploy clears the config and re-probes
			}
		}(dep, nodeType, cfgString(cfg, "image"))
	}
}

// probeVersion asks a node's engine for its version, falling back to the image tag for the
// pulled-image nodes that have no useful CLI (Keycloak, Watchtower).
//
// The probe is tried with bash and then with sh: a k3s node (the K3D frame) is busybox — it has no
// bash at all, so a bash-only probe silently returns nothing for it.
func (a *App) probeVersion(ctx context.Context, containerID, nodeType, image string) string {
	if script, ok := nodeVersionScripts[nodeType]; ok {
		for _, shell := range []string{"bash", "sh"} {
			res, err := a.docker.Exec(ctx, containerID, []string{shell, "-c", script}, nil)
			if err != nil || res.Code != 0 {
				continue
			}
			if v := parseVersionBanner(res.Stdout); v != "" {
				return v
			}
		}
	}
	return imageTagVersion(image)
}

func cfgString(cfg map[string]any, key string) string {
	s, _ := cfg[key].(string)
	return s
}
