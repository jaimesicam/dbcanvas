package embed

import (
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Every expected token list and embedding value below was captured from the
// reference implementation — sentence-transformers 5.x running
// sentence-transformers/all-MiniLM-L6-v2 on CPU (its fast HF tokenizer and
// PyTorch BertModel) — not derived from this package, so these tests check
// agreement with the reference rather than self-consistency.

// modelDir returns SUPPORTSIM_MODEL_DIR or skips: the weights are ~90 MB and
// downloaded at build time (scripts/fetch-model.sh), never committed.
func modelDir(tb testing.TB) string {
	dir := os.Getenv("SUPPORTSIM_MODEL_DIR")
	if dir == "" {
		tb.Skip("SUPPORTSIM_MODEL_DIR not set; run scripts/fetch-model.sh <dir> and export it")
	}
	return dir
}

var (
	loadOnce  sync.Once
	loaded    *Model
	loadedErr error
)

func testModel(tb testing.TB) *Model {
	dir := modelDir(tb)
	loadOnce.Do(func() { loaded, loadedErr = Load(dir) })
	if loadedErr != nil {
		tb.Fatal(loadedErr)
	}
	return loaded
}

// TestBasicTokenize needs no vocab: it covers cleaning, lowercasing, accent
// stripping, punctuation and CJK splitting — the pre-WordPiece steps.
func TestBasicTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Hello, World!", []string{"hello", ",", "world", "!"}},
		{"$20.99 + 15%", []string{"$", "20", ".", "99", "+", "15", "%"}},
		{"Café NAÏVE résumé Ñandú", []string{"cafe", "naive", "resume", "nandu"}},
		{"東京で", []string{"東", "京", "て"}},
		{"Ёлка Ελληνικά", []string{"елка", "ελληνικα"}},
		{"control\u0007bell zero​width", []string{"controlbell", "zerowidth"}},
		{"a b\tc\n", []string{"a", "b", "c"}},
		{"   \t\n ", nil},
		{"", nil},
	}
	for _, c := range cases {
		if got := basicTokenize(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("basicTokenize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWordpieceToyVocab checks greedy longest-match-first and the all-or-nothing
// [UNK] rule against a tiny hand-built vocab.
func TestWordpieceToyVocab(t *testing.T) {
	tk := &tokenizer{vocab: map[string]int{
		"[CLS]": 0, "[SEP]": 1, "[UNK]": 2, "un": 3, "unaff": 4, "##able": 5, "##aff": 6, "run": 7, "##ning": 8, "##n": 9,
	}}
	cases := map[string][]string{
		"unaffable":              {"unaff", "##able"}, // longest prefix wins over "un"
		"running":                {"run", "##ning"},   // after "run", the longest continuation is "##ning", not "##n"
		"runn":                   {"run", "##n"},
		"unx":                    {"[UNK]"}, // "##x" missing → the whole word is [UNK]
		strings.Repeat("a", 101): {"[UNK]"},
	}
	for in, want := range cases {
		if got := tk.wordpiece(in, nil); !reflect.DeepEqual(got, want) {
			t.Errorf("wordpiece(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTokenizeMatchesReference(t *testing.T) {
	m := testModel(t)
	cases := []struct {
		in   string
		want []string
	}{
		{"I was charged twice for my subscription",
			[]string{"[CLS]", "i", "was", "charged", "twice", "for", "my", "subscription", "[SEP]"}},
		{"Order #A-10293 shipped on 2024-03-15; ETA 3-5 days @ 08:00 UTC.",
			[]string{"[CLS]", "order", "#", "a", "-", "102", "##9", "##3", "shipped", "on", "202", "##4", "-", "03", "-", "15", ";", "eta", "3", "-", "5", "days", "@", "08", ":", "00", "utc", ".", "[SEP]"}},
		{"Can't log in!!! password reset email never arrives :( #help",
			[]string{"[CLS]", "can", "'", "t", "log", "in", "!", "!", "!", "password", "reset", "email", "never", "arrives", ":", "(", "#", "help", "[SEP]"}},
		{"Hello, world! {braces} $20.99 + 15% tax ~ ok",
			[]string{"[CLS]", "hello", ",", "world", "!", "{", "brace", "##s", "}", "$", "20", ".", "99", "+", "15", "%", "tax", "~", "ok", "[SEP]"}},
		{"Le café était très bon, naïve résumé",
			[]string{"[CLS]", "le", "cafe", "eta", "##it", "tres", "bon", ",", "naive", "resume", "[SEP]"}},
		{"supercalifragilisticexpialidocious",
			[]string{"[CLS]", "super", "##cal", "##if", "##rag", "##ilis", "##tic", "##ex", "##pia", "##lid", "##oc", "##ious", "[SEP]"}},
		{"東京で会議があります。 한국어 がぎぐ",
			[]string{"[CLS]", "東", "京", "て", "会", "[UNK]", "か", "##あ", "##り", "##ま", "##す", "。", "ᄒ", "##ᅡ", "##ᆫ", "##ᄀ", "##ᅮ", "##ᆨ", "##ᄋ", "##ᅥ", "か", "##き", "##く", "[SEP]"}},
		{strings.Repeat("a", 106) + " ok", []string{"[CLS]", "[UNK]", "ok", "[SEP]"}},
		{"", []string{"[CLS]", "[SEP]"}},
	}
	for _, c := range cases {
		if got := m.Tokenize(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Tokenize(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}

	// Truncation: at most 256 including [CLS]/[SEP], and [SEP] is kept last.
	long := strings.Repeat("The replication lag spiked to 300 seconds. ", 60)
	toks := m.Tokenize(long)
	if len(toks) != maxSeqLen || toks[0] != "[CLS]" || toks[len(toks)-1] != "[SEP]" {
		t.Errorf("long text: %d tokens, first %q last %q", len(toks), toks[0], toks[len(toks)-1])
	}
}

func TestEmbedMatchesReference(t *testing.T) {
	m := testModel(t)
	cases := []struct {
		in   string
		head []float32 // first six dimensions from the reference
	}{
		{"I was charged twice for my subscription",
			[]float32{0.0097802, -0.0550330, 0.0154189, 0.0048917, 0.0507378, -0.0345826}},
		{"", // [CLS] [SEP] only — the empty input still has a well-defined vector
			[]float32{-0.1188383, 0.0482986, -0.0025481, -0.0110112, 0.0519507, 0.0102918}},
	}
	for _, c := range cases {
		e := m.Embed(c.in)
		if len(e) != m.Dim() {
			t.Fatalf("Embed(%q): dim %d, want %d", c.in, len(e), m.Dim())
		}
		for i, want := range c.head {
			if d := math.Abs(float64(e[i] - want)); d > 1e-5 {
				t.Errorf("Embed(%q)[%d] = %.7f, want %.7f", c.in, i, e[i], want)
			}
		}
		var norm float64
		for _, v := range e {
			norm += float64(v) * float64(v)
		}
		if math.Abs(norm-1) > 1e-5 {
			t.Errorf("Embed(%q): |e|² = %f, want 1", c.in, norm)
		}
	}

	// Pairwise cosines from the reference.
	a := m.Embed("I was charged twice for my subscription")
	b := m.Embed("duplicate payment on my invoice")
	c := m.Embed("the database replica is lagging")
	for _, p := range []struct {
		name string
		got  float32
		want float64
	}{
		{"charged/duplicate", Cosine(a, b), 0.4890414},
		{"charged/replica", Cosine(a, c), 0.3021850},
		{"duplicate/replica", Cosine(b, c), 0.1641609},
	} {
		if math.Abs(float64(p.got)-p.want) > 1e-5 {
			t.Errorf("cosine %s = %.7f, want %.7f", p.name, p.got, p.want)
		}
	}
}

// TestSemanticSanity is the property the demo actually relies on: a billing
// complaint lands nearer another billing complaint than an ops problem, even
// with no words in common.
func TestSemanticSanity(t *testing.T) {
	m := testModel(t)
	q := m.Embed("I was charged twice for my subscription")
	billing := m.Embed("duplicate payment on my invoice")
	ops := m.Embed("the database replica is lagging")
	if Cosine(q, billing) <= Cosine(q, ops) {
		t.Errorf("billing %.3f should beat ops %.3f", Cosine(q, billing), Cosine(q, ops))
	}
	login := m.Embed("I can't sign in to my account")
	if Cosine(login, m.Embed("password reset email never arrives")) <= Cosine(login, billing) {
		t.Error("login trouble should be nearer password reset than billing")
	}
}

// TestEmbedBatch checks the batch path (single-threaded passes, one per CPU)
// gives bit-identical vectors to the single path, in input order.
func TestEmbedBatch(t *testing.T) {
	m := testModel(t)
	texts := []string{"refund please", "", "replica lag", "Ünïcödé text", "a b c d e f g", "refund please"}
	batch := m.EmbedBatch(texts)
	for i, txt := range texts {
		if !reflect.DeepEqual(batch[i], m.Embed(txt)) {
			t.Errorf("EmbedBatch[%d] (%q) differs from Embed", i, txt)
		}
	}
}

func TestCosine(t *testing.T) {
	if got := Cosine([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Errorf("orthogonal: %v", got)
	}
	if got := Cosine([]float32{2, 0}, []float32{3, 0}); math.Abs(float64(got)-1) > 1e-7 {
		t.Errorf("parallel: %v", got)
	}
	if got := Cosine([]float32{1}, []float32{1, 2}); got != 0 {
		t.Errorf("mismatched lengths: %v", got)
	}
	if got := Cosine([]float32{0, 0}, []float32{1, 2}); got != 0 {
		t.Errorf("zero vector: %v", got)
	}
}

// ~15 wordpieces including [CLS]/[SEP] — a typical ticket subject line.
const benchShort = "My invoice shows a duplicate charge for the March subscription renewal"

func BenchmarkEmbedShort(b *testing.B) {
	m := testModel(b)
	b.Logf("%d tokens", len(m.Tokenize(benchShort)))
	for b.Loop() {
		m.Embed(benchShort)
	}
}

func BenchmarkEmbedMaxLen(b *testing.B) {
	m := testModel(b)
	long := strings.Repeat("The replication lag spiked after the vacuum on the orders table. ", 30)
	for b.Loop() {
		m.Embed(long)
	}
}

// BenchmarkEmbedBatch64 reports per-batch time for 64 short sentences.
func BenchmarkEmbedBatch64(b *testing.B) {
	m := testModel(b)
	texts := make([]string, 64)
	for i := range texts {
		texts[i] = benchShort
	}
	for b.Loop() {
		m.EmbedBatch(texts)
	}
}
