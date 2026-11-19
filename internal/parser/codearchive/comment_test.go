package codearchive

import (
	"strings"
	"testing"

	"ragflow/internal/tokenizer"
)

func TestLeadingCommentsBelongToDeclaration(t *testing.T) {
	for _, tc := range []struct{ language, prefix, unit, name string }{
		{"go", "package p\n\n", "// Work explains the function.\n// More details.\nfunc Work() {}", "Work"},
		{"python", "import os\n\n", "# Work explains the function.\n@decorate\ndef work():\n    return 1", "work"},
		{"javascript", "const before = 1;\n\n", "/** Work explains the function. */\nexport function work() {}", "work"},
		{"typescript", "const before = 1;\n\n", "/** Work explains the function. */\nexport const work = () => 1;", "work"},
		{"java", "", "/** Work explains the class. */\npublic class Work {}", "Work"},
		{"cpp", "", "// Work explains the class.\ntemplate <typename T>\nclass Work {};", "Work"},
		{"c", "", "/* Work explains the function. */\nint work(void) { return 1; }", "work"},
		{"lua", "", "-- Work explains the function.\nlocal function work()\n  return 1\nend", "work"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			source := tc.prefix + tc.unit + "\n"
			parsed, err := analyzeLanguage(t.Context(), tc.language, source, .2)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.close()
			for _, s := range parsed.symbols {
				if s.name == tc.name {
					if got := source[s.start:s.end]; got != tc.unit {
						t.Fatalf("declaration lost comment: %q", got)
					}
					return
				}
			}
			t.Fatalf("missing symbol %s", tc.name)
		})
	}
}

func TestLeadingCommentsExcludeBlankLinesAndTrailingComments(t *testing.T) {
	for _, prefix := range []string{
		"package p // package trailing comment\n",
		"package p\n// detached comment\n\n",
		"package p\n/* detached comment */\n \t\n",
	} {
		unit := "// Work belongs here.\nfunc Work() {}"
		source := prefix + unit
		parsed, err := analyzeLanguage(t.Context(), "go", source, .2)
		if err != nil {
			t.Fatal(err)
		}
		defer parsed.close()
		for _, s := range parsed.symbols {
			if s.name == "Work" && source[s.start:s.end] != unit {
				t.Fatalf("attached unrelated text: %q", source[s.start:s.end])
			}
		}
	}
}

func TestFittingCommentAndFunctionStayTogetherAcrossChunkBoundaries(t *testing.T) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	counter, err := tokenizer.LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Filename: "review.zip", ChunkTokens: 128, CountTokens: tokenizer.NumTokensFromString, FitsEmbedding: func(s string) bool { return counter.Count(s) <= 8192 }}
	unit := "// CalculateInvoice adds the processing fee to the invoice total.\nfunc CalculateInvoice(total int) int {\n    return total + 7\n}"
	for n := 1; n <= 40; n++ {
		source := "package demo\n\nfunc Before() int {\n    value := 0\n" + strings.Repeat("    value += 1\n", n) + "    return value\n}\n\n" + unit + "\n"
		result, err := Process(t.Context(), archive(t, member{"src/main.go", source, 0}), opts)
		if err != nil {
			t.Fatal(err)
		}
		var rebuilt strings.Builder
		found := false
		for _, c := range result.Chunks {
			_, body, _ := strings.Cut(c.Text, "\n")
			if strings.TrimSpace(body) == "" {
				t.Fatalf("whitespace-only body with %d padding statements: %q", n, c.Text)
			}
			start := rebuilt.Len()
			rebuilt.WriteString(body)
			if c.StartLine != strings.Count(source[:start], "\n")+1 || c.EndLine != strings.Count(source[:rebuilt.Len()-1], "\n")+1 {
				t.Fatalf("incorrect source lines: %+v", c)
			}
			if opts.CountTokens(c.Text) > opts.ChunkTokens || !opts.FitsEmbedding(c.Text) {
				t.Fatal("token budget exceeded")
			}
			found = found || strings.Contains(body, unit)
		}
		if rebuilt.String() != source {
			t.Fatal("source changed or duplicated")
		}
		if !found {
			t.Fatalf("fitting comment and function separated with %d padding statements", n)
		}
	}
}
