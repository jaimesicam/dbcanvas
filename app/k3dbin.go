package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// k3dbin.go — the k3d program K3D clusters are made with. k3d is a Docker API client: dbcanvas
// runs it against the same daemon. The app container carried it; the dbcanvas command downloads
// the pinned release for this OS and architecture into ~/.dbcanvas/bin the first time a K3D
// cluster needs it, and refuses a download whose SHA-256 is not the published one. K3D_BIN
// names another; a k3d on PATH is used when the download cannot be made.

const k3dVersion = "v5.8.3"

// k3dChecksums are k3d's published SHA-256s for k3dVersion (its release's checksums.txt).
var k3dChecksums = map[string]string{
	"darwin-amd64": "fd0f8e9e8ea4d8bc3674572ca6ed0833b639bf57c43c708616d937377324cfea",
	"darwin-arm64": "8da468daa7dc7cf7cdd4735f90a9bb05179fa27858250f62e3d8cdf5b5ca0698",
	"linux-amd64":  "dbaa79a76ace7f4ca230a1ff41dc7d8a5036a8ad0309e9c54f9bf3836dbe853e",
	"linux-arm64":  "0b8110f2229631af7402fb828259330985918b08fefd38b7f1b788a1c8687216",
}

var k3dDownload sync.Mutex

func k3dPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// k3dPinnedPath is where the downloaded k3d lives, or "" without a data folder.
func k3dPinnedPath() string {
	dir, err := dataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "bin", "k3d-"+k3dVersion)
}

// k3dBinary resolves the k3d executable, downloading the pinned release when needed.
func k3dBinary() (string, error) {
	if p := strings.TrimSpace(os.Getenv("K3D_BIN")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("K3D_BIN=%s does not exist", p)
	}
	if p := k3dPinnedPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		if _, ok := k3dChecksums[k3dPlatform()]; ok {
			if err := downloadK3D(p); err == nil {
				return p, nil
			} else if onPath, perr := exec.LookPath("k3d"); perr == nil {
				return onPath, nil
			} else {
				return "", fmt.Errorf("could not download k3d %s: %v", k3dVersion, err)
			}
		}
	}
	return exec.LookPath("k3d")
}

// k3dInstalled reports whether k3d is there or can be had, for validation: a design with a
// K3D frame then fails with a clear message instead of mid-deploy.
func k3dInstalled() bool {
	if p := strings.TrimSpace(os.Getenv("K3D_BIN")); p != "" {
		_, err := os.Stat(p)
		return err == nil
	}
	if _, ok := k3dChecksums[k3dPlatform()]; ok && k3dPinnedPath() != "" {
		return true // downloaded on first use
	}
	_, err := exec.LookPath("k3d")
	return err == nil
}

// downloadK3D fetches the pinned k3d to dest, verified against its published checksum.
func downloadK3D(dest string) error {
	k3dDownload.Lock()
	defer k3dDownload.Unlock()
	if _, err := os.Stat(dest); err == nil {
		return nil // another caller just did
	}
	want := k3dChecksums[k3dPlatform()]
	url := fmt.Sprintf("https://github.com/k3d-io/k3d/releases/download/%s/k3d-%s", k3dVersion, k3dPlatform())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "dbcanvas/"+appVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp := dest + ".download"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 256<<20))
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return fmt.Errorf("the download's SHA-256 is %s, not k3d %s's published %s: refused", got, k3dVersion, want)
	}
	return os.Rename(tmp, dest)
}
