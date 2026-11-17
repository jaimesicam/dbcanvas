package main

import (
	"embed"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

// bundle.go — the files the binary carries besides its code (bundle/README.md): the node
// images' Dockerfiles, Pager Panic's banks, the licence texts and Big Hole. The app container
// shipped them beside its binary; the dbcanvas command embeds them, and the service unpacks them
// once per version into the data folder and points each feature at them, unless the
// environment already does.

//go:embed all:bundle
var bundleFS embed.FS

// bundleEnv maps a bundled path to the setting that points a feature at it.
var bundleEnv = []struct{ path, env string }{
	{"images", "DBCANVAS_IMAGES_DIR"},
	{"pagerpanic", "PAGERPANIC_BANKS"},
	{"licenses", "DBCANVAS_LICENSE_DIR"},
	{"bighole.tar.gz", "BIGHOLE_TARBALL"},
}

// applyBundle unpacks the bundle into dataDir/assets/<version> (once) and sets each feature's
// setting to it where the environment leaves it unset.
func applyBundle(dataDir string) {
	dest := filepath.Join(dataDir, "assets", appVersion)
	if _, err := os.Stat(filepath.Join(dest, ".complete")); err != nil {
		if err := unpackBundle(dest); err != nil {
			log.Printf("could not unpack the bundled files into %s: %v", dest, err)
			return
		}
	}
	for _, b := range bundleEnv {
		p := filepath.Join(dest, b.path)
		if _, err := os.Stat(p); err != nil {
			continue // not in this build
		}
		if _, set := os.LookupEnv(b.env); !set {
			os.Setenv(b.env, p)
		}
	}
}

// unpackBundle writes every bundled file under dest, then marks it complete.
func unpackBundle(dest string) error {
	os.RemoveAll(dest)
	err := fs.WalkDir(bundleFS, "bundle", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("bundle", path)
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := bundleFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, ".complete"), nil, 0o600)
}
