package codearchive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	tspack "github.com/xberg-io/tree-sitter-language-pack/packages/go"
)

func TestMain(m *testing.M) {
	// Tests use deployment-prepared real grammars. An absent cache fails grammar
	// fixtures; it is never silently skipped or replaced by fake syntax trees.
	if cache := os.Getenv("RAGFLOW_CODE_ARCHIVE_GRAMMAR_CACHE"); cache != "" {
		if err := tspack.Configure(tspack.PackConfig{CacheDir: &cache}); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

func TestRealGrammarDeclarationBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, language, source, declaration, symbol string }{
		{"decorator.py", "python", "import os\n\n@decorate\ndef work():\n    return 1\n", "@decorate\ndef work():\n    return 1", "work"},
		{"a.go", "go", "package p\ntype Thing struct { X int }\nfunc (t Thing) Run() int { return t.X }\n", "type Thing struct { X int }", "Thing"},
		{"A.java", "java", "public record A(int x) { public int run() { return x; } }\n", "public record A(int x) { public int run() { return x; } }", "A"},
		{"a.js", "javascript", "export const run = (x) => x + 1;\n", "export const run = (x) => x + 1;", "run"},
		{"a.ts", "typescript", "export interface Thing { x: number }\n", "export interface Thing { x: number }", "Thing"},
		{"a.tsx", "tsx", "export const View = () => <div>中文</div>;\n", "export const View = () => <div>中文</div>;", "View"},
		{"a.c", "c", "struct Thing { int x; };\nint run(void) { return 1; }\n", "struct Thing { int x; };", "Thing"},
		{"a.h", "cpp", "template <typename T>\nclass Box { public: T get() { return value; } T value; };\n", "template <typename T>\nclass Box { public: T get() { return value; } T value; };", "Box"},
		{"a.lh", "lua", "local function run(x)\n  return x + 1\nend\n", "local function run(x)\n  return x + 1\nend", "run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := analyze(context.Background(), SourceFile{Path: tc.name, Language: tc.language, Text: tc.source}, .2)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.close()
			if parsed.fallback {
				t.Fatal("valid fixture fell back")
			}
			foundRange, foundSymbol := false, false
			for _, s := range parsed.spans {
				if tc.source[s.start:s.end] == tc.declaration {
					foundRange = true
				}
			}
			for _, s := range parsed.symbols {
				if s.name == tc.symbol {
					foundSymbol = true
				}
			}
			if !foundRange || !foundSymbol {
				t.Fatalf("range=%t symbol=%t parsed=%+v", foundRange, foundSymbol, parsed)
			}
		})
	}
}

func TestVueUnsupportedScriptLanguageStaysSourceText(t *testing.T) {
	source := "<template><div>x</div></template>\n<script lang=\"coffee\">\nsquare = (x) -> x * x\n</script>\n<script setup lang=\"ts\">\nexport function run(): number { return 1; }\n</script>\n"
	result, err := Process(context.Background(), archive(t, member{"a.vue", source, 0}), options())
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	found := false
	for _, c := range result.Chunks {
		_, body, _ := strings.Cut(c.Text, "\n")
		got.WriteString(body)
		found = found || slices.Contains(c.Symbols, "run")
	}
	if got.String() != source || !found {
		t.Fatalf("source=%t run symbol=%t", got.String() == source, found)
	}
}

