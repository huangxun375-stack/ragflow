package task

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"ragflow/internal/common"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/service/archivepublication"
)

func archiveIndexFixture(t *testing.T) (*gorm.DB, archiveartifact.Scope) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if err = db.AutoMigrate(&entity.Document{}, &entity.Knowledgebase{}, &entity.IngestionTask{}, &entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}, &entity.ArchiveDatasetFence{}); err != nil {
		t.Fatal(err)
	}
	scope := archiveartifact.Scope{DocID: "doc", KBID: "kb", TaskID: "task", RunID: "run"}
	for _, row := range []any{&entity.Document{ID: "doc", KbID: "kb", ParserID: codearchive.ParserID, ParserConfig: entity.JSONMap{}}, &entity.Knowledgebase{ID: "kb", TenantID: "tenant", Name: "kb"}, &entity.IngestionTask{ID: "task", UserID: "user", DocumentID: "doc", DatasetID: "kb", PipelineLogID: &scope.RunID, Status: common.RUNNING}} {
		if err = db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err = archivepublication.Queue(t.Context(), db, scope); err != nil {
		t.Fatal(err)
	}
	scope, err = archivepublication.Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return db, scope
}

func TestArchiveBatchLateAfterDatasetDeleteIsCompensated(t *testing.T) {
	db, scope := archiveIndexFixture(t)
	ctx := archiveartifact.WithScope(t.Context(), scope)
	checked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var mu sync.Mutex
	index := map[string]bool{}
	insert := guardArchiveInsert(func(context.Context, []map[string]any, string, string) ([]string, error) {
		close(checked)
		<-release
		mu.Lock()
		index[scope.AttemptID] = true
		mu.Unlock()
		return []string{"chunk"}, nil
	}, db, scope)
	writer := newChunkIndexWriter(insert, "ragflow_tenant", scope.KBID, 1)
	executor := &PipelineExecutor{deps: &ingestion.Deps{DB: db}, indexWriter: writer, deleteChunksFunc: func(_ context.Context, condition map[string]any, _ string, _ string) (int64, error) {
		if condition["doc_id"] != scope.DocID || condition["archive_run_kwd"] != scope.RunID || condition["archive_attempt_kwd"] != scope.AttemptID {
			return 0, fmt.Errorf("unscoped compensation: %#v", condition)
		}
		mu.Lock()
		delete(index, scope.AttemptID)
		mu.Unlock()
		return 1, nil
	}}
	go func() {
		err := writer.Write(ctx, []map[string]any{{"id": "chunk"}})
		done <- errors.Join(err, executor.CompensateArchive(ctx, nil))
	}()
	<-checked
	if err := archivepublication.RevokeDataset(t.Context(), db, scope.KBID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clear(index)
	index["other-attempt"] = true
	mu.Unlock()
	if err := db.Delete(&entity.Document{}, "id = ?", scope.DocID).Error; err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, archivepublication.ErrSuperseded) {
		t.Fatalf("late batch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if index[scope.AttemptID] || !index["other-attempt"] {
		t.Fatalf("compensation changed wrong generation: %+v", index)
	}
}

func TestArchiveCompensationPreservesScopeTimeoutAndCancellation(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, stopped := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch=%t/stopped=%t", batch, stopped), func(t *testing.T) {
				db, scope := archiveIndexFixture(t)
				cause := context.Canceled
				wantTaskStatus := common.RUNNING
				if stopped {
					wantTaskStatus = common.STOPPING
					cause = errors.New("index failed after user stop")
					if err := db.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Update("status", wantTaskStatus).Error; err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(archiveartifact.WithScope(t.Context(), scope))
				cancel()
				wantCondition := map[string]any{"doc_id": scope.DocID, "kb_id": scope.KBID, "archive_run_kwd": scope.RunID, "archive_attempt_kwd": scope.AttemptID}
				timeout := 10 * time.Second
				if batch {
					delete(wantCondition, "doc_id")
					wantCondition["id"] = []string{"child", "parent"}
					timeout = 5 * time.Second
				}
				deleteErr := errors.New("external deletion failed")
				started := time.Now()
				calls := 0
				executor := &PipelineExecutor{
					taskCtx: &TaskContext{Doc: entity.Document{ID: scope.DocID, KbID: scope.KBID}},
					deps:    &ingestion.Deps{DB: db}, indexWriter: newChunkIndexWriter(nil, "ragflow_tenant", scope.KBID, 1),
					deleteChunksFunc: func(cleanupCtx context.Context, condition map[string]any, baseName, kbID string) (int64, error) {
						calls++
						if cleanupCtx.Err() != nil || cleanupCtx.Done() == nil {
							t.Fatalf("cleanup context not detached and bounded: %v", cleanupCtx.Err())
						}
						deadline, ok := cleanupCtx.Deadline()
						if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > timeout || deadline.Before(started.Add(timeout)) {
							t.Fatalf("cleanup deadline remaining=%s, want (0, %s]", remaining, timeout)
						}
						if got, ok := archiveartifact.ScopeFrom(cleanupCtx); !ok || got != scope {
							t.Fatalf("lost context scope: %+v", got)
						}
						if !reflect.DeepEqual(condition, wantCondition) || baseName != "ragflow_tenant" || kbID != scope.KBID {
							t.Fatalf("delete scope: %#v %s %s", condition, baseName, kbID)
						}
						if err := archivepublication.CheckActive(cleanupCtx, db, scope); !errors.Is(err, archivepublication.ErrSuperseded) {
							t.Fatalf("attempt not revoked before deletion: %v", err)
						}
						return 0, deleteErr
					},
				}
				var err error
				if batch {
					err = executor.compensateFailedIndexWrite(ctx, []map[string]any{{"id": "child"}, {"id": "child"}, {"id": "parent"}, {"id": ""}, {"id": 1}}, cause)
				} else {
					err = executor.CompensateArchive(ctx, cause)
				}
				if !errors.Is(err, deleteErr) || calls != 1 {
					t.Fatalf("cleanup error=%v calls=%d", err, calls)
				}
				var task entity.IngestionTask
				if err := db.Take(&task, "id = ?", scope.TaskID).Error; err != nil || task.Status != wantTaskStatus {
					t.Fatalf("cleanup settled caller-owned terminal state: %+v %v", task, err)
				}
				var row entity.DocumentCodeArchive
				if err := db.Take(&row, "doc_id = ?", scope.DocID).Error; err != nil || row.Status != "pending" || row.LeaseUntil != 0 || row.FailureReason != "" {
					t.Fatalf("cleanup did not release attempt: %+v %v", row, err)
				}
			})
		}
	}
}

