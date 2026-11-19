package codearchive

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"ragflow/internal/tokenizer"
)

func TestSymbolsForOrderDeduplicationAndShadowing(t *testing.T) {
	b := chunkBuilder{symbols: []symbol{
		{name: "imports", kind: "import", start: 0, end: 5},
		{name: "Box", kind: "class", start: 10, end: 90},
		{name: "Box.z", kind: "method", start: 20, end: 30},
		{name: "Box.z", kind: "method", start: 35, end: 45},
		{name: "Box.a", kind: "method", start: 50, end: 60},
		{name: "After", kind: "function", start: 100, end: 110},
		{name: "After", kind: "function", start: 120, end: 130},
	}}
	for _, tc := range []struct {
		name string
		span byteSpan
		want []string
	}{
		{"whole", byteSpan{0, 140}, []string{"Box", "Box.z", "Box.a", "After"}},
		{"whole container", byteSpan{10, 90}, []string{"Box", "Box.z", "Box.a"}},
		{"shadowed container", byteSpan{15, 65}, []string{"Box.z", "Box.a"}},
		{"partial method", byteSpan{25, 27}, []string{"Box.z"}},
		{"container gap", byteSpan{30, 35}, []string{"Box"}},
		{"repeated name", byteSpan{100, 130}, []string{"After"}},
		{"imports only", byteSpan{0, 5}, nil},
		{"half-open gap", byteSpan{90, 100}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := b.symbolsFor(tc.span)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("symbols=%v want=%v", got, tc.want)
			}
			cached, ok := b.spanSymbols[tc.span]
			if !ok || !reflect.DeepEqual(cached, tc.want) || !reflect.DeepEqual(b.symbolsFor(tc.span), tc.want) {
				t.Fatalf("cached symbols=%v present=%t", cached, ok)
			}
			if len(got) > 0 && &got[0] != &b.symbolsFor(tc.span)[0] {
				t.Fatal("cache no longer reuses the symbol slice")
			}
		})
	}
}

