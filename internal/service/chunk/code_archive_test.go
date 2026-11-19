package chunk

import (
	"context"
	"errors"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/engine/types"
	"ragflow/internal/entity"
	"ragflow/internal/service"
	"ragflow/internal/service/archivepublication"
	"reflect"
	"strings"
	"testing"
)

func archiveChunkFixture(t *testing.T) map[string]any {
	t.Helper()
	db := setupChunkTestDB(t)
	pushChunkTestDB(t, db)
	if err := db.AutoMigrate(&entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}); err != nil {
		t.Fatal(err)
	}
	insertChunkTestUserTenant(t, "user-1", "tenant-1")
	insertChunkTestKB(t, "kb-1", "tenant-1")
	insertChunkTestDoc(t, "doc-1", "kb-1")
	if err := db.Model(&entity.Document{}).Where("id = ?", "doc-1").Updates(map[string]any{"parser_id": "code_archive", "type": "code"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&entity.IngestionTask{ID: "task-1", DocumentID: "doc-1", DatasetID: "kb-1", Status: common.COMPLETED, PipelineLogID: new("run")}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&entity.DocumentCodeArchive{DocID: "doc-1", KBID: "kb-1", TaskID: "task-1", RunID: "run", AttemptID: "attempt", Status: "ready"}).Error; err != nil {
		t.Fatal(err)
	}
	return map[string]any{"id": "chunk-1", "doc_id": "doc-1", "kb_id": "kb-1", "content_with_weight": "code", "source_path_kwd": "src/a###b.ts", "start_line_int": 4, "end_line_int": 9, "code_lang_kwd": "typescript", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt"}
}

func TestArchiveVectorHydrationWithoutKBUsesTenantScope(t *testing.T) {
	row := archiveChunkFixture(t)
	row["q_2_vec"] = []float64{.1, .2}
	eng := &mockVectorFetcher{engineType: "elasticsearch", searchResults: map[string]*types.SearchResult{"ragflow_tenant-1": {Chunks: []map[string]any{row}}}, filterCapture: map[string]any{}}
	got := FetchChunkVectors(t.Context(), eng, []string{"chunk-1"}, []string{"tenant-1"}, nil, 2)
	if len(got["chunk-1"]) != 2 || got["chunk-1"][0] != .1 || eng.filterCapture["and"] == nil {
		t.Fatalf("%v %v", got, eng.filterCapture)
	}
	row["archive_attempt_kwd"] = "old"
	if got := FetchChunkVectors(t.Context(), eng, []string{"chunk-1"}, []string{"tenant-1"}, nil, 2); len(got) != 0 {
		t.Fatal("stale vector", got)
	}
}

func TestArchiveChunkListFiltersBeforePaginationAndMapsSource(t *testing.T) {
	row := archiveChunkFixture(t)
	eng := &listChunksSearchEngine{chunks: []map[string]any{row}}
	svc := &ChunkService{docEngine: eng, kbDAO: dao.NewKnowledgebaseDAO(), documentDAO: dao.NewDocumentDAO()}
	response, err := svc.List(chunkTestContext(t, "user-1", "tenant-1"), &service.ListChunksRequest{DatasetID: "kb-1", DocID: "doc-1", Page: new(2), Size: new(10)})
	if err != nil {
		t.Fatal(err)
	}
	if eng.searchReq.Offset != 10 || eng.searchReq.Filter["and"] == nil {
		t.Fatalf("unfiltered pagination: %+v", eng.searchReq)
	}
	if len(response.Chunks) != 1 || response.Chunks[0]["source_path"] != "src/a###b.ts" || response.Chunks[0]["archive_run_id"] != "run" || response.Doc["archive_status"] != "ready" {
		t.Fatalf("response=%+v", response)
	}
	if _, ok := response.Chunks[0]["archive_attempt_kwd"]; ok {
		t.Fatal("internal attempt leaked")
	}
}

type archiveRaceEngine struct {
	listChunksSearchEngine
	afterSearch func()
}

func (e *archiveRaceEngine) Search(ctx context.Context, req *types.SearchRequest) (*types.SearchResult, error) {
	result, err := e.listChunksSearchEngine.Search(ctx, req)
	e.afterSearch()
	return result, err
}

func TestArchiveChunkListRejectsPublicationRace(t *testing.T) {
	row := archiveChunkFixture(t)
	eng := &archiveRaceEngine{listChunksSearchEngine: listChunksSearchEngine{chunks: []map[string]any{row}}, afterSearch: func() {
		dao.DB.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ?", "doc-1").Update("status", "pending")
	}}
	svc := &ChunkService{docEngine: eng, kbDAO: dao.NewKnowledgebaseDAO()}
	if _, err := svc.List(chunkTestContext(t, "user-1", "tenant-1"), &service.ListChunksRequest{DatasetID: "kb-1", DocID: "doc-1"}); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatalf("race returned content: %v", err)
	}
}

