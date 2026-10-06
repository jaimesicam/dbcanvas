// Package embed turns a sentence into a 384-dimensional vector with
// sentence-transformers/all-MiniLM-L6-v2, in pure Go.
//
// It exists so the support demo can show semantic search end to end — ticket
// text in, vector out, nearest neighbours from the database — without a Python
// sidecar, an ONNX runtime, cgo, or a call to a hosted embedding API. The model
// is small (6 layers, 22M parameters, ~90 MB of float32 weights) and a short
// sentence is only a few hundred million multiply-adds, so a careful scalar
// implementation is comfortably fast enough for an interactive UI.
//
// Everything a reference implementation does is reproduced here: BERT's uncased
// WordPiece tokenizer, the 6-layer BERT encoder, mean pooling and L2
// normalisation. Output matches sentence-transformers to float32 rounding
// (cosine > 0.9999 per sentence; see embed_test.go for captured reference
// values), so vectors produced here are interchangeable with vectors produced by
// any other all-MiniLM-L6-v2 deployment.
//
// The weights are not vendored: Load reads model.safetensors and vocab.txt from
// a directory at runtime (scripts/fetch-model.sh downloads them at image build
// time). See NOTICE for the model's licence and credits.
//
// Known limitation: accent stripping uses a precomputed table (accents.go)
// instead of full Unicode NFD, because the standard library has no
// normalisation tables. It covers Latin, Greek, Cyrillic, Vietnamese, kana and
// (algorithmically) Hangul; an accented character from another script keeps its
// accent and may tokenize differently from the reference — typically into
// [UNK] — which degrades but does not break that sentence's embedding. Literal
// special-token strings typed into the text (e.g. "[SEP]") are tokenized as
// ordinary punctuation and words, not as special tokens.
package embed

import (
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"sync"
)

// DefaultDir is where the container image puts the model files.
const DefaultDir = "/model"

// Model is a loaded all-MiniLM-L6-v2. It is immutable after Load and safe for
// concurrent use by any number of goroutines.
type Model struct {
	tok  *tokenizer
	bert *bert
}

// Load reads model.safetensors and vocab.txt from dir (DefaultDir if empty) and
// validates every tensor's name and shape against the architecture, so a
// truncated download or the wrong checkpoint fails here rather than producing
// vectors that look fine but mean nothing.
func Load(dir string) (*Model, error) {
	if dir == "" {
		dir = DefaultDir
	}
	tok, err := loadVocab(filepath.Join(dir, "vocab.txt"))
	if err != nil {
		return nil, fmt.Errorf("embed: vocab: %w", err)
	}
	if len(tok.vocab) != vocabSize {
		return nil, fmt.Errorf("embed: vocab.txt has %d tokens, want %d", len(tok.vocab), vocabSize)
	}
	ts, err := readSafetensors(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, fmt.Errorf("embed: weights: %w", err)
	}
	b, err := newBERT(ts)
	if err != nil {
		return nil, fmt.Errorf("embed: weights: %w", err)
	}
	return &Model{tok: tok, bert: b}, nil
}

// Name is the model's identifier, for display and for tagging stored vectors so
// they are never compared against vectors from a different model.
func (m *Model) Name() string { return "all-MiniLM-L6-v2" }

// Dim is the embedding width.
func (m *Model) Dim() int { return hidden }

// Tokenize returns the wordpiece tokens the encoder actually sees, including
// [CLS] and [SEP] and after truncation to 256 — useful for showing learners why
// "refunded" and "refund" end up close ("refund", "##ed").
func (m *Model) Tokenize(text string) []string {
	toks, _ := m.tok.encode(text, maxSeqLen)
	return toks
}

// Embed returns the L2-normalised sentence embedding of text. Each matmul fans
// out across all CPUs, which is what you want for one interactive query.
func (m *Model) Embed(text string) []float32 {
	_, ids := m.tok.encode(text, maxSeqLen)
	return m.bert.forward(ids, runtime.NumCPU())
}

// EmbedBatch embeds many texts, returning vectors in input order. Parallelism is
// across sentences — one single-threaded forward pass per CPU — because for a
// batch that is strictly better than parallelising inside each pass: no
// per-matmul goroutine fan-out, and every core stays busy on its own sentence.
func (m *Model) EmbedBatch(texts []string) [][]float32 {
	out := make([][]float32, len(texts))
	if len(texts) == 1 {
		out[0] = m.Embed(texts[0])
		return out
	}
	workers := min(runtime.NumCPU(), len(texts))
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				_, ids := m.tok.encode(texts[i], maxSeqLen)
				out[i] = m.bert.forward(ids, 1)
			}
		}()
	}
	for i := range texts {
		next <- i
	}
	close(next)
	wg.Wait()
	return out
}

// Cosine is the cosine similarity of a and b. For vectors from Embed (already
// unit length) this equals their dot product, but Cosine normalises anyway so it
// is also correct for vectors from elsewhere. Mismatched lengths or a zero
// vector give 0.
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var ab, aa, bb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		ab += x * y
		aa += x * x
		bb += y * y
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return float32(ab / math.Sqrt(aa*bb))
}
