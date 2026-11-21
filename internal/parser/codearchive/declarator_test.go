package codearchive

import (
	"slices"
	"strings"
	"testing"

	"ragflow/internal/tokenizer"
)

func TestCDeclaratorStatementRanges(t *testing.T) {
	for _, tc := range []struct {
		name, language, declaration string
		names                       []string
	}{
		{"c_pointer", "c", "const char *load(void);", []string{"load"}},
		{"c_double_pointer", "c", "int **lookup(int id);", []string{"lookup"}},
		{"c_parenthesized", "c", "int *(load)(void);", []string{"load"}},
		{"c_function_pointer", "c", "int (*callback)(int value);", []string{"callback"}},
		{"c_function_pointer_array", "c", "int (*callbacks[2])(int value);", []string{"callbacks"}},
		{"c_function_pointer_return", "c", "int (*factory(void))(double);", []string{"factory"}},
		{"c_attributed", "c", "int *load(void) __attribute__((warn_unused_result));", []string{"load"}},
		{"c_parameter", "c", "void install(int (*callback)(int));", []string{"install"}},
		{"c_typedef", "c", "typedef int (*Callback)(int value);", []string{"Callback"}},
		{"c_multiple", "c", "int *first(void), *second(void);", []string{"first", "second"}},
		{"cpp_pointer", "cpp", "Item* load();", []string{"load"}},
		{"cpp_reference", "cpp", "Item& load();", []string{"load"}},
		{"cpp_rvalue_reference", "cpp", "Item&& load();", []string{"load"}},
		{"cpp_parenthesized", "cpp", "Item* (load)();", []string{"load"}},
		{"cpp_function_pointer", "cpp", "int (*callback)(int value);", []string{"callback"}},
		{"cpp_attributed", "cpp", "[[nodiscard]] Item* load();", []string{"load"}},
		{"cpp_default_argument", "cpp", "Item* load(int value = compute());", []string{"load"}},
		{"cpp_multiple", "cpp", "Item *first(), *second();", []string{"first", "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const doc = "// Declaration documentation.\n"
			source := "int before;\n\n" + doc + tc.declaration + "\n\nint after;\n"
			parsed, err := analyzeLanguage(t.Context(), tc.language, source, .2)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.close()
			if parsed.fallback {
				t.Fatal("fixture fell back to line splitting")
			}
			var names []string
			for _, s := range parsed.symbols {
				names = append(names, s.name)
				if got := source[s.start:s.end]; got != doc+tc.declaration {
					t.Errorf("%s range = %q, want the documented declaration", s.name, got)
				}
				if s.signature != tc.declaration {
					t.Errorf("%s signature = %q, want %q", s.name, s.signature, tc.declaration)
				}
				if s.doc != "Declaration documentation." {
					t.Errorf("%s documentation = %q", s.name, s.doc)
				}
			}
			if !slices.Equal(names, tc.names) {
				t.Errorf("symbols = %v, want %v", names, tc.names)
			}
		})
	}
}

func TestCppMemberDeclaratorRanges(t *testing.T) {
	for _, declaration := range []string{
		"Item* load();", "virtual Item* load() const = 0;", "Item& load();",
		"Item&& load();", "int (*load)(int argument);",
	} {
		t.Run(declaration, func(t *testing.T) {
			const doc = "// Load documentation.\n"
			source := "class Holder {\n" + doc + declaration + "\nint after;\n};\n"
			parsed, err := analyzeLanguage(t.Context(), "cpp", source, .2)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.close()
			if parsed.fallback {
				t.Fatal("fixture fell back to line splitting")
			}
			var names []string
			for _, s := range parsed.symbols {
				names = append(names, s.name)
				if s.name != "Holder.load" {
					continue
				}
				if source[s.start:s.end] != doc+declaration || s.signature != declaration || s.doc != "Load documentation." {
					t.Errorf("incomplete member: range=%q signature=%q doc=%q", source[s.start:s.end], s.signature, s.doc)
				}
			}
			if !slices.Equal(names, []string{"Holder", "Holder.load"}) {
				t.Errorf("unexpected members: %v", names)
			}
		})
	}
}

func TestCDeclaratorInlineReturnType(t *testing.T) {
	for _, language := range []string{"c", "cpp"} {
		t.Run(language, func(t *testing.T) {
			const declaration = "struct Item { int value; } *load(void);"
			parsed, err := analyzeLanguage(t.Context(), language, declaration, .2)
			if err != nil {
				t.Fatal(err)
			}
			defer parsed.close()
			found := false
			for _, s := range parsed.symbols {
				if s.name == "load" {
					found = true
					if s.signature != declaration {
						t.Errorf("return type body obscured the callable signature: %q", s.signature)
					}
				}
			}
			if !found || parsed.fallback {
				t.Fatal("function prototype not recognized")
			}
		})
	}
}

func TestCppDeclaratorChunksPreserveCompleteStatements(t *testing.T) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	counter, err := tokenizer.LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, declaration string }{
		{"pointer", "Item* load();"},
		{"double_pointer", "Item** load();"},
		{"reference", "Item& load();"},
		{"rvalue_reference", "Item&& load();"},
		{"parenthesized", "int* (load)();"},
		{"macro", "API Item* load();"},
	} {
		for _, overview := range []bool{false, true} {
			name := tc.name
			if overview {
				name += "_overview"
			}
			t.Run(name, func(t *testing.T) {
				const doc = "// Load documentation.\n"
				source := "#define API\nnamespace sample {\nclass Item;\n// " + strings.Repeat("padding ", 700) +
					"\n\n" + doc + tc.declaration + "\n\n// Other documentation.\nvoid other();\n}\n"
				opts := options()
				opts.Overview = overview
				opts.CountTokens = tokenizer.NumTokensFromString
				opts.FitsEmbedding = func(text string) bool { return counter.Count(text) <= 8192 }
				result, err := Process(t.Context(), archive(t, member{"iterator.hpp", source, 0}), opts)
				if err != nil {
					t.Fatal(err)
				}
				if result.Stats.SyntaxFallbacks != 0 {
					t.Fatal("fixture fell back to line splitting")
				}
				code := Result{}
				found, summarized := false, false
				for _, c := range result.Chunks {
					_, body, _ := strings.Cut(c.Text, "\n")
					if opts.CountTokens(c.Text) > opts.ChunkTokens || !opts.FitsEmbedding(c.Text) {
						t.Fatal("chunk exceeds the complete-text budget")
					}
					if strings.HasPrefix(body, "概览 ") {
						summarized = summarized || strings.Contains(body, "sample.load: "+tc.declaration+" — Load documentation.")
						continue
					}
					c.Order = len(code.Chunks)
					code.Chunks = append(code.Chunks, c)
					if slices.Contains(c.Symbols, "sample.load") {
						found = true
						if !strings.Contains(body, doc+tc.declaration) {
							t.Errorf("short declaration or its documentation was split: %q", body)
						}
					}
				}
				if !found || overview && !summarized {
					t.Errorf("complete declaration found=%t summarized=%t", found, summarized)
				}
				assertLosslessNonblankChunks(t, code, source, opts)
			})
		}
	}
}
