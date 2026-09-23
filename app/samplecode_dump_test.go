package main

import (
	"os"
	"path/filepath"
	"testing"
)

// samplecode_dump_test.go — the off-line verification harness for the generated samples.
//
// TestSampleCodeRendersEverySample proves the templates render. It cannot prove the result
// *compiles*, and a generated program that does not compile is worse than no feature at all. So
// this writes every sample in the registry to a directory, where a real toolchain can be pointed
// at it:
//
//	SCDUMP=/tmp/samples SCTLS=verify SCMTLS=alice go test -run TestSampleCodeDump ./app
//	find /tmp/samples -name '*.py' -exec python3 -m py_compile {} +
//	find /tmp/samples -name '*.js' -exec node --check {} \;
//	find /tmp/samples -name '*.sh' -exec bash -n {} \;
//	for d in $(find /tmp/samples -name go.mod -exec dirname {} \;); do (cd $d && go mod tidy && go vet ./...); done
//	javac -cp <the driver jars> DbCanvasCrud.java
//	for d in $(find /tmp/samples -name '*.csproj' -exec dirname {} \;); do (cd $d && dotnet build); done
//
// Skipped without SCDUMP, so it costs a normal test run nothing. SCTLS and SCMTLS pick the TLS
// posture, because the TLS branches are where the per-driver differences — and so the mistakes —
// actually live.
func TestSampleCodeDump(t *testing.T) {
	out := os.Getenv("SCDUMP")
	if out == "" {
		t.Skip("no SCDUMP")
	}
	tlsMode := os.Getenv("SCTLS")
	if tlsMode == "" {
		tlsMode = scTLSVerify
	}
	mtls := os.Getenv("SCMTLS")
	for _, c := range scClients {
		for _, s := range scScenarios {
			if !c.offers(s.ID) {
				continue
			}
			id := scSampleID(c.Database, c.Language, c.ID, s.ID)
			tgt := scApplyTLSChoice(scTestTarget(c.Database), tlsMode, "oraclelinux")
			g := scNewGen(id, c, s, tgt, "oraclelinux", mtls)
			dir := filepath.Join(out, filepath.FromSlash(id))
			os.MkdirAll(dir, 0o755)
			for _, f := range c.Files(g) {
				p := filepath.Join(dir, filepath.FromSlash(f.Name))
				os.MkdirAll(filepath.Dir(p), 0o755)
				os.WriteFile(p, []byte(f.Body), 0o644)
			}
		}
	}
}
