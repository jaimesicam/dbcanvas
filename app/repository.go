package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"
)

// repository.go — the Repository node: a yum/apt mirror of the Percona repositories and a Docker
// registry, carrying only what the design names, that other nodes install from.
//
// ONE CONTAINER, TWO SERVERS. An Ubuntu 24.04 systemd node running nginx on port 80 (the package
// trees, the Helm chart repository, the operator manifests and a browsable index.html) and the
// distribution's docker-registry on 5000. Both ports are published to the host so the index can be
// opened from a browser and the registry pulled from outside the stack; inside it the node is
// <host>.<DOMAIN>, served by the Intranet's bind like every other node — and also a Docker network
// alias, which is what a k3s node's containerd resolves through (k3d nodes do not use bind).
//
// WHAT IS MIRRORED, AND HOW IT STAYS SMALL. See repo_meta.go: per repository, OS release and
// architecture, the upstream metadata is read and only the chosen builds are fetched. The RPMs
// keep Percona's signatures, so a node's gpgcheck against PERCONA-PACKAGING-KEY still passes over
// metadata this node regenerated (createrepo_c). An apt Release file cannot keep Percona's
// signature once its Packages list is a subset, so the node signs its own with a key it generates
// on first deploy, published as /dbcanvas-repo.gpg, and a node using the mirror is pointed at it.
//
// HOW ANOTHER NODE USES IT. A node's "Repository" association (RepositoryNodeID, on nodes and
// frames) installs dbcanvas-repo-rewrite plus thin dnf/yum/apt-get/apt wrappers in /usr/local/bin.
// Before every package-manager run the script rewrites repo.percona.com to this node in whatever
// .repo/.list files exist — the ones percona-release writes and the ones DBCanvas writes by hand
// for the series it cannot enable — but only for the repositories this node actually carries for
// that OS release and architecture, a list it re-reads from /dbcanvas/carried/ each time. So
// nothing in any provisioner had to learn about mirrors, a repository added here after the fact is
// picked up by nodes already running, and a repository not carried keeps coming from upstream
// (unless the Repository is strict, which disables those instead).
//
// A K3D frame associated with it gets a registries.yaml (docker.io and every other registry the
// node holds images from, mirrored to :5000), and its operator source from here when this node
// carries that operator version — the same tarball GitHub serves.

const (
	repoRoot         = "/var/lib/dbcanvas-repo"
	repoWWW          = repoRoot + "/www"
	repoRegistryDir  = repoRoot + "/registry"
	repoGnupgHome    = repoRoot + "/gnupg"
	repoHTTPPort     = 80
	repoRegistryPort = 5000
	repoUpstream     = "http://repo.percona.com"
	repoHelmIndexURL = "https://percona.github.io/percona-helm-charts/index.yaml"
	repoOSImage      = "ubuntu"
	repoOSVersion    = "24.04"
)

// repoOperatorSpec is an operator release to carry: its bundle.yaml / cr.yaml / secrets.yaml, the
// source tarball, the images those manifests name, and its two Helm charts.
type repoOperatorSpec struct {
	Kind    string `json:"kind"`    // pxc | ps | psmdb | pg
	Version string `json:"version"` // "" → the catalog's latest
}

// repoOperatorCharts are the percona-helm-charts chart names for an operator kind.
var repoOperatorCharts = map[string][]string{
	"pxc":   {"pxc-operator", "pxc-db"},
	"ps":    {"ps-operator", "ps-db"},
	"psmdb": {"psmdb-operator", "psmdb-db"},
	"pg":    {"pg-operator", "pg-db"},
}

// repoTarget is an OS release a Repository can carry packages for.
type repoTarget struct {
	OS       string `json:"os"`
	Version  string `json:"version"`
	Label    string `json:"label"`
	Family   string `json:"family"`             // "yum" | "apt"
	Codename string `json:"codename,omitempty"` // apt only
}

func (t repoTarget) ID() string { return t.OS + "-" + t.Version }

// repoTargets is every OS release a node can run (images.yaml's matrix, plus CentOS 7's el7
// repositories are not offered: percona-release stopped publishing for it).
var repoTargets = []repoTarget{
	{OS: "oraclelinux", Version: "8", Label: "Oracle Linux 8 (el8)", Family: "yum"},
	{OS: "oraclelinux", Version: "9", Label: "Oracle Linux 9 (el9)", Family: "yum"},
	{OS: "oraclelinux", Version: "10", Label: "Oracle Linux 10 (el10)", Family: "yum"},
	{OS: "ubuntu", Version: "22.04", Label: "Ubuntu 22.04 (jammy)", Family: "apt", Codename: "jammy"},
	{OS: "ubuntu", Version: "24.04", Label: "Ubuntu 24.04 (noble)", Family: "apt", Codename: "noble"},
	{OS: "debian", Version: "12", Label: "Debian 12 (bookworm)", Family: "apt", Codename: "bookworm"},
	{OS: "debian", Version: "13", Label: "Debian 13 (trixie)", Family: "apt", Codename: "trixie"},
}

func repoTargetByID(id string) (repoTarget, bool) {
	for _, t := range repoTargets {
		if t.ID() == id {
			return t, true
		}
	}
	return repoTarget{}, false
}

// repoTargetForNode is the target a consuming node's OS maps to, by major release (a node on
// "9" or "9.4" both read el9).
func repoTargetForNode(os, osVersion string) (repoTarget, bool) {
	v := strings.TrimSpace(osVersion)
	for _, t := range repoTargets {
		if t.OS != os {
			continue
		}
		if t.Family == "yum" {
			if major, _, _ := strings.Cut(v, "."); major == t.Version {
				return t, true
			}
		} else if v == t.Version {
			return t, true
		}
	}
	return repoTarget{}, false
}

// repoArchName is an architecture as each family spells it.
func repoArchName(family, arch string) string {
	if family == "yum" {
		if arch == "arm64" {
			return "aarch64"
		}
		return "x86_64"
	}
	if arch == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// repoCarriedKey names the list of repositories carried for one OS release + architecture — the
// file dbcanvas-repo-rewrite reads, computed on the consuming node from /etc/os-release and
// uname/dpkg, so both sides must spell it identically.
func repoCarriedKey(t repoTarget, arch string) string {
	if t.Family == "yum" {
		return "yum-" + t.Version + "-" + repoArchName("yum", arch)
	}
	return "apt-" + t.Codename + "-" + repoArchName("apt", arch)
}

// repositoryConfig is a deployed Repository's profile: what it was asked to carry and what it holds.
type repositoryConfig struct {
	Image        string             `json:"image"`
	Hostname     string             `json:"hostname"`
	FQDN         string             `json:"fqdn"`
	HTTPPort     int                `json:"httpPort"`     // host port → 80
	RegistryPort int                `json:"registryPort"` // host port → 5000
	UseProxy     bool               `json:"useProxy"`
	Domain       string             `json:"domain"`
	Targets      []string           `json:"targets"`
	Arches       []string           `json:"arches"`
	Packages     []repoPkgSpec      `json:"packages"`
	Images       []string           `json:"images"`
	Operators    []repoOperatorSpec `json:"operators"`
	IncludeDebug bool               `json:"includeDebug"`
	Strict       bool               `json:"strict"`
	// Added is what was asked for after deploy, from the node's "Add packages" tab. It is kept
	// across a redeploy, alongside the design's own lists, because the data volume is.
	Added    repoAdditions  `json:"added"`
	Contents repoContents   `json:"contents"`
	Sync     repoSyncStatus `json:"sync"`
}

type repoAdditions struct {
	Packages  []repoPkgSpec      `json:"packages,omitempty"`
	Images    []string           `json:"images,omitempty"`
	Operators []repoOperatorSpec `json:"operators,omitempty"`
}

// repoContents is what a sync found and fetched.
type repoContents struct {
	Repos     []repoMirrored    `json:"repos"`
	Images    []repoImage       `json:"images"`
	Operators []repoOperatorRec `json:"operators"`
	Charts    []repoChart       `json:"charts"`
	DiskBytes int64             `json:"diskBytes"`
	UpdatedAt string            `json:"updatedAt"`
}

type repoMirrored struct {
	Repo     string   `json:"repo"`
	Target   string   `json:"target"`
	Arch     string   `json:"arch"`
	Family   string   `json:"family"`
	Path     string   `json:"path"` // URL path of the tree on this node
	Packages int      `json:"packages"`
	Bytes    int64    `json:"bytes"`
	Versions []string `json:"versions"` // what the specs asked for ("" → latest)
	Error    string   `json:"error,omitempty"`
}

type repoImage struct {
	Source string `json:"source"` // as named, e.g. percona/percona-xtradb-cluster:8.0.42
	Local  string `json:"local"`  // path in this registry, e.g. percona/percona-xtradb-cluster:8.0.42
	From   string `json:"from,omitempty"`
	Error  string `json:"error,omitempty"`
}

type repoOperatorRec struct {
	Kind    string   `json:"kind"`
	Version string   `json:"version"`
	Path    string   `json:"path"`
	Images  []string `json:"images"`
	Charts  []string `json:"charts"`
	Error   string   `json:"error,omitempty"`
}

type repoChart struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	AppVersion string `json:"appVersion"`
	File       string `json:"file"`
}

