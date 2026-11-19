package agentic_rag

import (
	"context"
	"errors"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"ragflow/internal/agent/runtime"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/engine"
	"ragflow/internal/engine/types"
	"ragflow/internal/entity"
	"ragflow/internal/service/archivepublication"
	"testing"
)

func setupArchiveVisibilityTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&entity.Knowledgebase{}, &entity.Document{}, &entity.IngestionTask{}, &entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}); err != nil {
		t.Fatal(err)
	}
	old := dao.DB
	dao.DB = db
	t.Cleanup(func() { dao.DB = old; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	return db
}

type archivePassageEngine struct {
	engine.DocEngine
	row     map[string]any
	request *types.SearchRequest
}

func (e *archivePassageEngine) Search(_ context.Context, req *types.SearchRequest) (*types.SearchResult, error) {
	e.request = req
	return &types.SearchResult{Chunks: []map[string]any{e.row}, Total: 1}, nil
}

func TestArchiveKGPassagesCheckGeneration(t *testing.T) {
	db := setupArchiveVisibilityTestDB(t)
	for _, row := range []any{&entity.Document{ID: "doc", KbID: "kb", ParserID: "code_archive", ParserConfig: entity.JSONMap{}}, &entity.IngestionTask{ID: "task", DocumentID: "doc", DatasetID: "kb", Status: common.COMPLETED, PipelineLogID: new("run")}, &entity.DocumentCodeArchive{DocID: "doc", KBID: "kb", TaskID: "task", RunID: "run", AttemptID: "attempt", Status: "ready"}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	row := map[string]any{"id": "chunk", "doc_id": "doc", "kb_id": "kb", "content_with_weight": "code", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt"}
	eng := &archivePassageEngine{row: row}
	adapter := NewKgAdapter(eng)
	if got := adapter.loadPassages(t.Context(), "tenant", []string{"kb"}, []string{"chunk"}, 10); len(got) != 1 {
		t.Fatal(got)
	}
	if eng.request.Filter["and"] == nil {
		t.Fatal("missing prefilter", eng.request.Filter)
	}
	row["archive_attempt_kwd"] = "old"
	if got := adapter.loadPassages(t.Context(), "tenant", []string{"kb"}, []string{"chunk"}, 10); len(got) != 0 {
		t.Fatal("stale passage", got)
	}
	if err := db.Where("id = ?", "doc").Delete(&entity.Document{}).Error; err != nil {
		t.Fatal(err)
	}
	if got := adapter.loadPassages(t.Context(), "tenant", []string{"kb"}, []string{"chunk"}, 10); len(got) != 0 {
		t.Fatal("deleted passage", got)
	}
}

func TestArchiveTenantOnlyGrepScope(t *testing.T) {
	db := setupArchiveVisibilityTestDB(t)
	for _, row := range []any{&entity.Knowledgebase{ID: "kb", TenantID: "tenant"}, &entity.Document{ID: "doc", KbID: "kb", ParserID: "code_archive", ParserConfig: entity.JSONMap{}}, &entity.IngestionTask{ID: "task", DocumentID: "doc", DatasetID: "kb", Status: common.COMPLETED, PipelineLogID: new("run")}, &entity.DocumentCodeArchive{DocID: "doc", KBID: "kb", TaskID: "task", RunID: "run", AttemptID: "attempt", Status: "ready"}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	row := map[string]any{"id": "chunk", "doc_id": "doc", "kb_id": "kb", "content_with_weight": "code", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt", "source_path_kwd": "a.go"}
	eng := &grepFakeEngine{regexpChunks: []map[string]any{row}}
	adapter := NewGrepAdapter(eng)
	req := runtime.GrepRequest{TenantID: "tenant", ChunkScope: []string{"chunk"}}
	if got, err := adapter.FetchChunksByID(t.Context(), req); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	row["archive_attempt_kwd"] = "old"
	if _, err := adapter.FetchChunksByID(t.Context(), req); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatal(err)
	}
}

func TestArchiveDeepReadGuardsIndexAndContentPasses(t *testing.T) {
	db := setupArchiveVisibilityTestDB(t)
	for _, row := range []any{&entity.Document{ID: "doc", KbID: "kb", ParserID: "code_archive", ParserConfig: entity.JSONMap{}}, &entity.IngestionTask{ID: "task", DocumentID: "doc", DatasetID: "kb", Status: common.COMPLETED, PipelineLogID: new("run")}, &entity.DocumentCodeArchive{DocID: "doc", KBID: "kb", TaskID: "task", RunID: "run", AttemptID: "attempt", Status: "ready"}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	row := map[string]any{"id": "chunk", "doc_id": "doc", "kb_id": "kb", "content_with_weight": "code", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt", "source_path_kwd": "src/a###b.py", "start_line_int": 1, "end_line_int": 3, "code_lang_kwd": "python"}
	eng := &grepFakeEngine{regexpChunks: []map[string]any{row}}
	adapter := NewGrepAdapter(eng)
	req := runtime.GrepRequest{TenantID: "tenant", DatasetIDs: []string{"kb"}, DocScope: []string{"doc"}, ChunkScope: []string{"chunk"}}
	if chunks, err := adapter.ListDocumentChunkIndex(t.Context(), req); err != nil || len(chunks) != 1 {
		t.Fatalf("index=%v err=%v", chunks, err)
	}
	if chunks, err := adapter.FetchChunksByID(t.Context(), req); err != nil || len(chunks) != 1 || chunks[0].SourcePath != "src/a###b.py" {
		t.Fatalf("content=%v err=%v", chunks, err)
	}
	row["archive_attempt_kwd"] = "stale"
	if _, err := adapter.FetchChunksByID(t.Context(), req); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatalf("stale deep read=%v", err)
	}
}
