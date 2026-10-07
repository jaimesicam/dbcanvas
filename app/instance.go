package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// instance.go — this installation's name on the Docker daemon.
//
// Several people can run DBCanvas against one Docker host, each from their own checkout. Before
// this, everything any of them deployed had the same names: both installations' stack 1 was
// dbcanvas-1-<node> on network dbcanvas-stack-1, so the second deploy removed the first one's
// containers (every provisioner clears a container with its target name), a teardown swept the
// other's stack by prefix, and each dashboard showed the other's containers as its own.
//
// DBCANVAS_INSTANCE (docker-compose.yml; `make env` sets dbcanvas-<login>) names the
// installation, and every container, network, volume and K3D cluster it creates carries it.
// The installation that existed before the setting is "dbcanvas", and for it every name comes out
// exactly as it always has — nothing to migrate.
//
// Ownership works on the same names: a container is this installation's only when the instance
// prefix is followed directly by a stack id, so "dbcanvas" never claims "dbcanvas-jane-3-pg".
// For that to hold one way round as well as the other, no dash-separated part of an instance name
// may be all digits — an instance called "dbcanvas-5" would make its containers look like
// stack 5 of the installation called "dbcanvas" (see validInstance).

const legacyInstance = "dbcanvas"

var instanceRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// validInstance says what is wrong with an instance name, or "" when nothing is.
func validInstance(s string) string {
	if !instanceRe.MatchString(s) {
		return "lowercase letters, digits and single dashes, starting with a letter"
	}
	if len(s) > 40 {
		return "at most 40 characters (it prefixes container and K3D node host names, which stop at 63)"
	}
	for _, part := range strings.Split(s, "-") {
		if _, err := strconv.Atoi(part); err == nil {
			return fmt.Sprintf("no part may be only digits (%q) — it would read as a stack id in another installation's names", part)
		}
		// K3D cluster names end -s<stackID>[-<tag>]: a part like "s2" would read as a stack scope.
		if _, err := strconv.Atoi(strings.TrimPrefix(part, "s")); err == nil {
			return fmt.Sprintf("no part may be s followed by digits (%q) — it would read as a stack in K3D cluster names", part)
		}
	}
	return ""
}

// instanceName is this installation's name, read once at start-up (main checks it is valid).
var instanceName = func() string {
	if v := strings.TrimSpace(os.Getenv("DBCANVAS_INSTANCE")); v != "" {
		return v
	}
	return legacyInstance
}()

// instancePrefix starts every container, network and volume name this installation creates.
func instancePrefix() string { return instanceName + "-" }

// instanceTag is what scopes a K3D cluster name to this installation: "" for the legacy instance
// (its clusters keep the names they have), otherwise the instance minus a leading "dbcanvas-",
// which every cluster name would otherwise spend 9 of its 63 characters repeating.
func instanceTag() string {
	if instanceName == legacyInstance {
		return ""
	}
	return strings.TrimPrefix(instanceName, legacyInstance+"-")
}

// stackIDFromInstanceName reads the stack id out of a name this installation gave a stack's
// container: <instance>-<stackID>-…. Anything else — another installation's container, the
// app's own, a transient helper — is not a stack container of ours.
func stackIDFromInstanceName(name string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, instancePrefix())
	if !ok {
		return 0, false
	}
	i := strings.IndexByte(rest, '-')
	if i <= 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(rest[:i], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// stackContainerPrefix is how every container of one stack starts — what teardown sweeps by.
func stackContainerPrefix(stackID int64) string {
	return fmt.Sprintf("%s%d-", instancePrefix(), stackID)
}

// imageRelease is the release every image DBCanvas builds is tagged with — v<VERSION>, the same
// string images/platform.sh's image_release reads from the VERSION file. Installations on one
// daemon share an image only when they are on the same release, so one checkout's `make images`
// cannot replace what another, older or newer, deploys from. Big Hole is the exception: its tag
// is the upstream commit, which already says exactly what is in it.
func imageRelease() string { return "v" + appVersion }

// helperName names a transient container that belongs to no stack (an image build, a probe).
func helperName(kind string, n int64) string {
	return fmt.Sprintf("%s%s-%d", instancePrefix(), kind, n)
}

// releaseTagRe finds a release-tagged image of ours in a validation message, and what it was
// called before images carried a release: dbcanvas-systemd:oraclelinux-9-amd64-v0.0.14 was
// dbcanvas-systemd:oraclelinux-9-amd64, dbcanvas-carsim:v0.0.14 was dbcanvas-carsim:latest.
var releaseTagRe = regexp.MustCompile(`(dbcanvas-[a-z0-9]+):((?:[a-z0-9.]+-)*?)-?(v[0-9][0-9a-z.]*)\b`)

// adoptableImageHints adds, to each "missing image" issue whose image this host has under its
// pre-release name, how to use that one instead of rebuilding: an installation upgraded across
// the change to release-tagged images would otherwise be told to rebuild everything it has.
func (a *App) adoptableImageHints(ctx context.Context, issues []issue) []issue {
	if a.docker == nil {
		return issues
	}
	for i, it := range issues {
		if it.Level != "error" || !strings.Contains(strings.ToLower(it.Message), "image") {
			continue
		}
		m := releaseTagRe.FindStringSubmatch(it.Message)
		if m == nil || m[3] != imageRelease() {
			continue
		}
		legacy := m[1] + ":" + strings.TrimSuffix(m[2], "-")
		if m[2] == "" {
			legacy = m[1] + ":latest"
		}
		if ok, _ := a.docker.ImageExists(ctx, legacy); ok {
			issues[i].Message += " — this host has " + legacy + " from before images were tagged by release; if it was built from this checkout, `make adopt-images` tags it for " + imageRelease() + " without rebuilding"
		}
	}
	return issues
}
