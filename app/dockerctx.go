package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// dockerContextSocket is the Unix socket of the Docker CLI's current context
// (DOCKER_CONTEXT, else ~/.docker/config.json's currentContext), which is how Docker
// Desktop, Rancher Desktop, Colima and OrbStack point the docker command at their engine.
// "" when there is no such context or it is not a local socket.
func dockerContextSocket() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	cfgDir := os.Getenv("DOCKER_CONFIG")
	if cfgDir == "" {
		cfgDir = filepath.Join(home, ".docker")
	}
	name := os.Getenv("DOCKER_CONTEXT")
	if name == "" {
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		if b, err := os.ReadFile(filepath.Join(cfgDir, "config.json")); err == nil {
			json.Unmarshal(b, &cfg)
		}
		name = cfg.CurrentContext
	}
	if name == "" || name == "default" {
		return ""
	}
	sum := sha256.Sum256([]byte(name))
	b, err := os.ReadFile(filepath.Join(cfgDir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return ""
	}
	if h := meta.Endpoints["docker"].Host; strings.HasPrefix(h, "unix://") {
		return strings.TrimPrefix(h, "unix://")
	}
	return ""
}
