package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// extraimages.go — the images a node needs on top of the operating-system bases,
// and building one from the web interface.
//
// `make images` builds the OS bases and the Intranet baked onto one of them — the
// DNS and CA a stack is built against, which is why it ships with the bases rather
// than here. Everything else — the VNC image, the two third-party tool images, the
// K3D collector, the six demo applications — is `make extra-images`, because those
// reach into npm, GitHub and Percona's repositories and fail for reasons that have
// nothing to do with the machine you are on. Most stacks need none of them.
//
// The Intranet is still in the catalog below, because the question this file answers
// is "the image this node needs is missing, now what" — and it is missing whenever
// its build was the one thing that failed, which is exactly the case `make images`
// tolerates so that the rest of a first run can finish.
//
// The cost of that split is the moment this file exists for: you design a stack,
// hit Validate, and are told to go back to a terminal, in the repository, and run a
// make target — which is a poor answer when DBCanvas is already talking to the same
// Docker daemon that would do the build. So an admin can build one from here.
//
// WHY THIS CAN WORK AT ALL: not one of images/*.Dockerfile copies anything from its
// build context (every COPY is --from=<stage>; the sources are cloned inside the
// build). So the context is the Dockerfile and nothing else, which is something
// this process can produce without the repository being anywhere near it.
//
// WHY IT IS NOT the Docker API's /build endpoint: that is the classic builder, and
// these Dockerfiles use BuildKit — `FROM --platform=$BUILDPLATFORM` and $TARGETARCH,
// which is how a cross-compiled image gets built at native speed instead of under
// emulation. The classic builder does not set those, and fails outright:
//
//	failed to parse platform : "" is an invalid OS component
//
// So the build runs in a throwaway `docker:cli` container with the daemon's socket
// mounted — the same shape as the K3D diagnostics collector (k8sdiag.go), and the
// docker CLI there brings buildx, so the daemon does a real BuildKit build.

const (
	// buildhelperImage is the throwaway container the build runs in. Pinned to the
	// CLI variant on purpose: it carries buildx and nothing else worth carrying,
	// and it is NOT dind — there is no second daemon here, only a client for the
	// one DBCanvas already uses.
	buildhelperImage = "docker:cli"
	buildhelperRepo  = "docker"
	buildhelperTag   = "cli"
	// Where the one-file build context is assembled inside that container.
	buildContextDir = "/ctx"
	// A build that reaches npm and GitHub over an emulated architecture can take a
	// long time; this is the ceiling before it is called a failure.
	extraImageBuildTimeout = 30 * time.Minute
	// How much of the build log is kept. Enough to see which step failed and why.
	extraImageLogLines = 200
)

// extraImage is one image that a node needs and `make images` does not build.
type extraImage struct {
	ID    string   `json:"id"`    // stable id used by the API and the UI
	Tag   string   `json:"tag"`   // the reference a node asks Docker for
	Label string   `json:"label"` // what to call it on screen
	About string   `json:"about"`
	Make  string   `json:"make"`  // the make target that builds it from a checkout
	Needs []string `json:"needs"` // node types that will not deploy without it
	// Dockerfile is the file under dockerfilesDir() this is built from, and
	// Buildable says whether DBCanvas can do it: an image whose build context is a
	// source tree in the repository cannot be built from a running container that
	// does not have that tree.
	Dockerfile string            `json:"-"`
	Buildable  bool              `json:"buildable"`
	Why        string            `json:"why,omitempty"` // why not, when not
	Args       map[string]string `json:"-"`
	// Platform pins the build to one architecture (the collector's packages are
	// amd64-only). Empty means the platform this installation targets.
	Platform string `json:"-"`
	// BaseOS names a systemd base this is baked onto, as {os, version}. It becomes
	// the BASE_IMAGE build arg, and it has to exist first — which is `make images`.
	BaseOS [2]string `json:"-"`
}