type repoSyncStatus struct {
	State    string   `json:"state"` // "" | running | done | error
	Phase    string   `json:"phase"`
	Started  string   `json:"started"`
	Finished string   `json:"finished"`
	Message  string   `json:"message"`
	Log      []string `json:"log"`
}

func repoDataVolume(stackID int64, nodeID string) string {
	return fmt.Sprintf("dbcanvas-repo-%d-%s", stackID, nodeID)
}

func repositoryImage(arch string) string { return pxcImage(repoOSImage, repoOSVersion, archOr(arch)) }

// repoTargetsOf / repoArchesOf are a node's lists with the defaults filled in: Oracle Linux 9,
// and the one architecture this installation builds for.
func repoTargetsOf(n designNode) []string {
	if len(n.RepoTargets) == 0 {
		return []string{"oraclelinux-9"}
	}
	return n.RepoTargets
}

func repoArchesOf(n designNode) []string {
	if len(n.RepoArches) == 0 {
		return []string{platformArch()}
	}
	return n.RepoArches
}

// effective lists: the design's plus what was added after deploy, de-duplicated.
func (c *repositoryConfig) allPackages() []repoPkgSpec {
	var out []repoPkgSpec
	for _, p := range append(append([]repoPkgSpec{}, c.Packages...), c.Added.Packages...) {
		if p.Repo = strings.TrimSpace(p.Repo); p.Repo != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *repositoryConfig) allOperators() []repoOperatorSpec {
	seen := map[string]bool{}
	var out []repoOperatorSpec
	cat := loadOperatorCatalog()
	for _, o := range append(append([]repoOperatorSpec{}, c.Operators...), c.Added.Operators...) {
		// A version the catalog does not know is kept as asked: the tarball fetch is what decides,
		// and it says so in the log.
		if v, ok := cat.resolveOperatorVersion(o.Kind, strings.TrimSpace(o.Version)); ok {
			o.Version = v
		}
		if k := o.Kind + "@" + o.Version; !seen[k] {
			seen[k] = true
			out = append(out, o)
		}
	}
	return out
}

func (c *repositoryConfig) explicitImages() []string {
	var out []string
	for _, i := range append(append([]string{}, c.Images...), c.Added.Images...) {
		if i = strings.TrimSpace(i); i != "" {
			out = appendUnique(out, i)
		}
	}
	return out
}

// ---------------------------------------------------------------- validation

var (
	repoNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	repoImageRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$`)
)

// repositoryIssues checks a Repository node's own design.
func repositoryIssues(n designNode) []issue {
	var out []issue
	e := func(f string, a ...any) { out = append(out, issue{Level: "error", Message: fmt.Sprintf(f, a...)}) }
	for _, t := range repoTargetsOf(n) {
		if _, ok := repoTargetByID(t); !ok {
			e("Repository %s: %q is not an OS release it can carry", n.Label, t)
		}
	}
	for _, a := range repoArchesOf(n) {
		if a != "amd64" && a != "arm64" {
			e("Repository %s: architecture %q is neither amd64 nor arm64", n.Label, a)
		}
	}
	out = append(out, repoSpecIssues(n.Label, n.RepoPackages, n.RepoImages, n.RepoOperators)...)
	named := 0
	for _, p := range n.RepoPackages {
		if strings.TrimSpace(p.Repo) != "" {
			named++
		}
	}
	if named == 0 && len(n.RepoImages) == 0 && len(n.RepoOperators) == 0 {
		out = append(out, issue{Level: "warning", Message: "Repository " + n.Label + " carries nothing yet — add packages, images or an operator (they can also be added once it is running)"})
	}
	return out
}

// repoSpecIssues checks package/image/operator lists — the design's, and a later "add".
func repoSpecIssues(label string, pkgs []repoPkgSpec, images []string, ops []repoOperatorSpec) []issue {
	var out []issue
	e := func(f string, a ...any) { out = append(out, issue{Level: "error", Message: fmt.Sprintf(f, a...)}) }
	blank := 0
	for _, p := range pkgs {
		// A row added in the form and not filled in yet is not a mistake worth refusing a deploy
		// over — it is ignored (allPackages drops it) and said so, once.
		if strings.TrimSpace(p.Repo) == "" {
			blank++
			continue
		}
		if !repoNameRE.MatchString(strings.TrimSpace(p.Repo)) {
			e("Repository %s: %q is not a repository name (e.g. ps-84-lts, ppg-17, psmdb-80)", label, p.Repo)
		}
		for _, g := range p.Packages {
			if _, err := path.Match(strings.TrimSpace(g), ""); err != nil {
				e("Repository %s: package pattern %q in %s is malformed", label, g, p.Repo)
			}
		}
	}
	if blank > 0 {
		out = append(out, issue{Level: "warning", Message: fmt.Sprintf("Repository %s has %d package row(s) with no repository name — ignored; fill them in or remove them", label, blank)})
	}
	for _, i := range images {
		if i = strings.TrimSpace(i); i != "" && !repoImageRE.MatchString(i) {
			e("Repository %s: %q is not an image reference", label, i)
		}
	}
	for _, o := range ops {
		if _, ok := repoOperatorCharts[o.Kind]; !ok {
			e("Repository %s: operator %q is not one it can carry (pxc, ps, psmdb or pg)", label, o.Kind)
		}
	}
	return out
}

// repositoryConsumerIssues checks every node and frame that names a Repository: that it exists,
// and that it carries anything for that node's OS release — a mismatch is a warning, not an error,
// because the node still installs, from upstream.
func repositoryConsumerIssues(doc designDoc) []issue {
	repos := map[string]designNode{}
	for _, n := range doc.Nodes {
		if n.Type == "repository" {
			repos[n.ID] = n
		}
	}
	var out []issue
	check := func(label, repoID, os, osVersion, arch string, k3d bool) {
		if repoID == "" {
			return
		}
		r, ok := repos[repoID]
		if !ok {
			out = append(out, issue{Level: "error", Message: label + " uses a Repository that is not on the canvas — add one or clear the association"})
			return
		}
		if k3d {
			// The images are copied for the Repository's architectures, and a k3s node runs
			// K3D_PLATFORM's — which is set apart from DOCKER_PLATFORM precisely so it can differ.
			_, ka, _ := strings.Cut(k3dPlatform(), "/")
			for _, x := range repoArchesOf(r) {
				if x == ka {
					return
				}
			}
			out = append(out, issue{Level: "warning", Message: label + " runs " + ka + " k3s nodes (K3D_PLATFORM), but Repository " + r.Label + " copies images for " + strings.Join(repoArchesOf(r), ", ") + " only — tick " + ka + " on it, or pods will be handed images they cannot run"})
			return
		}
		t, ok := repoTargetForNode(os, osVersion)
		if !ok {
			out = append(out, issue{Level: "warning", Message: label + " runs " + os + " " + osVersion + ", which no Repository can carry — its packages come from repo.percona.com"})
			return
		}
		carried := false
		for _, id := range repoTargetsOf(r) {
			carried = carried || id == t.ID()
		}
		if !carried {
			out = append(out, issue{Level: "warning", Message: label + " runs " + t.Label + ", which Repository " + r.Label + " does not carry — its packages come from repo.percona.com"})
			return
		}
		a := archOr(arch)
		archOK := false
		for _, x := range repoArchesOf(r) {
			archOK = archOK || x == a
		}
		if !archOK {
			out = append(out, issue{Level: "warning", Message: label + " is " + a + ", which Repository " + r.Label + " does not carry — its packages come from repo.percona.com"})
		}
	}
	for _, n := range doc.Nodes {
		if n.FrameID == "" && n.Type != "repository" {
			check("Node "+n.Label, n.RepositoryNodeID, n.OS, n.OSVersion, n.Arch, false)
		}
	}
	for _, f := range doc.Frames {
		check("Frame "+f.Label, f.RepositoryNodeID, f.OS, f.OSVersion, f.Arch, f.Type == "k3d")
	}
	return out
}

// ---------------------------------------------------------------- provisioning

// repositoryInstallScript installs the two servers and the tools a sync uses.
const repositoryInstallScript = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq --no-install-recommends nginx docker-registry skopeo createrepo-c dpkg-dev \
  apt-utils gnupg curl ca-certificates zstd xz-utils >/dev/null
install -d ` + repoWWW + `/percona/yum ` + repoWWW + `/dbcanvas/carried ` + repoWWW + `/charts ` + repoWWW + `/operators ` + repoRegistryDir + `
install -d -m 700 ` + repoGnupgHome + `
chown -R docker-registry: ` + repoRegistryDir + ` 2>/dev/null || true
`

// repositoryConfigureScript writes the nginx and registry configuration, creates the apt signing
// key once (it lives on the data volume, so a redeploy keeps it and nodes keep trusting it), and
// starts both services.
const repositoryConfigureScript = `set -e
cat >/etc/nginx/sites-available/default <<'NGINX'
server {
  listen 80 default_server;
  listen [::]:80 default_server;
  server_name _;
  root ` + repoWWW + `;
  index index.html;
  autoindex on;
  autoindex_exact_size off;
  autoindex_localtime on;
  location ~ \.(yaml|yml|txt|asc|repo|list|json)$ {
    types { text/plain yaml yml txt asc repo list; application/json json; }
  }
  # The registry, same origin: index.html lists its catalog, and a client configured for plain
  # http can pull from port 80 as well as 5000.
  location /v2/ {
    proxy_pass http://127.0.0.1:5000;
    proxy_set_header Host $http_host;
    proxy_read_timeout 900;
    client_max_body_size 0;
  }
}
NGINX
install -d /etc/docker/registry
cat >/etc/docker/registry/config.yml <<'REG'
version: 0.1
log:
  level: warn
storage:
  filesystem:
    rootdirectory: ` + repoRegistryDir + `
  delete:
    enabled: true
http:
  addr: :5000
  headers:
    X-Content-Type-Options: [nosniff]
REG
export GNUPGHOME=` + repoGnupgHome + `
if ! gpg --batch --list-secret-keys 2>/dev/null | grep -q sec; then
  gpg --batch --passphrase '' --quick-gen-key "DBCanvas Repository $LABEL <repo@$DOMAIN>" rsa3072 sign never >/dev/null 2>&1
fi
gpg --batch --yes --export >` + repoWWW + `/dbcanvas-repo.gpg
gpg --batch --yes --armor --export >` + repoWWW + `/dbcanvas-repo.asc
[ -s ` + repoWWW + `/percona/yum/PERCONA-PACKAGING-KEY ] || curl -fsSL --retry 3 -o ` + repoWWW + `/percona/yum/PERCONA-PACKAGING-KEY https://repo.percona.com/yum/PERCONA-PACKAGING-KEY || true
systemctl enable --now docker-registry >/dev/null 2>&1
systemctl restart docker-registry
systemctl enable nginx >/dev/null 2>&1
systemctl restart nginx
for i in $(seq 1 30); do curl -fsS -o /dev/null http://127.0.0.1:5000/v2/ && curl -fsS -o /dev/null http://127.0.0.1/dbcanvas-repo.gpg && exit 0; sleep 1; done
echo "nginx or the registry did not answer" >&2; exit 1`

func (a *App) provisionRepository(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	fqdn := fqdnOf(host, domain)
	arch := archOr(n.Arch)
	image := repositoryImage(arch)

	cfg := repositoryConfig{
		Image: image, Hostname: host, FQDN: fqdn, UseProxy: n.UseProxy, Domain: domain,
		Targets: repoTargetsOf(n), Arches: repoArchesOf(n),
		Packages: n.RepoPackages, Images: n.RepoImages, Operators: n.RepoOperators,
		IncludeDebug: n.RepoDebug, Strict: n.RepoStrict,
	}
	// A redeploy keeps the published ports (links stay put), what was added after deploy, and what
	// the volume already holds.
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old repositoryConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			cfg.HTTPPort, cfg.RegistryPort = old.HTTPPort, old.RegistryPort
			cfg.Added, cfg.Contents = old.Added, old.Contents
		}
	}
	for _, p := range []*int{&cfg.HTTPPort, &cfg.RegistryPort} {
		if *p == 0 {
			if free, err := freeHostPort(); err == nil {
				*p = free
			}
		}
	}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: []byte("{}")})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Waiting for Intranet to be ready", 5)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 10)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		vol := repoDataVolume(st.ID, n.ID)
		if err := a.engCtx(ctx).VolumeCreate(ctx, vol); err != nil {
			pr.logln("warning: could not create the repository data volume: " + err.Error())
		}
		spec := ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID),
			// The FQDN is an alias as well as a bind record: a k3s node's containerd resolves
			// through Docker's embedded DNS, not the Intranet, and must find the registry by the
			// same name the Guide tab gives.
			Aliases:    []string{host, fqdn},
			PublishMap: []PortMap{{ContainerPort: repoHTTPPort, HostPort: cfg.HTTPPort}, {ContainerPort: repoRegistryPort, HostPort: cfg.RegistryPort}},
			Binds:      []string{vol + ":" + repoRoot},
			DNS:        []string{intranetIP}, DNSSearch: []string{domain},
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
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
		a.repoReadPorts(ctx, id, &cfg)
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: []byte("{}")})
		pr.logln(fmt.Sprintf("container started (http %d, registry %d)", cfg.HTTPPort, cfg.RegistryPort))

		pr.phase("Waiting for systemd", 15)
		if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, repoOSImage, pr.logln)
		if n.UseProxy {
			if err := a.runStep(ctx, id, pkgProxyDebian, []string{"PROXY=" + repoProxyURL(domain)}, pr.logln); err != nil {
				pr.fail("configure package proxy: %v", err)
				return
			}
			pr.logln("package egress via Intranet proxy")
		}

		pr.phase("Installing nginx, the registry and repository tools", 20)
		if err := a.runStep(ctx, id, repositoryInstallScript, nil, pr.logln); err != nil {
			pr.fail("install: %v", err)
			return
		}
		pr.phase("Configuring nginx and the registry", 25)
		if err := a.runStep(ctx, id, repositoryConfigureScript, []string{"LABEL=" + n.Label, "DOMAIN=" + domain}, pr.logln); err != nil {
			pr.fail("configure: %v", err)
			return
		}
		a.reconcileStackDNS(ctx, st.ID)
		pr.logln("serving http://" + fqdn + "/ and registry " + fqdn + ":5000")

		// The first sync is part of the deploy: a node that uses this Repository waits for it to
		// be running, and running means it holds what the design asked for.
		pr.phase("Mirroring packages and images", 30)
		s := a.newRepoSyncer(ctx, st.ID, n.ID, id, &cfg)
		s.progress = func(phase string, pct int) { pr.phase(phase, 30+pct*65/100) }
		s.echo = pr.logln
		s.run()

		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: []byte("{}")})
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		if cfg.Sync.State == "error" {
			pr.p.Message = "provisioned — " + cfg.Sync.Message
		}
		pr.save()
		log.Printf("stack %d repository %s: provisioned (%s)", st.ID, n.ID, cfg.Sync.Message)
	}()
}

