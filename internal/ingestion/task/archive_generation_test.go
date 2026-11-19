package task

import (
	"ragflow/internal/ingestion/archiveartifact"
	"testing"
)

func TestArchiveGenerationIDsSeparateAttemptsAndRelinkParents(t *testing.T) {
	chunks := []map[string]any{{"id": "child", "mom_id": "parent"}, {"id": "parent"}}
	first := archiveartifact.Scope{DocID: "doc", RunID: "run", AttemptID: "first"}
	stampArchiveGeneration(chunks, first)
	if chunks[0]["mom_id"] != chunks[1]["id"] {
		t.Fatal("parent reference was not remapped")
	}
	second := []map[string]any{{"id": "child", "mom_id": "parent"}, {"id": "parent"}}
	first.AttemptID = "second"
	stampArchiveGeneration(second, first)
	if chunks[0]["id"] == second[0]["id"] || second[0]["archive_attempt_kwd"] != "second" {
		t.Fatalf("attempt collision: %+v %+v", chunks, second)
	}
}