func TestZeroRatioKeepsEachDeclarationSeparate(t *testing.T) {
	source := "package p\n// First documented declaration.\nfunc First() { println(1) }\n\n// Second documented declaration.\nfunc Second() { println(2) }\n"
	policy := DefaultPolicy()
	policy.ChunkTargetRatio = 0
	opts := options()
	opts.Policy = &policy
	result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []Chunk{
		{Text: "// a.go:L1-L3 First\npackage p\n// First documented declaration.\nfunc First() { println(1) }", Symbols: []string{"First"}, SourcePath: "a.go", Language: "go", StartLine: 1, EndLine: 3},
		{Text: "// a.go:L3-L6 Second\n\n\n// Second documented declaration.\nfunc Second() { println(2) }\n", Symbols: []string{"Second"}, SourcePath: "a.go", Language: "go", StartLine: 3, EndLine: 6, Order: 1},
	}
	if !reflect.DeepEqual(result.Chunks, want) {
		t.Fatalf("ratio zero merged declarations: %+v", result.Chunks)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	policy.ChunkTargetRatio = .5
	merged, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil || len(merged.Chunks) != 1 {
		t.Fatalf("same input did not merge: %+v %v", merged, err)
	}
}

func TestTargetGreedyBoundaryAndNoMicroException(t *testing.T) {
	source := "package p\nfunc One() {}\nfunc Two() {}\nfunc End() {}\n"
	for _, ratio := range []float64{0, .5, 1} {
		policy := DefaultPolicy()
		policy.ChunkTargetRatio = ratio
		opts := options()
		opts.Policy = &policy
		result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
		if err != nil {
			t.Fatal(err)
		}
		assertLosslessNonblankChunks(t, result, source, opts)
		want := 1
		if ratio == 0 {
			want = 3
		}
		if len(result.Chunks) != want {
			t.Fatalf("ratio=%g chunks=%+v", ratio, result.Chunks)
		}
	}
	policy := DefaultPolicy()
	opts := options()
	opts.Policy = &policy
	prefix := "package p\nfunc One() {}\nfunc Two() {}"
	limit := len("// a.go:L1-L3 One, Two\n" + prefix)
	for _, delta := range []int{-1, 0, 1} {
		opts.ChunkTokens = 2*(limit+delta) + 1
		result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
		if err != nil {
			t.Fatal(err)
		}
		assertLosslessNonblankChunks(t, result, source, opts)
		merged := slices.Contains(result.Chunks[0].Symbols, "Two")
		if merged != (delta >= 0) || slices.Contains(result.Chunks[0].Symbols, "End") {
			t.Fatalf("inclusive/floored target delta=%d: %+v", delta, result.Chunks)
		}
	}
	b := chunkBuilder{opts: opts}
	policy.ChunkTargetRatio = .0001
	if b.target() != 1 {
		t.Fatal("positive target must be at least one")
	}
}

func TestOverTargetDeclarationStaysWholeAndIsolated(t *testing.T) {
	source := "package p\nfunc Before() {}\n// Work documentation\nfunc Work() { println(\"" + strings.Repeat("x", 160) + "\") }\nfunc After() {}\nfunc Last() {}\n"
	opts := options()
	opts.ChunkTokens = 400
	result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	found := 0
	for _, c := range result.Chunks {
		if slices.Contains(c.Symbols, "Work") {
			found++
			if len(c.Symbols) != 1 || opts.CountTokens(c.Text) <= 200 || !strings.Contains(c.Text, "// Work documentation\nfunc Work()") || !strings.Contains(c.Text, strings.Repeat("x", 160)) {
				t.Fatalf("over-target declaration split or merged: %+v", c)
			}
		}
	}
	if found != 1 {
		t.Fatalf("Work in %d chunks", found)
	}
}

func TestOversizedContainerMembersMergeRecursively(t *testing.T) {
	for _, tc := range []struct{ path, head, method, tail string }{
		{"a.py", "class Box:\n    value = 1\n", "    # method docs\n    def work%d(self):\n        return self.value\n", ""},
		{"A.java", "class Box {\n int value = 1;\n", " // method docs\n int work%d() { return value; }\n", "}\n"},
		{"a.ts", "class Box {\n value = 1;\n", " // method docs\n work%d() { return this.value; }\n", "}\n"},
		{"a.cpp", "class Box {\n int value = 1;\n", " // method docs\n int work%d() { return value; }\n", "};\n"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			source := tc.head
			for i := 0; i < 12; i++ {
				source += fmt.Sprintf(tc.method, i)
			}
			source += tc.tail
			opts := options()
			opts.ChunkTokens = 400
			result, err := Process(t.Context(), archive(t, member{tc.path, source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, result, source, opts)
			merged := 0
			for _, c := range result.Chunks {
				if len(c.Symbols) > 1 {
					merged++
					if opts.CountTokens(c.Text) > 200 || strings.Contains(c.Text, tc.head) || slices.Contains(c.Symbols, "Box") {
						t.Fatalf("container head or target violated: %+v", c)
					}
				}
			}
			if merged == 0 {
				t.Fatal("members were not merged")
			}
			for i := 0; i < 12; i++ {
				name := fmt.Sprintf("Box.work%d", i)
				found := 0
				for _, c := range result.Chunks {
					if slices.Contains(c.Symbols, name) {
						found++
						if !strings.Contains(c.Text, strings.TrimSpace(fmt.Sprintf(tc.method, i))) {
							t.Fatalf("method split: %s", name)
						}
					}
				}
				if found != 1 {
					t.Fatalf("%s in %d chunks", name, found)
				}
			}
		})
	}
}

func TestTargetMergeHonorsEmbeddingAndCachesBothLimits(t *testing.T) {
	source := "package p\nfunc One() {}\nfunc Two() {}\n"
	opts := options()
	opts.FitsEmbedding = func(s string) bool { return !(strings.Contains(s, "func One") && strings.Contains(s, "func Two")) }
	result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil || len(result.Chunks) != 2 {
		t.Fatalf("embedding predicate bypassed: %+v %v", result, err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	policy := DefaultPolicy()
	counts, embeddings := 0, 0
	b := chunkBuilder{ctx: t.Context(), file: SourceFile{Path: "a.go", Text: source}, lines: lineStarts(source), result: &Result{}, opts: Options{Policy: &policy, ChunkTokens: 512,
		CountTokens: func(s string) int { counts++; return len(s) / 2 }, FitsEmbedding: func(s string) bool { embeddings++; return true }}}
	span := byteSpan{0, len(source)}
	if b.sourceWithin(span, 0, 1) || !b.sourceFits(span, 0) || !b.sourceWithin(span, 0, 256) || counts != 1 || embeddings != 1 {
		t.Fatalf("cached target failure poisoned hard budget: count=%d embedding=%d", counts, embeddings)
	}
}

func TestTargetMergingNeverCrossesFilesAndFailsAtomically(t *testing.T) {
	policy := DefaultPolicy()
	opts := options()
	opts.Policy = &policy
	one := "package p\nfunc One() {}\nfunc Two() {}\n"
	blob := archive(t, member{"a.go", one, 0}, member{"b.go", one, 0})
	result, err := Process(t.Context(), blob, opts)
	if err != nil || len(result.Chunks) != 2 {
		t.Fatalf("file boundaries not preserved: %+v %v", result, err)
	}
	for i, c := range result.Chunks {
		if c.SourcePath != []string{"a.go", "b.go"}[i] || !reflect.DeepEqual(c.Symbols, []string{"One", "Two"}) {
			t.Fatalf("foreign symbols or provenance: %+v", c)
		}
		_, body, _ := strings.Cut(c.Text, "\n")
		if body != one {
			t.Fatal("source changed")
		}
	}
	policy.Limits.MaxChunks = 1
	result, err = Process(t.Context(), blob, opts)
	if Reason(err) != "ZIP_TOO_MANY_CHUNKS" || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("partial archive leaked: %+v %v", result, err)
	}
}

func TestMergedHeaderTruncatesWithoutDroppingSymbols(t *testing.T) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	source := "package p\n"
	var names []string
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("VeryLongDeclarationName%d", i)
		names = append(names, name)
		source += "func " + name + "() {}\n"
	}
	opts := options()
	opts.ChunkTokens = 1024
	opts.CountTokens = tokenizer.NumTokensFromString
	result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil || len(result.Chunks) != 1 {
		t.Fatalf("merge failed: %+v %v", result, err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	header, _, _ := strings.Cut(result.Chunks[0].Text, "\n")
	if !strings.HasSuffix(header, " …") || !reflect.DeepEqual(result.Chunks[0].Symbols, names) {
		t.Fatalf("symbol metadata truncated: %+v", result.Chunks[0])
	}
}