func repoProxyURL(domain string) string { return "http://intranet." + domain + ":3128" }

func (a *App) repoReadPorts(ctx context.Context, id string, cfg *repositoryConfig) {
	if hp, err := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", repoHTTPPort)); err == nil {
		if p, err := strconv.Atoi(hp); err == nil {
			cfg.HTTPPort = p
		}
	}
	if hp, err := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", repoRegistryPort)); err == nil {
		if p, err := strconv.Atoi(hp); err == nil {
			cfg.RegistryPort = p
		}
	}
}

// ---------------------------------------------------------------- sync

// repoSyncs serializes syncs per node: one "add" while another is still downloading would race
// on the same metadata files.
var repoSyncs sync.Map // "stack/node" → *sync.Mutex

type repoSyncer struct {
	a        *App
	ctx      context.Context
	stackID  int64
	nodeID   string
	id       string // container
	cfg      *repositoryConfig
	progress func(phase string, pct int)
	echo     func(string)
	last     time.Time
}

func (a *App) newRepoSyncer(ctx context.Context, stackID int64, nodeID, id string, cfg *repositoryConfig) *repoSyncer {
	return &repoSyncer{a: a, ctx: ctx, stackID: stackID, nodeID: nodeID, id: id, cfg: cfg}
}

func (s *repoSyncer) logln(msg string) {
	st := &s.cfg.Sync
	st.Log = append(st.Log, time.Now().UTC().Format("15:04:05")+" "+msg)
	if len(st.Log) > 300 {
		st.Log = st.Log[len(st.Log)-300:]
	}
	if s.echo != nil {
		s.echo(msg)
	}
	s.persist(false)
}

func (s *repoSyncer) phase(p string, pct int) {
	s.cfg.Sync.Phase = p
	if s.progress != nil {
		s.progress(p, pct)
	}
	s.persist(true)
}

// persist writes the sync status into the deployment row the UI polls — at most every two seconds
// unless forced, since a big sync logs a line per repository and architecture.
func (s *repoSyncer) persist(force bool) {
	if !force && time.Since(s.last) < 2*time.Second {
		return
	}
	s.last = time.Now()
	s.a.repoSaveConfig(s.stackID, s.nodeID, s.cfg)
}

// repoSaveConfig replaces a Repository's config, keeping its state and container — and never
// recreating a row a teardown has already deleted.
func (a *App) repoSaveConfig(stackID int64, nodeID string, cfg *repositoryConfig) {
	dep, err := a.store.GetDeployment(stackID, nodeID)
	if err != nil {
		return
	}
	b, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: stackID, NodeID: nodeID, ContainerID: dep.ContainerID,
		State: dep.State, Config: b, Secrets: dep.Secrets})
}

// sh runs a script in the Repository container with the proxy exported when the node uses one.
func (s *repoSyncer) sh(script string, env ...string) (string, error) {
	pre := "set -o pipefail\n"
	if s.cfg.UseProxy {
		p := repoProxyURL(s.cfg.Domain)
		pre += "export http_proxy=" + p + " https_proxy=" + p + " HTTP_PROXY=" + p + " HTTPS_PROXY=" + p + " no_proxy=localhost,127.0.0.1 NO_PROXY=localhost,127.0.0.1\n"
	}
	res, err := s.a.engCtx(s.ctx).Exec(s.ctx, s.id, []string{"bash", "-c", pre + script}, env)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("exit %d", res.Code)
		}
		return res.Stdout, fmt.Errorf("%s", lastLines(msg, 600))
	}
	return res.Stdout, nil
}