func TestIndexCompensationPreservesOrdinaryRowsAndNoopGuards(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	executor := &PipelineExecutor{
		taskCtx:     &TaskContext{Doc: entity.Document{KbID: "ordinary-kb"}},
		indexWriter: newChunkIndexWriter(nil, "ragflow_tenant", "ordinary-kb", 1),
		deleteChunksFunc: func(cleanupCtx context.Context, condition map[string]any, baseName, kbID string) (int64, error) {
			calls++
			want := map[string]any{"id": []string{"child", "parent"}, "kb_id": "ordinary-kb"}
			if cleanupCtx.Err() != nil || !reflect.DeepEqual(condition, want) || baseName != "ragflow_tenant" || kbID != "ordinary-kb" {
				t.Fatalf("ordinary compensation changed: %#v %s %s %v", condition, baseName, kbID, cleanupCtx.Err())
			}
			return 2, nil
		},
	}
	if err := executor.CompensateArchive(ctx, nil); err != nil || calls != 0 {
		t.Fatalf("ordinary archive cleanup: %v calls=%d", err, calls)
	}
	archiveCtx := archiveartifact.WithScope(ctx, archiveartifact.Scope{DocID: "doc", KBID: "kb", TaskID: "task", RunID: "run", AttemptID: "attempt"})
	if err := executor.compensateFailedIndexWrite(archiveCtx, []map[string]any{{"id": ""}, {"id": 1}}, nil); err != nil || calls != 0 {
		t.Fatalf("empty-ID cleanup reached database or delete: %v calls=%d", err, calls)
	}
	if err := executor.compensateFailedIndexWrite(ctx, []map[string]any{{"id": "child"}, {"id": "child"}, {"id": "parent"}}, nil); err != nil || calls != 1 {
		t.Fatalf("ordinary cleanup: %v calls=%d", err, calls)
	}
	executor.deleteChunksFunc = nil
	if err := executor.CompensateArchive(archiveCtx, nil); err != nil {
		t.Fatal(err)
	}
	if err := executor.compensateFailedIndexWrite(archiveCtx, []map[string]any{{"id": "child"}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveCompensationDoesNotDeleteAmbiguousPublishedCommit(t *testing.T) {
	db, scope := archiveIndexFixture(t)
	ctx := archiveartifact.WithScope(t.Context(), scope)
	artifact := archiveartifact.Artifact{RootName: "src", Tree: []codearchive.TreeNode{{Name: "a.go", Path: "a.go", Type: "file"}}, Stats: codearchive.Stats{AcceptedFiles: 1}}
	if err := archivepublication.Publish(ctx, db, scope, artifact, archivepublication.Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	deleted := false
	executor := &PipelineExecutor{taskCtx: &TaskContext{Doc: entity.Document{ID: scope.DocID, KbID: scope.KBID}}, deps: &ingestion.Deps{DB: db}, indexWriter: newChunkIndexWriter(nil, "ragflow_tenant", scope.KBID, 1), deleteChunksFunc: func(context.Context, map[string]any, string, string) (int64, error) { deleted = true; return 1, nil }}
	if err := executor.CompensateArchive(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := executor.compensateFailedIndexWrite(ctx, []map[string]any{{"id": "chunk"}}, nil); err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("published generation deleted after lost commit acknowledgement")
	}
}

func TestArchiveCompensationFailsClosedWhenCommitStateCannotBeRead(t *testing.T) {
	db, scope := archiveIndexFixture(t)
	ctx := archiveartifact.WithScope(t.Context(), scope)
	sqlDB, _ := db.DB()
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	deleted := false
	executor := &PipelineExecutor{taskCtx: &TaskContext{Doc: entity.Document{ID: scope.DocID, KbID: scope.KBID}}, deps: &ingestion.Deps{DB: db}, indexWriter: newChunkIndexWriter(nil, "ragflow_tenant", scope.KBID, 1), deleteChunksFunc: func(context.Context, map[string]any, string, string) (int64, error) { deleted = true; return 1, nil }}
	if err := executor.CompensateArchive(ctx, nil); err == nil {
		t.Fatal("unknown commit state accepted")
	}
	if err := executor.compensateFailedIndexWrite(ctx, []map[string]any{{"id": "chunk"}}, nil); err == nil {
		t.Fatal("unknown commit state accepted for batch cleanup")
	}
	if deleted {
		t.Fatal("deleted an attempt whose commit state was unavailable")
	}
}
