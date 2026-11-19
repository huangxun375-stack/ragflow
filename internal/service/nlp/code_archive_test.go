package nlp

import (
	"errors"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/service/archivepublication"
	"testing"
)

func TestArchiveSearchFiltersAndRejectsStaleGeneration(t *testing.T) {
	db := newRetrievalTestDB(t, "doc-archive")
	if err := db.AutoMigrate(&entity.IngestionTask{}, &entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&entity.Document{}).Where("id = ?", "doc-archive").Update("parser_id", "code_archive").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&entity.IngestionTask{ID: "task", DocumentID: "doc-archive", DatasetID: "kb-1", Status: common.COMPLETED, PipelineLogID: new("run")}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&entity.DocumentCodeArchive{DocID: "doc-archive", KBID: "kb-1", TaskID: "task", RunID: "run", AttemptID: "attempt", Status: "ready"}).Error; err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"id": "chunk", "doc_id": "doc-archive", "kb_id": "kb-1", "content_with_weight": "code", "source_path_kwd": "src/a###b.ts", "start_line_int": 2, "end_line_int": 6, "code_lang_kwd": "typescript", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt"}
	eng := &retrievalCountEngine{rows: []map[string]any{row}}
	svc := NewRetrievalService(eng, dao.NewDocumentDAO())
	result, err := svc.Search(t.Context(), &RetrievalSearchRequest{KbIDs: []string{"kb-1"}, TenantIDs: []string{"tenant"}, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if eng.searchFilters[0]["and"] == nil || result.Field["chunk"]["source_path_kwd"] != "src/a###b.ts" {
		t.Fatalf("query/projection lost publication: %+v %+v", eng.searchFilters, result.Field)
	}
	row["archive_attempt_kwd"] = "old"
	if _, err := svc.Search(t.Context(), &RetrievalSearchRequest{KbIDs: []string{"kb-1"}, TenantIDs: []string{"tenant"}, Page: 1, PageSize: 10}); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatalf("stale search: %v", err)
	}
}

func TestArchiveSearchDatabaseFailureDoesNotQueryIndex(t *testing.T) {
	old := dao.DB
	dao.DB = nil
	t.Cleanup(func() { dao.DB = old })
	eng := &retrievalCountEngine{}
	if _, err := NewRetrievalService(eng, nil).Search(t.Context(), &RetrievalSearchRequest{KbIDs: []string{"kb"}}); err == nil {
		t.Fatal("missing DB accepted")
	}
	if len(eng.searchFilters) != 0 {
		t.Fatal("index queried after failed visibility lookup")
	}
}
