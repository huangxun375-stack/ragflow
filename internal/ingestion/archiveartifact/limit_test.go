package archiveartifact

import (
	"ragflow/internal/parser/codearchive"
	"strings"
	"testing"
)

func TestCollectorRejectsOversizedTreeBeforePublication(t *testing.T) {
	collector := &Collector{}
	err := collector.Set(Artifact{RootName: "snapshot", Stats: codearchive.Stats{AcceptedFiles: 1}, Tree: []codearchive.TreeNode{{Name: "file.py", Path: strings.Repeat("x", maxArtifactBytes), Type: "file"}}}, maxArtifactBytes)
	if codearchive.Reason(err) != "ZIP_ARTIFACT_LIMIT_EXCEEDED" {
		t.Fatalf("oversized artifact reason: %v", err)
	}
	if _, err = collector.Take(); err == nil {
		t.Fatal("rejected artifact remained publishable")
	}
}
