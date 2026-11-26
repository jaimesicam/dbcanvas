package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestSystemdImageParts(t *testing.T) {
	cases := []struct {
		image, os, ver, arch string
		ok                   bool
	}{
		{"dbcanvas-systemd:oraclelinux-9-arm64-v0.1.0", "oraclelinux", "9", "arm64", true},
		{"ghcr.io/jaimesicam/dbcanvas-systemd:ubuntu-22.04-amd64-v0.1.0", "ubuntu", "22.04", "amd64", true},
		{"dbcanvas-systemd:debian-13-amd64", "debian", "13", "amd64", true},
		{"dbcanvas-intranet:oraclelinux-9-arm64-v0.1.0", "", "", "", false},
		{"dbcanvas-kubeadm:ubuntu-24.04-arm64-v0.1.0", "ubuntu", "24.04", "arm64", true}, // a Kubeadm node
		{"dbcanvas-systemd:oraclelinux-9-s390x-v0.1.0", "", "", "", false},
		{"dbcanvas-systemd", "", "", "", false},
		{"percona/pmm-server:3", "", "", "", false},
	}
	for _, c := range cases {
		os_, ver, arch, ok := systemdImageParts(c.image)
		if ok != c.ok || os_ != c.os || ver != c.ver || arch != c.arch {
			t.Errorf("%s: got %q %q %q %v, want %q %q %q %v", c.image, os_, ver, arch, ok, c.os, c.ver, c.arch, c.ok)
		}
	}
}

func TestIsKubeadmImage(t *testing.T) {
	if !isKubeadmImage("ghcr.io/x/dbcanvas-kubeadm:ubuntu-24.04-amd64-v0.1.0") || isKubeadmImage("dbcanvas-systemd:ubuntu-24.04-amd64-v0.1.0") {
		t.Fatal("isKubeadmImage")
	}
}

func TestLimaInstanceName(t *testing.T) {
	a := limaInstanceName("dbcanvas-someone-13-n-pxc-cluster-01-2")
	if a != limaInstanceName("dbcanvas-someone-13-n-pxc-cluster-01-2") {
		t.Fatal("instance name is not stable")
	}
	if a == limaInstanceName("dbcanvas-someone-13-n-pxc-cluster-01-3") {
		t.Fatal("two nodes share an instance name")
	}
	if !regexp.MustCompile(`^dbc-[0-9a-f]{12}$`).MatchString(a) {
		t.Fatalf("instance name %q", a)
	}
}

func TestLimaHomeProblem(t *testing.T) {
	if p := limaHomeProblem("/Users/someone/.dbcanvas/lima"); p != "" {
		t.Fatalf("usual home refused: %s", p)
	}
	if p := limaHomeProblem("/" + strings.Repeat("x", 90)); p == "" {
		t.Fatal("a home too long for Lima's sockets was accepted")
	}
}

