package codearchive

import (
	"slices"
	"strings"
	"testing"

	"ragflow/internal/tokenizer"
)

func TestClassExecutionScopesPreserveBlocksAndHideLocals(t *testing.T) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, head, block, tail string }{
		{"java_static", "Box.java", "class Box {", "static {\nint hidden=1;\n}", "int run(){return 1;}\n}"},
		{"java_instance", "Box.java", "class Box {", "{\nint hidden=1;\n}", "int run(){return 1;}\n}"},
		{"java_compact_constructor", "Box.java", "record Box(int value) {", "Box {\nint hidden=value;\n}", "int run(){return value;}\n}"},
		{"javascript_static", "box.js", "class Box {", "static {\nconst hidden=1;\n}", "run(){return 1;}\n}"},
		{"typescript_static", "box.ts", "class Box {", "static {\nconst hidden=1;\n}", "run(): number {return 1;}\n}"},
		{"tsx_static", "box.tsx", "class Box {", "static {\nconst hidden=1;\n}", "run(){return <div/>;}\n}"},
	} {
		for _, overview := range []bool{false, true} {
			name := tc.name
			if overview {
				name += "_overview"
			}
			t.Run(name, func(t *testing.T) {
				const doc = "// Execution documentation.\n"
				source := tc.head + "\n// " + strings.Repeat("padding ", 700) + "\n\n" + doc + tc.block + "\n" + tc.tail + "\n"
				opts := options()
				opts.CountTokens = tokenizer.NumTokensFromString
				opts.Overview = overview
				result, err := Process(t.Context(), archive(t, member{tc.path, source, 0}), opts)
				if err != nil {
					t.Fatal(err)
				}
				if result.Stats.SyntaxFallbacks != 0 {
					t.Fatal("valid execution scope fell back")
				}
				code := Result{}
				found := false
				for _, c := range result.Chunks {
					_, body, _ := strings.Cut(c.Text, "\n")
					if slices.Contains(c.Symbols, "Box.hidden") || strings.Contains(body, "Box.hidden:") {
						t.Errorf("local was exported as a class member: %s", c.Text)
					}
					if strings.HasPrefix(body, "概览 ") {
						continue
					}
					found = found || strings.Contains(body, doc+tc.block)
					c.Order = len(code.Chunks)
					code.Chunks = append(code.Chunks, c)
				}
				if !found {
					t.Fatal("fitting execution scope or its documentation was split")
				}
				assertLosslessNonblankChunks(t, code, source, opts)
			})
		}
	}
}

func TestDeclarationFormsPreserveNamesAndWrappers(t *testing.T) {
	for _, tc := range []struct {
		name, language, source string
		names                  []string
	}{
		{"typescript_ambient", "typescript", "// Work documentation.\nexport declare function work(value: string): number;\n", []string{"work"}},
		{"cpp_alias", "cpp", "namespace sample { using Value = int; }\n", []string{"sample", "sample.Value"}},
		{"go_interface_alias", "go", "package p\ntype Reader = interface { Read(p []byte) (int,error) }\n", []string{"Reader", "Reader.Read"}},
		{"java_annotation", "java", "public @interface Marker { String value() default \"x\"; }\n", []string{"Marker", "Marker.value"}},
		{"java_compact_constructor", "java", "record Box(int value) { Box { int hidden=value; } }", []string{"Box", "Box.Box"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := analyzeLanguage(t.Context(), tc.language, tc.source, .2)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.close()
			if parsed.fallback || parsed.tree.RootNode().HasError() {
				t.Fatal("invalid or unsupported grammar fixture")
			}
			var names []string
			for _, s := range parsed.symbols {
				names = append(names, s.name)
				if tc.language == "typescript" && (tc.source[s.start:s.end] != strings.TrimSuffix(tc.source, "\n") || s.doc != "Work documentation.") {
					t.Errorf("ambient declaration wrapper or documentation missing: %+v", s)
				}
			}
			if !slices.Equal(names, tc.names) {
				t.Errorf("names=%v want=%v", names, tc.names)
			}
		})
	}
}
