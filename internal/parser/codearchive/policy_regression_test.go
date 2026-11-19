package codearchive

import (
	"context"
	"strings"
	"testing"
)

func TestRootNamePrefersCommonDirectoryAfterMetadataFiltering(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []member
		want  string
	}{
		{"common directory", []member{{"project/src/main.py", "pass", 0}, {"project/README.md", "docs", 0}, {"__MACOSX/._project", "meta", 0}, {".DS_Store", "meta", 0}}, "project"},
		{"top level file", []member{{"project/main.py", "pass", 0}, {"README.md", "docs", 0}}, "snapshot"},
		{"different directories", []member{{"one/main.py", "pass", 0}, {"two/main.py", "pass", 0}}, "snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Process(context.Background(), archive(t, tc.files...), options())
			if err != nil {
				t.Fatal(err)
			}
			if result.RootName != tc.want {
				t.Fatalf("root = %q, want %q", result.RootName, tc.want)
			}
		})
	}
}

func TestOversizedGitignoreIsSkippedWithoutConsumingReadBudget(t *testing.T) {
	opts := options()
	policy := DefaultPolicy()
	opts.Policy = &policy
	policy.Limits.MaxGitignoreBytes = 4
	policy.Limits.MaxReadBytes = 4
	result, err := Process(context.Background(), archive(t, member{".gitignore", "*.py\n", 0}, member{"main.py", "pass", 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stats.AcceptedFiles != 1 || result.Stats.ReadBytes != 4 {
		t.Fatalf("stats = %+v", result.Stats)
	}
}

func TestSyntaxFallbackCountsErrorCoveredLinesInsteadOfBytes(t *testing.T) {
	source := "# " + strings.Repeat("long valid comment ", 150) + "\npass\npass\n@\n@\n"
	parsed, err := analyze(context.Background(), SourceFile{Path: "main.py", Language: "python", Text: source}, .2)
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.close()
	if !parsed.fallback {
		t.Fatal("two malformed lines out of five must fall back despite the long valid comment")
	}
}
