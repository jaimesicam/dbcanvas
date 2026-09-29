package main

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// repo_meta.go — reading upstream yum/apt metadata and choosing the subset a Repository node
// mirrors. Pure functions, no Docker: repository.go fetches the bytes and acts on the answer.
//
// WHY A SUBSET AND NOT A MIRROR. repo.percona.com/ps-84-lts alone is every 8.4 build ever
// published, for every OS, with debug symbols — tens of gigabytes nobody deploying one version
// will read. A Repository node carries what its design names: a repository, the versions of it
// that are wanted, optionally a handful of package names (and what they depend on inside that
// repository), for the OS releases and architectures picked. Everything else stays upstream.
//
// WHY THE UPSTREAM PATHS ARE KEPT. The mirror is laid out exactly like repo.percona.com —
// /percona/<repo>/yum/release/<N>/RPMS/<arch>/ and /percona/<repo>/apt/{dists,pool}/ — so a node
// is redirected by swapping a host name in the files percona-release already wrote, and nothing
// about which directory a package lives in has to be relearned.

// repoPkgSpec is one line of a Repository node's package list: a percona-release repository, the
// versions of it to carry, and optionally which package names.
type repoPkgSpec struct {
	Repo string `json:"repo"` // repository directory on repo.percona.com, e.g. "ps-84-lts", "ppg-17"
	// Versions to carry. Empty → the newest build of every package. "*" → every build. Otherwise
	// each entry is a version prefix in the catalog's spelling ("8.4.5-5.1", "8.4.5", "17.6") —
	// matched against RPM and Debian builds alike, see repoVersionMatch.
	Versions []string `json:"versions"`
	// Packages narrows the repository to these names (shell globs allowed) plus whatever they
	// depend on that the same repository carries. Empty → every package in it.
	Packages []string `json:"packages"`
}

// repoPkg is one package build as upstream metadata describes it.
type repoPkg struct {
	Name     string
	Arch     string
	Epoch    string
	Version  string // RPM: ver-rel. Debian: the Version field without its epoch.
	Location string // path relative to the repository base (RPM location href / Debian Filename)
	Size     int64
	SHA256   string
	Requires []string // names this build needs (RPM requires / Debian Depends+Pre-Depends)
	Provides []string // names it satisfies besides its own
	Stanza   string   // Debian only: the verbatim Packages paragraph, re-emitted unchanged
}

// ---------------------------------------------------------------- RPM