func TestVueMultipleScriptsRetainOriginalOffsetsAndAllBlocks(t *testing.T) {
	source := "<template>\n  <div>中文</div>\n</template>\n<script lang=\"ts\">\nexport function run(): number { return 1; }\n</script>\n<script setup lang='tsx'>\nconst View = () => <span />;\n</script>\n<style>.x { color: red; }</style>\n"
	parsed, err := analyze(context.Background(), SourceFile{Path: "a.vue", Language: "vue", Text: source}, .2)
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.close()
	want := map[string]int{"run": 5, "View": 8}
	for _, s := range parsed.symbols {
		if line, ok := want[s.name]; ok {
			if strings.Count(source[:s.start], "\n")+1 != line {
				t.Fatalf("bad offset: %+v", s)
			}
			delete(want, s.name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing: %v", want)
	}
	opts := options()
	opts.ChunkTokens = 95
	result, err := Process(context.Background(), archive(t, member{"a.vue", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, c := range result.Chunks {
		_, body, _ := strings.Cut(c.Text, "\n")
		got.WriteString(body)
	}
	if got.String() != source {
		t.Fatalf("lost Vue content: %q", got.String())
	}
}

func TestOverviewPrecedesCodeAndUsesASTNames(t *testing.T) {
	opts := options()
	opts.Overview = true
	result, err := Process(context.Background(), archive(t, member{"a.py", "def work():\n    return 'body-secret'\n\ndef other():\n    return 'another-secret'\n", 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Chunks) < 2 || !strings.Contains(result.Chunks[0].Text, "概览") || !strings.Contains(result.Chunks[0].Text, "work") || strings.Contains(result.Chunks[0].Text, "body-secret") {
		t.Fatalf("chunks: %+v", result.Chunks)
	}
}

func TestSyntaxErrorsFallbackButMissingGrammarFails(t *testing.T) {
	parsed, err := analyze(context.Background(), SourceFile{Path: "a.py", Language: "python", Text: "def ( @@@@\n\\\\\\\n"}, .2)
	if err != nil || !parsed.fallback {
		t.Fatalf("fallback=%t err=%v", parsed.fallback, err)
	}
	defer parsed.close()
	_, err = analyze(context.Background(), SourceFile{Path: "a.py", Language: "nonexistent-grammar", Text: "pass"}, .2)
	if Reason(err) != "ZIP_UNSUPPORTED_DEPLOYMENT" {
		t.Fatal(err)
	}
}

func TestSyntaxFallbackDiagnosticsUseFinalFileAnalysis(t *testing.T) {
	for _, tc := range []struct {
		name, source, language string
		errorLines             int
	}{
		{"rescued.h", "int class;\nint template;\nint namespace;\n", "c", 0},
		{"broken.h", "@@@\n@@@\n", "cpp", 2},
		{"broken.vue", "<script>let first = ;</script><script>let second = ;</script>\n", "vue", 1},
		{"partial.vue", "<template>\n" + strings.Repeat("<p>ok</p>\n", 40) + "</template>\n<script>let first = ;</script>\n<script>let second = ;</script>\n", "vue", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options()
			var diagnostics []SyntaxFallback
			opts.OnSyntaxFallback = func(f SyntaxFallback) { diagnostics = append(diagnostics, f) }
			result, err := Process(t.Context(), archive(t, member{tc.name, tc.source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if tc.errorLines != 0 {
				wantCount = 1
			}
			if len(diagnostics) != wantCount || result.Stats.SyntaxFallbacks != wantCount {
				t.Fatalf("diagnostics=%+v stats=%+v", diagnostics, result.Stats)
			}
			if wantCount != 0 {
				total := len(lineStarts(tc.source))
				want := SyntaxFallback{Path: tc.name, Language: tc.language, ErrorLines: tc.errorLines, TotalLines: total,
					Ratio: float64(tc.errorLines) / float64(total), Threshold: DefaultPolicy().SyntaxErrorRatio}
				if diagnostics[0] != want {
					t.Fatalf("diagnostic = %+v, want %+v", diagnostics[0], want)
				}
			}
			assertLosslessNonblankChunks(t, result, tc.source, opts)
		})
	}
}

func TestMissingGrammarCacheFailsWithoutDownload(t *testing.T) {
	if os.Getenv("CODE_ARCHIVE_MISSING_CACHE_CHILD") == "1" {
		if err := CheckGrammars(DefaultPolicy()); Reason(err) != "ZIP_UNSUPPORTED_DEPLOYMENT" {
			t.Fatalf("startup grammar validation: %v", err)
		}
		_, err := Process(context.Background(), archive(t, member{"a.py", "pass", 0}), options())
		if Reason(err) != "ZIP_UNSUPPORTED_DEPLOYMENT" {
			t.Fatal(err)
		}
		return
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); http.Error(w, "blocked", 503) }))
	defer server.Close()
	t.Setenv("TREE_SITTER_LANGUAGE_PACK_MANIFEST_URL", server.URL)
	t.Setenv("RAGFLOW_CODE_ARCHIVE_GRAMMAR_CACHE", t.TempDir())
	t.Setenv("CODE_ARCHIVE_MISSING_CACHE_CHILD", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(executable, "-test.run=^TestMissingGrammarCacheFailsWithoutDownload$").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if requests.Load() != 0 {
		t.Fatalf("unexpected download requests: %d", requests.Load())
	}
}

func TestCheckGrammarsLoadsConfiguredLanguages(t *testing.T) {
	if err := CheckGrammars(DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
}
