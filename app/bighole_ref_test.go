package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The commit is the version here, and it is written in two places that have to agree: the Go
// constant the panel reports and app/Dockerfile's BIGHOLE_REF, which builds the files every node
// serves. Drifting means a node claiming one Big Hole and serving another.
func TestBigHoleRefIsPinnedEverywhere(t *testing.T) {
	if len(bigHoleRef) != 40 {
		t.Fatalf("bigHoleRef = %q, want a full 40-character commit", bigHoleRef)
	}
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ARG BIGHOLE_REF="+bigHoleRef) {
		t.Errorf("app/Dockerfile does not build Big Hole at %s", bigHoleRef)
	}
	if !strings.Contains(string(b), bigHoleTarballDefault) {
		t.Errorf("app/Dockerfile does not put the files where bighole.go reads them (%s)", bigHoleTarballDefault)
	}
}

func TestMCAImageIsUpstreamAndPinned(t *testing.T) {
	const upstream = "ghcr.io/przemekmalkowski/mclusteradmin"
	if mcaImageRepo != upstream {
		t.Errorf("the panel runs %q, not upstream's published image %q", mcaImageRepo, upstream)
	}
	if !strings.HasSuffix(mcaImage, ":"+mcaVersion) {
		t.Errorf("image %q does not carry version %q", mcaImage, mcaVersion)
	}
	if mcaVersion == "latest" || mcaVersion == "" {
		t.Errorf("the panel's image tag is %q — pin a release, so a redeploy is the same panel", mcaVersion)
	}
	// And nothing is left building one locally. Matched on the names a build would
	// have to use — the Dockerfile, the tag, the make target — rather than on the
	// word, so the scripts can still say why they no longer build it.
	for f, stale := range map[string][]string{
		"../images/service.sh": {"mclusteradmin.Dockerfile", "MCA_TAG", "MCA_VERSION", "dbcanvas-mclusteradmin"},
		"../Makefile":          {"mclusteradmin-image", "dbcanvas-mclusteradmin"},
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, bad := range stale {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s still carries %q — the node pulls %s, nothing builds it", f, bad, mcaImage)
			}
		}
	}
	if _, err := os.Stat("../images/mclusteradmin.Dockerfile"); err == nil {
		t.Error("images/mclusteradmin.Dockerfile is still there — nothing builds the panel any more")
	}
	// A pulled image has no catalogue entry, so a Validate must not offer a Build
	// button for it: there is nothing to build and the deploy pulls it anyway.
	if _, ok := extraImageByID("mclusteradmin"); ok {
		t.Error("mclusteradmin is still in the buildable-image catalogue")
	}
}

