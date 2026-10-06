package embed

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"
)

const (
	tokCLS = "[CLS]"
	tokSEP = "[SEP]"
	tokUNK = "[UNK]"

	// maxWordChars is BertTokenizer's max_input_chars_per_word: a "word" longer
	// than this (a pasted hash, a base64 blob, a stack-trace path with no spaces)
	// becomes a single [UNK] rather than dozens of meaningless ## pieces.
	maxWordChars = 100
)

// tokenizer is BERT's uncased WordPiece tokenizer, reproduced step for step from
// HF's BertTokenizer (BasicTokenizer + WordpieceTokenizer, do_lower_case=true).
// It is the half of the model people forget: the encoder only ever sees vocab
// ids, so a tokenizer that splits "can't" or "Café" differently from the one
// the model was trained with silently produces a different — worse — embedding.
type tokenizer struct {
	vocab map[string]int
}

// loadVocab reads vocab.txt, where the line number (from 0) is the token id.
func loadVocab(path string) (*tokenizer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vocab := make(map[string]int, 32000)
	sc := bufio.NewScanner(f)
	for id := 0; sc.Scan(); id++ {
		// Only the line terminator is stripped — exactly what HF does — so a
		// vocab entry is never altered by trimming.
		tok := strings.TrimSuffix(sc.Text(), "\r")
		if _, dup := vocab[tok]; !dup {
			vocab[tok] = id
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, special := range []string{tokCLS, tokSEP, tokUNK} {
		if _, ok := vocab[special]; !ok {
			return nil, fmt.Errorf("%s: missing special token %s", path, special)
		}
	}
	return &tokenizer{vocab: vocab}, nil
}

// encode returns the wordpiece tokens and their ids, wrapped in [CLS] … [SEP]
// and truncated so the whole sequence is at most maxLen — the same truncation
// sentence-transformers applies (max_seq_length=256 for this model), so a long
// ticket is embedded from its first ~250 wordpieces, not rejected.
func (t *tokenizer) encode(text string, maxLen int) ([]string, []int) {
	pieces := t.wordpieces(text)
	if len(pieces) > maxLen-2 {
		pieces = pieces[:maxLen-2]
	}
	toks := make([]string, 0, len(pieces)+2)
	toks = append(toks, tokCLS)
	toks = append(toks, pieces...)
	toks = append(toks, tokSEP)
	ids := make([]int, len(toks))
	for i, tok := range toks {
		ids[i] = t.vocab[tok]
	}
	return toks, ids
}

// wordpieces runs the basic tokenizer (clean, CJK split, lowercase, strip
// accents, split on punctuation) and then greedy WordPiece on each word.
func (t *tokenizer) wordpieces(text string) []string {
	var out []string
	for _, word := range basicTokenize(text) {
		out = t.wordpiece(word, out)
	}
	return out
}

// wordpiece is greedy longest-match-first: take the longest vocab prefix, then
// the longest "##"-prefixed continuation of the remainder, and so on. If any
// remainder has no match the whole word becomes [UNK] — BERT never emits a
// partial word, because a half-matched word would read as a different word.
func (t *tokenizer) wordpiece(word string, out []string) []string {
	chars := []rune(word)
	if len(chars) > maxWordChars {
		return append(out, tokUNK)
	}
	start := len(out)
	for begin := 0; begin < len(chars); {
		end := len(chars)
		var match string
		for ; end > begin; end-- {
			sub := string(chars[begin:end])
			if begin > 0 {
				sub = "##" + sub
			}
			if _, ok := t.vocab[sub]; ok {
				match = sub
				break
			}
		}
		if match == "" {
			return append(out[:start], tokUNK)
		}
		out = append(out, match)
		begin = end
	}
	return out
}

// basicTokenize is BasicTokenizer.tokenize: clean the text, put spaces around
// CJK ideographs (each is its own token — Chinese has no spaces to split on),
// split on whitespace, lowercase + strip accents each word, then split each
// word on punctuation so "refund?" is ["refund", "?"].
func basicTokenize(text string) []string {
	var b strings.Builder
	b.Grow(len(text) + 8)
	for _, r := range text {
		switch {
		case r == 0 || r == 0xFFFD || isControl(r):
			// dropped, like BERT's _clean_text
		case isWhitespace(r):
			b.WriteByte(' ')
		case isCJK(r):
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	var out []string
	for _, word := range strings.Fields(b.String()) {
		word = stripAccents(strings.ToLower(word))
		out = splitOnPunct(word, out)
	}
	return out
}

// splitOnPunct makes every punctuation character its own token and splits the
// word around it.
func splitOnPunct(word string, out []string) []string {
	start := -1
	for i, r := range word {
		if isPunct(r) {
			if start >= 0 {
				out = append(out, word[start:i])
				start = -1
			}
			out = append(out, string(r))
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, word[start:])
	}
	return out
}

// stripAccents is NFD followed by dropping every Mn (non-spacing mark), which is
// how "café" and "cafe" become the same token. See accentBase for why this is a
// table rather than a full Unicode normalizer, and what it does not cover.
func stripAccents(s string) string {
	plain := true
	for _, r := range s {
		if r >= 0x80 {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r):
			// an already-decomposed combining mark
		case r >= hangulBase && r < hangulBase+hangulCount:
			// Hangul syllables decompose to conjoining jamo (letters, not
			// marks), so NFD changes them but nothing is dropped. Unlike
			// accents this is pure arithmetic, so it is done exactly.
			s := r - hangulBase
			b.WriteRune(0x1100 + s/(21*28))
			b.WriteRune(0x1161 + (s%(21*28))/28)
			if t := s % 28; t != 0 {
				b.WriteRune(0x11A7 + t)
			}
		default:
			if base, ok := accentBase[r]; ok {
				r = base
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

const (
	hangulBase  = 0xAC00
	hangulCount = 11172
)

// isWhitespace is BERT's _is_whitespace: space, tab, newline, CR, or any Zs.
func isWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || unicode.Is(unicode.Zs, r)
}

// isControl is BERT's _is_control: any C* category (Cc, Cf, Co, Cs) except the
// three whitespace controls, which isWhitespace turns into spaces instead.
func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.Is(unicode.C, r)
}

// isPunct is BERT's _is_punctuation. It deliberately counts every non-alphanumeric
// ASCII symbol ($, +, <, ^, `, |, ~ …) as punctuation even though Unicode files
// some of them under S* (symbols), so "$20" is ["$", "20"].
func isPunct(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r)
}

// isCJK is BERT's _is_chinese_char: the CJK Unified Ideographs blocks and their
// extensions/compatibility forms. Hiragana, Katakana and Hangul are NOT here —
// those are written with spaces or wordpiece normally.
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) ||
		(r >= 0x2B820 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0x2F800 && r <= 0x2FA1F)
}
