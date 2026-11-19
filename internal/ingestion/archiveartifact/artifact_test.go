package archiveartifact

import (
	"encoding/json"
	"ragflow/internal/parser/codearchive"
	"strings"
	"testing"
)

func TestCollectorOwnsSnapshotAndIsSingleUse(t *testing.T) {
	ctx, c := WithCollector(WithScope(t.Context(), Scope{DocID: "doc", RunID: "run"}))
	source := Artifact{RootName: "src", Tree: []codearchive.TreeNode{{Name: "a.go", Path: "a.go", Type: "file"}}, Stats: codearchive.Stats{AcceptedFiles: 1, FilteredEntries: []codearchive.FilteredEntry{{Path: "node_modules", Type: "directory", Reason: "dir", FileCount: 2}}}}
	if err := c.Set(source, maxArtifactBytes); err != nil {
		t.Fatal(err)
	}
	source.Tree[0].Name = "mutated"
	source.Stats.FilteredEntries[0].Path = "mutated"
	if err := c.Set(source, maxArtifactBytes); err == nil {
		t.Fatal("second producer accepted")
	}
	got, err := c.Take()
	if err != nil || got.Tree[0].Name != "a.go" || got.Stats.FilteredEntries[0].Path != "node_modules" {
		t.Fatalf("snapshot: %+v %v", got, err)
	}
	if _, err = c.Take(); err == nil {
		t.Fatal("second consumer accepted")
	}
	if scope, ok := ScopeFrom(ctx); !ok || scope.DocID != "doc" {
		t.Fatal(scope, ok)
	}
	if got, ok := CollectorFrom(ctx); !ok || got != c {
		t.Fatal("collector missing")
	}
}

func TestCollectorTrimsDiagnosticPathsBeforeRejectingArtifact(t *testing.T) {
	base := Artifact{RootName: "src", Tree: []codearchive.TreeNode{{Name: "a.go", Path: "", Type: "file"}}, Stats: codearchive.Stats{TotalFiles: 513, AcceptedFiles: 1, SkippedSuffix: 512}}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Tree[0].Path = strings.Repeat("a", maxArtifactBytes-len(raw)-4096)
	for i := 0; i < 512; i++ {
		base.Stats.FilteredEntries = append(base.Stats.FilteredEntries, codearchive.FilteredEntry{Path: strings.Repeat("x", 100), Type: "file", Reason: "suffix", FileCount: 1})
	}
	c := &Collector{}
	if err := c.Set(base, maxArtifactBytes); err != nil {
		t.Fatal(err)
	}
	if len(c.raw) > maxArtifactBytes {
		t.Fatal("artifact exceeds limit")
	}
	got, err := c.Take()
	if err != nil || !got.Stats.FilteredEntriesTruncated || len(got.Stats.FilteredEntries) >= 512 || got.Stats.SkippedSuffix != 512 || got.Tree[0].Path != base.Tree[0].Path {
		t.Fatalf("diagnostic trimming changed result: entries=%d truncated=%t err=%v", len(got.Stats.FilteredEntries), got.Stats.FilteredEntriesTruncated, err)
	}
	if len(base.Stats.FilteredEntries) != 512 || base.Stats.FilteredEntriesTruncated {
		t.Fatal("collector mutated caller stats")
	}
	base.Tree[0].Path = "a.go"
	limited := &Collector{}
	if err := limited.Set(base, 2048); err != nil {
		t.Fatal(err)
	}
	if len(limited.raw) > 2048 {
		t.Fatal("configured artifact cap ignored")
	}
	got, err = limited.Take()
	if err != nil || !got.Stats.FilteredEntriesTruncated || got.Stats.SkippedSuffix != 512 {
		t.Fatalf("configured cap: %+v %v", got.Stats, err)
	}
}
