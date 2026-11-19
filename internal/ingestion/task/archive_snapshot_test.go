package task

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/zeebo/xxh3"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/ingestion/component"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/permission"
	"ragflow/internal/service"
	"ragflow/internal/service/archivepublication"
	"ragflow/internal/storage"
)

func archiveSnapshotExecutor(t *testing.T) (*PipelineExecutor, context.Context, storage.Storage) {
	t.Helper()
	db, scope := archiveIndexFixture(t)
	if err := db.AutoMigrate(&entity.UserTenant{}, &entity.TenantModelProvider{}, &entity.TenantModelInstance{}, &entity.TenantModel{}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&entity.UserTenant{ID: "owner", UserID: "user", TenantID: "tenant", Role: "owner", Status: new("1")},
		&entity.TenantModelProvider{ID: "provider", UserID: "user", TenantID: "tenant", ProviderName: "OpenAI"},
		&entity.TenantModelInstance{ID: "instance", ProviderID: "provider", InstanceName: "default", APIKey: "credential", Status: "active", Extra: `{"base_url":"https://example.com/v1"}`},
		&entity.TenantModel{ID: "model", ProviderID: "provider", InstanceID: "instance", ModelName: "text-embedding-3-small", ModelType: int(entity.ModelTypeEmbedding), Status: "active", Extra: `{"max_tokens":2048}`},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&entity.Knowledgebase{}).Where("id = ?", scope.KBID).Update("embd_id", "model").Error; err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	f, err := w.Create("src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("package example\nfunc FrozenSource() string { return \"snapshot\" }\nfunc FrozenVersion() int { return 4 }\n")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	backend := storage.NewMemoryStorage()
	if err = backend.Put(t.Context(), "original-bucket", "original.zip", archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	prior := storage.GetStorageFactory().GetStorage()
	storage.GetStorageFactory().SetStorage(backend)
	t.Cleanup(func() { storage.GetStorageFactory().SetStorage(prior) })
	_, identity, err := service.ArchiveEmbeddingModel(permission.WithTenantID(t.Context(), "tenant"), db, "model")
	if err != nil {
		t.Fatal(err)
	}
	hash := xxh3.Hash128(archive.Bytes()).Bytes()
	snapshot := archiveartifact.Snapshot{Config: codearchive.Config{ChunkTokens: 512, Overview: true}, Policy: codearchive.DefaultPolicy(),
		Source: archiveartifact.Source{Filename: "frozen.zip", Bucket: "original-bucket", Path: "original.zip", Hash: hex.EncodeToString(hash[:]), Size: int64(archive.Len())}, Embedding: identity}
	if err = archivepublication.SaveSnapshot(t.Context(), db, scope, snapshot); err != nil {
		t.Fatal(err)
	}
	executor := &PipelineExecutor{deps: &ingestion.Deps{DB: db}, taskCtx: &TaskContext{
		Doc: entity.Document{ID: scope.DocID, KbID: scope.KBID, ParserID: codearchive.ParserID, Name: new("renamed.zip"), Location: new("replacement.zip"), ContentHash: new("changed"), Size: 1, ParserConfig: entity.JSONMap{"CodeArchive:Archive": map[string]any{"chunk_token_num": 1}}},
		KB:  entity.Knowledgebase{ID: scope.KBID, TenantID: "tenant", EmbdID: "model"}, Tenant: entity.Tenant{ID: "tenant"},
	}}
	return executor, archiveartifact.WithScope(t.Context(), scope), backend
}

func TestArchiveWorkerUsesFrozenSourceAndConfig(t *testing.T) {
	executor, ctx, backend := archiveSnapshotExecutor(t)
	ctx, collector := archiveartifact.WithCollector(ctx)
	prepared, err := executor.prepareCodeArchive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if *executor.taskCtx.Doc.Name != "frozen.zip" {
		t.Fatal("current document name replaced snapshot name")
	}
	component, err := component.NewCodeArchiveComponent(map[string]any{"chunk_token_num": 1, "code_file_overview": false})
	if err != nil {
		t.Fatal(err)
	}
	output, err := component.Invoke(prepared, executor.deps.DB, map[string]any{"doc_id": "doc"})
	if err != nil {
		t.Fatal(err)
	}
	chunks, ok := output["chunks"].([]map[string]any)
	if !ok || len(chunks) < 2 {
		t.Fatalf("snapshot overview ignored: %#v", output)
	}
	text, _ := chunks[0]["text"].(string)
	_, body, _ := strings.Cut(text, "\n")
	if !strings.HasPrefix(body, "概览 ") {
		t.Fatalf("snapshot overview ignored: %#v", output)
	}
	artifact, err := collector.Take()
	if err != nil || artifact.RootName != "src" || artifact.Stats.AcceptedFiles != 1 {
		t.Fatalf("wrong source artifact: %+v %v", artifact, err)
	}
	if err = backend.Put(t.Context(), "original-bucket", "original.zip", []byte("modified source")); err != nil {
		t.Fatal(err)
	}
	prepared, _ = archiveartifact.WithCollector(prepared)
	if _, err = component.Invoke(prepared, executor.deps.DB, map[string]any{"doc_id": "doc"}); codearchive.Reason(err) != "ZIP_SOURCE_CHANGED" {
		t.Fatalf("changed original accepted: %v", err)
	}
}

func TestArchiveWorkerRejectsChangedEmbeddingBeforeSourceRead(t *testing.T) {
	executor, ctx, backend := archiveSnapshotExecutor(t)
	if err := backend.Remove(t.Context(), "original-bucket", "original.zip"); err != nil {
		t.Fatal(err)
	}
	executor.taskCtx.KB.EmbdID = "replacement"
	if _, err := executor.prepareCodeArchive(ctx); codearchive.Reason(err) != "ZIP_EMBEDDING_CHANGED" {
		t.Fatalf("new KB model adopted: %v", err)
	}
	executor.taskCtx.KB.EmbdID = "model"
	if err := executor.deps.DB.Model(&entity.TenantModel{}).Where("id = ?", "model").Update("extra", `{"max_tokens":1024}`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := executor.prepareCodeArchive(ctx); codearchive.Reason(err) != "ZIP_EMBEDDING_CHANGED" {
		t.Fatalf("model parameter drift adopted: %v", err)
	}
}
