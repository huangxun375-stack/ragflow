package codearchive

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"
	tspack "github.com/xberg-io/tree-sitter-language-pack/packages/go"

	"ragflow/internal/tokenizer"
)

func TestRealAnonymousCallableGrammar(t *testing.T) {
	for _, tc := range []struct{ language, source string }{
		{"javascript", "invoke(x => { const hidden = 1; }); invoke(function() { const hidden = 1; }); invoke(function*() { const hidden = 1; yield hidden; }); class Box { handle = () => { const hidden = 1; }; other = function() {}; }"},
		{"typescript", "invoke(x => { const hidden = 1; }); invoke(function() { const hidden = 1; }); invoke(function*() { const hidden = 1; yield hidden; }); class Box { handle = () => { const hidden = 1; }; other = function() {}; }"},
		{"tsx", "invoke(x => { const hidden = 1; }); invoke(function() { const hidden = 1; }); invoke(function*() { yield 1; }); class Box { handle = () => <div/>; }"},
		{"python", "invoke(lambda x: x + 1)\n"},
		{"java", "class Box { static { invoke(() -> { class hidden {} }); } Runnable value = () -> { int hidden = 1; }; }"},
		{"cpp", "auto value = []() { struct hidden { int value; }; return 1; };"},
		{"go", "package p\nvar value = func() { type hidden int }\n"},
		{"lua", "describe('sample', function() local function helper() end helper() end)\n"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			tree, err := parseTree(t.Context(), tc.language, []byte(tc.source))
			if err != nil {
				t.Fatal(err)
			}
			defer tree.Close()
			if tree.RootNode().HasError() {
				t.Fatalf("invalid fixture: %s", tree.RootNode().ToSexp())
			}
			t.Logf("%s: %s", tc.language, tree.RootNode().ToSexp())
			seen := map[string]bool{}
			var visit func(*ts.Node)
			visit = func(n *ts.Node) {
				seen[n.Kind()] = true
				if n.Kind() == "field_definition" || n.Kind() == "public_field_definition" {
					field := "name"
					if tc.language == "javascript" {
						field = "property"
					}
					if n.ChildByFieldName(field) == nil || !callableField(n) {
						t.Fatalf("unexpected field: %s", n.ToSexp())
					}
				}
				for i := uint(0); i < n.NamedChildCount(); i++ {
					visit(n.NamedChild(i))
				}
			}
			visit(tree.RootNode())
			want := map[string][]string{
				"javascript": {"arrow_function", "function_expression", "generator_function", "field_definition"},
				"typescript": {"arrow_function", "function_expression", "generator_function", "public_field_definition"},
				"tsx":        {"arrow_function", "function_expression", "generator_function", "public_field_definition"},
				"python":     {"lambda"}, "java": {"lambda_expression"}, "cpp": {"lambda_expression"},
				"go": {"func_literal"}, "lua": {"function_definition", "function_declaration"},
			}[tc.language]
			for _, kind := range want {
				if !seen[kind] {
					t.Errorf("missing real node %s", kind)
				}
			}
			if tc.language == "javascript" || tc.language == "typescript" || tc.language == "tsx" {
				grammar, err := tspack.GetLanguage(tc.language)
				if err != nil {
					t.Fatal(err)
				}
				for id := uint32(0); id < grammar.NodeKindCount(); id++ {
					if grammar.NodeKindForId(uint16(id)) == "function" && grammar.NodeKindIsNamed(uint16(id)) {
						t.Fatal("unhandled named function expression grammar")
					}
				}
			}
			parsed, err := analyzeLanguage(t.Context(), tc.language, tc.source, .2)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.close()
			for _, s := range parsed.symbols {
				if strings.Contains(s.name, "hidden") || s.name == "helper" {
					t.Errorf("anonymous local escaped: %+v", s)
				}
			}
		})
	}
}

