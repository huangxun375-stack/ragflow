package codearchive

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestPolicyRejectsRelaxedSafetyCapsAndUnsupportedSelection(t *testing.T) {
	for name, change := range map[string]func(*Policy){
		"archive bytes":             func(p *Policy) { p.Limits.MaxCompressedBytes++ },
		"file count":                func(p *Policy) { p.Limits.MaxFiles++ },
		"depth":                     func(p *Policy) { p.Limits.MaxDepth++ },
		"read bytes":                func(p *Policy) { p.Limits.MaxReadBytes++ },
		"file bytes":                func(p *Policy) { p.Limits.MaxFileBytes++ },
		"ignore bytes":              func(p *Policy) { p.Limits.MaxGitignoreBytes++ },
		"chunks":                    func(p *Policy) { p.Limits.MaxChunks++ },
		"artifact bytes":            func(p *Policy) { p.MaxArtifactBytes++ },
		"zero limit":                func(p *Policy) { p.Limits.MaxDepth = 0 },
		"negative limit":            func(p *Policy) { p.Limits.MaxReadBytes = -1 },
		"zero artifact":             func(p *Policy) { p.MaxArtifactBytes = 0 },
		"zero chunk default":        func(p *Policy) { p.DefaultChunkTokens = 0 },
		"chunk default":             func(p *Policy) { p.DefaultChunkTokens = 2049 },
		"nan target ratio":          func(p *Policy) { p.ChunkTargetRatio = math.NaN() },
		"infinite target ratio":     func(p *Policy) { p.ChunkTargetRatio = math.Inf(1) },
		"negative infinite ratio":   func(p *Policy) { p.ChunkTargetRatio = math.Inf(-1) },
		"large target ratio":        func(p *Policy) { p.ChunkTargetRatio = 1.01 },
		"negative target ratio":     func(p *Policy) { p.ChunkTargetRatio = -.01 },
		"nan threshold":             func(p *Policy) { p.SyntaxErrorRatio = math.NaN() },
		"large threshold":           func(p *Policy) { p.SyntaxErrorRatio = 1.01 },
		"negative threshold":        func(p *Policy) { p.SyntaxErrorRatio = -.01 },
		"ignored directory removed": func(p *Policy) { p.IgnoredDirs = []string{"vendor"} },
		"ignored directory path":    func(p *Policy) { p.IgnoredDirs = append(p.IgnoredDirs, "a/b") },
		"unknown language":          func(p *Policy) { p.Languages = []string{"rust"} },
		"no languages":              func(p *Policy) { p.Languages = nil },
		"unknown extension":         func(p *Policy) { p.Extensions = []string{".txt"} },
		"no extensions":             func(p *Policy) { p.Extensions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			policy := DefaultPolicy()
			change(&policy)
			if err := policy.Validate(); Reason(err) != "ZIP_CONFIG_INVALID" {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestPolicySnapshotRoundTripPreservesRestrictedProcessing(t *testing.T) {
	policy := DefaultPolicy()
	policy.Languages = []string{"python", "go"}
	policy.Extensions = []string{".py"}
	policy.IgnoredDirs = append(policy.IgnoredDirs, "generated")
	policy.RespectGitignore = false
	policy.DefaultChunkTokens = 80
	policy.ChunkTargetRatio = .75
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	var restored Policy
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	opts := options()
	opts.Policy = &restored
	opts.ChunkTokens = 0
	result, err := Process(context.Background(), archive(t,
		member{".gitignore", "*.py\n", 0},
		member{"a.py", strings.Repeat("pass\n", 30), 0},
		member{"a.go", "package p", 0},
		member{"a.java", "class A {}", 0},
		member{"generated/a.py", "pass", 0},
		member{"vendor/a.py", "pass", 0},
	), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stats.AcceptedFiles != 1 || result.Stats.SkippedSuffix != 2 || result.Stats.SkippedDir != 2 || result.Stats.SkippedGitignore != 0 {
		t.Fatalf("stats = %+v", result.Stats)
	}
	if len(result.Chunks) < 2 {
		t.Fatal("configured default chunk size was not applied")
	}
	for _, chunk := range result.Chunks {
		if chunk.SourcePath != "a.py" || len(chunk.Text) > 80 {
			t.Fatalf("unexpected chunk: %+v", chunk)
		}
	}
	if !reflect.DeepEqual(policy, restored) {
		t.Fatal("processing mutated the policy snapshot")
	}
}

func TestConfiguredSyntaxRatioAndUnionOfErrorLines(t *testing.T) {
	source := "pass\npass\npass\npass\n@ @\n"
	for _, tc := range []struct {
		threshold float64
		want      int
	}{{.2, 0}, {.19, 1}} {
		policy := DefaultPolicy()
		policy.SyntaxErrorRatio = tc.threshold
		opts := options()
		opts.Policy = &policy
		blob := archive(t, member{"main.py", source, 0})
		baseline, err := Process(t.Context(), blob, opts)
		if err != nil {
			t.Fatal(err)
		}
		var diagnostics []SyntaxFallback
		opts.OnSyntaxFallback = func(f SyntaxFallback) { diagnostics = append(diagnostics, f) }
		result, err := Process(t.Context(), blob, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result, baseline) {
			t.Fatal("syntax diagnostics changed parser output")
		}
		if result.Stats.SyntaxFallbacks != tc.want || len(diagnostics) != tc.want {
			t.Fatalf("threshold %g: fallback = %d, diagnostics = %v, want %d", tc.threshold, result.Stats.SyntaxFallbacks, diagnostics, tc.want)
		}
		if tc.want != 0 {
			want := SyntaxFallback{Path: "main.py", Language: "python", ErrorLines: 1, TotalLines: 5, Ratio: .2, Threshold: tc.threshold}
			if diagnostics[0] != want {
				t.Fatalf("diagnostic = %+v, want %+v", diagnostics[0], want)
			}
		}
	}
}
