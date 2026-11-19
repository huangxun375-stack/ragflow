package task

import (
	"strings"
	"testing"

	"ragflow/internal/entity/models"
	"ragflow/internal/tokenizer"
)

func TestEmbedderFitsInputMatchesTrim(t *testing.T) {
	spm, err := tokenizer.LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []tokenizer.Counter{tokenizer.CountCL100K(), spm} {
		for _, limit := range []int{1, 32, 512, 8192} {
			e := &embedder{model: models.NewEmbeddingModel(nil, strPtr("BAAI/bge-m3"), &models.APIConfig{}, limit)}
			e.limiterOnce.Do(func() { e.limiterVal = tokenizer.NewExactLimiter(c) })
			for _, s := range []string{"", "hello 中文😀\n", "\x01", strings.Repeat("func f() { return 123 }\n", 3000)} {
				trimmed, _ := e.Trim(s)
				if e.FitsInput(s) != (trimmed == s) {
					t.Fatalf("%s max=%d len=%d differs", c.ID(), limit, len(s))
				}
			}
		}
	}
}
