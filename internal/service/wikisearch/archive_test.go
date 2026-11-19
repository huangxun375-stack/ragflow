package wikisearch

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/service/archivepublication"
)

func wikiArchiveDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&entity.Document{}, &entity.IngestionTask{}, &entity.DocumentCodeArchive{}); err != nil {
		t.Fatal(err)
	}
	prior := dao.DB
	dao.DB = db
	t.Cleanup(func() { dao.DB = prior; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	return db
}

func TestArchiveWikiBackfillRejectsStaleAndDeletedRows(t *testing.T) {
	db := wikiArchiveDB(t)
	for _, row := range []any{
		&entity.Document{ID: "zip", KbID: "kb", ParserID: "code_archive", ParserConfig: entity.JSONMap{}},
		&entity.IngestionTask{ID: "task", DocumentID: "zip", DatasetID: "kb", PipelineLogID: new("run"), Status: common.COMPLETED},
		&entity.DocumentCodeArchive{DocID: "zip", KBID: "kb", TaskID: "task", RunID: "run", AttemptID: "attempt", Status: "ready"},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	row := map[string]any{"id": "chunk", "doc_id": "zip", "kb_id": "kb", "content_with_weight": "source", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt", "source_path_kwd": "a.go"}
	eng := &fakeDocEngine{chunks: map[string]any{"chunk": row}}
	svc := NewEngineService(eng)
	if got, err := svc.BackfillChunks(t.Context(), "tenant", []string{"kb"}, []string{"chunk"}); err != nil || len(got) != 1 || got[0]["source_path"] != "a.go" {
		t.Fatalf("%v %v", got, err)
	}
	row["archive_attempt_kwd"] = "old"
	if _, err := svc.BackfillChunks(t.Context(), "tenant", []string{"kb"}, []string{"chunk"}); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatal(err)
	}
	row["archive_attempt_kwd"] = "attempt"
	if err := db.Where("id = ?", "zip").Delete(&entity.Document{}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BackfillChunks(t.Context(), "tenant", []string{"kb"}, []string{"chunk"}); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatal(err)
	}
}
