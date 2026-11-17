package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// soloversions.go — `dbcanvas versions`: which version catalogue dbcanvas reads, and
// updating it. The binary carries the catalogue of its build (catalogembed.go). Package
// repositories publish new releases between builds, so `dbcanvas versions update`
// downloads the newest catalogue from the project's repository into the data folder,
// where it is read ahead of the built-in one (versionsFilePath). It runs only when asked;
// dbcanvas never fetches it by itself. `dbcanvas versions reset` goes back to the
// built-in catalogue.

// catalogSource is where `versions update` downloads from: the main branch of the
// project's repository, unless DBCANVAS_CATALOG_URL names another (a fork, a mirror).
func catalogSource() string {
	if u := strings.TrimSpace(os.Getenv("DBCANVAS_CATALOG_URL")); u != "" {
		return strings.TrimRight(u, "/") + "/"
	}
	return "https://raw.githubusercontent.com/jaimesicam/dbcanvas/main/"
}

// catalogNames are the catalogue files, one per platform.
var catalogNames = []string{"versions.yaml", "versions-arm64.yaml"}

// catalogMaxBytes bounds a download; a catalogue is well under 1 MB.
const catalogMaxBytes = 16 << 20

// catalogGeneratedAt reads a catalogue's versions_generated_at.
func catalogGeneratedAt(path string) (time.Time, bool) {
	f, err := openCatalog(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	return generatedAtIn(f)
}

func generatedAtIn(r io.Reader) (time.Time, bool) {
	sc := bufio.NewScanner(r)
	for i := 0; sc.Scan() && i < 40; i++ {
		if v, ok := strings.CutPrefix(sc.Text(), "versions_generated_at:"); ok {
			t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
			return t, err == nil
		}
	}
	return time.Time{}, false
}

func describeCatalog(path string) string {
	where := path
	if name, ok := strings.CutPrefix(path, embeddedPrefix); ok {
		where = "built into dbcanvas (" + name + ")"
	}
	if t, ok := catalogGeneratedAt(path); ok {
		return fmt.Sprintf("%s, generated %s", where, t.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return where
}

func cliVersions(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "":
		path := versionsFilePath()
		if path == "" {
			return errors.New("no version catalogue: this build carries none and none was found (dbcanvas versions update downloads one)")
		}
		fmt.Printf("Version catalogue for %s: %s\n", platformArch(), describeCatalog(path))
		if !strings.HasPrefix(path, embeddedPrefix) {
			if b := embeddedCatalog(versionsFileName()); b != "" {
				fmt.Printf("Built in: %s\n", describeCatalog(b))
			}
		}
		return nil
	case "update":
		return updateCatalog()
	case "reset":
		dir, err := dataDir()
		if err != nil {
			return err
		}
		removed := 0
		for _, n := range catalogNames {
			if err := os.Remove(filepath.Join(dir, n)); err == nil {
				removed++
			}
		}
		if removed == 0 {
			fmt.Println("No downloaded catalogue to remove.")
		} else {
			fmt.Println("Removed the downloaded catalogue.")
		}
		fmt.Println("Now reading:", describeCatalog(versionsFilePath()))
		return nil
	}
	return fmt.Errorf("unknown `versions` command %q: use versions, versions update or versions reset", sub)
}

// updateCatalog downloads each platform's catalogue and keeps it when it parses and is
// newer than what dbcanvas would read without it.
func updateCatalog() error {
	dir, err := ensureDataDir()
	if err != nil {
		return err
	}
	src := catalogSource()
	client := &http.Client{Timeout: 60 * time.Second}
	updated := 0
	for _, name := range catalogNames {
		b, err := fetchCatalog(client, src+name)
		if err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		got, ok := generatedAtIn(bytes.NewReader(b))
		if !ok {
			return fmt.Errorf("%s from %s is not a version catalogue (no versions_generated_at)", name, src)
		}
		tmp := filepath.Join(dir, "."+name+".download")
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if imgs := parseImageCatalog(tmp, ""); len(imgs) == 0 {
			os.Remove(tmp)
			return fmt.Errorf("%s from %s lists no images: not kept", name, src)
		}
		// Newer than both the built-in copy and an earlier download, or it is not kept.
		have, haveFrom := time.Time{}, "none"
		for _, p := range []string{embeddedCatalog(name), filepath.Join(dir, name)} {
			if p == "" {
				continue
			}
			if t, ok := catalogGeneratedAt(p); ok && t.After(have) {
				have, haveFrom = t, describeCatalog(p)
			}
		}
		if !got.After(have) {
			os.Remove(tmp)
			fmt.Printf("%s: up to date (%s)\n", name, haveFrom)
			continue
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			os.Remove(tmp)
			return err
		}
		updated++
		fmt.Printf("%s: updated to the catalogue generated %s\n", name, got.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if path := versionsFilePath(); !strings.HasPrefix(path, embeddedPrefix) && filepath.Dir(path) != dir {
		fmt.Printf("Note: %s is read ahead of the data folder, so the update does not apply while it is there.\n", path)
	}
	if updated > 0 {
		fmt.Println("A running dbcanvas uses it from its next deploy; nothing needs restarting.")
	}
	return nil
}

func fetchCatalog(client *http.Client, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dbcanvas/"+appVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, catalogMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > catalogMaxBytes {
		return nil, fmt.Errorf("%s is larger than a catalogue can be", url)
	}
	return b, nil
}
