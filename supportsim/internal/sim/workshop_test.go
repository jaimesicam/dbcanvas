package sim

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// BSON binary vectors (subtype 9): dtype byte, padding byte, then the values.
func TestBinaryVectors(t *testing.T) {
	v := []float32{0.5, -0.25, 1}
	f := float32Vector(v)
	if f.Subtype != 9 || f.Data[0] != 0x27 || f.Data[1] != 0 || len(f.Data) != 2+4*3 {
		t.Fatalf("float32 header/length: % x", f.Data)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(f.Data[6:])); got != -0.25 {
		t.Errorf("float32 value 1 = %v", got)
	}
	i := int8Vector(v)
	if i.Subtype != 9 || i.Data[0] != 0x03 || len(i.Data) != 2+3 {
		t.Fatalf("int8 header/length: % x", i.Data)
	}
	// Scaled by the largest component (1), so 1 → 127, 0.5 → 64, −0.25 → −32.
	if got := []int8{int8(i.Data[2]), int8(i.Data[3]), int8(i.Data[4])}; got[0] != 64 || got[1] != -32 || got[2] != 127 {
		t.Errorf("int8 values %v", got)
	}
}

func TestVariantDefinitions(t *testing.T) {
	names := map[string]bool{}
	for _, v := range mongoVariants {
		if names[v.Name] {
			t.Errorf("duplicate %s", v.Name)
		}
		names[v.Name] = true
		sh := v.Shell()
		if !strings.Contains(sh, `"`+v.Path+`"`) || !strings.Contains(sh, v.Similarity) || (v.Quantization != "" && !strings.Contains(sh, v.Quantization)) {
			t.Errorf("%s shell does not match its definition:\n%s", v.Name, sh)
		}
	}
	if mongoVariants[0].Name != "v_cosine" {
		t.Error("the baseline must come first: Bench measures recall against it")
	}
	if s := rawScale(42); s < 0.2 || s > 5 || s != rawScale(42) {
		t.Errorf("rawScale must be stable and within 0.2..5: %v", s)
	}
}

// pgvector's text input format, and the real[] fallback.
func TestVecLiterals(t *testing.T) {
	if got := vecLit([]float32{0.5, -0.25, 1}); got != "[0.5,-0.25,1]" {
		t.Errorf("vecLit = %q", got)
	}
	if got := arrLit([]float32{0.5, -0.25}); got != "{0.5,-0.25}" {
		t.Errorf("arrLit = %q", got)
	}
	if got := parseVec("[0.5,-0.25,1]"); len(got) != 3 || got[1] != -0.25 {
		t.Errorf("parseVec = %v", got)
	}
}

func TestPGVariants(t *testing.T) {
	names := map[string]bool{}
	for _, v := range pgVariants {
		if names[v.Name] {
			t.Errorf("duplicate %s", v.Name)
		}
		names[v.Name] = true
		ddl := v.ddl()
		if !strings.Contains(ddl, "CREATE TABLE supportsim."+v.table) {
			t.Errorf("%s ddl: %s", v.Name, ddl)
		}
		if (v.index == "") != strings.Contains(ddl, "no index") {
			t.Errorf("%s: index/ddl mismatch", v.Name)
		}
	}
	if pgVariants[0].index != "" {
		t.Error("the baseline must be the unindexed exact scan")
	}
}

// The SQL shown on the page is the SQL that runs, with the vector abbreviated.
func TestPGShow(t *testing.T) {
	out := pgShow([]string{"SET LOCAL hnsw.ef_search = 40"}, "SELECT 1", []any{vecLit(make([]float32, 384)), "resolved"})
	for _, want := range []string{"BEGIN;", "SET LOCAL hnsw.ef_search = 40;", "SELECT 1;", "COMMIT;", "… 384 numbers]'", "-- $2 = 'resolved'"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