// The catalogue behind the in-app build (app/extraimages.go) has to agree with the
// scripts, or an admin's Build button produces a different image from `make
// images`. Everything here is checked against the shell rather than assumed:
// the tag, the Dockerfile's existence, and the fact that nothing needs a build
// context, which is the property the whole feature rests on.
func TestExtraImageCatalogAgreesWithTheScripts(t *testing.T) {
	service, err := os.ReadFile("../images/service.sh")
	if err != nil {
		t.Fatal(err)
	}
	apps, err := os.ReadFile("../images/apps.sh")
	if err != nil {
		t.Fatal(err)
	}
	sh := string(service) + string(apps)

	seen := map[string]bool{}
	for _, e := range extraImageCatalog() {
		if seen[e.ID] {
			t.Errorf("%s appears twice in the catalogue", e.ID)
		}
		seen[e.ID] = true
		if e.Tag == "" || e.Label == "" || e.About == "" || e.Make == "" || len(e.Needs) == 0 {
			t.Errorf("%s is missing part of its description: %+v", e.ID, e)
		}
		// The make target has to exist, since both the UI and the error messages
		// tell people to run it.
		mk, err := os.ReadFile("../Makefile")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(mk), "\n"+e.Make+":") {
			t.Errorf("%s names `make %s`, which the Makefile does not have", e.ID, e.Make)
		}

		if !e.Buildable {
			if e.Why == "" {
				t.Errorf("%s cannot be built here but does not say why", e.ID)
			}
			if e.Dockerfile != "" {
				t.Errorf("%s is not buildable yet names a Dockerfile", e.ID)
			}
			continue
		}
		if e.Why != "" {
			t.Errorf("%s is buildable but carries a reason it is not: %q", e.ID, e.Why)
		}
		// The Dockerfile is read at build time from a directory the app image
		// carries; here it is read from the repository, which is the same file.
		df, err := os.ReadFile(filepath.Join("../images", e.Dockerfile))
		if err != nil {
			t.Errorf("%s: %v", e.ID, err)
			continue
		}
		// THE load-bearing property: a COPY from the build context would need the
		// repository, which the app has no copy of. Every COPY must be --from=.
		for i, line := range strings.Split(string(df), "\n") {
			f := strings.Fields(strings.TrimSpace(line))
			if len(f) > 1 && (f[0] == "COPY" || f[0] == "ADD") && !strings.HasPrefix(f[1], "--from=") {
				t.Errorf("%s:%d copies from the build context (%q) — the in-app build passes only the Dockerfile", e.Dockerfile, i+1, line)
			}
		}
		// Whatever the scripts build, the app must ask for the same tag, and every
		// build arg the app passes must be one the Dockerfile declares.
		if !strings.Contains(sh, e.Tag) && !strings.Contains(sh, tagStem(e.Tag)) {
			t.Errorf("%s builds %s, which appears in neither images/service.sh nor images/apps.sh", e.ID, e.Tag)
		}
		for k := range e.Args {
			if !strings.Contains(string(df), "ARG "+k) {
				t.Errorf("%s passes --build-arg %s, which %s does not declare", e.ID, k, e.Dockerfile)
			}
		}
	}

	// And every node type that names a missing image has a catalogue entry, or
	// missingImageIssue would silently produce a message with no Build button.
	for _, id := range []string{"supportsim", "stocksim", "intranet", "vnc", "k8scollector"} {
		if _, ok := extraImageByID(id); !ok {
			t.Errorf("no catalogue entry for %q", id)
		}
	}
	if got := missingImageIssue("vnc"); got.Image != "vnc" || !strings.Contains(got.Message, vncImage("")) {
		t.Errorf("the missing-image issue does not carry the id and the tag: %+v", got)
	}
	if got := missingImageIssue("stocksim"); got.Image != "stocksim" || !strings.Contains(got.Message, "checkout") {
		t.Errorf("a non-buildable image should send you to a checkout: %+v", got)
	}
}

// `make images` builds every image a node can need: the OS bases, then the images baked on
// them (the Intranet among them — the DNS and CA every other node is built against), then the
// demo applications. A drift test with history: the target's documentation once claimed it built
// the Intranet after the recipe had stopped doing so, and a comment cannot fail a build.
func TestMakeImagesBuildsEveryImage(t *testing.T) {
	mk, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	recipe := makeRecipe(string(mk), "images")
	if recipe == "" {
		t.Fatal("the Makefile has no `images` target")
	}
	for _, want := range []string{"images/build.sh", "images/service.sh all", "images/apps.sh"} {
		if !strings.Contains(recipe, want) {
			t.Errorf("`make images` does not run %s:\n%s", want, recipe)
		}
	}
	// And it does not fail the rest of a first run over one image, which is what lets
	// `make install` finish with DBCanvas running and the missing image reported at Validate.
	if strings.Count(recipe, "||") < 2 {
		t.Errorf("a failed node or application image would abort `make images`:\n%s", recipe)
	}
	if !strings.Contains(string(mk), "\ninstall: images compose") {
		t.Error("`make install` is not `images compose`")
	}
}

// makeRecipe returns the recipe lines (the tab-indented ones) of one target.
func makeRecipe(mk, target string) string {
	_, rest, ok := strings.Cut(mk, "\n"+target+":")
	if !ok {
		return ""
	}
	var out []string
	for _, line := range strings.Split(rest, "\n")[1:] {
		if !strings.HasPrefix(line, "\t") {
			break
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// tagStem is "dbcanvas-intranet" from "dbcanvas-intranet:oraclelinux-9-arm64": the
// scripts compose those tags from shell variables, so the repository half is what
// can be compared.
func tagStem(tag string) string {
	if i := strings.Index(tag, ":"); i > 0 {
		return tag[:i]
	}
	return tag
}
