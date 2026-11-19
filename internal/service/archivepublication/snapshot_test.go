package archivepublication

import (
	"errors"
	"testing"

	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/parser/codearchive"
)

func testSnapshot() archiveartifact.Snapshot {
	return archiveartifact.Snapshot{Config: codearchive.Config{ChunkTokens: 512}, Policy: codearchive.DefaultPolicy(),
		Source:    archiveartifact.Source{Filename: "src.zip", Bucket: "kb", Path: "original", Hash: "abcd", Size: 100},
		Embedding: archiveartifact.Embedding{Reference: "model", Fingerprint: "identity"}}
}

func TestSnapshotIsImmutableAcrossRetryAndPolicyChanges(t *testing.T) {
	db, scope := publicationDB(t)
	if err := db.AutoMigrate(&entity.ArchiveRun{}); err != nil {
		t.Fatal(err)
	}
	want := testSnapshot()
	if err := SaveSnapshot(t.Context(), db, scope, want); err != nil {
		t.Fatal(err)
	}
	changed := testSnapshot()
	changed.Config.ChunkTokens = 1024
	changed.Policy.Limits.MaxChunks = 10
	changed.Source.Path = "replacement"
	changed.Embedding.Reference = "new-model"
	if err := SaveSnapshot(t.Context(), db, scope, changed); !errors.Is(err, ErrSnapshotExists) {
		t.Fatalf("snapshot overwritten: %v", err)
	}
	got, err := LoadSnapshot(t.Context(), db, scope)
	if err != nil {
		t.Fatal(err)
	}
	if got.Config != want.Config || got.Source != want.Source || got.Embedding != want.Embedding || got.Policy.Limits != want.Policy.Limits {
		t.Fatalf("snapshot drifted: %+v", got)
	}
	got.Policy.IgnoredDirs[0] = "mutated"
	again, err := LoadSnapshot(t.Context(), db, scope)
	if err != nil || again.Policy.IgnoredDirs[0] == "mutated" {
		t.Fatalf("snapshot aliases caller: %+v %v", again, err)
	}
}

func TestSnapshotRejectsInvalidContentAndWrongScope(t *testing.T) {
	db, scope := publicationDB(t)
	if err := db.AutoMigrate(&entity.ArchiveRun{}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSnapshot(t.Context(), db, scope, testSnapshot()); err != nil {
		t.Fatal(err)
	}
	wrong := scope
	wrong.DocID = "other"
	if _, err := LoadSnapshot(t.Context(), db, wrong); err == nil {
		t.Fatal("foreign run accepted")
	}
	snapshot := testSnapshot()
	snapshot.Source.Hash = ""
	if err := snapshot.Validate(); codearchive.Reason(err) != "ZIP_SNAPSHOT_INVALID" {
		t.Fatalf("invalid snapshot accepted: %v", err)
	}
}
