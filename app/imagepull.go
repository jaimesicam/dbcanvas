package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

// imagepull.go — DBCanvas's own images (dbcanvas-*) pulled from the image registry when a
// deploy needs one this host does not have.
//
// `make images` builds them and `make pull-images` (scripts/registry.sh) fetches them; either
// can leave one out — a build that failed, a node type added after the install, a release
// bumped without rebuilding. A deploy then pulls what it is missing instead of failing, the
// way it always has for upstream images such as PMM's:
//
//   - Validate reports a missing image the registry has as a warning that the deploy pulls it,
//     and one the registry cannot provide as an error with the registry's own reason.
//   - Creating a container whose image is missing pulls it and creates it again (the Docker
//     client's pullMissing hook), so every node type is covered at the one place its
//     container is made; a provisioner that checks first uses ensureImage instead.
//   - A failed pull is the node's error: what was asked for, from where, and why it failed.
//
// The names follow scripts/registry.sh: an image is <IMAGE_REGISTRY>/<local name>, and a demo
// application (a tag with no architecture in it) is pulled by its architecture tag first,
// <tag>-<arch>, then by its plain multi-architecture one. The pulled image is checked to be of
// the architecture asked for, then tagged with the local name the deploy uses.

// defaultImageRegistry is where releases are published (Makefile IMAGE_REGISTRY).
const defaultImageRegistry = "ghcr.io/jaimesicam"

// imageRegistry is the registry DBCanvas pulls its own images from, without a trailing slash,
// or "" when pulling is turned off (IMAGE_REGISTRY=off).
func imageRegistry() string {
	r := strings.TrimSuffix(strings.TrimSpace(envOr("IMAGE_REGISTRY", defaultImageRegistry)), "/")
	if r == "off" || r == "none" {
		return ""
	}
	return r
}

// ownImageRe finds a DBCanvas image reference in text: dbcanvas-<name>:<tag>.
var ownImageRe = regexp.MustCompile(`\bdbcanvas-[a-z0-9]+(?:-[a-z0-9]+)*:[A-Za-z0-9][A-Za-z0-9._-]*[A-Za-z0-9]`)

// ownImage reports whether ref is one of DBCanvas's own images, the only ones pulled here
// (an upstream image is pulled by the node that uses it).
func ownImage(ref string) bool {
	return strings.HasPrefix(ref, "dbcanvas-") && strings.Contains(ref, ":") && !strings.Contains(ref, "/")
}

func splitRef(ref string) (repo, tag string) {
	i := strings.LastIndex(ref, ":")
	return ref[:i], ref[i+1:]
}

// tagArch is the architecture an image's tag names (…-arm64-v0.0.30), or "" for a tag that
// names none: a demo application, built for the host.
func tagArch(tag string) string {
	for _, a := range []string{"amd64", "arm64"} {
		if strings.Contains("-"+tag+"-", "-"+a+"-") {
			return a
		}
	}
	return ""
}

// normArch maps Docker's /info architecture (x86_64, aarch64) to an image's (amd64, arm64).
func normArch(a string) string {
	switch {
	case strings.Contains(a, "aarch64"), strings.Contains(a, "arm64"):
		return "arm64"
	case strings.Contains(a, "x86_64"), strings.Contains(a, "amd64"):
		return "amd64"
	}
	return a
}

// imageMakeTarget is the make target that builds ref, for the message when it cannot be pulled.
func imageMakeTarget(ref string) string {
	repo, _ := splitRef(ref)
	switch name := strings.TrimPrefix(repo, "dbcanvas-"); name {
	case "systemd":
		return "images"
	default:
		return name + "-image"
	}
}

// pullSource is one registry reference to try for an image, and the platform to ask for.
type pullSource struct {
	repo, tag, platform string
}

func (p pullSource) String() string { return p.repo + ":" + p.tag }

// pullSources lists where ref comes from, in the order to try them.
func pullSources(registry, ref, hostArch string) []pullSource {
	repo, tag := splitRef(ref)
	remote := registry + "/" + repo
	if arch := tagArch(tag); arch != "" {
		return []pullSource{{remote, tag, "linux/" + arch}}
	}
	arch := normArch(hostArch)
	if arch == "" {
		arch = "amd64"
	}
	plat := "linux/" + arch
	return []pullSource{{remote, tag + "-" + arch, plat}, {remote, tag, plat}}
}

// imagePulls makes the nodes that need one image share one pull: a three-member cluster asks
// for its base image three times at once.
var imagePulls = struct {
	sync.Mutex
	m map[string]*imagePullCall
}{m: map[string]*imagePullCall{}}

type imagePullCall struct {
	done chan struct{}
	err  error
}

