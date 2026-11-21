package codearchive

import (
	"strings"
	"testing"

	"ragflow/internal/tokenizer"
)

func TestOverviewSignaturesExcludeNestedImplementations(t *testing.T) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, source string }{
		{"cpp_member", "source.cpp", `class Box { int (*callback)(int) = [](int x) { const char* text = "BODY_MARKER"; return x; }; int run(){return 1;} };`},
		{"cpp_mixed", "source.cpp", `int make(), state=[](){const char* text="BODY_MARKER";return 1;}(); void other();`},
		{"cpp_global_pointer", "source.cpp", `int (*callback)(int)=[](int x){const char* text="BODY_MARKER";return x;}; void other();`},
		{"cpp_default_argument", "source.cpp", `int call(int (*callback)(int)=[](int x){int BODY_MARKER=x;return x;}); void other();`},
		{"cpp_inline_return_type", "source.cpp", `struct Item { int run(){return BODY_MARKER;} } *load(void); void other();`},
		{"javascript_default_argument", "source.js", `export function run(callback=()=>{return BODY_MARKER;}) { return callback(); } function other(){}`},
		{"typescript_default_argument", "source.ts", `export function run(callback=()=>{return BODY_MARKER;}) { return callback(); } function other(){}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options()
			opts.Overview = true
			opts.CountTokens = tokenizer.NumTokensFromString
			result, err := Process(t.Context(), archive(t, member{tc.path, tc.source, 0}), opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Stats.SyntaxFallbacks != 0 {
				t.Fatal("valid fixture fell back")
			}
			code := Result{}
			overviews := 0
			for _, c := range result.Chunks {
				_, body, _ := strings.Cut(c.Text, "\n")
				if strings.HasPrefix(body, "概览 ") {
					overviews++
					if strings.Contains(body, "BODY_MARKER") {
						t.Errorf("implementation appeared in overview: %s", body)
					}
					continue
				}
				c.Order = len(code.Chunks)
				code.Chunks = append(code.Chunks, c)
			}
			if overviews == 0 {
				t.Fatal("overview was not generated")
			}
			assertLosslessNonblankChunks(t, code, tc.source, opts)
		})
	}
}
