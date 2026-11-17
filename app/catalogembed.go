package main

import (
	"embed"
	"io"
	"os"
	"strings"
)

// catalogembed.go — the version catalogue built into the binary. The catalogue is read
// from a file when there is one (versionsFilePath); the dbcanvas binary, downloaded on
// its own, has none, and reads the copy embedded at build time instead. Its path is then
// "embedded:<name>", which openCatalog understands.

//go:embed catalog
var catalogFS embed.FS

const embeddedPrefix = "embedded:"

// embeddedCatalog returns "embedded:<name>" when the binary carries that file, else "".
func embeddedCatalog(name string) string {
	if _, err := catalogFS.ReadFile("catalog/" + name); err != nil {
		return ""
	}
	return embeddedPrefix + name
}

// openCatalog opens a catalogue path: a file, or an embedded copy.
func openCatalog(path string) (io.ReadCloser, error) {
	if name, ok := strings.CutPrefix(path, embeddedPrefix); ok {
		return catalogFS.Open("catalog/" + name)
	}
	return os.Open(path)
}