// fetch returns a URL's body, decompressed by suffix.
func (s *repoSyncer) fetch(url string) ([]byte, error) {
	dec := "cat"
	switch {
	case strings.HasSuffix(url, ".gz"):
		dec = "gzip -dc"
	case strings.HasSuffix(url, ".zst"):
		dec = "zstd -dc"
	case strings.HasSuffix(url, ".xz"):
		dec = "xz -dc"
	case strings.HasSuffix(url, ".bz2"):
		dec = "bzip2 -dc"
	}
	out, err := s.sh(`curl -fsSL --retry 3 --connect-timeout 20 "$URL" | `+dec, "URL="+url)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// repoDownloadScript fetches a list of "url dest sha256 size" lines four at a time, skipping
// files already present at the right size and checking every new one against its checksum.
const repoDownloadScript = `cd "$DEST"
fetch_one() {
  url=$1; f=$2; sum=$3; size=$4
  if [ -f "$f" ] && [ "$(stat -c %s "$f")" = "$size" ]; then exit 0; fi
  mkdir -p "$(dirname "$f")"
  if ! curl -fsSL --retry 3 --connect-timeout 20 -o "$f.part" "$url"; then rm -f "$f.part"; echo "failed: $url" >&2; exit 1; fi
  if [ "$sum" != "-" ] && ! echo "$sum  $f.part" | sha256sum -c --status; then rm -f "$f.part"; echo "checksum mismatch: $url" >&2; exit 1; fi
  mv "$f.part" "$f"
}
export -f fetch_one
xargs -r -a "$LIST" -P 4 -n 4 bash -c 'fetch_one "$@"' _`

// download fetches pkgs from base into dest (a directory under the web root).
func (s *repoSyncer) download(base, dest string, pkgs []repoPkg) error {
	if len(pkgs) == 0 {
		_, err := s.sh("mkdir -p " + shellQuote(dest))
		return err
	}
	var b strings.Builder
	for _, p := range pkgs {
		sum := p.SHA256
		if sum == "" {
			sum = "-"
		}
		fmt.Fprintf(&b, "%s %s %s %d\n", base+p.Location, p.Location, sum, p.Size)
	}
	if _, err := s.sh("mkdir -p " + shellQuote(dest)); err != nil {
		return err
	}
	list := "/tmp/dbcanvas-repo-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".list"
	if err := s.a.engCtx(s.ctx).CopyFile(s.ctx, s.id, "/tmp", path.Base(list), 0o644, []byte(b.String())); err != nil {
		return err
	}
	defer s.sh("rm -f " + list)
	_, err := s.sh(repoDownloadScript, "DEST="+dest, "LIST="+list)
	return err
}

// run is one sync: every repository × OS release × architecture, then operators, then images,
// then the index files. A failure in one item is recorded against it and the rest go on — a
// mistyped repository name should not cost the images that were spelled right.
func (s *repoSyncer) run() {
	muAny, _ := repoSyncs.LoadOrStore(fmt.Sprintf("%d/%s", s.stackID, s.nodeID), &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	st := &s.cfg.Sync
	*st = repoSyncStatus{State: "running", Started: time.Now().UTC().Format(time.RFC3339), Log: []string{}}
	s.phase("Reading repository metadata", 0)

	var repos []repoMirrored
	failures := 0
	byRepo := map[string][]repoPkgSpec{}
	var order []string
	for _, p := range s.cfg.allPackages() {
		p.Repo = strings.TrimSpace(p.Repo)
		if _, ok := byRepo[p.Repo]; !ok {
			order = append(order, p.Repo)
		}
		byRepo[p.Repo] = append(byRepo[p.Repo], p)
	}
	type unit struct {
		repo string
		t    repoTarget
		arch string
	}
	var units []unit
	for _, r := range order {
		for _, tid := range s.cfg.Targets {
			t, ok := repoTargetByID(tid)
			if !ok {
				continue
			}
			for _, a := range s.cfg.Arches {
				units = append(units, unit{r, t, a})
			}
		}
	}
	aptDists := map[string]map[string]bool{} // web path of dists/<codename> → apt arches present
	for i, u := range units {
		if s.ctx.Err() != nil {
			break
		}
		s.phase(fmt.Sprintf("Mirroring %s for %s %s (%d/%d)", u.repo, u.t.Label, u.arch, i+1, len(units)), 5+i*55/max(len(units), 1))
		var m repoMirrored
		if u.t.Family == "yum" {
			m = s.syncYum(u.repo, byRepo[u.repo], u.t, u.arch)
		} else {
			m = s.syncApt(u.repo, byRepo[u.repo], u.t, u.arch)
			if m.Error == "" {
				d := "/percona/" + u.repo + "/apt/dists/" + u.t.Codename
				if aptDists[d] == nil {
					aptDists[d] = map[string]bool{}
				}
				aptDists[d][repoArchName("apt", u.arch)] = true
			}
		}
		if m.Error != "" {
			failures++
			s.logln(fmt.Sprintf("%s %s %s: %s", u.repo, u.t.ID(), u.arch, m.Error))
		} else {
			s.logln(fmt.Sprintf("%s %s %s: %d packages, %s", u.repo, u.t.ID(), u.arch, m.Packages, humanBytes(float64(m.Bytes))))
		}
		repos = append(repos, m)
	}
	for _, d := range repoKeys(aptDists) {
		if err := s.signAptDist(d, aptDists[d]); err != nil {
			failures++
			s.logln("sign " + d + ": " + err.Error())
		}
	}

	s.phase("Mirroring operators and Helm charts", 60)
	ops, charts, opImages, opFail := s.syncOperators()
	failures += opFail

	images := s.cfg.explicitImages()
	from := map[string]string{}
	for _, oi := range opImages {
		if !repoHas(images, oi.ref) {
			images = append(images, oi.ref)
			from[oi.ref] = oi.from
		}
	}
	var imgs []repoImage
	for i, ref := range images {
		if s.ctx.Err() != nil {
			break
		}
		s.phase(fmt.Sprintf("Copying image %s (%d/%d)", ref, i+1, len(images)), 70+i*25/max(len(images), 1))
		im := s.syncImage(ref)
		im.From = from[ref]
		if im.Error != "" {
			failures++
			s.logln("image " + ref + ": " + im.Error)
		} else {
			s.logln("image " + ref + " → " + s.cfg.FQDN + ":5000/" + im.Local)
		}
		imgs = append(imgs, im)
	}

	s.phase("Writing the index", 97)
	c := &s.cfg.Contents
	c.Repos, c.Images, c.Operators, c.Charts = repos, imgs, ops, charts
	if out, err := s.sh("du -sb " + repoRoot + " | cut -f1"); err == nil {
		c.DiskBytes, _ = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	}
	c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.writeIndex(); err != nil {
		failures++
		s.logln("index: " + err.Error())
	}

	st.Finished = time.Now().UTC().Format(time.RFC3339)
	switch {
	case s.ctx.Err() != nil:
		st.State, st.Message = "error", "interrupted"
	case failures > 0:
		st.State, st.Message = "error", fmt.Sprintf("%d item(s) could not be mirrored — see the log", failures)
	default:
		st.State, st.Message = "done", fmt.Sprintf("%d repositories, %d images, %s on disk", len(repos), len(imgs), humanBytes(float64(c.DiskBytes)))
	}
	s.phase("Done", 100)
	s.logln(st.Message)
	s.persist(true)
}

func repoHas(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func repoKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func repoSpecVersions(specs []repoPkgSpec) []string {
	var out []string
	for _, s := range specs {
		if len(s.Versions) == 0 {
			out = appendUnique(out, "latest")
		}
		for _, v := range s.Versions {
			if v = strings.TrimSpace(v); v != "" {
				out = appendUnique(out, v)
			}
		}
	}
	return out
}

// selectUnion applies every spec naming one repository and merges the answers.
func selectUnion(pkgs []repoPkg, specs []repoPkgSpec, deb, debug bool) []repoPkg {
	seen := map[string]bool{}
	var out []repoPkg
	for _, sp := range specs {
		for _, p := range selectRepoPackages(pkgs, sp, deb, debug) {
			if !seen[p.Location] {
				seen[p.Location] = true
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Location < out[j].Location })
	return out
}

// syncYum mirrors one repository for one el release and architecture: the architecture's
// directory, and its noarch sibling — percona-release writes a section for both, so both must
// answer once a node is pointed here, even if noarch is empty.
func (s *repoSyncer) syncYum(repo string, specs []repoPkgSpec, t repoTarget, arch string) repoMirrored {
	ra := repoArchName("yum", arch)
	m := repoMirrored{Repo: repo, Target: t.ID(), Arch: arch, Family: "yum", Versions: repoSpecVersions(specs),
		Path: "/percona/" + repo + "/yum/release/" + t.Version + "/RPMS/" + ra + "/"}
	for _, dir := range []string{ra, "noarch"} {
		rel := repo + "/yum/release/" + t.Version + "/RPMS/" + dir + "/"
		base := repoUpstream + "/" + rel
		dest := repoWWW + "/percona/" + rel
		repomd, err := s.fetch(base + "repodata/repomd.xml")
		if err != nil {
			if dir == "noarch" {
				// Not published: an empty repository keeps the node's noarch section answering.
				if _, err := s.sh("mkdir -p " + shellQuote(dest) + " && createrepo_c -q --update " + shellQuote(dest)); err != nil {
					m.Error = "createrepo noarch: " + err.Error()
					return m
				}
				continue
			}
			m.Error = fmt.Sprintf("not published for %s %s (%v)", t.Label, ra, err)
			return m
		}
		href, err := repomdPrimaryHref(repomd)
		if err != nil {
			m.Error = err.Error()
			return m
		}
		primary, err := s.fetch(base + href)
		if err != nil {
			m.Error = "primary metadata: " + err.Error()
			return m
		}
		all, err := parsePrimaryXML(strings.NewReader(string(primary)))
		if err != nil {
			m.Error = err.Error()
			return m
		}
		chosen := selectUnion(all, specs, false, s.cfg.IncludeDebug)
		if dir == ra && len(chosen) == 0 {
			m.Error = "no package matched " + strings.Join(m.Versions, ", ")
		}
		if err := s.download(base, dest, chosen); err != nil {
			m.Error = "download: " + err.Error()
			return m
		}
		if _, err := s.sh("createrepo_c -q --update --workers 2 " + shellQuote(dest)); err != nil {
			m.Error = "createrepo: " + err.Error()
			return m
		}
		m.Packages += len(chosen)
		for _, p := range chosen {
			m.Bytes += p.Size
		}
	}
	return m
}

// syncApt mirrors one repository for one Debian/Ubuntu release and architecture. The Packages
// index is the upstream paragraphs for the chosen builds; Release is written and signed once per
// release, after every architecture (signAptDist).
func (s *repoSyncer) syncApt(repo string, specs []repoPkgSpec, t repoTarget, arch string) repoMirrored {
	aa := repoArchName("apt", arch)
	m := repoMirrored{Repo: repo, Target: t.ID(), Arch: arch, Family: "apt", Versions: repoSpecVersions(specs),
		Path: "/percona/" + repo + "/apt/dists/" + t.Codename + "/"}
	base := repoUpstream + "/" + repo + "/apt/"
	idx := "dists/" + t.Codename + "/main/binary-" + aa + "/"
	raw, err := s.fetch(base + idx + "Packages.gz")
	if err != nil {
		if raw, err = s.fetch(base + idx + "Packages"); err != nil {
			m.Error = fmt.Sprintf("not published for %s %s (%v)", t.Label, aa, err)
			return m
		}
	}
	all, err := parseDebPackages(strings.NewReader(string(raw)))
	if err != nil {
		m.Error = err.Error()
		return m
	}
	chosen := selectUnion(all, specs, true, s.cfg.IncludeDebug)
	if len(chosen) == 0 {
		m.Error = "no package matched " + strings.Join(m.Versions, ", ")
		return m
	}
	dest := repoWWW + "/percona/" + repo + "/apt/"
	if err := s.download(base, dest, chosen); err != nil {
		m.Error = "download: " + err.Error()
		return m
	}
	if _, err := s.sh("mkdir -p " + shellQuote(dest+idx)); err != nil {
		m.Error = err.Error()
		return m
	}
	if err := s.a.engCtx(s.ctx).CopyFile(s.ctx, s.id, dest+idx, "Packages", 0o644, []byte(debPackagesIndex(chosen))); err != nil {
		m.Error = "write Packages: " + err.Error()
		return m
	}
	if _, err := s.sh("gzip -9kf " + shellQuote(dest+idx+"Packages")); err != nil {
		m.Error = "gzip Packages: " + err.Error()
		return m
	}
	m.Packages = len(chosen)
	for _, p := range chosen {
		m.Bytes += p.Size
	}
	return m
}

// signAptDist writes dists/<codename>/Release for the architectures present and signs it with the
// node's own key (InRelease + Release.gpg).
func (s *repoSyncer) signAptDist(webPath string, arches map[string]bool) error {
	codename := path.Base(webPath)
	archList := strings.Join(repoKeys(arches), " ")
	script := `set -e
cd "$DIR"
rm -f Release InRelease Release.gpg
apt-ftparchive -o APT::FTPArchive::Release::Origin="DBCanvas" \
  -o APT::FTPArchive::Release::Label="DBCanvas Repository" \
  -o APT::FTPArchive::Release::Suite="$CODENAME" -o APT::FTPArchive::Release::Codename="$CODENAME" \
  -o APT::FTPArchive::Release::Architectures="$ARCHES" -o APT::FTPArchive::Release::Components=main \
  release . >/tmp/dbcanvas-Release
mv /tmp/dbcanvas-Release Release
export GNUPGHOME=` + repoGnupgHome + `
gpg --batch --yes --clearsign -o InRelease Release
gpg --batch --yes -abs -o Release.gpg Release`
	_, err := s.sh(script, "DIR="+repoWWW+webPath, "CODENAME="+codename, "ARCHES="+archList)
	return err
}

// ---------------------------------------------------------------- operators, charts, images

type repoOpImage struct{ ref, from string }

// repoImageLineRE finds `image:` values in a manifest — uncommented lines only: cr.yaml's commented
// images are options the default resource does not start.
var repoImageLineRE = regexp.MustCompile(`(?m)^[ \t-]*image:[ \t]*["']?([^\s"'#]+)`)

// repoManifestImages lists the images a manifest names, in order, once each.
func repoManifestImages(manifest string) []string {
	var out []string
	for _, m := range repoImageLineRE.FindAllStringSubmatch(manifest, -1) {
		out = appendUnique(out, m[1])
	}
	return out
}

// repoImageLocal is where an image lives in this registry: Docker Hub images at their own path
// (library/ for official images), so a containerd mirror for docker.io finds them unmodified;
// anything else under its registry host, which is what registries.yaml's rewrite for that host
// adds.
func repoImageLocal(ref string) string {
	_, local := repoImageSplit(ref)
	return local
}

// repoImageSource is an image reference fully qualified, as skopeo wants it.
func repoImageSource(ref string) string {
	reg, _ := repoImageSplit(ref)
	first, rest, ok := strings.Cut(ref, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		ref = rest
	} else if !ok {
		ref = "library/" + ref
	}
	return reg + "/" + ref
}

// repoImageSplit is an image's registry and its path in this one.
func repoImageSplit(ref string) (registry, local string) {
	first, rest, ok := strings.Cut(ref, "/")
	if !ok || !(strings.ContainsAny(first, ".:") || first == "localhost") {
		registry, rest = "docker.io", ref
		if !ok {
			rest = "library/" + ref
		}
	} else {
		registry = first
	}
	if registry == "docker.io" || registry == "index.docker.io" || registry == "registry-1.docker.io" {
		registry = "docker.io"
		local = rest
	} else {
		local = registry + "/" + rest
	}
	if name, dig, ok := strings.Cut(local, "@"); ok {
		// A tag cannot be a digest; the manifest is copied with its digest preserved, so a pull
		// by digest through the mirror still finds it.
		local = name + ":" + strings.ReplaceAll(dig, ":", "-")
	} else if i := strings.LastIndex(local, ":"); i < strings.LastIndex(local, "/") || i < 0 {
		local += ":latest"
	}
	return registry, local
}

// repoRewriteImages points every image a manifest names at this registry — cr.local.yaml and
// bundle.local.yaml, for a cluster that is not configured with a mirror.
func repoRewriteImages(manifest, registryHost string) string {
	return repoImageLineRE.ReplaceAllStringFunc(manifest, func(line string) string {
		m := repoImageLineRE.FindStringSubmatch(line)
		return strings.Replace(line, m[1], registryHost+"/"+repoImageLocal(m[1]), 1)
	})
}

func (s *repoSyncer) syncImage(ref string) repoImage {
	im := repoImage{Source: ref}
	var src string
	im.Local, src = repoImageLocal(ref), repoImageSource(ref)
	arch := "--override-os linux --override-arch " + s.cfg.Arches[0]
	if len(s.cfg.Arches) > 1 {
		arch = "--multi-arch all"
	}
	script := `if skopeo inspect --tls-verify=false --raw "docker://127.0.0.1:5000/$LOCAL" >/dev/null 2>&1; then echo present; exit 0; fi
skopeo copy -q --retry-times 3 --preserve-digests --dest-tls-verify=false ` + arch + ` "docker://$SRC" "docker://127.0.0.1:5000/$LOCAL"`
	if _, err := s.sh(script, "SRC="+src, "LOCAL="+im.Local); err != nil {
		im.Error = err.Error()
	}
	return im
}

// syncOperators fetches each operator release's source and manifests, the images they name, and
// the matching Helm charts, and writes charts/index.yaml.
func (s *repoSyncer) syncOperators() ([]repoOperatorRec, []repoChart, []repoOpImage, int) {
	ops := s.cfg.allOperators()
	if len(ops) == 0 {
		return nil, nil, nil, 0
	}
	fails := 0
	var recs []repoOperatorRec
	var images []repoOpImage
	var index struct {
		Entries map[string][]map[string]any `json:"entries"`
	}
	raw, ierr := s.fetch(repoHelmIndexURL)
	if ierr == nil {
		ierr = yaml.Unmarshal(raw, &index)
	}
	if ierr != nil {
		fails++
		s.logln("Helm chart index: " + ierr.Error())
	}
	localIndex := map[string][]map[string]any{}
	var charts []repoChart
	registry := s.cfg.FQDN + ":5000"
	for _, o := range ops {
		rec := repoOperatorRec{Kind: o.Kind, Version: o.Version, Path: "/operators/" + o.Kind + "/" + o.Version + "/"}
		repo := k3dOperatorRepos[o.Kind]
		if repo == "" || o.Version == "" {
			rec.Error = "unknown operator or version"
			recs = append(recs, rec)
			fails++
			continue
		}
		dir := repoWWW + rec.Path
		url := fmt.Sprintf(operatorTarballFmt, repo, o.Version)
		script := `set -e
mkdir -p "$D"; cd "$D"
if [ ! -s source.tar.gz ]; then curl -fsSL --retry 3 -o source.tar.gz.part "$URL"; mv source.tar.gz.part source.tar.gz; fi
tar -xzf source.tar.gz --wildcards --strip-components=2 '*/deploy/bundle.yaml' '*/deploy/cr.yaml' 2>/dev/null
tar -xzf source.tar.gz --wildcards --strip-components=2 '*/deploy/secrets.yaml' 2>/dev/null || true
cat bundle.yaml; echo; echo '#---dbcanvas-split---'; cat cr.yaml`
		out, err := s.sh(script, "D="+dir, "URL="+url)
		if err != nil {
			rec.Error = err.Error()
			recs = append(recs, rec)
			fails++
			s.logln(fmt.Sprintf("operator %s %s: %v", o.Kind, o.Version, err))
			continue
		}
		bundle, cr, _ := strings.Cut(out, "#---dbcanvas-split---")
		for _, img := range append(repoManifestImages(bundle), repoManifestImages(cr)...) {
			rec.Images = appendUnique(rec.Images, img)
		}
		for _, img := range rec.Images {
			images = append(images, repoOpImage{ref: img, from: o.Kind + " " + o.Version})
		}
		eng := s.a.engCtx(s.ctx)
		if err := eng.CopyFile(s.ctx, s.id, dir, "cr.local.yaml", 0o644, []byte(repoRewriteImages(cr, registry))); err != nil {
			rec.Error = err.Error()
		}
		if err := eng.CopyFile(s.ctx, s.id, dir, "bundle.local.yaml", 0o644, []byte(repoRewriteImages(bundle, registry))); err != nil {
			rec.Error = err.Error()
		}
		// Helm: the operator chart and the database chart whose appVersion is this release —
		// newest chart version when there are several.
		for _, name := range repoOperatorCharts[o.Kind] {
			e := repoPickChart(index.Entries[name], o.Version)
			if e == nil {
				if ierr == nil {
					s.logln(fmt.Sprintf("Helm chart %s: no release for operator %s", name, o.Version))
				}
				continue
			}
			urls, _ := e["urls"].([]any)
			if len(urls) == 0 {
				continue
			}
			src := fmt.Sprint(urls[0])
			file := path.Base(src)
			if _, err := s.sh(`cd `+repoWWW+`/charts && { [ -s "$F" ] || { curl -fsSL --retry 3 -o "$F.part" "$SRC" && mv "$F.part" "$F"; }; }`, "F="+file, "SRC="+src); err != nil {
				fails++
				s.logln("Helm chart " + file + ": " + err.Error())
				continue
			}
			local := map[string]any{}
			for k, v := range e {
				local[k] = v
			}
			// Relative: helm resolves it against wherever the repository was added from, so the
			// same index works from inside the stack and through the published port.
			local["urls"] = []string{file}
			localIndex[name] = append(localIndex[name], local)
			c := repoChart{Name: name, Version: fmt.Sprint(e["version"]), AppVersion: fmt.Sprint(e["appVersion"]), File: file}
			charts = append(charts, c)
			rec.Charts = append(rec.Charts, name+"-"+c.Version)
		}
		recs = append(recs, rec)
		s.logln(fmt.Sprintf("operator %s %s: manifests, %d images, charts %s", o.Kind, o.Version, len(rec.Images), strings.Join(rec.Charts, " ")))
	}
	idx := map[string]any{"apiVersion": "v1", "entries": localIndex, "generated": time.Now().UTC().Format(time.RFC3339)}
	b, _ := yaml.Marshal(idx)
	if err := s.a.engCtx(s.ctx).CopyFile(s.ctx, s.id, repoWWW+"/charts", "index.yaml", 0o644, b); err != nil {
		fails++
		s.logln("charts/index.yaml: " + err.Error())
	}
	return recs, charts, images, fails
}

// repoPickChart is the newest chart entry built for an operator version.
func repoPickChart(entries []map[string]any, opVersion string) map[string]any {
	var best map[string]any
	for _, e := range entries {
		if strings.TrimPrefix(fmt.Sprint(e["appVersion"]), "v") != opVersion {
			continue
		}
		if best == nil || rpmVerCmp(fmt.Sprint(e["version"]), fmt.Sprint(best["version"])) > 0 {
			best = e
		}
	}
	return best
}

// writeIndex writes the files a consuming node and the browser read: the carried lists, the
// manifest index.html renders, a registries.yaml for k3s, and index.html itself.
func (s *repoSyncer) writeIndex() error {
	eng := s.a.engCtx(s.ctx)
	carried := map[string][]string{}
	for _, tid := range s.cfg.Targets {
		t, ok := repoTargetByID(tid)
		if !ok {
			continue
		}
		for _, a := range s.cfg.Arches {
			carried[repoCarriedKey(t, a)] = []string{}
		}
	}
	for _, m := range s.cfg.Contents.Repos {
		if m.Error != "" {
			continue
		}
		t, _ := repoTargetByID(m.Target)
		k := repoCarriedKey(t, m.Arch)
		carried[k] = appendUnique(carried[k], m.Repo)
	}
	if _, err := s.sh("rm -f " + repoWWW + "/dbcanvas/carried/*.txt; mkdir -p " + repoWWW + "/dbcanvas/carried"); err != nil {
		return err
	}
	for k, list := range carried {
		body := strings.Join(list, "\n")
		if body != "" {
			body += "\n"
		}
		if err := eng.CopyFile(s.ctx, s.id, repoWWW+"/dbcanvas/carried", k+".txt", 0o644, []byte(body)); err != nil {
			return err
		}
	}
	manifest := map[string]any{
		"fqdn": s.cfg.FQDN, "registry": s.cfg.FQDN + ":5000", "strict": s.cfg.Strict,
		"targets": s.cfg.Targets, "arches": s.cfg.Arches, "contents": s.cfg.Contents, "carried": carried,
	}
	mj, _ := json.MarshalIndent(manifest, "", "  ")
	if err := eng.CopyFile(s.ctx, s.id, repoWWW+"/dbcanvas", "manifest.json", 0o644, mj); err != nil {
		return err
	}
	if err := eng.CopyFile(s.ctx, s.id, repoWWW+"/dbcanvas", "registries.yaml", 0o644, []byte(repoRegistriesYAML(s.cfg.FQDN, s.cfg.Contents.Images))); err != nil {
		return err
	}
	return eng.CopyFile(s.ctx, s.id, repoWWW, "index.html", 0o644, []byte(repoIndexHTML))
}

// repoRegistriesYAML is k3s's /etc/rancher/k3s/registries.yaml for a cluster pulling through this
// node: docker.io straight through, every other registry it holds images from with a rewrite that
// puts the host back in front of the path (see repoImageLocal). containerd still falls back to the
// upstream registry for anything the mirror does not have.
func repoRegistriesYAML(fqdn string, images []repoImage) string {
	endpoint := "http://" + fqdn + ":5000"
	var b strings.Builder
	b.WriteString("mirrors:\n  docker.io:\n    endpoint:\n      - \"" + endpoint + "\"\n")
	hosts := map[string]bool{}
	for _, im := range images {
		if reg, _ := repoImageSplit(im.Source); reg != "docker.io" {
			hosts[reg] = true
		}
	}
	for _, h := range repoKeys(hosts) {
		fmt.Fprintf(&b, "  %s:\n    endpoint:\n      - \"%s\"\n    rewrite:\n      \"^(.*)$\": \"%s/$1\"\n", h, endpoint, h)
	}
	return b.String()
}

// ---------------------------------------------------------------- consuming nodes

// repoRewriteScript is installed on a node that uses a Repository as
// /usr/local/sbin/dbcanvas-repo-rewrite. See the file comment for why it runs before every
// package-manager call rather than once.
const repoRewriteScript = `#!/bin/bash
# Written by DBCanvas. Points the Percona repositories this node has configured at a DBCanvas
# Repository node — only those it carries for this OS release and architecture — and runs before
# every dnf/yum/apt-get/apt through the wrappers in /usr/local/bin. Safe to run by hand.
. /etc/dbcanvas/repository.env
[ -r /etc/os-release ] && . /etc/os-release
if [ -d /etc/apt ] && command -v dpkg >/dev/null 2>&1; then
  FAM=apt; KEY="apt-${VERSION_CODENAME}-$(dpkg --print-architecture)"
else
  FAM=yum; KEY="yum-${VERSION_ID%%.*}-$(uname -m)"
fi
CACHE=/etc/dbcanvas/carried-$KEY.txt
if L=$(curl -fsS -m 5 --noproxy '*' "$BASE/dbcanvas/carried/$KEY.txt" 2>/dev/null); then printf '%s\n' "$L" >"$CACHE"; fi
CARRIED=$(cat "$CACHE" 2>/dev/null)
M="$BASE/percona"
if [ "$FAM" = yum ]; then
  for f in /etc/yum.repos.d/*.repo; do
    [ -f "$f" ] || continue
    for r in $CARRIED; do
      sed -i -E "s#^(baseurl *= *)https?://repo\.percona\.com/$r/#\1$M/$r/#" "$f"
    done
    sed -i -E "s#https?://repo\.percona\.com/yum/PERCONA-PACKAGING-KEY#$M/yum/PERCONA-PACKAGING-KEY#g" "$f"
    if [ "$STRICT" = 1 ] && grep -q 'repo\.percona\.com' "$f"; then
      awk '
        function flush() { if (!n) return; for (i = 1; i <= n; i++) { l = buf[i]; if (up && l ~ /^enabled *=/) l = "enabled = 0"; print l }; if (up && !en) print "enabled = 0"; n = 0; up = 0; en = 0 }
        /^\[/ { flush() }
        { buf[++n] = $0; if ($0 ~ /^baseurl *=.*repo\.percona\.com/) up = 1; if ($0 ~ /^enabled *=/) en = 1 }
        END { flush() }' "$f" >"$f.dbcanvas" && cat "$f.dbcanvas" >"$f" && rm -f "$f.dbcanvas"
    fi
  done
else
  if [ ! -s /etc/apt/keyrings/dbcanvas-repo.gpg ]; then
    install -d /etc/apt/keyrings
    curl -fsS -m 10 --noproxy '*' -o /etc/apt/keyrings/dbcanvas-repo.gpg "$BASE/dbcanvas-repo.gpg" || rm -f /etc/apt/keyrings/dbcanvas-repo.gpg
  fi
  for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.list; do
    [ -f "$f" ] || continue
    for r in $CARRIED; do
      sed -i -E "/^deb(-src)? .*https?:\/\/repo\.percona\.com\/$r\/apt /{ s@^deb-src @#deb-src @; s@\[signed-by=[^]]*\] ?@@; s@^deb (http)@deb [signed-by=/etc/apt/keyrings/dbcanvas-repo.gpg] \1@; s@https?://repo\.percona\.com/$r/apt@$M/$r/apt@ }" "$f"
    done
    [ "$STRICT" = 1 ] && sed -i -E 's@^(deb(-src)? .*repo\.percona\.com)@#\1@' "$f"
  done
fi
exit 0
`

// repoUseScript installs the rewrite script and the wrappers, and runs it once.
const repoUseScript = `set -e
install -d /etc/dbcanvas /usr/local/sbin /usr/local/bin
printf 'BASE=%s\nSTRICT=%s\n' "$BASE" "$STRICT" >/etc/dbcanvas/repository.env
cat >/usr/local/sbin/dbcanvas-repo-rewrite <<'DBCANVAS_REWRITE'
` + repoRewriteScript + `DBCANVAS_REWRITE
chmod 755 /usr/local/sbin/dbcanvas-repo-rewrite
for pm in dnf yum apt-get apt; do
  real=$(PATH=/usr/sbin:/usr/bin:/sbin:/bin command -v $pm) || continue
  printf '#!/bin/sh\n# DBCanvas: see /usr/local/sbin/dbcanvas-repo-rewrite\n/usr/local/sbin/dbcanvas-repo-rewrite >/dev/null 2>&1 || true\nexec %s "$@"\n' "$real" >/usr/local/bin/$pm
  chmod 755 /usr/local/bin/$pm
done
/usr/local/sbin/dbcanvas-repo-rewrite
curl -fsS -m 10 --noproxy '*' -o /dev/null "$BASE/dbcanvas-repo.gpg"`

// waitRepository waits for a Repository node to be running — which includes its first sync — and
// returns its profile.
func (a *App) waitRepository(ctx context.Context, stackID int64, nodeID string) (repositoryConfig, error) {
	var cfg repositoryConfig
	// Mirroring is downloading: allow it several deploy timeouts.
	deadline := time.Now().Add(3 * deployTimeout())
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return cfg, err
		}
		if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
			if dep.State == DeployError {
				return cfg, fmt.Errorf("the Repository failed to provision")
			}
			if dep.State == DeployRunning {
				json.Unmarshal(dep.Config, &cfg)
				return cfg, nil
			}
		}
		select {
		case <-ctx.Done():
			return cfg, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return cfg, fmt.Errorf("the Repository was not running within %s", 3*deployTimeout())
}

// useRepository points a node's Percona repositories at the Repository it is associated with.
// Called right after ensureDNFIPv4 — before any percona-release or install — by every provisioner
// that installs from repo.percona.com. No association, no change.
func (a *App) useRepository(ctx context.Context, st Stack, id, repoNodeID string, logln func(string)) error {
	if repoNodeID == "" {
		return nil
	}
	logln("waiting for the Repository to finish mirroring")
	cfg, err := a.waitRepository(ctx, st.ID, repoNodeID)
	if err != nil {
		return err
	}
	strict := "0"
	if cfg.Strict {
		strict = "1"
	}
	if err := a.runStep(ctx, id, repoUseScript, []string{"BASE=http://" + cfg.FQDN, "STRICT=" + strict}, logln); err != nil {
		return fmt.Errorf("point packages at the Repository: %w", err)
	}
	logln("Percona packages from the Repository at http://" + cfg.FQDN + "/ (for what it carries; see /usr/local/sbin/dbcanvas-repo-rewrite)")
	return nil
}

// repositoryFor returns a node id's Repository profile when it is running.
func (a *App) repositoryFor(stackID int64, repoNodeID string) (repositoryConfig, Deployment, bool) {
	var cfg repositoryConfig
	dep, err := a.store.GetDeployment(stackID, repoNodeID)
	if err != nil || dep.State != DeployRunning || json.Unmarshal(dep.Config, &cfg) != nil {
		return cfg, dep, false
	}
	return cfg, dep, true
}

// repoCarriesOperator reports whether a Repository holds an operator release's source tarball.
func (cfg repositoryConfig) carriesOperator(kind, version string) bool {
	for _, o := range cfg.Contents.Operators {
		if o.Kind == kind && o.Version == version && o.Error == "" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- API

// handleRepositoryCatalog serves what the design form offers: OS releases, the common Percona
// repositories with the versions each node picker already knows, and the operator releases.
func (a *App) handleRepositoryCatalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	type suggestion struct {
		Repo     string   `json:"repo"`
		Label    string   `json:"label"`
		Group    string   `json:"group"`
		Versions []string `json:"versions"`
	}
	versionsOf := func(key, major string) []string {
		var out []string
		for _, img := range loadImageCatalog(key) {
			if img.OS != "oraclelinux" {
				continue
			}
			for _, v := range img.Versions[major] {
				out = appendUnique(out, v)
			}
		}
		return out
	}
	sugg := []suggestion{
		{"ps-80", "Percona Server 8.0", "MySQL", versionsOf("percona_server", "8.0")},
		{"ps-84-lts", "Percona Server 8.4 LTS", "MySQL", versionsOf("percona_server", "8.4")},
		{"ps-97-lts", "Percona Server 9.7 LTS", "MySQL", versionsOf("percona_server", "9.7")},
		{"pxc-80", "Percona XtraDB Cluster 8.0", "MySQL", versionsOf("percona_xtradb_cluster", "8.0")},
		{"pxc-84-lts", "Percona XtraDB Cluster 8.4 LTS", "MySQL", versionsOf("percona_xtradb_cluster", "8.4")},
		{"pxb-80", "Percona XtraBackup 8.0", "MySQL", nil},
		{"pxb-84-lts", "Percona XtraBackup 8.4", "MySQL", nil},
		{"pxb-97-lts", "Percona XtraBackup 9.7", "MySQL", nil},
		{"proxysql", "ProxySQL", "MySQL", nil},
		{"psmdb-60", "Percona Server for MongoDB 6.0", "MongoDB", versionsOf("percona_server_mongodb", "6.0")},
		{"psmdb-70", "Percona Server for MongoDB 7.0", "MongoDB", versionsOf("percona_server_mongodb", "7.0")},
		{"psmdb-80", "Percona Server for MongoDB 8.0", "MongoDB", versionsOf("percona_server_mongodb", "8.0")},
		{"pbm", "Percona Backup for MongoDB", "MongoDB", nil},
		{"valkey-91", "Percona Valkey 9.1", "Valkey", versionsOf("percona_valkey", "9.1")},
		{"pmm3-client", "PMM 3 client", "Common", nil},
		{"telemetry", "Percona telemetry agent", "Common", nil},
		{"tools", "Percona tools", "Common", nil},
		{"pt", "Percona Toolkit", "Common", nil},
		{"prel", "percona-release", "Common", nil},
	}
	for _, m := range []string{"13", "14", "15", "16", "17", "18"} {
		sugg = append(sugg, suggestion{"ppg-" + m, "Percona Distribution for PostgreSQL " + m, "PostgreSQL", versionsOf("percona_postgresql", m)})
	}
	ops := map[string]any{}
	for _, k := range []string{"pxc", "ps", "psmdb", "pg"} {
		cat := loadOperatorCatalog()[k]
		ops[k] = map[string]any{"versions": cat.Versions, "latest": cat.Latest}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"targets": repoTargets, "arches": []string{"amd64", "arm64"}, "defaultArch": platformArch(),
		"suggestions": sugg, "operators": ops,
	})
}

// loadRunningRepository resolves a running Repository node for a handler.
func (a *App) loadRunningRepository(w http.ResponseWriter, r *http.Request) (Stack, Deployment, repositoryConfig, bool) {
	st, dep, ok := a.loadRunningPMM(w, r)
	if !ok {
		return st, dep, repositoryConfig{}, false
	}
	var cfg repositoryConfig
	if json.Unmarshal(dep.Config, &cfg) != nil || cfg.FQDN == "" {
		writeErr(w, http.StatusBadRequest, "node is not a Repository")
		return st, dep, cfg, false
	}
	return st, dep, cfg, true
}

// startRepoSync runs a sync in the background for a running Repository.
func (a *App) startRepoSync(st Stack, dep Deployment, cfg repositoryConfig) {
	ctx := withEngine(context.Background(), a.docker)
	go func() {
		c := cfg
		s := a.newRepoSyncer(ctx, st.ID, dep.NodeID, dep.ContainerID, &c)
		s.run()
		// The canvas card reads the deploy progress, whose message would otherwise still describe
		// the first sync.
		if d, err := a.store.GetDeployment(st.ID, dep.NodeID); err == nil {
			var p provProgress
			if json.Unmarshal(d.Progress, &p) == nil {
				p.Message = "provisioned — last sync: " + c.Sync.Message
				b, _ := json.Marshal(p)
				a.store.SetDeploymentProgress(st.ID, dep.NodeID, b)
			}
		}
		a.notifyStack(st.ID, "repository.sync", map[bool]string{true: "error", false: "info"}[c.Sync.State == "error"],
			"Repository sync finished", dep.NodeID+": "+c.Sync.Message, dep.NodeID)
	}()
}

func (a *App) handleRepositoryAdd(w http.ResponseWriter, r *http.Request) {
	st, dep, cfg, ok := a.loadRunningRepository(w, r)
	if !ok {
		return
	}
	var b repoAdditions
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var pkgs []repoPkgSpec
	for _, p := range b.Packages {
		p.Repo = strings.TrimSpace(p.Repo)
		if p.Repo != "" {
			pkgs = append(pkgs, p)
		}
	}
	var ops []repoOperatorSpec
	for _, o := range b.Operators {
		if o.Kind != "" {
			ops = append(ops, o)
		}
	}
	if issues := repoSpecIssues(cfg.Hostname, pkgs, b.Images, ops); len(issues) > 0 {
		writeErr(w, http.StatusBadRequest, issues[0].Message)
		return
	}
	if len(pkgs) == 0 && len(ops) == 0 && len(strings.TrimSpace(strings.Join(b.Images, ""))) == 0 {
		writeErr(w, http.StatusBadRequest, "nothing to add")
		return
	}
	if cfg.Sync.State == "running" {
		writeErr(w, http.StatusConflict, "a sync is already running — add these when it finishes")
		return
	}
	cfg.Added.Packages = append(cfg.Added.Packages, pkgs...)
	cfg.Added.Operators = append(cfg.Added.Operators, ops...)
	for _, i := range b.Images {
		if i = strings.TrimSpace(i); i != "" {
			cfg.Added.Images = appendUnique(cfg.Added.Images, i)
		}
	}
	cfg.Sync = repoSyncStatus{State: "running", Phase: "Queued", Started: time.Now().UTC().Format(time.RFC3339), Log: []string{}}
	a.repoSaveConfig(st.ID, dep.NodeID, &cfg)
	a.startRepoSync(st, dep, cfg)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "running"})
}

func (a *App) handleRepositorySync(w http.ResponseWriter, r *http.Request) {
	st, dep, cfg, ok := a.loadRunningRepository(w, r)
	if !ok {
		return
	}
	if cfg.Sync.State == "running" {
		writeErr(w, http.StatusConflict, "a sync is already running")
		return
	}
	cfg.Sync = repoSyncStatus{State: "running", Phase: "Queued", Started: time.Now().UTC().Format(time.RFC3339), Log: []string{}}
	a.repoSaveConfig(st.ID, dep.NodeID, &cfg)
	a.startRepoSync(st, dep, cfg)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "running"})
}

