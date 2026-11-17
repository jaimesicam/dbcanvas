package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// soloconfig.go — ~/.dbcanvas/config.yaml, the dbcanvas command's settings.
//
// The app container is configured by environment variables (docs/CONFIGURATION.md); the
// command reads the same settings from this file, so nothing has to be exported in a
// shell profile. Each setting becomes the environment variable it names, and a variable
// already set in the environment wins over the file. Unknown keys are refused, so a typo
// is an error rather than a setting silently ignored.

const soloConfigName = "config.yaml"

// soloConfig is config.yaml.
type soloConfig struct {
	Instance          string `yaml:"instance"`          // DBCANVAS_INSTANCE
	Port              int    `yaml:"port"`              // APP_PORT
	PublicURL         string `yaml:"publicUrl"`         // PUBLIC_URL
	Domain            string `yaml:"domain"`            // DOMAIN
	DockerSocket      string `yaml:"dockerSocket"`      // DOCKER_SOCK
	Platform          string `yaml:"platform"`          // DOCKER_PLATFORM
	ImageRegistry     string `yaml:"imageRegistry"`     // IMAGE_REGISTRY
	DeploymentTimeout string `yaml:"deploymentTimeout"` // DEPLOYMENT_TIMEOUT
	// Env sets any other setting by its variable name (a node password, EXPERIMENTAL).
	Env map[string]string `yaml:"env"`
}

// soloConfigTemplate is written on first run: every setting, commented out.
const soloConfigTemplate = `# dbcanvas settings. Uncomment a line to set it. A setting also set as an environment
# variable (named after each line) uses the environment's value.

# The name everything dbcanvas creates on Docker starts with (DBCANVAS_INSTANCE).
# Default: dbcanvas-<your login>.
#instance: dbcanvas-me

# The browser's port on 127.0.0.1 (APP_PORT). Default: 7480, or a free one when it is taken.
#port: 7480

# The address your own tunnel or reverse proxy gives dbcanvas, used in shared-session join
# links (PUBLIC_URL). dbcanvas itself listens on 127.0.0.1 only.
#publicUrl: https://lab.example.com

# The DNS domain of every stack's nodes (DOMAIN).
#domain: example.net

# The Docker API socket (DOCKER_SOCK). Default: the current Docker context's.
#dockerSocket: /var/run/docker.sock

# The platform of the node images (DOCKER_PLATFORM). Default: this machine's.
#platform: linux/arm64

# Where node images are pulled from when they are not local (IMAGE_REGISTRY).
#imageRegistry: ghcr.io/example

# How long a node may take to deploy (DEPLOYMENT_TIMEOUT), e.g. 45m.
#deploymentTimeout: 45m

# Any other setting from the configuration reference, by its variable name: the node
# accounts' passwords, EXPERIMENTAL, ...
#env:
#  MYSQL_ROOT_PASSWORD: change-me
#  EXPERIMENTAL: "1"
`

var envNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// readSoloConfig reads config.yaml from dir; a missing file is no settings.
func readSoloConfig(dir string) (soloConfig, error) {
	var c soloConfig
	b, err := os.ReadFile(filepath.Join(dir, soloConfigName))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	// An empty or all-comment file decodes as io.EOF: no settings.
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return c, fmt.Errorf("%s: %v", filepath.Join(dir, soloConfigName), err)
	}
	for k := range c.Env {
		if !envNameRe.MatchString(k) {
			return c, fmt.Errorf("%s: env: %q is not a setting's variable name", filepath.Join(dir, soloConfigName), k)
		}
	}
	return c, nil
}

// vars is the configuration as environment variables.
func (c soloConfig) vars() map[string]string {
	out := map[string]string{}
	for k, v := range c.Env {
		out[k] = v
	}
	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	set("DBCANVAS_INSTANCE", c.Instance)
	if c.Port != 0 {
		out["APP_PORT"] = strconv.Itoa(c.Port)
	}
	set("PUBLIC_URL", c.PublicURL)
	set("DOMAIN", c.Domain)
	set("DOCKER_SOCK", c.DockerSocket)
	set("DOCKER_PLATFORM", c.Platform)
	set("IMAGE_REGISTRY", c.ImageRegistry)
	set("DEPLOYMENT_TIMEOUT", c.DeploymentTimeout)
	return out
}

// applySoloConfig reads dir's config.yaml into the environment, below what the
// environment already sets, and writes the commented template when there is no file yet.
func applySoloConfig(dir string) error {
	path := filepath.Join(dir, soloConfigName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, []byte(soloConfigTemplate), 0o600); err != nil {
			return err
		}
	}
	c, err := readSoloConfig(dir)
	if err != nil {
		return err
	}
	for k, v := range c.vars() {
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return nil
}
