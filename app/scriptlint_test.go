package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// scriptlint_test.go — every shell script embedded in the package (a string constant named
// …Script) through shellcheck, at warning severity: a quoting slip, an unterminated construct, an
// `rm -rf "$DIR"/*` that an empty DIR turns into `rm -rf /*`, or a cd that can fail silently is
// reported here instead of inside a container at deploy time. A deliberate exception is marked on
// its line in the script (# shellcheck disable=SCxxxx # why). The
// scripts' own unit tests check what they say; this checks they are valid shell.
//
// The constants are read from the source with go/parser, and a value built from other constants
// ("…" + mysqlKeyringDir + "…") is put together the way the compiler would. It skips when
// shellcheck is not installed; CI installs it.

// embeddedScripts returns every …Script string constant of the package, by name.
func embeddedScripts(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	exprs := map[string]ast.Expr{}
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				g, ok := d.(*ast.GenDecl)
				if !ok || g.Tok != token.CONST {
					continue
				}
				for _, s := range g.Specs {
					vs := s.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							exprs[n.Name] = vs.Values[i]
						}
					}
				}
			}
		}
	}
	var eval func(e ast.Expr, depth int) (string, bool)
	eval = func(e ast.Expr, depth int) (string, bool) {
		if depth > 20 {
			return "", false
		}
		switch x := e.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return "", false
			}
			l, ok1 := eval(x.X, depth+1)
			r, ok2 := eval(x.Y, depth+1)
			return l + r, ok1 && ok2
		case *ast.ParenExpr:
			return eval(x.X, depth+1)
		case *ast.Ident:
			if v, ok := exprs[x.Name]; ok {
				return eval(v, depth+1)
			}
		}
		return "", false
	}
	out := map[string]string{}
	for name, e := range exprs {
		if !strings.HasSuffix(name, "Script") {
			continue
		}
		if s, ok := eval(e, 0); ok && strings.TrimSpace(s) != "" {
			out[name] = s
		}
	}
	return out
}

// The extraction itself is tested without shellcheck, so a refactor that hides the scripts from
// the linter is noticed even where it is not installed.
func TestEmbeddedScriptsFound(t *testing.T) {
	s := embeddedScripts(t)
	if len(s) < 200 {
		t.Fatalf("found only %d embedded scripts", len(s))
	}
	if !strings.Contains(s["mysqlKeyringFileStageComponentScript"], "install -d -o mysql -g mysql -m 0750 "+mysqlKeyringDir) {
		t.Error("a script concatenated from constants was not put together")
	}
}

// shellcheckExclusions are checks that are wrong for these scripts as a class, each with why.
var shellcheckExclusions = []string{
	// The scripts receive their inputs as environment variables the Go side sets; shellcheck
	// sees them as never assigned.
	"SC2154",
	// Variables are often set for a heredoc, an exported child or a sourced file the linter
	// cannot see.
	"SC2034",
	// Files sourced from a path built at run time (an env file the script itself wrote).
	"SC1090",
}

// notShell are …Script constants that are not shell, by what they are.
var notShell = map[string]string{
	"cephStatusScript": "Python, run with python3 -c",
}

func TestEmbeddedScriptsShellcheck(t *testing.T) {
	bin, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck is not installed")
	}
	scripts := embeddedScripts(t)
	dir := t.TempDir()
	var names []string
	for n := range scripts {
		if notShell[n] == "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var files []string
	for _, n := range names {
		body := scripts[n]
		if !strings.HasPrefix(body, "#!") {
			body = "#!/bin/bash\n" + body
		}
		p := filepath.Join(dir, n+".sh")
		if err := os.WriteFile(p, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	args := append([]string{"-S", "warning", "-f", "gcc", "-e", strings.Join(shellcheckExclusions, ",")}, files...)
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		report := strings.ReplaceAll(string(out), dir+string(filepath.Separator), "")
		t.Errorf("shellcheck found errors in embedded scripts (file name = the Go constant):\n%s", report)
	}
	fmt.Fprintf(os.Stderr, "shellcheck: %d embedded scripts checked\n", len(files))
}
