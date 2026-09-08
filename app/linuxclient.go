package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// Linux Client — a bare systemd host running one of dbcanvas's supported base OS
// images (the same dbcanvas-systemd:* images every other systemd node type uses),
// with nothing installed on top and no PMM monitoring wired up. It exists purely as
// a jump box / test client: join the stack's Intranet DNS and CA trust, then use the
// node's terminal to install and exercise whatever client tools are needed against
// the stack's other nodes.

// linuxClientConfig is the non-secret profile shown for a deployed Linux Client node. A client set
// up for core-dump analysis carries gdbNodeConfig too, embedded so the two halves land in one flat
// JSON object — the node panel and the analyzer read the same record.
type linuxClientConfig struct {
	Image    string `json:"image"`
	OS       string `json:"os"`
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
	UseProxy bool   `json:"useProxy"`
	gdbNodeConfig
}

// provisionLinuxClient records the deployment then boots a single bare OS host —
// no product installed, no PMM.
func (a *App) provisionLinuxClient(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)
	image := pxcImage(n.OS, n.OSVersion, n.Arch)

	cfg := linuxClientConfig{Image: image, OS: n.OS, Hostname: host, FQDN: fqdn, UseProxy: n.UseProxy}
	if n.GDBEnabled {
		cfg.gdbNodeConfig = gdbNodeConfig{
			Enabled: true, CoreDir: path.Clean(n.GDBCoreDir), LibDir: path.Clean(n.GDBLibDir),
			Product: gdbProductOf(n), Major: n.GDBMajor, Version: n.GDBVersion,
			Status: "pending",
		}
	}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Waiting for Intranet to be ready", 10)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 30)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		spec := ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		}
		if cfg.gdbNodeConfig.Enabled {
			// Probe the two host directories *before* creating the node. Docker resolves a bind
			// source on the daemon's side and creates a missing one as an empty directory rather
			// than failing, so a typo here otherwise produces a node that comes up perfectly and
			// an analyzer that says the core directory is empty — with nothing in any log to say
			// the path was wrong. See gdbProbeMounts.
			pr.phase("Checking the mounted paths", 35)
			cores, libs, binaries, err := a.gdbProbeMounts(ctx, image, cfg.CoreDir, cfg.LibDir)
			if err != nil {
				pr.fail("check the mounted paths: %v", err)
				return
			}
			if cores == 0 {
				pr.fail("%s holds no files on the Docker host — check that the core dumps are there and readable", cfg.CoreDir)
				return
			}
			if libs == 0 {
				pr.logln("warning: " + cfg.LibDir + " holds no shared libraries — gdb will fall back to this node's own, which are not the ones the crashed process had")
			}
			// The recipe copies `mysqld` into that directory alongside everything
			// `ldd $(which mysqld)` named. When it is there, it is the binary that crashed and
			// gdb reads it in preference to the installed package's — no version guess can be
			// wrong about the actual bytes.
			if binaries == 0 {
				pr.logln("note: no mysqld in " + cfg.LibDir + " — the installed package's binary will be used instead, so the version picked here has to be exactly the one that crashed")
			}
			pr.logln(fmt.Sprintf("mounting %s (%d file(s)) and %s (%d library file(s), %d mysqld), both read-only",
				cfg.CoreDir, cores, cfg.LibDir, libs, binaries))
			spec.Binds = append(spec.Binds, gdbBinds(cfg.CoreDir, cfg.LibDir)...)
		}
		applyVMSize(&spec, n.limits())
		id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON})

		pr.phase("Waiting for systemd", 55)
		if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}

		a.trustIntranetCA(ctx, st, id, n.OS, pr.logln)
		a.ensureDNFIPv4(ctx, id, n.OS, pr.logln)

		if n.UseProxy {
			pr.phase("Configuring package proxy", 75)
			proxyScript := pkgProxyRHEL
			if isDebianOS(n.OS) {
				proxyScript = pkgProxyDebian
			}
			if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
				pr.fail("configure package proxy: %v", err)
				return
			}
			pr.logln("package egress via Intranet proxy")
		}

		if cfg.gdbNodeConfig.Enabled {
			a.linuxClientInstallGDB(ctx, id, n, &cfg, pr)
			cfgJSON, _ = json.Marshal(cfg)
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// gdbProductOf is the product a Linux Client's debug symbols come from, defaulting to Percona
// Server — the overwhelmingly common case, and the one the field's absence used to mean.
func gdbProductOf(n designNode) string {
	if p := strings.TrimSpace(n.GDBProduct); p != "" {
		return p
	}
	return "ps"
}

// linuxClientInstallGDB installs gdb and the debug symbols for the release the core came from, and
// records what it found.
//
// It never fails the deploy. A node whose symbols did not install is still a usable Linux Client,
// and it is still a node with the core file mounted — you can read the stack there with whatever
// gdb makes of it. What matters is that the *reason* survives into the node's config, because the
// analyzer's first job is telling you whether the backtrace it is about to show means anything.
func (a *App) linuxClientInstallGDB(ctx context.Context, id string, n designNode, cfg *linuxClientConfig, pr *pxcProg) {
	product, major, version := cfg.Product, cfg.Major, cfg.Version
	pkgs := gdbPackages(product, n.OS, major)
	pr.phase("Installing gdb and debug symbols", 80)
	pr.logln(fmt.Sprintf("%s %s%s: %s", gdbProductLabel(product), major,
		map[bool]string{true: " " + version, false: " (latest)"}[version != ""],
		strings.Join(pkgs, ", ")))

	script := gdbInstallRHEL
	if isDebianOS(n.OS) {
		script = gdbInstallDebian
	}
	if err := a.runStep(ctx, id, script, gdbInstallEnv(product, n.OS, major, version), pr.logln); err != nil {
		cfg.Status = "the debug symbols did not install: " + lastLines(err.Error(), 160)
		pr.logln("core-dump analysis will be limited: " + cfg.Status)
		return
	}

	// What actually landed, asked of the node rather than assumed. "The package installed" and
	// "gdb has symbols for this binary" are different statements, and only the second one is
	// worth anything — see gdbPackages on why they come apart.
	info := a.gdbResolveBinary(ctx, id)
	// The package put its debug file where gdb would look for /usr/sbin/mysqld. When the binary
	// being read is the copy in the mount, that is the wrong place — see gdbLinkDebugScript.
	if info.Path != "" && info.From == "mounted" {
		if err := a.runStep(ctx, id, gdbLinkDebugScript, []string{"BIN=" + info.Path}, pr.logln); err != nil {
			pr.logln("could not link the debug files next to the mounted binary: " + err.Error())
		} else {
			info = a.gdbResolveBinary(ctx, id) // ask again, now that they are reachable
		}
	}
	cfg.Binary, cfg.BinaryFrom = info.Path, info.From
	cfg.BuildID, cfg.HasSyms = info.BuildID, info.HasSyms
	switch {
	case info.Path == "":
		cfg.Status = "no mysqld to debug against — neither in the mounted library directory nor from the package"
	case !info.HasSyms:
		cfg.Status = "no separate debug symbols for " + info.Path +
			" — backtraces will name functions without arguments or line numbers"
	default:
		cfg.Status = "ready"
	}
	// Percona's mysqld carries no build-id, so "are these the right symbols?" cannot be answered
	// by comparing ids — it is answered by comparing the binaries. A copy that differs from the
	// package installed here is the one failure that yields a *wrong* stack rather than a poor
	// one: the symbols will happily decorate a different build with the wrong names.
	if info.SameAsInstalled == "no" {
		cfg.Status = "the copied mysqld is not the same build as the " + gdbProductLabel(product) +
			" " + version + " packages installed here — pick the version and OS the crashed server ran"
		pr.logln("warning: " + cfg.Status)
	}
	if cores, err := a.gdbListCores(ctx, id, info.BuildID); err == nil {
		cfg.CoreCount = len(cores)
	}
	pr.logln(fmt.Sprintf("gdb ready: %s (%s), symbols %v%s, %d core file(s) mounted",
		info.Path, map[string]string{"mounted": "the copy from the crashed host", "installed": "from the package"}[info.From],
		info.HasSyms,
		map[string]string{"yes": " (identical to the installed build)", "no": " (DIFFERENT build from the installed one)"}[info.SameAsInstalled],
		cfg.CoreCount))
}

// shortBuildID trims a 40-character hex build-id to something a log line can carry.
func shortBuildID(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	if id == "" {
		return "unknown"
	}
	return id
}