// repoIndexHTML is the Repository's front page: it renders /dbcanvas/manifest.json and the
// registry catalog, and links into the autoindexed trees.
const repoIndexHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>DBCanvas Repository</title>
<style>
:root{--bg:#f7f7f8;--fg:#1c1d21;--mut:#62646c;--card:#fff;--line:#e3e4e8;--acc:#2f6fdf;--bad:#c2372f;--code:#f0f1f4}
@media (prefers-color-scheme:dark){:root{--bg:#121316;--fg:#e8e9ec;--mut:#9a9ca5;--card:#1b1c20;--line:#2c2e34;--acc:#7aa7ff;--bad:#ff7b72;--code:#23252b}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,sans-serif}
main{max-width:980px;margin:0 auto;padding:24px 16px 64px}
h1{font-size:24px;margin:0 0 4px}h2{font-size:17px;margin:28px 0 8px}
.mut{color:var(--mut)}a{color:var(--acc);text-decoration:none}a:hover{text-decoration:underline}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:4px 14px;overflow-x:auto}
table{border-collapse:collapse;width:100%;font-size:14px}td,th{text-align:left;padding:7px 8px;border-bottom:1px solid var(--line);white-space:nowrap}
tr:last-child td{border-bottom:0}th{color:var(--mut);font-weight:600}
code,pre{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px;background:var(--code);border-radius:6px}
code{padding:1px 5px}pre{padding:10px 12px;overflow-x:auto;margin:8px 0}.bad{color:var(--bad)}
nav a{margin-right:14px}
</style></head><body><main>
<h1>DBCanvas Repository</h1>
<div class="mut" id="sub">Loading…</div>
<nav style="margin-top:12px"><a href="/percona/">/percona/</a><a href="/operators/">/operators/</a><a href="/charts/">/charts/</a><a href="/dbcanvas/">/dbcanvas/</a><a href="/v2/_catalog">/v2/_catalog</a></nav>
<h2>Package repositories</h2><div class="card"><table id="repos"><tr><td class="mut">—</td></tr></table></div>
<h2>Operators</h2><div class="card"><table id="ops"><tr><td class="mut">—</td></tr></table></div>
<h2>Helm charts</h2><div class="card"><table id="charts"><tr><td class="mut">—</td></tr></table></div>
<h2>Container images</h2><div class="card"><table id="images"><tr><td class="mut">—</td></tr></table></div>
<h2>Use it</h2>
<div class="card" style="padding:10px 14px" id="use"></div>
</main><script>
const esc=s=>String(s??'').replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
const size=n=>{if(!n)return'';const u=['B','KiB','MiB','GiB','TiB'];let i=0;while(n>=1024&&i<4){n/=1024;i++}return n.toFixed(i?1:0)+' '+u[i]};
function table(id,head,rows){document.getElementById(id).innerHTML=rows.length?'<tr>'+head.map(h=>'<th>'+h+'</th>').join('')+'</tr>'+rows.map(r=>'<tr>'+r.map(c=>'<td>'+c+'</td>').join('')+'</tr>').join(''):'<tr><td class="mut">Nothing here yet.</td></tr>'}
fetch('/dbcanvas/manifest.json').then(r=>r.json()).then(m=>{
  const c=m.contents||{};const host=location.host;
  document.getElementById('sub').textContent=m.fqdn+' · '+size(c.diskBytes)+' on disk · updated '+(c.updatedAt||'never');
  table('repos',['Repository','OS','Arch','Packages','Size','Versions',''],(c.repos||[]).map(x=>[esc(x.repo),esc(x.target),esc(x.arch),x.error?'':x.packages,x.error?'':size(x.bytes),esc((x.versions||[]).join(', ')),x.error?'<span class="bad">'+esc(x.error)+'</span>':'<a href="'+esc(x.path)+'">browse</a>']));
  table('ops',['Operator','Version','Images','Charts',''],(c.operators||[]).map(o=>[esc(o.kind),esc(o.version),(o.images||[]).length,esc((o.charts||[]).join(', ')),o.error?'<span class="bad">'+esc(o.error)+'</span>':'<a href="'+esc(o.path)+'">manifests</a>']));
  table('charts',['Chart','Version','App version',''],(c.charts||[]).map(x=>[esc(x.name),esc(x.version),esc(x.appVersion),'<a href="/charts/'+esc(x.file)+'">'+esc(x.file)+'</a>']));
  table('images',['Image','In this registry','From',''],(c.images||[]).map(x=>[esc(x.source),'<code>'+esc(m.registry+'/'+x.local)+'</code>',esc(x.from||''),x.error?'<span class="bad">'+esc(x.error)+'</span>':'']));
  document.getElementById('use').innerHTML='<p>Helm: <code>helm repo add dbcanvas http://'+esc(m.fqdn)+'/charts</code></p><p>k3s / containerd mirror: <a href="/dbcanvas/registries.yaml">registries.yaml</a> → <code>/etc/rancher/k3s/registries.yaml</code></p><p>apt signing key: <a href="/dbcanvas-repo.gpg">dbcanvas-repo.gpg</a> (<a href="/dbcanvas-repo.asc">ASCII</a>) · RPMs keep Percona\'s signature: <a href="/percona/yum/PERCONA-PACKAGING-KEY">PERCONA-PACKAGING-KEY</a></p><p class="mut">From outside the stack this page is '+esc(host)+'; inside it, http://'+esc(m.fqdn)+'/.</p>';
}).catch(e=>{document.getElementById('sub').textContent='No manifest yet — the first sync has not finished.'});
</script></body></html>
`