// extraImageCatalog is the whole set, in the order the UI lists them. The tags,
// build args and base pins must match images/service.sh and images/apps.sh, which a
// test checks by reading those files.
func extraImageCatalog() []extraImage {
	return []extraImage{
		{
			ID: "intranet", Tag: intranetImage(""), Label: "Intranet",
			About: "OpenLDAP, bind DNS, an internal CA, a Squid proxy and Roundcube webmail, pre-installed on Oracle Linux 9. Every stack needs one, and baking it saves about a minute per deploy.",
			Make:  "intranet-image", Needs: []string{"intranet"},
			Dockerfile: "intranet.Dockerfile", Buildable: true,
			BaseOS: [2]string{"oraclelinux", "9"},
		},
		{
			ID: "vnc", Tag: vncImage(""), Label: "Ubuntu VNC desktop",
			About: "XFCE, TigerVNC/noVNC, Firefox and the Percona clients on Ubuntu 24.04 — the way to open a node's web console from inside the stack network.",
			Make:  "vnc-image", Needs: []string{"vnc"},
			Dockerfile: "vnc.Dockerfile", Buildable: true,
			BaseOS: [2]string{"ubuntu", "24.04"},
		},
		{
			ID: "k8scollector", Tag: k8sCollectorImage(), Label: "K3D diagnostics collector",
			About: "Debian 12 plus percona-toolkit, for pt-k8s-debug-collector. Pinned to amd64: Percona publishes the toolkit for that architecture only.",
			Make:  "k8scollector-image", Needs: []string{"k3d"},
			Dockerfile: "k8scollector.Dockerfile", Buildable: true,
			Platform: platformAMD64,
		},
		{
			ID: "mclusteradmin", Tag: mcaImage, Label: "MClusterAdmin",
			About: "A MongoDB administration panel, built from upstream source at a pinned tag (upstream publishes no image).",
			Make:  "mclusteradmin-image", Needs: []string{"mclusteradmin"},
			Dockerfile: "mclusteradmin.Dockerfile", Buildable: true,
			Args: map[string]string{"MCA_VERSION": "v" + mcaVersion},
		},
		{
			ID: "bighole", Tag: bigHoleImage, Label: "Big Hole",
			About: "A browser-only MongoDB FTDC viewer, built from upstream source at a pinned commit (upstream publishes no image).",
			Make:  "bighole-image", Needs: []string{"bighole"},
			Dockerfile: "bighole.Dockerfile", Buildable: true,
			Args: map[string]string{"BIGHOLE_REF": bigHoleRef},
		},
		// The demo applications. Listed so the page tells the whole truth about what
		// a stack might be missing, and not buildable from here: their build context
		// is a directory of this repository's own source, which a running container
		// has no copy of.
		{ID: "trafficsim", Tag: trafficSimImage, Label: "Traffic Sim", Make: "trafficsim-image", Needs: []string{"trafficsim"},
			About: "The Valkey Traffic Lab demo app.", Why: whyNotBuildable("trafficsim")},
		{ID: "hotelsim", Tag: hotelSimImage, Label: "Hotel Sim", Make: "hotelsim-image", Needs: []string{"hotelsim"},
			About: "The MongoDB Hotel Reservation Lab demo app.", Why: whyNotBuildable("hotelsim")},
		{ID: "airlinesim", Tag: airlineSimImage, Label: "Airline Sim", Make: "airlinesim-image", Needs: []string{"airlinesim"},
			About: "The MySQL Airline Reservation Lab demo app.", Why: whyNotBuildable("airlinesim")},
		{ID: "carsim", Tag: carSimImage, Label: "Car Rental Sim", Make: "carsim-image", Needs: []string{"carsim"},
			About: "The PostgreSQL Car Rental Lab demo app.", Why: whyNotBuildable("carsim")},
		{ID: "marketchaos", Tag: marketChaosImage, Label: "Unoptimized MySQL Challenge", Make: "marketchaos-image", Needs: []string{"marketchaos"},
			About: "The MarketChaos stock-exchange demo app.", Why: whyNotBuildable("marketchaos")},
		{ID: "stocksim", Tag: stockSimImage, Label: "Stock Market Sim", Make: "stocksim-image", Needs: []string{"stocksim"},
			About: "The CRUD-and-reports stock-exchange demo app.", Why: whyNotBuildable("stocksim")},
	}
}

// whyNotBuildable is the one reason any of these carry, written once.
func whyNotBuildable(dir string) string {
	return "Built from " + dir + "/ in the DBCanvas repository, so it needs a checkout — run `make " + dir + "-image` there."
}

// extraImageByID finds one entry.
func extraImageByID(id string) (extraImage, bool) {
	for _, e := range extraImageCatalog() {
		if e.ID == id {
			return e, true
		}
	}
	return extraImage{}, false
}