func TestAnonymousCallbacksDoNotPublishLocalSymbols(t *testing.T) {
	for _, ext := range []string{"js", "ts", "tsx"} {
		for _, ratio := range []float64{0, .5} {
			t.Run(fmt.Sprintf("%s/%g", ext, ratio), func(t *testing.T) {
				var source string
				var calls []string
				for i := 0; i < 8; i++ {
					call := fmt.Sprintf("test('%d', t => {\n  const hidden%d = makeValue(%d);\n  t.equal(hidden%d, %d);\n});\n", i, i, i, i, i)
					calls = append(calls, call)
					source += call
				}
				policy := DefaultPolicy()
				policy.ChunkTargetRatio = ratio
				opts := options()
				opts.Policy, opts.ChunkTokens = &policy, 230
				result, err := Process(t.Context(), archive(t, member{"calls." + ext, source, 0}), opts)
				if err != nil {
					t.Fatal(err)
				}
				assertLosslessNonblankChunks(t, result, source, opts)
				again, err := Process(t.Context(), archive(t, member{"calls." + ext, source, 0}), opts)
				if err != nil || !reflect.DeepEqual(result, again) {
					t.Fatalf("nondeterministic output: %v", err)
				}
				for _, c := range result.Chunks {
					t.Logf("ratio=%g %s:L%d-L%d symbols=%v", ratio, c.SourcePath, c.StartLine, c.EndLine, c.Symbols)
					header, _, _ := strings.Cut(c.Text, "\n")
					if len(c.Symbols) != 0 || strings.Contains(header, "hidden") {
						t.Errorf("callback locals escaped: %+v", c)
					}
				}
				for _, call := range calls {
					if !slices.ContainsFunc(result.Chunks, func(c Chunk) bool { return strings.Contains(c.Text, call) }) {
						t.Errorf("fitting call split: %s", call)
					}
				}
			})
		}
	}
}

