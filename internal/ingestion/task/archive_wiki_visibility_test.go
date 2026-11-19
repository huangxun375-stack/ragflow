package task

import (
	"context"
	"errors"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/engine"
	"ragflow/internal/engine/types"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion/knowledge_compile"
	"ragflow/internal/service/archivepublication"
)

type archiveWikiSourceEngine struct {
	engine.DocEngine
	row     map[string]any
	request *types.SearchRequest
}

func (e *archiveWikiSourceEngine) Search(_ context.Context, req *types.SearchRequest) (*types.SearchResult, error) {
	e.request = req
	return &types.SearchResult{Chunks: []map[string]any{e.row}, Total: 1}, nil
}

func TestArchiveDirtyWikiSourceVisibility(t *testing.T) {
	db, scope := archiveIndexFixture(t)
	if err := db.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Update("status", common.COMPLETED).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ?", scope.DocID).Update("status", "ready").Error; err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"id": "chunk", "doc_id": scope.DocID, "kb_id": scope.KBID, "content_with_weight": "code", "archive_run_kwd": scope.RunID, "archive_attempt_kwd": scope.AttemptID}
	eng := &archiveWikiSourceEngine{row: row}
	prior := kcWiringDeps
	InitKnowledgeCompilerDeps(db, eng)
	t.Cleanup(func() { kcWiringDeps = prior })
	req := knowledge_compile.WikiDirtyRequest{TenantID: "tenant", DatasetID: scope.KBID, DocumentID: scope.DocID}
	if got, err := loadActiveSourceChunks(t.Context(), req); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if eng.request.Filter["and"] == nil {
		t.Fatal("missing prefilter")
	}
	row["archive_attempt_kwd"] = "old"
	if _, err := loadActiveSourceChunks(t.Context(), req); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatal(err)
	}
	if err := db.Where("id = ?", scope.DocID).Delete(&entity.Document{}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := loadActiveSourceChunks(t.Context(), req); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatal(err)
	}
}