// repomdPrimaryHref returns the location of primary.xml(.gz|.zst) from a repomd.xml.
func repomdPrimaryHref(repomd []byte) (string, error) {
	var doc struct {
		Data []struct {
			Type     string `xml:"type,attr"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(repomd, &doc); err != nil {
		return "", fmt.Errorf("repomd.xml: %v", err)
	}
	for _, d := range doc.Data {
		if d.Type == "primary" {
			return d.Location.Href, nil
		}
	}
	return "", fmt.Errorf("repomd.xml has no primary metadata")
}

// parsePrimaryXML reads a (decompressed) primary.xml.
func parsePrimaryXML(r io.Reader) ([]repoPkg, error) {
	type entry struct {
		Name string `xml:"name,attr"`
	}
	type pkgXML struct {
		Name    string `xml:"name"`
		Arch    string `xml:"arch"`
		Version struct {
			Epoch string `xml:"epoch,attr"`
			Ver   string `xml:"ver,attr"`
			Rel   string `xml:"rel,attr"`
		} `xml:"version"`
		Checksum struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		Size struct {
			Package int64 `xml:"package,attr"`
		} `xml:"size"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
		Format struct {
			Provides []entry `xml:"provides>entry"`
			Requires []entry `xml:"requires>entry"`
		} `xml:"format"`
	}
	dec := xml.NewDecoder(r)
	var out []repoPkg
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("primary.xml: %v", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "package" {
			continue
		}
		var p pkgXML
		if err := dec.DecodeElement(&p, &se); err != nil {
			return nil, fmt.Errorf("primary.xml: %v", err)
		}
		rp := repoPkg{
			Name: p.Name, Arch: p.Arch, Epoch: p.Version.Epoch,
			Version:  p.Version.Ver + "-" + p.Version.Rel,
			Location: p.Location.Href, Size: p.Size.Package,
		}
		if rp.Epoch == "0" {
			rp.Epoch = ""
		}
		if strings.EqualFold(p.Checksum.Type, "sha256") {
			rp.SHA256 = strings.TrimSpace(p.Checksum.Value)
		}
		for _, e := range p.Format.Provides {
			rp.Provides = append(rp.Provides, e.Name)
		}
		for _, e := range p.Format.Requires {
			rp.Requires = append(rp.Requires, e.Name)
		}
		out = append(out, rp)
	}
	return out, nil
}

// ---------------------------------------------------------------- Debian

// parseDebPackages reads a (decompressed) Debian Packages index.
func parseDebPackages(r io.Reader) ([]repoPkg, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var out []repoPkg
	var para []string
	flush := func() {
		if len(para) == 0 {
			return
		}
		fields := map[string]string{}
		last := ""
		for _, l := range para {
			if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
				if last != "" {
					fields[last] += "\n" + l
				}
				continue
			}
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			last = k
			fields[k] = strings.TrimSpace(v)
		}
		epoch, ver := splitEpoch(fields["Version"])
		size, _ := strconv.ParseInt(fields["Size"], 10, 64)
		p := repoPkg{
			Name: fields["Package"], Arch: fields["Architecture"], Epoch: epoch, Version: ver,
			Location: fields["Filename"], Size: size, SHA256: fields["SHA256"],
			Requires: debRelationNames(fields["Depends"] + "," + fields["Pre-Depends"]),
			Provides: debRelationNames(fields["Provides"]),
			Stanza:   strings.Join(para, "\n"),
		}
		if p.Name != "" && p.Location != "" {
			out = append(out, p)
		}
		para = nil
	}
	for sc.Scan() {
		l := sc.Text()
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		para = append(para, l)
	}
	flush()
	return out, sc.Err()
}

// debRelationNames flattens a Depends-style field ("a (>= 1) | b, c:any") to package names. Every
// alternative is kept: which one apt picks is its business, and carrying both is what makes
// either pick work.
func debRelationNames(field string) []string {
	var out []string
	for _, clause := range strings.Split(field, ",") {
		for _, alt := range strings.Split(clause, "|") {
			alt = strings.TrimSpace(alt)
			if i := strings.IndexAny(alt, " (["); i >= 0 {
				alt = alt[:i]
			}
			alt, _, _ = strings.Cut(alt, ":")
			if alt != "" {
				out = append(out, alt)
			}
		}
	}
	return out
}

func splitEpoch(v string) (epoch, rest string) {
	if e, r, ok := strings.Cut(v, ":"); ok && e != "" && strings.IndexFunc(e, func(c rune) bool { return !unicode.IsDigit(c) }) < 0 {
		return e, r
	}
	return "", v
}

// ---------------------------------------------------------------- version order