func TestArchiveChunkGetAndWritesAreGuarded(t *testing.T) {
	row := archiveChunkFixture(t)
	eng := &getChunkTestEngine{chunk: row}
	svc := &ChunkService{docEngine: eng, kbDAO: dao.NewKnowledgebaseDAO(), documentDAO: dao.NewDocumentDAO()}
	ctx := chunkTestContext(t, "user-1", "tenant-1")
	got, err := svc.Get(ctx, &service.GetChunkRequest{DatasetID: "kb-1", DocumentID: "doc-1", ChunkID: "chunk-1"})
	if err != nil || got.Chunk["source_path"] != "src/a###b.ts" {
		t.Fatalf("get=%+v err=%v", got, err)
	}
	row["archive_attempt_kwd"] = "stale"
	if _, err := svc.Get(ctx, &service.GetChunkRequest{DatasetID: "kb-1", DocumentID: "doc-1", ChunkID: "chunk-1"}); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatalf("stale get: %v", err)
	}
	if _, err := svc.AddChunk(ctx, &service.AddChunkRequest{DatasetID: "kb-1", DocumentID: "doc-1", Content: "manual"}); err == nil || !strings.Contains(err.Error(), "ZIP_CHUNKS_READ_ONLY") {
		t.Fatalf("add=%v", err)
	}
	if err := svc.UpdateChunk(ctx, &service.UpdateChunkRequest{DatasetID: "kb-1", DocumentID: "doc-1", ChunkID: "chunk-1"}); err == nil || !strings.Contains(err.Error(), "ZIP_CHUNKS_READ_ONLY") {
		t.Fatalf("edit=%v", err)
	}
	if _, err := svc.RemoveChunks(ctx, &service.RemoveChunksRequest{DatasetID: "kb-1", DocID: "doc-1", DeleteAll: true}); err == nil || !strings.Contains(err.Error(), "ZIP_CHUNKS_READ_ONLY") {
		t.Fatalf("delete=%v", err)
	}
}

// Engine single-chunk updates accept only flat equality keys; the published
// generation must be expressed that way rather than as a visibility tree.
func TestArchiveSwitchChunksTargetsPublishedGenerationWithFlatCondition(t *testing.T) {
	archiveChunkFixture(t)
	eng := &switchChunksEngineMock{}
	svc := &ChunkService{docEngine: eng, kbDAO: dao.NewKnowledgebaseDAO()}
	ctx := chunkTestContext(t, "user-1", "tenant-1")
	if err := svc.SwitchChunks(ctx, "kb-1", "doc-1", 0, []string{"chunk-1"}); err != nil {
		t.Fatal(err)
	}
	if len(eng.updateCalls) != 1 || !reflect.DeepEqual(eng.updateCalls[0].condition, map[string]interface{}{
		"id": "chunk-1", "doc_id": "doc-1", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt",
	}) {
		t.Fatalf("calls=%#v", eng.updateCalls)
	}
	if err := dao.DB.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ?", "doc-1").Update("status", "pending").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.SwitchChunks(ctx, "kb-1", "doc-1", 1, []string{"chunk-1"}); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatalf("unpublished switch: %v", err)
	}
	if len(eng.updateCalls) != 1 {
		t.Fatal("unpublished archive reached the engine")
	}
}
