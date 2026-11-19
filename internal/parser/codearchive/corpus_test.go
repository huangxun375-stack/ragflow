package codearchive

import (
	"slices"
	"strings"
	"testing"

	"ragflow/internal/tokenizer"
)

func TestArchiveMultiFileRegression(t *testing.T) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	counter, err := tokenizer.LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	units := []struct{ path, symbol, text string }{
		{"src/functions.go", "AddFee", "// AddFee includes the processing fee.\nfunc AddFee(total int) int { return total + 7 }"},
		{"src/receiver.go", "Ledger.Credit", "// Credit records an incoming payment.\nfunc (ledger *Ledger) Credit(amount int) { ledger.total += amount }"},
		{"src/unicode.go", "Greeting", "// Greeting preserves the visitor's name.\nfunc Greeting(name string) string { return \"你好，\" + name + \"，欢迎\" }"},
	}
	sources := map[string]string{
		"src/functions.go": "package sample\n\n" + units[0].text + "\n\n// Discount reduces the total.\nfunc Discount(total int) int { return total - 3 }\n",
		"src/receiver.go":  "package sample\n\ntype Ledger struct { total int }\n\n" + units[1].text + "\n",
		"src/long.go":      "package sample\n\n// Accumulate handles a long sequence of updates.\nfunc Accumulate(value int) int {\n" + strings.Repeat("\tvalue += 17\n", 120) + "\treturn value\n}\n",
		"src/unicode.go":   "package sample\n\n" + units[2].text + "\n\nconst Message = \"" + strings.Repeat("中文账目é", 80) + "\"\n",
	}
	files := []member{
		{".gitignore", "ignored/\n", 0},
		{"ignored/skip.go", "package ignored\n", 0},
		{"node_modules/skip.js", "export const skipped = true;\n", 0},
		{".private.go", "package private\n", 0},
		{"README.md", "Not source code.\n", 0},
	}
	for name, source := range sources {
		files = append(files, member{name, source, 0})
	}
	blob := archive(t, files...)
	chunkCounts := make(map[string]int)
	for _, tc := range []struct {
		name           string
		tokens         int
		overview       bool
		ratio          float64
		embeddingLimit int
	}{
		{"default", 512, false, .5, 8192},
		{"small_budget", 128, false, .5, 8192},
		{"overview", 512, true, .5, 8192},
		{"ratio_zero", 512, false, 0, 8192},
		{"ratio_quarter", 512, false, .25, 8192},
		{"ratio_three_quarters", 512, false, .75, 8192},
		{"embedding_budget", 512, false, .5, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := DefaultPolicy()
			policy.ChunkTargetRatio = tc.ratio
			opts := options()
			opts.Policy, opts.ChunkTokens, opts.Overview = &policy, tc.tokens, tc.overview
			opts.CountTokens = tokenizer.NumTokensFromString
			opts.FitsEmbedding = func(text string) bool { return counter.Count(text) <= tc.embeddingLimit }
			result, err := Process(t.Context(), blob, opts)
			if err != nil {
				t.Fatal(err)
			}
			s := result.Stats
			if s.TotalFiles != len(files) || s.AcceptedFiles != len(sources) || s.SyntaxFallbacks != 0 || s.SkippedHidden != 2 || s.SkippedDir != 1 || s.SkippedGitignore != 1 || s.SkippedSuffix != 1 {
				t.Fatalf("unexpected source counts: %+v", s)
			}
			byPath := make(map[string]Result)
			overviews := 0
			for i, chunk := range result.Chunks {
				if _, ok := sources[chunk.SourcePath]; !ok {
					t.Fatalf("unexpected source path: %s", chunk.SourcePath)
				}
				_, body, ok := strings.Cut(chunk.Text, "\n")
				if !ok || strings.TrimSpace(body) == "" || chunk.Order != i {
					t.Fatalf("invalid chunk: %+v", chunk)
				}
				if opts.CountTokens(chunk.Text) > tc.tokens || !opts.FitsEmbedding(chunk.Text) {
					t.Fatalf("token budget exceeded: %+v", chunk)
				}
				if strings.HasPrefix(body, "概览 ") {
					overviews++
					if !tc.overview || len(chunk.Symbols) != 0 {
						t.Fatalf("unexpected overview: %+v", chunk)
					}
					continue
				}
				code := byPath[chunk.SourcePath]
				chunk.Order = len(code.Chunks)
				code.Chunks = append(code.Chunks, chunk)
				byPath[chunk.SourcePath] = code
			}
			for name, source := range sources {
				code := byPath[name]
				if len(code.Chunks) == 0 {
					t.Fatalf("missing source: %s", name)
				}
				assertLosslessNonblankChunks(t, code, source, opts)
			}
			if tc.overview && overviews == 0 {
				t.Fatal("overview did not add chunks")
			}
			if tc.tokens == 512 && tc.embeddingLimit == 8192 {
				for _, unit := range units {
					found := false
					for _, chunk := range byPath[unit.path].Chunks {
						_, body, _ := strings.Cut(chunk.Text, "\n")
						found = found || (strings.Contains(body, unit.text) && slices.Contains(chunk.Symbols, unit.symbol))
					}
					if !found {
						t.Errorf("comment or symbol detached from %s in %s", unit.symbol, unit.path)
					}
				}
			}
			chunkCounts[tc.name] = len(result.Chunks)
		})
	}
	if t.Failed() {
		return
	}
	if chunkCounts["small_budget"] <= chunkCounts["default"] {
		t.Fatal("smaller budget did not split more")
	}
	if chunkCounts["overview"] <= chunkCounts["default"] {
		t.Fatal("overview did not add chunks")
	}
	if chunkCounts["ratio_zero"] <= chunkCounts["default"] {
		t.Fatal("zero ratio did not keep declarations separate")
	}
}