// dockerfilesDir is where the Dockerfiles are read from. The app image carries a
// copy (app/Dockerfile COPYs images/*.Dockerfile into it), and a development run
// from the repository root finds them in place. DBCANVAS_IMAGES_DIR overrides both.
func dockerfilesDir() string {
	if d := strings.TrimSpace(os.Getenv("DBCANVAS_IMAGES_DIR")); d != "" {
		return d
	}
	for _, d := range []string{"/opt/dbcanvas/images", "images", "../images"} {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return "/opt/dbcanvas/images"
}

// ---------------------------------------------------------------- build state

// extraImageBuild is the state of one build, live or finished.
type extraImageBuild struct {
	ID         string    `json:"id"`
	Running    bool      `json:"running"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Error      string    `json:"error,omitempty"`
	Log        []string  `json:"log,omitempty"`
	// StartedBy is the account that asked for it, so a second admin watching sees
	// whose build they are looking at.
	StartedBy string `json:"startedBy,omitempty"`
}

// imageBuilds holds the build state per image id. In memory on purpose: a build is
// worth watching while it runs and worth reading right after, and a restarted app
// has nothing in flight to report anyway.
var (
	imageBuildsMu sync.Mutex
	imageBuilds   = map[string]*extraImageBuild{}
)

func imageBuildState(id string) *extraImageBuild {
	imageBuildsMu.Lock()
	defer imageBuildsMu.Unlock()
	b := imageBuilds[id]
	if b == nil {
		return nil
	}
	cp := *b
	cp.Log = append([]string(nil), b.Log...)
	return &cp
}

// ---------------------------------------------------------------- the build

// buildExtraImage builds one image in a throwaway docker:cli container. Blocking;
// callers run it in a goroutine and watch the state.
func (a *App) buildExtraImage(ctx context.Context, e extraImage) error {
	df, err := os.ReadFile(filepath.Join(dockerfilesDir(), e.Dockerfile))
	if err != nil {
		return fmt.Errorf("read %s: %w", e.Dockerfile, err)
	}

	platform := e.Platform
	if platform == "" {
		platform = pullPlatform()
	}
	args := map[string]string{}
	for k, v := range e.Args {
		args[k] = v
	}
	// A baked image is built FROM a systemd base, which `make images` produces. Say
	// so plainly rather than letting the build fail on a missing FROM.
	if e.BaseOS[0] != "" {
		base := fmt.Sprintf("dbcanvas-systemd:%s-%s-%s", e.BaseOS[0], e.BaseOS[1], platformArch())
		if ok, _ := a.docker.ImageExists(ctx, base); !ok {
			return fmt.Errorf("the base image %s is not built — run `make images` first (that one is the operating systems, and it cannot be built from here)", base)
		}
		args["BASE_IMAGE"] = base
	}

	// The helper runs on the HOST's architecture, not the target platform: it is a
	// client for the daemon, and running it emulated would slow every build for no
	// reason. The --platform below is what decides the architecture of the image.
	if err := a.docker.EnsureImage(ctx, buildhelperRepo, buildhelperTag, ""); err != nil {
		return fmt.Errorf("pull %s: %w", buildhelperImage, err)
	}

	name := fmt.Sprintf("dbcanvas-imagebuild-%s-%d", e.ID, time.Now().UnixNano())
	cid, err := a.docker.ContainerCreate(ctx, ContainerSpec{
		Name:  name,
		Image: buildhelperImage,
		// Long enough to outlive the build; the container is removed either way.
		Cmd:       []string{"sleep", fmt.Sprintf("%d", int(extraImageBuildTimeout.Seconds())+60)},
		Binds:     []string{"/var/run/docker.sock:/var/run/docker.sock"},
		NoRestart: true,
	})
	if err != nil {
		return fmt.Errorf("create the build container: %w", err)
	}
	defer a.docker.ContainerRemove(context.WithoutCancel(ctx), cid)
	if err := a.docker.ContainerStart(ctx, cid); err != nil {
		return fmt.Errorf("start the build container: %w", err)
	}

	// The Dockerfile goes in through exec, not a bind mount: a mount would need a
	// path on the Docker host, and this process has no idea what its own files look
	// like from there — it may not even be on the same machine.
	enc := base64.StdEncoding.EncodeToString(df)
	mk := fmt.Sprintf("mkdir -p %s && echo '%s' | base64 -d > %s/Dockerfile", buildContextDir, enc, buildContextDir)
	if res, err := a.docker.Exec(ctx, cid, []string{"sh", "-c", mk}, nil); err != nil || res.Code != 0 {
		return fmt.Errorf("stage the Dockerfile: %v %s", err, strings.TrimSpace(res.Stderr))
	}

	cmd := []string{"docker", "build", "--progress", "plain", "--platform", platform, "-t", e.Tag}
	for _, k := range sortedKeys(args) {
		cmd = append(cmd, "--build-arg", k+"="+args[k])
	}
	cmd = append(cmd, ".")
	line := "cd " + buildContextDir + " && " + strings.Join(quoteAll(cmd), " ") + " 2>&1"

	res, err := a.docker.Exec(ctx, cid, []string{"sh", "-c", line}, nil)
	log := lastLines(res.Stdout+res.Stderr, extraImageLogLines)
	a.appendBuildLog(e.ID, log)
	if err != nil {
		return fmt.Errorf("run the build: %w", err)
	}
	if res.Code != 0 {
		return fmt.Errorf("the build failed (exit %d) — the log below is its last %d lines", res.Code, extraImageLogLines)
	}
	// Believe the daemon rather than the exit code: a build that "succeeded" without
	// producing the tag is a bug worth reporting as one.
	if ok, _ := a.docker.ImageExists(ctx, e.Tag); !ok {
		return fmt.Errorf("the build reported success but %s is not present", e.Tag)
	}
	return nil
}

// quoteAll single-quotes each argument for the shell line the exec runs.
func quoteAll(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (a *App) appendBuildLog(id, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	imageBuildsMu.Lock()
	defer imageBuildsMu.Unlock()
	if b := imageBuilds[id]; b != nil {
		b.Log = strings.Split(strings.TrimRight(text, "\n"), "\n")
	}
}

// startExtraImageBuild registers a build and runs it. Returns false when one is
// already in flight for that image.
func (a *App) startExtraImageBuild(e extraImage, who string) bool {
	imageBuildsMu.Lock()
	if b := imageBuilds[e.ID]; b != nil && b.Running {
		imageBuildsMu.Unlock()
		return false
	}
	imageBuilds[e.ID] = &extraImageBuild{ID: e.ID, Running: true, StartedAt: time.Now(), StartedBy: who}
	imageBuildsMu.Unlock()

	go func() {
		// Detached from the request: a build outlives the HTTP call that asked for
		// it, and a browser that navigates away must not cancel it.
		ctx, cancel := context.WithTimeout(context.Background(), extraImageBuildTimeout)
		defer cancel()
		err := a.buildExtraImage(ctx, e)

		imageBuildsMu.Lock()
		defer imageBuildsMu.Unlock()
		b := imageBuilds[e.ID]
		if b == nil {
			return
		}
		b.Running = false
		b.FinishedAt = time.Now()
		if err != nil {
			b.Error = err.Error()
		}
	}()
	return true
}

// ---------------------------------------------------------------- handlers

// extraImageView is one catalogue entry plus what is true of it right now.
type extraImageView struct {
	extraImage
	Present bool             `json:"present"`
	Build   *extraImageBuild `json:"build,omitempty"`
}

// handleListExtraImages lists the catalogue with each image's presence and the
// state of any build. Readable by any signed-in account — a missing image is why a
// deploy will not start, and that is not privileged information — while building
// one is an admin action.
func (a *App) handleListExtraImages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := []extraImageView{}
	for _, e := range extraImageCatalog() {
		ok, _ := a.docker.ImageExists(ctx, e.Tag)
		out = append(out, extraImageView{extraImage: e, Present: ok, Build: imageBuildState(e.ID)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": out})
}

// handleBuildExtraImage starts a build. 202 with the state, so the caller can poll
// the list; 409 when that image is already building.
func (a *App) handleBuildExtraImage(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	e, found := extraImageByID(r.PathValue("id"))
	if !found {
		writeErr(w, http.StatusNotFound, "no such image")
		return
	}
	if !e.Buildable {
		writeErr(w, http.StatusBadRequest, e.Why)
		return
	}
	if ok, _ := a.docker.ImageExists(r.Context(), e.Tag); ok {
		// Not an error: a second admin clicking Build on something that arrived in
		// the meantime should be told it is there, not handed a rebuild.
		writeJSON(w, http.StatusOK, map[string]any{"present": true, "build": imageBuildState(e.ID)})
		return
	}
	if !a.startExtraImageBuild(e, u.Username) {
		writeErr(w, http.StatusConflict, "that image is already being built")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"build": imageBuildState(e.ID)})
}
