package codearchive_test

import (
	"strings"
	"testing"

	"ragflow/internal/ingestion/testutil"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/tokenizer"
)

func BenchmarkArchivePerf(b *testing.B) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		b.Fatal(err)
	}
	counter, err := tokenizer.LoadXLMRSentencePieceCounter()
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		tokens   int
		overview bool
		scale    bool
	}{
		{"source512", 512, false, false},
		{"source128", 128, false, false},
		{"sourceOverview", 512, true, false},
		{"scale", 512, false, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var blob []byte
			var err error
			if tc.scale {
				blob, _, err = testutil.ArchiveScale()
			} else {
				blob, err = testutil.ArchiveZIP(map[string]string{
					"src/ledger.go":   "package sample\n\ntype Ledger struct { total int }\n\n// Credit records an incoming payment.\nfunc (ledger *Ledger) Credit(amount int) { ledger.total += amount }\n\n// Balance returns the available total.\nfunc (ledger Ledger) Balance() int { return ledger.total }\n",
					"src/long.go":     "package sample\n\n// Accumulate handles a sequence of updates.\nfunc Accumulate(value int) int {\n" + strings.Repeat("\tvalue += 17\n", 120) + "\treturn value\n}\n",
					"src/greeting.ts": "// Preserve the visitor's Unicode name.\nexport function greet(name: string): string {\n  return '你好，' + name + '，欢迎';\n}\n",
				})
			}
			if err != nil {
				b.Fatal(err)
			}
			opts := codearchive.Options{
				Filename: "source.zip", ChunkTokens: tc.tokens, Overview: tc.overview,
				CountTokens:   tokenizer.NumTokensFromString,
				FitsEmbedding: func(text string) bool { return counter.Count(text) <= 8192 },
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(blob)))
			b.ResetTimer()
			var result codearchive.Result
			for i := 0; i < b.N; i++ {
				result, err = codearchive.Process(b.Context(), blob, opts)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if len(result.Chunks) == 0 {
				b.Fatal("no source chunks")
			}
			b.ReportMetric(float64(len(result.Chunks)), "chunks/op")
		})
	}
}
