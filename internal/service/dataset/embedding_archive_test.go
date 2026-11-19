package dataset

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	enginetypes "ragflow/internal/engine/types"
	"ragflow/internal/entity"
	"ragflow/internal/service"
	"ragflow/internal/service/archivepublication"
)

type archiveEmbeddingEngine struct {
	fakeChatDocEngine
	row       map[string]any
	requests  []*enginetypes.SearchRequest
	getCalls  int
	beforeGet func(int)
}

func (e *archiveEmbeddingEngine) Search(_ context.Context, req *enginetypes.SearchRequest) (*enginetypes.SearchResult, error) {
	e.requests = append(e.requests, req)
	return &enginetypes.SearchResult{Total: 1, Chunks: []map[string]any{e.row}}, nil
}
func (e *archiveEmbeddingEngine) GetChunk(context.Context, string, string, []string) (any, error) {
	e.getCalls++
	if e.beforeGet != nil {
		e.beforeGet(e.getCalls)
	}
	return e.row, nil
}

func archiveEmbeddingFixture(t *testing.T) *archiveEmbeddingEngine {
	t.Helper()
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	insertCreateDatasetTenant(t, "user-1")
	insertDatasetMetadataConfigKB(t, "kb-1", "user-1")
	for _, row := range []any{&entity.Document{ID: "doc-1", KbID: "kb-1", ParserID: "code_archive", ParserConfig: entity.JSONMap{}, CreatedBy: "user-1"}, &entity.IngestionTask{ID: "task", DocumentID: "doc-1", DatasetID: "kb-1", Status: common.COMPLETED, PipelineLogID: new("run")}, &entity.DocumentCodeArchive{DocID: "doc-1", KBID: "kb-1", TaskID: "task", RunID: "run", AttemptID: "attempt", Status: "ready"}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return &archiveEmbeddingEngine{row: map[string]any{"id": "chunk", "doc_id": "doc-1", "kb_id": "kb-1", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt", "content_ltks": "archive code", "q_2_vec": []float64{1, 0}}}
}

func TestArchiveEmbeddingSamplingFiltersCountsAndRejectsStaleRows(t *testing.T) {
	eng := archiveEmbeddingFixture(t)
	svc := &DatasetService{docEngine: eng}
	samples, err := svc.sampleRandomChunksWithVectors(t.Context(), "user-1", "kb-1", 1)
	if err != nil || len(samples) != 1 {
		t.Fatalf("samples=%v err=%v", samples, err)
	}
	if len(eng.requests) != 2 {
		t.Fatalf("searches=%d", len(eng.requests))
	}
	for _, req := range eng.requests {
		if req.Filter["and"] == nil {
			t.Fatalf("unfiltered count/sample: %+v", req)
		}
	}
	eng.row["archive_attempt_kwd"] = "old"
	if _, err := svc.sampleRandomChunksWithVectors(t.Context(), "user-1", "kb-1", 1); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatalf("stale sample accepted: %v", err)
	}
}

func TestArchiveEmbeddingCheckRechecksBeforeModelCall(t *testing.T) {
	eng := archiveEmbeddingFixture(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,0]}],"usage":{"total_tokens":1}}`)
	}))
	defer provider.Close()
	for _, row := range []any{&entity.TenantModelProvider{ID: "provider-embedding", TenantID: "user-1", UserID: "user-1", ProviderName: "OpenAI"}, &entity.TenantModelInstance{ID: "instance-embedding", ProviderID: "provider-embedding", InstanceName: "archive-test", APIKey: "test", Status: "active", Extra: fmt.Sprintf(`{"base_url":%q}`, provider.URL+"/v1")}, &entity.TenantModel{ID: "model-embedding", ProviderID: "provider-embedding", InstanceID: "instance-embedding", ModelName: "embedding-test", ModelType: int(entity.ModelTypeEmbedding), Status: "active", Extra: "{}"}} {
		if err := dao.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	eng.beforeGet = func(call int) {
		if call == 2 {
			if err := dao.DB.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ?", "doc-1").Update("status", "pending").Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	svc := &DatasetService{kbDAO: dao.NewKnowledgebaseDAO(), docEngine: eng}
	_, _, err := svc.CheckEmbedding(withDatasetTestContext(t.Context(), "user-1", "user-1"), "kb-1", &service.CheckEmbeddingRequest{EmbeddingID: "embedding-test@archive-test@OpenAI", CheckNum: new(1)})
	if !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatalf("recheck error=%v", err)
	}
	if eng.getCalls != 2 || calls.Load() != 0 {
		t.Fatalf("getCalls=%d modelCalls=%d", eng.getCalls, calls.Load())
	}
}
