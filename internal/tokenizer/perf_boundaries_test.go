package tokenizer

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/pkoukk/tiktoken-go"
)

func TestPerfCL100KRandomAndConcurrent(t *testing.T) {
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(237100))
	texts := perfSourceTexts(t)
	for i := 0; i < 1000; i++ {
		raw := make([]byte, rng.Intn(1024))
		if _, err := rng.Read(raw); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, string(raw))
	}
	wants := make([]int, len(texts))
	for i, s := range texts {
		wants[i] = len(enc.Encode(s, nil, nil))
		if got := NumTokensFromString(s); got != wants[i] {
			t.Fatalf("case=%d count=%d want=%d", i, got, wants[i])
		}
	}
	for worker := 0; worker < 8; worker++ {
		t.Run("parallel", func(t *testing.T) {
			t.Parallel()
			for i, s := range texts {
				if got := NumTokensFromString(s); got != wants[i] {
					t.Fatalf("case=%d count=%d want=%d", i, got, wants[i])
				}
			}
		})
	}
}

func TestPerfSPMFitsExactBoundaries(t *testing.T) {
	counter, err := LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	m := counter.(*sentencePieceCounter).model
	for i, text := range append(perfSourceTexts(t), strings.Repeat("ﷺ", 3000)) {
		want := referenceSPMCount(m, text)
		s := m.normalize(text)
		if want > len(s) {
			t.Fatalf("case=%d normalized-byte bound failed", i)
		}
		for _, limit := range []int{max(0, want-1), want, want + 1} {
			got := len(s) <= limit || m.countNormalized(s, limit) <= limit
			if got != (want <= limit) {
				t.Fatalf("case=%d count=%d limit=%d early stop differs", i, want, limit)
			}
		}
	}
}
