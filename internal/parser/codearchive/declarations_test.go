package codearchive

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"ragflow/internal/tokenizer"
)

func TestDeclarationsAcrossLanguages(t *testing.T) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, source string
		names        []string
		comments     []string
	}{
		{"a.py", "import os\n\n# first documentation for work with details\n@decorate\ndef first(value):\n    return value + 100\n\n# second documentation with details\ndef second(value):\n    return value + 200\n", []string{"first", "second"}, []string{"# first", "# second"}},
		{"a.go", "package p\n\n// First documentation for the listener method.\nfunc (l *Listener) First(value int) int { return value + 100 }\n\n// Second documentation for the listener method.\nfunc (l Listener) Second(value int) int { return value + 200 }\n", []string{"Listener.First", "Listener.Second"}, []string{"// First", "// Second"}},
		{"A.java", "/** First class documentation with details. */\nclass First { int work(int value) { return value + 100; } }\n\n/** Second class documentation with details. */\nclass Second { int work(int value) { return value + 200; } }\n", []string{"First", "Second"}, []string{"/** First", "/** Second"}},
		{"a.js", "/** First function documentation with details. */\nexport function first(value) { return value + 100; }\n\n/** Second function documentation with details. */\nexport const second = (value) => value + 200;\n", []string{"first", "second"}, []string{"/** First", "/** Second"}},
		{"a.jsx", "/** First component documentation with details. */\nexport const First = () => <div>first component</div>;\n\n/** Second component documentation with details. */\nexport const Second = () => <div>second component</div>;\n", []string{"First", "Second"}, []string{"/** First", "/** Second"}},
		{"a.ts", "/** First interface documentation with details. */\nexport interface First { work(value: number): number; }\n\n/** Second type documentation with details. */\nexport type Second = { value: number; name: string };\n", []string{"First", "Second"}, []string{"/** First", "/** Second"}},
		{"a.tsx", "/** First component documentation with details. */\nexport const First = (value: number) => <div>{value}</div>;\n\n/** Second component documentation with details. */\nexport const Second = () => <span>second component</span>;\n", []string{"First", "Second"}, []string{"/** First", "/** Second"}},
		{"a.c", "#include <stdio.h>\n\n/* First function documentation with details. */\nint first(int value) { return value + 100; }\n\n/* Second function documentation with details. */\nint second(int value) { return value + 200; }\n", []string{"first", "second"}, []string{"/* First", "/* Second"}},
		{"a.cpp", "// First class documentation with details.\ntemplate<typename T> class First { T work(T value) { return value; } };\n\n// Second function documentation with details.\nint Second::work(int value) { return value + 200; }\n", []string{"First", "Second.work"}, []string{"// First", "// Second"}},
		{"a.h", "// First class documentation with details.\nclass First { int work(int value) { return value + 100; } };\n\n// Second function declaration documentation with details.\nint second(int value);\n", []string{"First", "second"}, []string{"// First", "// Second"}},
		{"a.lua", "-- First function documentation with details.\nfunction M.first(value)\n  return value + 100\nend\n\n-- Second function documentation with details.\nfunction M:second(value)\n  return value + 200\nend\n", []string{"M.first", "M:second"}, []string{"-- First", "-- Second"}},
		{"a.vue", "<template><div>hello</div></template>\n<script setup lang=\"ts\">\n// First function documentation with details.\nfunction first(value: number) { return value + 100; }\n\n// Second function documentation with details.\nfunction second(value: number) { return value + 200; }\n</script>\n<style>div { color: red; }</style>\n", []string{"first", "second"}, []string{"// First", "// Second"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			opts := options()
			policy := DefaultPolicy()
			policy.ChunkTargetRatio = 0
			opts.Policy = &policy
			blob := archive(t, member{tc.path, tc.source, 0})
			result, err := Process(t.Context(), blob, opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Stats.SyntaxFallbacks != 0 {
				t.Fatal("valid syntax fell back")
			}
			assertLosslessNonblankChunks(t, result, tc.source, opts)
			for i, name := range tc.names {
				found := 0
				for _, c := range result.Chunks {
					if !slices.Contains(c.Symbols, name) {
						continue
					}
					found++
					if !strings.Contains(c.Text, tc.comments[i]) {
						t.Fatalf("detached comment: %+v", c)
					}
					for j, other := range tc.names {
						if i != j && slices.Contains(c.Symbols, other) {
							t.Fatalf("ordinary declarations merged: %+v", c)
						}
					}
				}
				if found != 1 {
					t.Fatalf("%s appears in %d chunks: %+v", name, found, result.Chunks)
				}
			}
			again, err := Process(t.Context(), blob, opts)
			if err != nil || !reflect.DeepEqual(result, again) {
				t.Fatalf("nondeterministic: %v", err)
			}
			policy.ChunkTargetRatio = .5
			opts.CountTokens = tokenizer.NumTokensFromString
			merged, err := Process(t.Context(), blob, opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, merged, tc.source, opts)
			if len(merged.Chunks) != 1 || opts.CountTokens(merged.Chunks[0].Text) > 256 {
				t.Fatalf("fitting declarations not merged: %+v", merged.Chunks)
			}
			header, _, _ := strings.Cut(merged.Chunks[0].Text, "\n")
			for _, c := range result.Chunks {
				for _, name := range c.Symbols {
					if !slices.Contains(merged.Chunks[0].Symbols, name) || !strings.Contains(header, name) {
						t.Fatalf("merged symbol missing: %s in %+v", name, merged.Chunks[0])
					}
				}
			}
			again, err = Process(t.Context(), blob, opts)
			if err != nil || !reflect.DeepEqual(merged, again) {
				t.Fatalf("nondeterministic merge: %v", err)
			}
		})
	}
}