func TestOversizedGoSwitchStaysWhole(t *testing.T) {
	prefix := "func Work(mode string) int {\n" + strings.Repeat("\tprintln(mode)\n", 18) + "\tswitch mode {\n\tcase \"a\":\n"
	switchText := "\tswitch mode {\n\tcase \"a\":\n\t\treturn 1\n\tdefault:\n\t\treturn 2\n\t}\n"
	source := "package p\n" + prefix + "\t\treturn 1\n\tdefault:\n\t\treturn 2\n\t}\n}\n"
	opts := options()
	opts.ChunkTokens = len(fmt.Sprintf("// a.go:L2-L%d#1 Work\n%s", strings.Count(prefix, "\n")+1, prefix))
	result, err := Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	if !slices.ContainsFunc(result.Chunks, func(c Chunk) bool { return strings.Contains(c.Text, switchText) }) {
		t.Fatalf("switch split: %+v", result.Chunks)
	}

	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	counter, err := tokenizer.LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	opts.CountTokens = tokenizer.NumTokensFromString
	opts.ChunkTokens = opts.CountTokens(fmt.Sprintf("// a.go:L2-L%d#1 Work\n%s", strings.Count(prefix, "\n")+1, prefix))
	opts.FitsEmbedding = func(s string) bool { return counter.Count(s) <= 180 }
	result, err = Process(t.Context(), archive(t, member{"a.go", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	if !slices.ContainsFunc(result.Chunks, func(c Chunk) bool { return strings.Contains(c.Text, switchText) }) {
		t.Fatalf("real-budget switch split: %+v", result.Chunks)
	}
}

func TestCallableClassFieldsAreCommentedMembers(t *testing.T) {
	for _, ext := range []string{"js", "ts", "tsx"} {
		t.Run(ext, func(t *testing.T) {
			source := "class Box {\n value = 1;\n"
			var units, names []string
			for i := 0; i < 10; i++ {
				name := fmt.Sprintf("handle%d", i)
				if i == 0 {
					name = "handle"
				}
				callable := "() =>"
				if i%2 == 1 {
					callable = "function()"
				}
				unit := fmt.Sprintf("// Member documentation.\n %s = %s { const hidden = 1; return hidden; };", name, callable)
				units = append(units, unit)
				names = append(names, "Box."+name)
				source += " " + unit + "\n"
			}
			source += "}\n"
			opts := options()
			opts.ChunkTokens = 600
			result, err := Process(t.Context(), archive(t, member{"box." + ext, source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, result, source, opts)
			merged := false
			for _, c := range result.Chunks {
				merged = merged || len(c.Symbols) > 1
				for _, name := range c.Symbols {
					if strings.Contains(name, "hidden") {
						t.Fatal(name)
					}
				}
			}
			if !merged {
				t.Fatal("callable members were not merged")
			}
			for i, name := range names {
				found := 0
				for _, c := range result.Chunks {
					if slices.Contains(c.Symbols, name) {
						found++
						if !strings.Contains(c.Text, units[i]) {
							t.Errorf("detached member %s", name)
						}
					}
				}
				if found != 1 {
					t.Errorf("%s in %d chunks", name, found)
				}
			}
		})
	}
}

func TestTopLevelDeclarationsAndObjectMethodsStayVisible(t *testing.T) {
	for _, tc := range []struct {
		language, source string
		names            []string
	}{
		{"javascript", "const f = () => { const hidden = 1; }; const X = 5; export default { handle() { const hidden = 2; } };", []string{"f", "X", "handle"}},
		{"typescript", "const f = () => { const hidden = 1; }; const X = 5; export default { handle() { const hidden = 2; } };", []string{"f", "X", "handle"}},
		{"cpp", "class Box { int handle(int value); int field; };", []string{"Box", "Box.handle"}},
	} {
		parsed, err := analyzeLanguage(t.Context(), tc.language, tc.source, .2)
		if err != nil {
			t.Fatal(err)
		}
		defer parsed.close()
		var names []string
		for _, s := range parsed.symbols {
			names = append(names, s.name)
		}
		if parsed.fallback || !reflect.DeepEqual(names, tc.names) {
			t.Fatalf("%s names=%v", tc.language, names)
		}
	}
}

func TestSyntaxCutRejectsContinuationAndAttachedComments(t *testing.T) {
	source := "def work():\n    value = (\n        1 +\n        2\n    )\n    # preceding comment\n\n    other = 3\n    return value\n"
	parsed, err := analyzeLanguage(t.Context(), "python", source, .2)
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.close()
	lines := lineStarts(source)
	for _, line := range []int{4, 7} {
		if _, ok := parsed.cutDepth(lines, line); ok {
			t.Errorf("line %d must not be a candidate", line+1)
		}
	}
	if _, ok := parsed.cutDepth(lines, 5); !ok {
		t.Fatal("leading comment must be a candidate")
	}
	for _, prefix := range []string{"    call() # trailing comment\n", "    # leading comment\n", "    # leading comment\n\n", "    '''not a comment'''\n"} {
		text := "def work():\n" + prefix + "    other = 3\n"
		p, err := analyzeLanguage(t.Context(), "python", text, .2)
		if err != nil {
			t.Fatal(err)
		}
		starts := lineStarts(text)
		_, ok := p.cutDepth(starts, len(starts)-1)
		p.close()
		if ok == strings.HasPrefix(prefix, "    #") {
			t.Fatalf("comment classification: %q candidate=%t", prefix, ok)
		}
	}
}

func TestOversizedPythonStatementsKeepLeadingComments(t *testing.T) {
	source := "def work():\n"
	var units []string
	for i := 0; i < 12; i++ {
		unit := fmt.Sprintf("    # Explain value %d.\n    value%d = compute(\n        %d,\n        %d,\n    )\n", i, i, i, i+1)
		units = append(units, unit)
		source += unit
	}
	opts := options()
	opts.ChunkTokens = 240
	result, err := Process(t.Context(), archive(t, member{"a.py", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessNonblankChunks(t, result, source, opts)
	for _, unit := range units {
		if !slices.ContainsFunc(result.Chunks, func(c Chunk) bool { return strings.Contains(c.Text, unit) }) {
			t.Fatalf("statement/comment split: %s", unit)
		}
	}
}

func TestSyntaxCutWindowDepthAndLatestTie(t *testing.T) {
	source := "package p\nfunc Work() {\n\tprintln(0)\n\tprintln(1)\n\tprintln(2)\n\tprintln(3)\n\tswitch mode {\n\tcase 1:\n\t\tprintln(4)\n\t}\n}\n"
	parsed, err := analyzeLanguage(t.Context(), "go", source, .2)
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.close()
	starts := lineStarts(source)
	var parts []byteSpan
	for i, start := range starts {
		end := len(source)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		parts = append(parts, byteSpan{start, end})
	}
	policy := DefaultPolicy()
	opts := options()
	opts.Policy = &policy
	b := chunkBuilder{ctx: t.Context(), opts: opts, file: SourceFile{Path: "a.go", Text: source}, lines: starts, result: &Result{}, syntax: &parsed}
	for _, tc := range []struct{ at, best, want int }{{1, 8, 6}, {2, 5, 5}} {
		got, err := b.syntaxCut(parts, tc.at, tc.best, 1)
		if err != nil || got != tc.want || got < tc.at+(tc.best-tc.at+1)/2 {
			t.Fatalf("window %+v chose %d: %v", tc, got, err)
		}
	}
	switchDepth, _ := parsed.cutDepth(starts, 6)
	caseDepth, _ := parsed.cutDepth(starts, 7)
	if caseDepth <= switchDepth {
		t.Fatalf("AST nesting collapsed to indentation: switch=%d case=%d", switchDepth, caseDepth)
	}
}

func TestSyntaxCutNoCandidateRetainsGreedyPrefix(t *testing.T) {
	source := "package p\nfunc Work() {\n/*\n" + strings.Repeat("plain comment continuation text\n", 25) + "*/\n}\n"
	parsed, err := analyzeLanguage(t.Context(), "go", source, .2)
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.close()
	policy := DefaultPolicy()
	opts := options()
	opts.Policy, opts.ChunkTokens = &policy, 180
	build := func(syn *syntax) []sourcePart {
		b := chunkBuilder{ctx: t.Context(), opts: opts, file: SourceFile{Path: "a.go", Text: source}, lines: lineStarts(source), result: &Result{}, syntax: syn}
		if err := b.splitLines(byteSpan{0, len(source)}); err != nil {
			t.Fatal(err)
		}
		return b.source
	}
	greedy := build(nil)
	if got := build(&parsed); !reflect.DeepEqual(got, greedy) {
		t.Fatalf("no-candidate changed greedy boundaries: %v vs %v", got, greedy)
	}
}

func TestFallbackAndVueNeverRetainBoundaryTree(t *testing.T) {
	for _, tc := range []struct {
		path, language, source string
		fallback               bool
	}{
		{"bad.py", "python", strings.Repeat("def ( @@@@\n\\\\\\\n", 20), true},
		{"a.vue", "vue", "<template><p>text</p></template>\n<script>\nexport default { handle() {\n" + strings.Repeat(" console.log(1);\n", 30) + "} };\n</script>\n", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			parsed, err := analyze(t.Context(), SourceFile{Path: tc.path, Language: tc.language, Text: tc.source}, .2)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.close()
			if parsed.tree != nil || parsed.source != nil || parsed.fallback != tc.fallback {
				t.Fatalf("retained unsafe tree: %+v", parsed)
			}
			opts := options()
			policy := DefaultPolicy()
			opts.Policy, opts.ChunkTokens = &policy, 140
			result, err := Process(t.Context(), archive(t, member{tc.path, tc.source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, result, tc.source, opts)
			plain := Result{}
			b := chunkBuilder{ctx: t.Context(), opts: opts, file: SourceFile{Path: tc.path, Language: tc.language, Text: tc.source}, lines: lineStarts(tc.source), result: &plain, symbols: parsed.symbols}
			if err := b.structure(byteSpan{0, len(tc.source)}, parsed.spans, false); err != nil {
				t.Fatal(err)
			}
			if err := b.finishSource(parsed.spans); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.Chunks, plain.Chunks) {
				t.Fatal("fallback/Vue did not use line-only splitting")
			}
		})
	}
}

func TestSyntaxTreeOwnershipAndHeaderSelection(t *testing.T) {
	for _, tc := range []struct {
		source, language string
		fallback         bool
	}{
		{"int class;\nint template;\nint namespace;\n", "c", false},
		{"template<typename T> class Box { T value; };\n", "cpp", false},
		{"@@@\n@@@\n", "cpp", true},
	} {
		parsed, err := analyze(t.Context(), SourceFile{Path: "a.h", Language: "cpp", Text: tc.source}, .2)
		if err != nil {
			t.Fatal(err)
		}
		defer parsed.close()
		if parsed.language != tc.language || parsed.fallback != tc.fallback || (parsed.tree == nil) != tc.fallback {
			t.Fatalf("wrong header selection: %+v", parsed)
		}
		if parsed.tree != nil && int(parsed.tree.RootNode().EndByte()) != len(tc.source) {
			t.Fatal("tree lost its source")
		}
		parsed.close()
		parsed.close()
		if parsed.tree != nil || parsed.source != nil {
			t.Fatal("close retained native ownership")
		}
	}
	parsed, err := analyzeLanguage(t.Context(), "python", "pass\n@@@\n", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.close()
	if parsed.tree == nil || len(parsed.errorLines) == 0 {
		t.Fatal("fixture needs a retained tree with errors")
	}
	parsed.applyErrorRatio("pass\n@@@\n", 0)
	if !parsed.fallback || parsed.tree != nil || parsed.source != nil {
		t.Fatal("fallback retained tree")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	failed, err := analyzeLanguage(ctx, "go", "package p\n", .2)
	if !errors.Is(err, context.Canceled) || failed.tree != nil {
		t.Fatalf("cancelled analysis retained a tree: %v", err)
	}
}

func TestBoundaryCancellationReturnsNoPartialArchive(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := options()
	opts.ChunkTokens = 140
	opts.FitsEmbedding = func(s string) bool {
		if strings.Contains(s, "b.go") {
			cancel()
		}
		return true
	}
	source := "package p\nfunc Work() {\n" + strings.Repeat(" println(1)\n", 60) + "}\n"
	result, err := Process(ctx, archive(t, member{"a.go", "package p\n", 0}, member{"b.go", source, 0}), opts)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("partial archive or wrong error: %+v %v", result, err)
	}
}