// pullOwnImage pulls ref from the registry and gives it its local name. note, when not nil, is
// told what is being pulled from where, for a node's deploy log.
func (a *App) pullOwnImage(ctx context.Context, d *Docker, ref string, note func(string)) error {
	if !ownImage(ref) {
		return fmt.Errorf("image %s is not on this host", ref)
	}
	registry := imageRegistry()
	if registry == "" {
		return fmt.Errorf("image %s is not on this host, and pulling DBCanvas images is turned off (IMAGE_REGISTRY=off) — run `make %s`", ref, imageMakeTarget(ref))
	}
	imagePulls.Lock()
	c, busy := imagePulls.m[ref]
	if !busy {
		c = &imagePullCall{done: make(chan struct{})}
		imagePulls.m[ref] = c
	}
	imagePulls.Unlock()
	if note != nil {
		note(fmt.Sprintf("Pulling %s from %s", ref, registry))
	}
	if busy {
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// The pull is shared, so it does not end with the first caller's context; it is
	// bounded on its own instead.
	pctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	c.err = a.doPullOwnImage(pctx, d, registry, ref)
	cancel()
	imagePulls.Lock()
	delete(imagePulls.m, ref)
	imagePulls.Unlock()
	close(c.done)
	return c.err
}

func (a *App) doPullOwnImage(ctx context.Context, d *Docker, registry, ref string) error {
	start := time.Now()
	var tried []string
	for _, src := range pullSources(registry, ref, d.HostArch(ctx)) {
		if err := d.ImagePull(ctx, src.repo, src.tag, src.platform); err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", src, err))
			continue
		}
		want := strings.TrimPrefix(src.platform, "linux/")
		if got, err := d.ImageArch(ctx, src.String()); err != nil || got != want {
			if err == nil {
				err = fmt.Errorf("it is %s, not %s", got, want)
			}
			tried = append(tried, fmt.Sprintf("%s: %v", src, err))
			continue
		}
		repo, tag := splitRef(ref)
		if err := d.ImageTag(ctx, src.String(), repo, tag); err != nil {
			return fmt.Errorf("pulled %s but could not tag it %s: %v", src, ref, err)
		}
		// A demo application also gets the architecture tag a local build gives it.
		if tagArch(tag) == "" {
			d.ImageTag(ctx, src.String(), repo, tag+"-"+want)
		}
		log.Printf("pulled %s from %s in %s", ref, src, time.Since(start).Round(time.Second))
		return nil
	}
	return fmt.Errorf("image %s is not on this host and could not be pulled — %s. Build it with `make %s`",
		ref, strings.Join(tried, "; "), imageMakeTarget(ref))
}

// ensureImage makes sure ref is on the engine's host, pulling one of DBCanvas's own images
// when it is not. note, when not nil, is told about a pull before it starts.
func (a *App) ensureImage(ctx context.Context, eng Engine, ref string, note func(string)) error {
	if ok, _ := eng.ImageExists(ctx, ref); ok {
		return nil
	}
	d, isDocker := eng.(*Docker)
	if !isDocker || !ownImage(ref) {
		return fmt.Errorf("image %s not found — run `make %s` first", ref, imageMakeTarget(ref))
	}
	return a.pullOwnImage(ctx, d, ref, note)
}

// pullMissing is the Docker client's hook for a container create that found no image.
func (a *App) pullMissing(ctx context.Context, ref string) error {
	if !ownImage(ref) {
		return fmt.Errorf("image %s is not on this host", ref)
	}
	return a.pullOwnImage(ctx, a.docker, ref, nil)
}

// remoteAvail remembers what the registry said about an image for a while, so Validate does
// not ask again on every click.
var remoteAvail = struct {
	sync.Mutex
	m map[string]remoteAnswer
}{m: map[string]remoteAnswer{}}

type remoteAnswer struct {
	src string // the reference that is there, when ok
	err string // why none is, otherwise
	ok  bool
	at  time.Time
}

// remoteImage asks the registry, through the Docker daemon and without downloading anything,
// whether it can provide ref. It returns the reference that would be pulled, or why none can.
func (a *App) remoteImage(ctx context.Context, d *Docker, ref string) (string, error) {
	registry := imageRegistry()
	if registry == "" {
		return "", fmt.Errorf("pulling DBCanvas images is turned off (IMAGE_REGISTRY=off)")
	}
	remoteAvail.Lock()
	ans, ok := remoteAvail.m[ref]
	remoteAvail.Unlock()
	ttl := 10 * time.Minute
	if ok && !ans.ok {
		ttl = time.Minute
	}
	if !ok || time.Since(ans.at) > ttl {
		ans = remoteAnswer{at: time.Now()}
		var why []string
		for _, src := range pullSources(registry, ref, d.HostArch(ctx)) {
			if err := d.RemoteImageExists(ctx, src.String(), src.platform); err != nil {
				why = append(why, fmt.Sprintf("%s: %v", src, err))
				continue
			}
			ans.ok, ans.src = true, src.String()
			break
		}
		if !ans.ok {
			ans.err = strings.Join(why, "; ")
		}
		remoteAvail.Lock()
		remoteAvail.m[ref] = ans
		remoteAvail.Unlock()
	}
	if !ans.ok {
		return "", fmt.Errorf("%s", ans.err)
	}
	return ans.src, nil
}

// pullableImageIssues turns Validate's "missing image" errors for DBCanvas's own images into
// what the deploy will do about them: a warning that it pulls the image when the registry has
// it, and the error with the registry's reason when it does not.
func (a *App) pullableImageIssues(ctx context.Context, issues []issue) []issue {
	if a.docker == nil {
		return issues
	}
	for i, it := range issues {
		if it.Level != "error" || !strings.Contains(strings.ToLower(it.Message), "image") {
			continue
		}
		ref := ownImageRe.FindString(it.Message)
		if ref == "" {
			continue
		}
		if ok, _ := a.docker.ImageExists(ctx, ref); ok {
			continue
		}
		src, err := a.remoteImage(ctx, a.docker, ref)
		if err != nil {
			issues[i].Message += " — and it cannot be pulled: " + err.Error()
			continue
		}
		issues[i] = issue{Level: "warning", Image: it.Image,
			Message: "Image " + ref + " is not on this host yet — the deploy pulls it from " + src}
	}
	return issues
}