func TestOversizedClassesSplitHeadAndQualifiedMethods(t *testing.T) {
	for _, tc := range []struct{ path, source, head, first, second string }{
		{"a.py", "class Box(Base):\n    value: int = 1\n\n    # First docs\n    def first(self):\n        return self.value + 100\n\n    # Second docs\n    def second(self):\n        return self.value + 200\n", "class Box", "Box.first", "Box.second"},
		{"A.java", "class Box extends Base {\n int value = 1;\n // First docs\n int first() { return value + 100; }\n // Second docs\n int second() { return value + 200; }\n}\n", "class Box", "Box.first", "Box.second"},
		{"a.ts", "class Box extends Base {\n value: number = 1;\n // First docs\n first() { return this.value + 100; }\n // Second docs\n second() { return this.value + 200; }\n}\n", "class Box", "Box.first", "Box.second"},
		{"a.cpp", "class Box : public Base {\n int value = 1;\n // First docs\n int first() { return value + 100; }\n // Second docs\n int second() { return value + 200; }\n};\n", "class Box", "Box.first", "Box.second"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			opts := options()
			opts.ChunkTokens = 120
			result, err := Process(t.Context(), archive(t, member{tc.path, tc.source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, result, tc.source, opts)
			for _, name := range []string{tc.first, tc.second} {
				found := false
				for _, c := range result.Chunks {
					if slices.Contains(c.Symbols, name) {
						found = true
						if len(c.Symbols) != 1 || strings.Contains(c.Text, tc.head) {
							t.Fatalf("head/method mixed: %+v", c)
						}
					}
				}
				if !found {
					t.Fatalf("missing %s: %+v", name, result.Chunks)
				}
			}
		})
	}
}

func TestSymbolHeaderTruncationAndBothBudgets(t *testing.T) {
	name := "函数" + strings.Repeat("VeryLongIdentifier", 40)
	source := "package p\nfunc " + name + "() { println(1) }\n"
	opts := options()
	opts.ChunkTokens = 120
	opts.FitsEmbedding = func(s string) bool { return utf8.RuneCountInString(s) <= 110 }
	result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	foundPart := false
	for _, c := range result.Chunks {
		if len(c.Symbols) == 0 {
			continue
		}
		header, _, _ := strings.Cut(c.Text, "\n")
		if !strings.HasSuffix(header, " …") || !slices.Contains(c.Symbols, name) {
			t.Fatalf("bad truncated header/metadata: %+v", c)
		}
		foundPart = foundPart || strings.Contains(header, "#1 ")
	}
	if !foundPart {
		t.Fatal("missing part number before symbol")
	}
}

func TestSkeletonEligibilityAndFormat(t *testing.T) {
	for _, tc := range []struct {
		source   string
		overview bool
	}{
		{"def one():\n    return 1\n", false},
		{"import os\n# One docs\ndef one(value):\n    return 'secret-body'\n\ndef two():\n    return 2\n", true},
	} {
		opts := options()
		opts.Overview = true
		result, err := Process(t.Context(), archive(t, member{"a.py", tc.source, 0}), opts)
		if err != nil {
			t.Fatal(err)
		}
		_, body, _ := strings.Cut(result.Chunks[0].Text, "\n")
		if strings.HasPrefix(body, "概览 ") != tc.overview {
			t.Fatalf("wrong eligibility: %+v", result.Chunks)
		}
		if tc.overview {
			if !strings.Contains(body, "imports: import os") || !strings.Contains(body, "def one(value):") || !strings.Contains(body, "One docs") || strings.Contains(body, "secret-body") || len(result.Chunks[0].Symbols) != 0 {
				t.Fatalf("bad skeleton: %s", body)
			}
		}
	}
}

func TestSkeletonSplitsAndFallbackNeverSummarizes(t *testing.T) {
	opts := options()
	opts.Overview = true
	opts.ChunkTokens = 120
	source := "import os\n"
	for i := 0; i < 12; i++ {
		source += "def function" + string(rune('A'+i)) + "(value):\n    return value + 1\n\n"
	}
	result, err := Process(t.Context(), archive(t, member{"a.py", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	code := Result{}
	overview := 0
	for _, c := range result.Chunks {
		_, body, _ := strings.Cut(c.Text, "\n")
		if strings.HasPrefix(body, "概览 ") {
			overview++
			if len(c.Symbols) != 0 || opts.CountTokens(c.Text) > opts.ChunkTokens {
				t.Fatal("overview symbols/budget")
			}
			continue
		}
		c.Order = len(code.Chunks)
		code.Chunks = append(code.Chunks, c)
	}
	if overview < 2 {
		t.Fatal("large skeleton not split")
	}
	assertLosslessNonblankChunks(t, code, source, opts)
	result, err = Process(t.Context(), archive(t, member{"a.py", "def ( @@@@\n\\\\\\\n", 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range result.Chunks {
		if strings.Contains(c.Text, "概览 ") {
			t.Fatal("fallback overview")
		}
	}
}

func TestFitsMemoAndByteFastPath(t *testing.T) {
	policy := DefaultPolicy()
	counts, embeddings := 0, 0
	b := chunkBuilder{ctx: t.Context(), file: SourceFile{Path: "a.go", Text: "package p\n"}, lines: []int{0}, result: &Result{}, opts: Options{Policy: &policy, ChunkTokens: 512, CountTokens: func(s string) int { counts++; return len(s) }, FitsEmbedding: func(s string) bool { embeddings++; return true }}}
	s := byteSpan{0, len(b.file.Text)}
	if !b.sourceFits(s, 0) || !b.sourceFits(s, 0) || counts != 0 || embeddings != 1 {
		t.Fatalf("counts=%d embedding=%d", counts, embeddings)
	}
	b.opts.FitsEmbedding = func(s string) bool { return false }
	if b.sourceFits(s, 1) {
		t.Fatal("byte shortcut bypassed embedding contract")
	}
}

func TestTargetIncludesPrelude(t *testing.T) {
	opts := options()
	source := "package p\n// " + strings.Repeat("x", 150) + "\n\nfunc One() {}\nfunc Two() {}\n"
	result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	for _, c := range result.Chunks {
		if len(c.Symbols) == 2 && opts.CountTokens(c.Text) > opts.ChunkTokens/2 {
			t.Fatalf("merged group exceeds target: %+v", c)
		}
	}
}

func TestNestedNamesAndSkeletonExcludeBodies(t *testing.T) {
	for _, tc := range []struct {
		language, source string
		names            []string
	}{
		{"go", "package p\ntype Reader interface { Read(p []byte) (int,error) }\nfunc Outer() { var hidden = 1; _ = hidden }\n", []string{"Reader", "Reader.Read", "Outer"}},
		{"typescript", "class Box { value = 1; method() { const hidden = 2; return hidden; } }\nfunction other() {}", []string{"Box", "Box.method", "other"}},
		{"lua", "function M.foo(value)\n return 'BODY_SECRET'\nend\nfunction M:bar(value)\n return 2\nend", []string{"M.foo", "M:bar"}},
	} {
		t.Run(tc.language, func(t *testing.T) {
			parsed, err := analyzeLanguage(t.Context(), tc.language, tc.source, .2)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.close()
			var names []string
			for _, s := range parsed.symbols {
				names = append(names, s.name)
				if strings.Contains(s.signature, "BODY_SECRET") {
					t.Fatalf("body leaked: %s", s.signature)
				}
			}
			if !reflect.DeepEqual(names, tc.names) {
				t.Fatalf("names=%v want=%v", names, tc.names)
			}
		})
	}
}

func TestChunkSymbolsPreserveDeclarationScope(t *testing.T) {
	for _, tc := range []struct {
		path, source string
		want         []string
	}{
		{"pages.js", "export function* pages() {\n  const cursor = 1;\n  yield cursor;\n}\n", []string{"pages"}},
		{"reader.ts", "export abstract class Reader {\n  abstract read(): string;\n  close(): void {}\n}\n", []string{"Reader", "Reader.read", "Reader.close"}},
		{"worker.hpp", "typedef struct { void run() {} } Worker;\n", []string{"Worker", "Worker.run"}},
		{"tagged.hpp", "typedef struct WorkerTag { void run() {} } Worker;\n", []string{"Worker", "Worker.run"}},
		{"pointer.hpp", "typedef struct Widget { void run() {} } *WidgetHandle;\n", []string{"WidgetHandle", "Widget.run"}},
		{"anonymous.hpp", "typedef struct { void run() {} } *Handle;\n", []string{"Handle", "run"}},
		{"array.hpp", "typedef struct Widget { void run() {} } Widgets[2];\n", []string{"Widgets", "Widget.run"}},
		{"parenthesized.hpp", "typedef struct Widget { void run() {} } (Worker);\n", []string{"Worker", "Worker.run"}},
		{"nested.hpp", "struct Outer { typedef struct Widget { void run() {} } *Handle; };\n", []string{"Outer", "Outer.Handle", "Outer.Widget.run"}},
		{"anonymous_nested.hpp", "struct Outer { typedef struct { void run() {} } *Handle; };\n", []string{"Outer", "Outer.Handle", "Outer.run"}},
		{"status.hpp", "typedef int Status;\n", []string{"Status"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			opts := options()
			result, err := Process(t.Context(), archive(t, member{tc.path, tc.source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, result, tc.source, opts)
			if result.Stats.SyntaxFallbacks != 0 || len(result.Chunks) != 1 {
				t.Fatalf("expected one structural chunk: %+v", result)
			}
			if got := result.Chunks[0].Symbols; !slices.Equal(got, tc.want) {
				t.Fatalf("published symbols = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGroupedGoTypesRemainSeparateWithoutMerging(t *testing.T) {
	source := "package p\ntype (\n // First has a documented field and must remain a unit.\n First struct { Value string }\n // Second has a documented field and must remain a unit.\n Second struct { Value int }\n)\n"
	opts := options()
	policy := DefaultPolicy()
	policy.ChunkTargetRatio = 0
	opts.Policy = &policy
	result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	for _, name := range []string{"First", "Second"} {
		found := false
		for _, c := range result.Chunks {
			if slices.Contains(c.Symbols, name) {
				found = true
				if len(c.Symbols) != 1 || !strings.Contains(c.Text, "// "+name) {
					t.Fatalf("grouped types merged/detached: %+v", c)
				}
			}
		}
		if !found {
			t.Fatalf("missing type %s", name)
		}
	}
}

func TestRealTokenBudgets(t *testing.T) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	counter, err := tokenizer.LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	source := "package p\nfunc Work() {\n" + strings.Repeat(" println(12345, \"中文\")\n", 100) + "}\nfunc After() {}\n"
	opts := Options{ChunkTokens: 64, CountTokens: tokenizer.NumTokensFromString, FitsEmbedding: func(s string) bool { return counter.Count(s) <= 80 }}
	result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
}
