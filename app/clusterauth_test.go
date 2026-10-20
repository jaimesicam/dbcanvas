package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A catalogue whose PXC 9.7 series still trails auth_openid_connect, as the real one did when
// the cluster logins were added: 8.4 has the plugin on its newest build, 9.7 does not.
const clusterAuthVersionsYAML = `image_prefix: dbcanvas-systemd
images:
  - os: oraclelinux
    version: "9"
    platform: linux/amd64
    arch: amd64
    percona_server:
      "8.4":
        - 8.4.11-11.1
        - 8.4.10-10.1
      "8.0":
        - 8.0.46-37.1
    percona_xtradb_cluster:
      "8.4":
        - 8.4.11-11.1
        - 8.4.10-10.1
      "9.7":
        - 9.7.1-1.1
      "8.0":
        - 8.0.46-38.1
pmm:
  repository: percona/pmm-server
  default_tag: "3"
  latest: "3.9.1"
  versions:
    - "3.9.1"
`

func clusterAuthCatalog(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "versions.yaml")
	if err := os.WriteFile(path, []byte(clusterAuthVersionsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSIONS_FILE", path)
	t.Setenv("IMAGES_FILE", path)
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
}

func TestFrameAuthIssues(t *testing.T) {
	clusterAuthCatalog(t)
	dirs := map[string]string{"dir": "intranet"}
	kc := map[string]bool{"kc": true}
	ssl := map[string]bool{"kc": true}
	base := func(typ string) designFrame {
		return designFrame{Type: typ, Label: "c1", OS: "oraclelinux", OSVersion: "9", Arch: "amd64"}
	}
	for _, c := range []struct {
		name  string
		f     func() designFrame
		wants string // "" → no issue
	}{
		{"repl ldap ok", func() designFrame {
			f := base("mysql")
			f.PSMajor, f.LdapAuth, f.LdapDirNodeID = "8.0", true, "dir"
			return f
		}, ""},
		{"repl ldap without directory", func() designFrame {
			f := base("mysql")
			f.PSMajor, f.LdapAuth = "8.0", true
			return f
		}, "no directory is selected"},
		{"repl ldap on 5.7", func() designFrame {
			f := base("mysql")
			f.PSMajor, f.LdapAuth, f.LdapDirNodeID = "5.7", true, "dir"
			return f
		}, "5.7 has no authentication_ldap_simple"},
		{"repl oidc on 8.4 latest", func() designFrame {
			f := base("mysql")
			f.PSMajor, f.EnableOIDC, f.KeycloakNodeID = "8.4", true, "kc"
			return f
		}, ""},
		{"repl oidc on 8.0", func() designFrame {
			f := base("mysql")
			f.PSMajor, f.EnableOIDC, f.KeycloakNodeID = "8.0", true, "kc"
			return f
		}, "does not carry the plugin"},
		{"repl oidc pinned too old", func() designFrame {
			f := base("mysql")
			f.PSMajor, f.PSVersion, f.EnableOIDC, f.KeycloakNodeID = "8.4", "8.4.10-10.1", true, "kc"
			return f
		}, "predates auth_openid_connect"},
		{"repl oidc without keycloak", func() designFrame {
			f := base("mysql")
			f.PSMajor, f.EnableOIDC = "8.4", true
			return f
		}, "not linked to a Keycloak node"},
		{"pxc oidc on 8.4 latest", func() designFrame {
			f := base("pxc")
			f.PXCMajor, f.EnableOIDC, f.KeycloakNodeID = "8.4", true, "kc"
			return f
		}, ""},
		// "Latest" 9.7 is 9.7.1 here, which predates the plugin: the catalog is consulted.
		{"pxc oidc on 9.7 latest", func() designFrame {
			f := base("pxc")
			f.PXCMajor, f.EnableOIDC, f.KeycloakNodeID = "9.7", true, "kc"
			return f
		}, "9.7.1-1.1, which predates"},
		{"pxc ldap ok", func() designFrame {
			f := base("pxc")
			f.PXCMajor, f.LdapAuth, f.LdapDirNodeID = "8.0", true, "dir"
			return f
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := frameAuthIssues(c.f(), dirs, kc, ssl)
			if c.wants == "" {
				if len(got) != 0 {
					t.Fatalf("want no issues, got %v", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0].Message, c.wants) {
				t.Fatalf("want one issue containing %q, got %v", c.wants, got)
			}
		})
	}
}

// The ldap/oidc links land on a Percona MySQL frame, not on its members: the deploy reads the
// frame, and a member copy would be a second, ignored spelling of one setting.
func TestComposeClusterAuthOnFrame(t *testing.T) {
	clusterAuthCatalog(t)
	for _, kind := range []string{"ps-repl", "pxc"} {
		t.Run(kind, func(t *testing.T) {
			doc, _, _, added := composeBuild(t, composeSpec{
				Name: "auth",
				Nodes: []composeNodeSpec{
					{Kind: kind, Version: "8.4", OS: "el9", LDAP: true, OIDC: true},
					{Kind: "keycloak"},
				},
			})
			if len(doc.Frames) != 1 {
				t.Fatalf("got %d frames, want 1", len(doc.Frames))
			}
			f := doc.Frames[0]
			kc := nodesOfType(doc, "keycloak")[0]
			intra := nodesOfType(doc, "intranet")[0]
			if !f.LdapAuth || f.LdapDirNodeID != intra.ID {
				t.Errorf("frame LDAP = %v → %q, want true → the Intranet (%s)", f.LdapAuth, f.LdapDirNodeID, intra.ID)
			}
			if !f.EnableOIDC || f.KeycloakNodeID != kc.ID {
				t.Errorf("frame OIDC = %v → %q, want true → %s", f.EnableOIDC, f.KeycloakNodeID, kc.ID)
			}
			if f.UseLDAP {
				t.Error("the Valkey-only useLdap flag was set on a MySQL frame")
			}
			for _, n := range doc.Nodes {
				if n.FrameID == f.ID && (n.LdapAuth || n.EnableOIDC) {
					t.Errorf("member %s carries the login flags itself", n.Label)
				}
			}
			if !kc.GenerateCert || !strings.Contains(strings.Join(added, " "), "HTTPS") {
				t.Errorf("Keycloak was not given a certificate for an HTTPS issuer (added: %v)", added)
			}
		})
	}
}

func TestComposeClusterOIDCVersionRefused(t *testing.T) {
	clusterAuthCatalog(t)
	for _, c := range []struct{ kind, version string }{
		{"ps-repl", "8.0"},
		{"pxc", "9.7"}, // newest PXC 9.7 here is 9.7.1, before the plugin
		{"pxc", "8.4.10"},
	} {
		_, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
			{Kind: c.kind, Version: c.version, OS: "el9", OIDC: true}, {Kind: "keycloak"},
		}})
		if err == nil || !strings.Contains(err.Error(), "auth_openid_connect") {
			t.Errorf("%s %s with oidc: err = %v, want the auth_openid_connect version refusal", c.kind, c.version, err)
		}
	}
}

// The scripts' stage-only and replica modes are what keep a cluster's accounts from reaching a
// server without the plugin; pin the switches the Go side passes.
func TestClusterAuthScriptModes(t *testing.T) {
	for _, want := range []string{`if [ "$RESTART" = "0" ]`, `systemctl restart "$UNIT"`, "PLUGIN_STATUS", "reverted /etc/my.cnf"} {
		if !strings.Contains(mysqlDirAuthScript, want) {
			t.Errorf("mysqlDirAuthScript lacks %q", want)
		}
	}
	for _, want := range []string{`if [ "$RESTART" != "0" ]`, `if [ "$SQL" != "0" ]`} {
		if !strings.Contains(mysqlOIDCScript, want) {
			t.Errorf("mysqlOIDCScript lacks %q", want)
		}
	}
	if strings.Contains(mysqlDirAuthScript, "systemctl restart mysqld") {
		t.Error("mysqlDirAuthScript hard-codes the RHEL unit; PXC and Ubuntu use mysql")
	}
}
