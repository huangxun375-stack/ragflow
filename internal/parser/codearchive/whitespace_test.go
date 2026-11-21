package codearchive

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"ragflow/internal/tokenizer"
)

func assertLosslessNonblankChunks(t *testing.T, result Result, source string, opts Options) {
	t.Helper()
	var rebuilt strings.Builder
	for i, chunk := range result.Chunks {
		_, body, ok := strings.Cut(chunk.Text, "\n")
		if !ok || strings.TrimSpace(body) == "" {
			t.Fatalf("blank body at chunk %d: %q", i, chunk.Text)
		}
		start := rebuilt.Len()
		rebuilt.WriteString(body)
		if rebuilt.Len() > len(source) || source[start:rebuilt.Len()] != body {
			t.Fatalf("source changed or duplicated at chunk %d", i)
		}
		if chunk.StartLine != strings.Count(source[:start], "\n")+1 || chunk.EndLine != strings.Count(source[:rebuilt.Len()-1], "\n")+1 {
			t.Fatalf("wrong original lines: %+v", chunk)
		}
		if opts.CountTokens(chunk.Text) > opts.ChunkTokens || !opts.FitsEmbedding(chunk.Text) || !utf8.ValidString(chunk.Text) {
			t.Fatalf("budget/UTF8 violation: %q", chunk.Text)
		}
		if chunk.Order != i {
			t.Fatalf("order=%d want=%d", chunk.Order, i)
		}
	}
	if rebuilt.String() != source {
		t.Fatal("source lost")
	}
}

func TestArchiveDistributesWhitespaceBetweenFittingDeclarations(t *testing.T) {
	one := "// One docs.\nfunc One() {}"
	two := "// Two docs.\nfunc Two() {}"
	source := "package p\n\n" + one + "\n\n" + two
	for _, embeddingLimited := range []bool{false, true} {
		t.Run(fmt.Sprint(embeddingLimited), func(t *testing.T) {
			opts := options()
			limit := utf8.RuneCountInString("// a.go:L3-L4 One\n"+one) + 1
			opts.ChunkTokens = limit
			if embeddingLimited {
				opts.ChunkTokens = 512
				opts.FitsEmbedding = func(text string) bool { return utf8.RuneCountInString(text) <= limit }
			}
			result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, result, source, opts)
			for _, unit := range []string{one, two} {
				found := false
				for _, chunk := range result.Chunks {
					_, body, _ := strings.Cut(chunk.Text, "\n")
					found = found || strings.Contains(body, unit)
				}
				if !found {
					t.Fatalf("fitting commented declaration split: %q", unit)
				}
			}
		})
	}
}

func TestArchiveRedistributesTrailingWhitespaceByMovingRuneBoundary(t *testing.T) {
	source := "value = '" + strings.Repeat("中", 115) + "'" + strings.Repeat(" ", 12) + "\n"
	opts := options()
	opts.ChunkTokens = 64
	opts.FitsEmbedding = func(text string) bool { return utf8.RuneCountInString(text) <= 60 }
	result, err := Process(t.Context(), archive(t, member{"a.py", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	policy := DefaultPolicy()
	policy.Limits.MaxChunks = len(result.Chunks) - 1
	opts.Policy = &policy
	result, err = Process(t.Context(), archive(t, member{"a.py", source, 0}), opts)
	if Reason(err) != "ZIP_TOO_MANY_CHUNKS" || len(result.Chunks) != 0 {
		t.Fatalf("redistribution bypassed chunk limit: chunks=%d err=%v", len(result.Chunks), err)
	}
}

func TestArchiveBudgetBoundaryRetainsTrailingWhitespace(t *testing.T) {
	for _, tc := range []struct {
		budget, lines, padding int
	}{
		{128, 12, 0}, {128, 12, 7}, {512, 44, 7}, {512, 66, 7},
	} {
		t.Run(fmt.Sprintf("%d/%d/%d", tc.budget, tc.lines, tc.padding), func(t *testing.T) {
			source := "export function handler(value: number): number {\n" +
				strings.Repeat("  value = value + 1;\n", tc.lines) +
				"  return value;\n}" + strings.Repeat(" ", tc.padding) + "\n"
			opts := options()
			opts.ChunkTokens = tc.budget
			opts.CountTokens = tokenizer.NumTokensFromString
			result, err := Process(t.Context(), archive(t, member{"src/a.ts", source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, result, source, opts)
		})
	}
}

func TestArchiveWhitespaceBoundariesRetainEveryByte(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"a.go", "\n\t\npackage p\n\nfunc Run() {\n" + strings.Repeat("    println(1)\n\n", 10) + "}\n\n\t\n"},
		{"fallback.py", "def ( @@@@\n\\\\\\\n\n" + strings.Repeat("@@@  broken  @@@\n\n", 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options()
			opts.ChunkTokens = 64
			opts.FitsEmbedding = func(text string) bool { return utf8.RuneCountInString(text) <= 60 }
			result, err := Process(t.Context(), archive(t, member{tc.name, tc.source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, result, tc.source, opts)
			if tc.name == "fallback.py" && result.Stats.SyntaxFallbacks != 1 {
				t.Fatal("fixture must exercise real grammar syntax fallback")
			}
		})
	}
}

func TestArchiveRejectsUnrepresentableWhitespaceWithoutPartialOutput(t *testing.T) {
	for _, source := range []string{
		strings.Repeat(" \t\n", 1000) + "package p\n",
		"package p\n" + strings.Repeat(" \t\n", 1000),
		"package p\n" + strings.Repeat(" \t\n", 1000) + "func Run() {}\n",
	} {
		opts := options()
		opts.ChunkTokens = 64
		result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
		if err == nil || Reason(err) != "ZIP_CONFIG_INVALID" || len(result.Chunks) != 0 {
			t.Fatalf("unrepresentable source must fail atomically, chunks=%d err=%v", len(result.Chunks), err)
		}
	}
}

func TestArchiveWhitespaceRedistributionRetainsTransformLineMap(t *testing.T) {
	opts := options()
	opts.ChunkTokens = 40
	text := "package p\n\n// One docs.\nfunc One() {}\n\n// Two docs.\nfunc Two() {}"
	original := strings.Repeat("\n", 30) + "package p\n"
	opts.Transform = func(_ context.Context, f SourceFile) (SourceFile, error) {
		f.Text = text
		for i := range lineStarts(text) {
			f.LineMap = append(f.LineMap, LineRange{Start: 2*i + 1, End: 2*i + 2})
		}
		return f, nil
	}
	result, err := Process(t.Context(), archive(t, member{"a.go", original, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	at := 0
	for _, chunk := range result.Chunks {
		_, body, _ := strings.Cut(chunk.Text, "\n")
		if strings.TrimSpace(body) == "" || !strings.HasPrefix(text[at:], body) {
			t.Fatalf("invalid body: %q", body)
		}
		start := strings.Count(text[:at], "\n")
		at += len(body)
		end := strings.Count(text[:at-1], "\n")
		if chunk.StartLine != 2*start+1 || chunk.EndLine != 2*end+2 || opts.CountTokens(chunk.Text) > opts.ChunkTokens || !opts.FitsEmbedding(chunk.Text) {
			t.Fatalf("wrong mapped range/budget: %+v", chunk)
		}
	}
	if at != len(text) {
		t.Fatal("transformed source lost")
	}
}