// rpmVerCmp is rpm's rpmvercmp: alternating runs of digits and letters compared segment by
// segment, digits numerically and newer than letters, "~" sorting before everything and "^"
// after the end.
func rpmVerCmp(a, b string) int {
	if a == b {
		return 0
	}
	for len(a) > 0 || len(b) > 0 {
		a = strings.TrimLeftFunc(a, func(c rune) bool { return !isAlnum(c) && c != '~' && c != '^' })
		b = strings.TrimLeftFunc(b, func(c rune) bool { return !isAlnum(c) && c != '~' && c != '^' })
		if strings.HasPrefix(a, "~") || strings.HasPrefix(b, "~") {
			if !strings.HasPrefix(a, "~") {
				return 1
			}
			if !strings.HasPrefix(b, "~") {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if strings.HasPrefix(a, "^") || strings.HasPrefix(b, "^") {
			if a == "" {
				return -1
			}
			if b == "" {
				return 1
			}
			if !strings.HasPrefix(a, "^") {
				return 1
			}
			if !strings.HasPrefix(b, "^") {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if a == "" || b == "" {
			break
		}
		digit := unicode.IsDigit(rune(a[0]))
		sa, ra := takeRun(a, digit)
		sb, rb := takeRun(b, digit)
		if sb == "" {
			// Different kinds of segment: a number is newer than letters.
			if digit {
				return 1
			}
			return -1
		}
		if digit {
			sa, sb = strings.TrimLeft(sa, "0"), strings.TrimLeft(sb, "0")
			if len(sa) != len(sb) {
				return cmpInt(len(sa), len(sb))
			}
		}
		if c := strings.Compare(sa, sb); c != 0 {
			return c
		}
		a, b = ra, rb
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	default:
		return 1
	}
}

func takeRun(s string, digit bool) (run, rest string) {
	i := 0
	for i < len(s) {
		c := rune(s[i])
		if digit && !unicode.IsDigit(c) || !digit && !unicode.IsLetter(c) {
			break
		}
		i++
	}
	return s[:i], s[i:]
}

func isAlnum(c rune) bool { return c < 128 && (unicode.IsDigit(c) || unicode.IsLetter(c)) }

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// debVerCmp is dpkg's version comparison over upstream-revision strings (the epoch is compared by
// the caller): non-digit runs by a modified ASCII order where "~" sorts before everything and
// letters before other characters, digit runs numerically.
func debVerCmp(a, b string) int {
	order := func(c byte) int {
		switch {
		case c == '~':
			return -1
		case c >= '0' && c <= '9':
			return 0
		case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z':
			return int(c)
		default:
			return int(c) + 256
		}
	}
	for len(a) > 0 || len(b) > 0 {
		for (len(a) > 0 && !(a[0] >= '0' && a[0] <= '9')) || (len(b) > 0 && !(b[0] >= '0' && b[0] <= '9')) {
			var ca, cb int
			if len(a) > 0 && !(a[0] >= '0' && a[0] <= '9') {
				ca = order(a[0])
			}
			if len(b) > 0 && !(b[0] >= '0' && b[0] <= '9') {
				cb = order(b[0])
			}
			if ca != cb {
				return cmpInt(ca, cb)
			}
			if len(a) > 0 && !(a[0] >= '0' && a[0] <= '9') {
				a = a[1:]
			}
			if len(b) > 0 && !(b[0] >= '0' && b[0] <= '9') {
				b = b[1:]
			}
		}
		na, ra := takeRun(a, true)
		nb, rb := takeRun(b, true)
		ia, _ := strconv.ParseUint(strings.TrimLeft(na, "0")+"0", 10, 64)
		ib, _ := strconv.ParseUint(strings.TrimLeft(nb, "0")+"0", 10, 64)
		if ia != ib {
			if ia < ib {
				return -1
			}
			return 1
		}
		a, b = ra, rb
	}
	return 0
}

// repoPkgCmp orders two builds of one package, epoch first.
func repoPkgCmp(deb bool, a, b repoPkg) int {
	ea, _ := strconv.Atoi(a.Epoch)
	eb, _ := strconv.Atoi(b.Epoch)
	if ea != eb {
		return cmpInt(ea, eb)
	}
	if deb {
		return debVerCmp(a.Version, b.Version)
	}
	return rpmVerCmp(a.Version, b.Version)
}

// repoVersionMatch reports whether a build's version is the one a spec asked for. One spelling has
// to work for both families, because a design names "8.4.7-7.1" once and mirrors it for Oracle
// Linux (8.4.7-7.1.el9) and Ubuntu (8.4.7-7-1.noble) alike: both sides have their epoch dropped and
// every "-" read as ".", and the wanted string must then be a prefix ending on a component
// boundary — so "8.4.1" never matches 8.4.10, and "17.6" matches 17.6-1.el9 and 17.6-1.noble.
func repoVersionMatch(version, want string) bool {
	_, version = splitEpoch(version)
	_, want = splitEpoch(strings.TrimSpace(want))
	if want == "" {
		return false
	}
	v := strings.ReplaceAll(version, "-", ".")
	w := strings.ReplaceAll(want, "-", ".")
	if !strings.HasPrefix(v, w) {
		return false
	}
	if len(v) == len(w) {
		return true
	}
	next := v[len(w)]
	last := w[len(w)-1]
	// A boundary is a separator, or a switch between digits and letters (…7.1 | .el9 is a
	// separator; 8.4.1 | 0 is not).
	return next == '.' || next == '+' || next == '~' || isDigitByte(next) != isDigitByte(last)
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// repoIsDebugName is the debug-symbol half of a repository, left out unless asked for: it is most
// of the bytes and none of what a deploy installs.
func repoIsDebugName(name string) bool {
	for _, s := range []string{"-debuginfo", "-debugsource", "-dbg", "-dbgsym"} {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return strings.Contains(name, "-debuginfo-")
}

// selectRepoPackages picks the builds of one repository (one OS release, one architecture) that a
// spec asks for.
//
// Per package name: every build for "*"; for named versions, the builds that match one — or, when
// none does, the newest build, because a repository also carries helpers on their own version
// series (a ppg-17 repository's pgBackRest is not 17.6) and pin_install installs the newest of
// those; for no versions, the newest build.
//
// A package-name filter keeps the named packages plus, transitively, whatever they require that
// this same repository provides. Requirements met by the OS (libc, openssl) are not followed:
// those come from the distribution's own mirrors, which the node still reaches.
func selectRepoPackages(pkgs []repoPkg, spec repoPkgSpec, deb, includeDebug bool) []repoPkg {
	byName := map[string][]repoPkg{}
	provider := map[string][]string{} // capability → package names
	for _, p := range pkgs {
		if p.Arch == "src" || (!includeDebug && repoIsDebugName(p.Name)) {
			continue
		}
		byName[p.Name] = append(byName[p.Name], p)
		provider[p.Name] = appendUnique(provider[p.Name], p.Name)
		for _, c := range p.Provides {
			provider[c] = appendUnique(provider[c], p.Name)
		}
	}
	all := false
	var wants []string
	for _, v := range spec.Versions {
		v = strings.TrimSpace(v)
		if v == "*" {
			all = true
		} else if v != "" {
			wants = append(wants, v)
		}
	}
	pick := func(builds []repoPkg) []repoPkg {
		if all {
			return builds
		}
		var hit []repoPkg
		for _, b := range builds {
			for _, w := range wants {
				if repoVersionMatch(b.Version, w) {
					hit = append(hit, b)
					break
				}
			}
		}
		if len(hit) > 0 {
			return hit
		}
		// Newest per architecture: an x86_64 directory also holds noarch builds of other names,
		// but one name can in principle exist as both.
		newest := map[string]repoPkg{}
		for _, b := range builds {
			if cur, ok := newest[b.Arch]; !ok || repoPkgCmp(deb, b, cur) > 0 {
				newest[b.Arch] = b
			}
		}
		out := make([]repoPkg, 0, len(newest))
		for _, b := range newest {
			out = append(out, b)
		}
		return out
	}

	var names []string
	if len(spec.Packages) == 0 {
		for n := range byName {
			names = append(names, n)
		}
	} else {
		seen := map[string]bool{}
		var queue []string
		for n := range byName {
			for _, g := range spec.Packages {
				if ok, _ := path.Match(strings.TrimSpace(g), n); ok {
					if !seen[n] {
						seen[n] = true
						queue = append(queue, n)
					}
					break
				}
			}
		}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			names = append(names, n)
			for _, b := range pick(byName[n]) {
				for _, req := range b.Requires {
					for _, dep := range provider[req] {
						if !seen[dep] {
							seen[dep] = true
							queue = append(queue, dep)
						}
					}
				}
			}
		}
	}
	sort.Strings(names)
	var out []repoPkg
	for _, n := range names {
		chosen := pick(byName[n])
		sort.Slice(chosen, func(i, j int) bool { return chosen[i].Location < chosen[j].Location })
		out = append(out, chosen...)
	}
	return out
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// debPackagesIndex renders the Packages file for a chosen set: the upstream paragraphs, verbatim,
// so every hash apt checks is the one Percona published.
func debPackagesIndex(pkgs []repoPkg) string {
	var b strings.Builder
	for _, p := range pkgs {
		b.WriteString(p.Stanza)
		b.WriteString("\n\n")
	}
	return b.String()
}
