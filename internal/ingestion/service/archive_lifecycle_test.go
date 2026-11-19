package service

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"ragflow/internal/entity"
	"ragflow/internal/ingestion"
	"ragflow/internal/ingestion/archiveartifact"
)

// A task whose document is gone must reach the ordinary run path, which marks
// it FAILED; an archive claim error would be nacked until delivery is exhausted.
func TestClaimArchiveAttemptLeavesOrdinaryAndMissingDocumentsUnclaimed(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:test-archive-claim?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&entity.Document{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&entity.Document{ID: "ordinary", KbID: "kb", ParserID: "naive", ParserConfig: entity.JSONMap{}}).Error; err != nil {
		t.Fatal(err)
	}
	e := &Ingestor{deps: &ingestion.Deps{DB: db}}
	for _, doc := range []string{"ordinary", "deleted"} {
		ctx, release, err := e.claimArchiveAttempt(t.Context(), &entity.IngestionTask{ID: "task-" + doc, DocumentID: doc, DatasetID: "kb"})
		if err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		release()
		if _, scoped := archiveartifact.ScopeFrom(ctx); scoped {
			t.Fatalf("%s acquired an archive scope", doc)
		}
	}
}
