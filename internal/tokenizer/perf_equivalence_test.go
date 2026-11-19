package tokenizer

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/pkoukk/tiktoken-go"
	"golang.org/x/text/unicode/norm"
)

func normalizerLite(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return isDeletedByCharmap(r) || isCharmapSpace(r) }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case isDeletedByCharmap(r):
			continue
		case isCharmapSpace(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func collapseSpaces(s string) string {
	if !strings.Contains(s, "  ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prev := false
	for _, r := range s {
		if r == ' ' && prev {
			continue
		}
		prev = r == ' '
		b.WriteRune(r)
	}
	return b.String()
}

// Reference count/trim oracle: no memo, pooled state, or production normalization.
func referenceSPMNormalize(text string) string {
	if text == "" {
		return ""
	}
	s := collapseSpaces(normalizerLite(norm.NFKC.String(text)))
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.WriteRune(preTokenSpace)
	prevSpace := false
	for i, r := range s {
		if unicode.IsSpace(r) {
			if prevSpace || i == 0 {
				prevSpace = true
				continue
			}
			prevSpace = true
			b.WriteRune(preTokenSpace)
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

func referenceSPMCount(m *sentencePieceModel, text string) int {
	total := 0
	for _, s := range splitPreTokens(referenceSPMNormalize(text)) {
		n := len(s)
		best, count := make([]float64, n+1), make([]int, n+1)
		for i := range best {
			best[i] = -math.MaxFloat64
		}
		best[0] = 0
		for i := 0; i < n; i++ {
			if best[i] == -math.MaxFloat64 {
				continue
			}
			matched := false
			node := int32(0)
			for j := i; j < n; j++ {
				next, ok := m.trie.nodes[node].next[s[j]]
				if !ok {
					break
				}
				node = next
				if piece := m.trie.nodes[node].piece; piece >= 0 {
					matched = true
					if cand := best[i] + float64(m.pieces[piece].score); cand > best[j+1] {
						best[j+1], count[j+1] = cand, count[i]+1
					}
				}
			}
			if matched {
				continue
			}
			runEnd := i
			for runEnd < n {
				_, size := utf8.DecodeRuneInString(s[runEnd:])
				if size <= 0 || m.hasPieceAt(s, runEnd) {
					break
				}
				runEnd += size
			}
			if runEnd == i {
				total += count[i]
				break
			}
			score, cost := float64(m.unkScore), 1
			if m.byteFallback {
				score, cost = 0, runEnd-i
				for k := i; k < runEnd; k++ {
					if idx, ok := m.byteByValue[s[k]]; ok {
						score += float64(m.pieces[idx].score)
					} else {
						score += float64(m.unkScore)
					}
				}
			}
			if cand := best[i] + score; cand > best[runEnd] {
				best[runEnd], count[runEnd] = cand, count[i]+cost
			}
		}
		if best[n] == -math.MaxFloat64 {
			total += n
		} else {
			total += count[n]
		}
	}
	return total
}

func referenceSPMTrim(m *sentencePieceModel, text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if referenceSPMCount(m, text) <= limit {
		return text
	}
	runes := []rune(text)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if referenceSPMCount(m, string(runes[:mid])) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo])
}

func TestPerfCounterReferenceEquivalence(t *testing.T) {
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	m := c.(*sentencePieceCounter).model
	texts := append(perfSourceTexts(t), "<|endoftext|><|fim_prefix|><|fim_middle|><|fim_suffix|><|endofprompt|>", string([]byte{0xff, 0xfe, 'a'}))
	for i, text := range texts {
		if got, want := NumTokensFromString(text), len(enc.Encode(text, nil, nil)); got != want {
			t.Fatalf("cl100k case=%d: %d != %d", i, got, want)
		}
		if got, want := c.Count(text), referenceSPMCount(m, text); got != want {
			t.Fatalf("SPM case=%d: %d != %d", i, got, want)
		}
		if got, want := m.normalize(text), referenceSPMNormalize(text); got != want {
			t.Fatalf("normalization case=%d differs", i)
		}
		for _, limit := range []int{-1, 0, 1, 8, 64, 512, 8192} {
			if got, want := c.TrimToLimit(text, limit), referenceSPMTrim(m, text, limit); got != want {
				t.Fatalf("SPM trim case=%d limit=%d differs", i, limit)
			}
			tokens := enc.Encode(text, nil, nil)
			want := text
			if len(tokens) > max(0, limit) {
				want = clampToValidUTF8(enc.Decode(tokens[:max(0, limit)]))
				for n := 0; n < utf8.UTFMax && len(want) > 0 && len(enc.Encode(want, nil, nil)) > max(0, limit); n++ {
					want = dropLastRune(want)
				}
			}
			if got := TrimContentToTokenLimit(text, limit); got != want {
				t.Fatalf("cl100k trim case=%d limit=%d differs", i, limit)
			}
		}
	}
	t.Logf("%d texts; Count, normalization and 7 Trim limits match the reference", len(texts))
}

func TestPerfCounterConcurrentEquivalence(t *testing.T) {
	c, err := LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	texts := perfSourceTexts(t)[:32]
	wants := make([]int, len(texts))
	for i, s := range texts {
		wants[i] = referenceSPMCount(c.(*sentencePieceCounter).model, s)
	}
	var wg sync.WaitGroup
	for k := 0; k < 8; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 3; n++ {
				for i, s := range texts {
					if got := c.Count(s); got != wants[i] {
						t.Errorf("concurrent %d != %d", got, wants[i])
					}
				}
			}
		}()
	}
	wg.Wait()
}

func TestPerfTokenByteBounds(t *testing.T) {
	c, err := LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range perfSourceTexts(t) {
		if got := NumTokensFromString(s); got > len(s) {
			t.Fatalf("valid UTF-8 cl100k count %d > bytes %d", got, len(s))
		}
	}
	// NFKC can expand one input rune to many words; an input-byte bound is false.
	found := false
	for _, s := range []string{"ﷺ", "㍿", "㌀", "a"} {
		n := c.Count(s)
		t.Log(fmt.Sprintf("SPM %q: bytes=%d count=%d", s, len(s), n))
		found = found || n > len(s)
	}
	if !found {
		t.Fatal("expected a SentencePiece input-byte bound counterexample")
	}
}