func TestParseLimaVersion(t *testing.T) {
	for in, want := range map[string][3]int{
		"limactl version 2.3.0\n":       {2, 3, 0},
		"limactl version v2.0.1-beta.1": {2, 0, 1},
		"limactl version 1.2":           {1, 2, 0},
		"":                              {0, 0, 0},
	} {
		if got := parseLimaVersion(in); got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
	if !versionAtLeast([3]int{2, 3, 0}, limaMinVersion) || versionAtLeast([3]int{1, 11, 9}, limaMinVersion) {
		t.Fatal("version comparison is wrong")
	}
}

func TestLimaCreateArgs(t *testing.T) {
	other := "amd64"
	if runtime.GOARCH == "amd64" {
		other = "arm64"
	}
	native := limaCreateArgs("dbc-1", "template:oraclelinux-9", runtime.GOARCH, 0, 0)
	for _, want := range []string{"--plain", "--tty=false", "--name=dbc-1", "--cpus=2", "--memory=2", "template:oraclelinux-9"} {
		if !slices.Contains(native, want) {
			t.Errorf("native args %v lack %s", native, want)
		}
	}
	if slices.ContainsFunc(native, func(s string) bool { return strings.HasPrefix(s, "--vm-type") }) {
		t.Errorf("a native VM must keep Lima's default VM type: %v", native)
	}
	if native[len(native)-1] != "template:oraclelinux-9" {
		t.Errorf("the template must come last: %v", native)
	}
	emu := limaCreateArgs("dbc-2", "template:ubuntu-24.04", other, 1, 1536)
	for _, want := range []string{"--vm-type=qemu", "--arch=" + limaArch(other), "--cpus=1", "--memory=1.5"} {
		if !slices.Contains(emu, want) {
			t.Errorf("emulated args %v lack %s", emu, want)
		}
	}
}

func TestLimaTemplateOverride(t *testing.T) {
	if tpl, ok := limaTemplate("ubuntu", "24.04"); !ok || tpl != "template:ubuntu-24.04" {
		t.Fatalf("got %q %v", tpl, ok)
	}
	t.Setenv("DBCANVAS_LIMA_TEMPLATE_UBUNTU_24_04", "/srv/own.yaml")
	if tpl, _ := limaTemplate("ubuntu", "24.04"); tpl != "/srv/own.yaml" {
		t.Fatalf("override ignored: %q", tpl)
	}
	if _, ok := limaTemplate("alpine", "3"); ok {
		t.Fatal("an OS dbcanvas has no image for got a template")
	}
}

func TestParseLimaList(t *testing.T) {
	out := `{"name":"dbc-aaa","status":"Running","dir":"/x/dbc-aaa"}
{"name":"dbc-bbb","status":"Stopped"}
not json
`
	got := parseLimaList(out)
	if len(got) != 2 || got["dbc-aaa"].Status != "Running" || limaStateToDocker(got["dbc-bbb"].Status) != "exited" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseLimaSSHConfig(t *testing.T) {
	key := filepath.Join(t.TempDir(), "user")
	os.WriteFile(key, []byte("k"), 0o600)
	cfg := `IdentityFile "/nonexistent/key"
IdentityFile "` + key + `"
StrictHostKeyChecking no
Host lima-dbc-0123456789ab
  Hostname 127.0.0.1
  Port 50022
  User someone
  ControlMaster auto
`
	info := parseLimaSSHConfig(cfg)
	if info.alias != "lima-dbc-0123456789ab" || info.host != "127.0.0.1" || info.port != 50022 || info.user != "someone" || info.identity != key {
		t.Fatalf("got %+v", info)
	}
}

func TestLimaRemoteCommand(t *testing.T) {
	if got := limaRemoteCommand("", []string{"sh", "-c", "echo 'hi'"}, []string{"A=b c"}); got != `sudo env 'PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin' 'A=b c' 'sh' '-c' 'echo '\''hi'\'''` {
		t.Fatalf("root: %s", got)
	}
	if got := limaRemoteCommand("postgres", []string{"psql"}, nil); got != `sudo -u 'postgres' env 'PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin' 'psql'` {
		t.Fatalf("user: %s", got)
	}
	if got := limaRemoteCommand("0", []string{"id"}, nil); got != `sudo env 'PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin' 'id'` {
		t.Fatalf("uid 0: %s", got)
	}
}

// TestLimaBaselineMatchesImages keeps a VM's baseline in step with the node images: every package
// a Dockerfile installs (systemd aside, which a VM has) must be in the VM's baseline too.
func TestLimaBaselineMatchesImages(t *testing.T) {
	for file, script := range map[string]string{
		"../images/rhel.Dockerfile":   limaBaselineRHEL,
		"../images/debian.Dockerfile": limaBaselineDebian,
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(b), "\\\n", " ")
		var pkgs []string
		for _, m := range regexp.MustCompile(`(?:yum|apt-get) (?:-y )?install (?:-y )?(?:--no-install-recommends )?([^;]+);`).FindAllStringSubmatch(text, -1) {
			for _, p := range strings.Fields(m[1]) {
				if !strings.HasPrefix(p, "-") && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "http") && p != "systemd" && p != "systemd-sysv" {
					pkgs = append(pkgs, p)
				}
			}
		}
		if len(pkgs) < 5 {
			t.Fatalf("%s: found only %v; has the install line changed shape?", file, pkgs)
		}
		words := strings.Fields(script)
		for _, p := range pkgs {
			if !slices.Contains(words, p) {
				t.Errorf("%s installs %s, the VM baseline does not", file, p)
			}
		}
		if file == "../images/debian.Dockerfile" && !strings.Contains(script, "/usr/sbin/policy-rc.d") {
			t.Error("the Debian baseline keeps packages from starting their services, as the image does")
		}
		for _, step := range []string{"percona-release setup pt", "percona-toolkit"} {
			if strings.Contains(text, step) && !strings.Contains(script, step) {
				t.Errorf("%s runs %q, the VM baseline does not", file, step)
			}
		}
	}
}

// TestLimaPrepareOrder: the boot ordering records the units present when it runs, so it comes after
// everything the preparation installs — else a Kubeadm VM's containerd waits for a stack link that
// comes up only once the preparation is over.
func TestLimaPrepareOrder(t *testing.T) {
	s := limaPrepareScript(limaMeta{OS: "ubuntu", Hostname: "kube-2", SELinux: "enforcing"}, true)
	kube, order := strings.Index(s, limaKubeadmBaseline), strings.Index(s, limaBootOrder)
	if kube < 0 || order < 0 || kube > order || !strings.HasSuffix(s, limaBootOrder) {
		t.Fatalf("the boot ordering must come last, after the Kubeadm preparation (kubeadm at %d, ordering at %d)", kube, order)
	}
	if !strings.HasPrefix(s, "hostnamectl set-hostname 'kube-2'") {
		t.Fatal("hostname first")
	}
}
