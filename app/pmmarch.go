package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// pmmarch.go — which architecture a PMM 3 server runs on.
//
// percona/pmm-server publishes arm64 images from 3.10.0; earlier releases are amd64 only. So a
// PMM node runs on its own architecture (or Automatic's) when its release has an image for it,
// which the registry says, and otherwise on amd64, under emulation on an arm64 host. A PMM that
// is deployed keeps the architecture it was built with: its data volume outlives the container,
// and a recreate (an upgrade) must not move it to the other one.

// pmmFirstArm64 is the first PMM release with an arm64 image, for messages.
const pmmFirstArm64 = "3.10.0"

var registryPlatforms = struct {
	sync.Mutex
	m map[string]platformsAt
}{m: map[string]platformsAt{}}

type platformsAt struct {
	platforms []string
	at        time.Time
}

// RemotePlatforms is what the registry lists for ref ("linux/amd64", "linux/arm64"); empty when
// it lists none (a single-platform image).
func (d *Docker) RemotePlatforms(ctx context.Context, ref string) ([]string, error) {
	resp, err := d.do(ctx, "GET", "/distribution/"+ref+"/json", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, errBody("look up image", resp)
	}
	var out struct {
		Platforms []struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"Platforms"`
	}
	if err := json.Unmarshal(drain(resp), &out); err != nil {
		return nil, err
	}
	var list []string
	for _, p := range out.Platforms {
		if p.OS != "unknown" && p.Architecture != "unknown" {
			list = append(list, p.OS+"/"+p.Architecture)
		}
	}
	return list, nil
}

// imageHasArch reports whether ref has an image for arch, and whether that is known: the
// registry's answer (kept for an hour), else a local copy of that architecture. A
// single-platform image counts as amd64, which every such image DBCanvas pulls is.
func (a *App) imageHasArch(ctx context.Context, ref, arch string) (has, known bool) {
	registryPlatforms.Lock()
	e, ok := registryPlatforms.m[ref]
	registryPlatforms.Unlock()
	if !ok || time.Since(e.at) > time.Hour {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		list, err := a.docker.RemotePlatforms(cctx, ref)
		cancel()
		if err != nil {
			if exists, _ := a.docker.ImageExists(ctx, ref); exists {
				if have, err := a.docker.ImageArch(ctx, ref); err == nil && have != "" {
					return have == arch, true
				}
			}
			return false, false
		}
		e = platformsAt{platforms: list, at: time.Now()}
		registryPlatforms.Lock()
		registryPlatforms.m[ref] = e
		registryPlatforms.Unlock()
	}
	if len(e.platforms) == 0 {
		return arch == "amd64", true
	}
	for _, p := range e.platforms {
		if p == "linux/"+arch {
			return true, true
		}
	}
	return false, true
}

// pmmArch is the architecture PMM node n runs on in stack stackID, with a note when that is
// not the one it would have, or an error when an arch it was given cannot be had.
func (a *App) pmmArch(ctx context.Context, stackID int64, n designNode) (arch, note string, err error) {
	if dep, err := a.store.GetDeployment(stackID, n.ID); err == nil && len(dep.Config) > 0 {
		var cfg pmmConfig
		if json.Unmarshal(dep.Config, &cfg) == nil && cfg.Arch != "" {
			return cfg.Arch, "", nil // as it was built
		}
	}
	_, tag, ref := pmmImage(loadPMMCatalog(), n.Version)
	want := archOr(n.Arch)
	if want == "amd64" {
		return "amd64", "", nil
	}
	has, known := a.imageHasArch(ctx, ref, want)
	if has {
		return want, "", nil
	}
	if n.Arch != "" {
		if !known {
			return "", "", fmt.Errorf("could not find out whether PMM %s has an %s image (the registry did not answer); leave the architecture on Automatic, or choose amd64", tag, want)
		}
		return "", "", fmt.Errorf("PMM %s has no %s image (PMM publishes arm64 from %s): choose %s or later, or amd64", tag, want, pmmFirstArm64, pmmFirstArm64)
	}
	return "amd64", fmt.Sprintf("PMM %s has no %s image, so it runs as amd64 under emulation (arm64 images start at PMM %s)", tag, want, pmmFirstArm64), nil
}

// pmmArchIssues checks every PMM node's architecture against its release.
func (a *App) pmmArchIssues(ctx context.Context, stackID int64, doc designDoc) []issue {
	var out []issue
	for _, n := range doc.Nodes {
		if n.Type != "pmm" {
			continue
		}
		what := firstNonEmpty(n.Label, n.ID)
		arch, note, err := a.pmmArch(ctx, stackID, n)
		switch {
		case err != nil:
			out = append(out, issue{Level: "error", Message: what + ": " + err.Error()})
		case note != "":
			out = append(out, issue{Level: "info", Message: what + ": " + note})
		case a.hostArch != "" && arch != a.hostArch:
			out = append(out, issue{Level: "info", Message: fmt.Sprintf("%s runs as %s under emulation on this %s Docker host: it works, more slowly", what, arch, a.hostArch)})
		}
	}
	return out
}
