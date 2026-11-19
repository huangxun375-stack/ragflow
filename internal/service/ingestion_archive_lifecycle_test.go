package service

import (
	"context"
	"errors"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/service/archivepublication"
	"strings"
	"testing"
	"time"
)

func TestArchiveRequeuePreservesEnqueuedSourceConfigAndModel(t *testing.T) {
	svc, task := archiveTaskFixture(t)
	svc.SetTaskPublisher(&recordingTaskPublisher{})
	created, err := svc.CreateAndEnqueue(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	scope := archiveartifact.Scope{DocID: created.DocumentID, KBID: created.DatasetID, TaskID: created.ID, RunID: *created.PipelineLogID}
	before, err := archivepublication.LoadSnapshot(t.Context(), dao.DB, scope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(before.Embedding.Parameters, "credential-not-in-snapshot") {
		t.Fatal("snapshot leaked API key")
	}
	if err = dao.DB.Model(&entity.Document{}).Where("id = ?", scope.DocID).Updates(map[string]any{"location": "new.zip", "size": 200, "content_hash": "newhash", "parser_config": entity.JSONMap{"CodeArchive:Archive": map[string]any{"chunk_token_num": 128}}}).Error; err != nil {
		t.Fatal(err)
	}
	if err = dao.DB.Model(&entity.Knowledgebase{}).Where("id = ?", scope.KBID).Update("embd_id", "other-model").Error; err != nil {
		t.Fatal(err)
	}
	var doc entity.Document
	if err = dao.DB.Take(&doc, "id = ?", scope.DocID).Error; err != nil {
		t.Fatal(err)
	}
	if err = queueArchiveSnapshot(t.Context(), dao.DB, scope, &doc); err != nil {
		t.Fatal(err)
	}
	after, err := archivepublication.LoadSnapshot(t.Context(), dao.DB, scope)
	if err != nil {
		t.Fatal(err)
	}
	if after.Config != before.Config || after.Source != before.Source || after.Embedding != before.Embedding {
		t.Fatalf("retry adopted current mutable inputs: before=%+v after=%+v", before, after)
	}
}

func archiveTaskFixture(t *testing.T) (*IngestionTaskService, *entity.IngestionTask) {
	t.Helper()
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	if err := db.AutoMigrate(&entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}, &entity.ArchiveDatasetFence{}, &entity.TenantModelProvider{}, &entity.TenantModelInstance{}, &entity.TenantModel{}); err != nil {
		t.Fatal(err)
	}
	insertTestKB(t, "kb-archive", "tenant", 1, 0, 0)
	insertTestDoc(t, "doc-archive", "kb-archive", 0, 0)
	for _, row := range []any{
		&entity.UserTenant{ID: "archive-owner", UserID: "user", TenantID: "tenant", Role: "owner", Status: new("1")},
		&entity.TenantModelProvider{ID: "archive-provider", UserID: "user", TenantID: "tenant", ProviderName: "OpenAI"},
		&entity.TenantModelInstance{ID: "archive-instance", ProviderID: "archive-provider", InstanceName: "default", APIKey: "credential-not-in-snapshot", Status: "active", Extra: `{"base_url":"https://example.com/v1"}`},
		&entity.TenantModel{ID: "archive-model", ProviderID: "archive-provider", InstanceID: "archive-instance", ModelName: "text-embedding-3-small", ModelType: int(entity.ModelTypeEmbedding), Status: "active", Extra: `{"max_tokens":2048}`},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&entity.Knowledgebase{}).Where("id = ?", "kb-archive").Update("embd_id", "archive-model").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&entity.Document{}).Where("id = ?", "doc-archive").Updates(map[string]any{"location": "source.zip", "size": 100, "content_hash": "sourcehash"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&entity.Document{}).Where("id = ?", "doc-archive").Updates(map[string]any{"name": "snapshot.zip", "suffix": "zip", "parser_id": codearchive.ParserID, "parser_config": entity.JSONMap{"CodeArchive:Archive": map[string]any{"chunk_token_num": 512, "code_file_overview": false}}}).Error; err != nil {
		t.Fatal(err)
	}
	return NewIngestionTaskService(), &entity.IngestionTask{DocumentID: "doc-archive", DatasetID: "kb-archive", UserID: "user"}
}

func TestArchiveRunIsBoundBeforeQueueAndPendingStopIsInvisible(t *testing.T) {
	svc, task := archiveTaskFixture(t)
	svc.SetTaskPublisher(&recordingTaskPublisher{beforeReturn: func(id string) {
		var current entity.IngestionTask
		if err := dao.DB.Take(&current, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		var pub entity.DocumentCodeArchive
		if err := dao.DB.Take(&pub, "doc_id = ?", task.DocumentID).Error; err != nil {
			t.Fatal(err)
		}
		if current.PipelineLogID == nil || pub.RunID != *current.PipelineLogID || pub.Status != "pending" {
			t.Fatalf("unbound queue publication: %+v %+v", current, pub)
		}
	}})
	created, err := svc.CreateAndEnqueue(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestStop(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := archivepublication.Metadata(t.Context(), dao.DB, []string{task.DocumentID})
	if err != nil || rows[task.DocumentID].Status != "cancelled" {
		t.Fatal(rows, err)
	}
	if _, err = archivepublication.Ready(t.Context(), dao.DB, task.DatasetID, task.DocumentID); !errors.Is(err, archivepublication.ErrNotReady) {
		t.Fatal(err)
	}
}

func TestAmbiguousArchiveEnqueueFailureCannotFailClaimedAttempt(t *testing.T) {
	svc, task := archiveTaskFixture(t)
	var claimed archiveartifact.Scope
	svc.SetTaskPublisher(&recordingTaskPublisher{err: errors.New("ack lost"), beforeReturn: func(id string) {
		running, err := svc.TransitionTaskToRunning(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err = archivepublication.Acquire(t.Context(), dao.DB, archiveartifact.Scope{DocID: running.DocumentID, KBID: running.DatasetID, TaskID: id, RunID: *running.PipelineLogID}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
	}})
	if _, err := svc.CreateAndEnqueue(t.Context(), task); err == nil {
		t.Fatal("expected queue acknowledgement error")
	}
	var current entity.IngestionTask
	if err := dao.DB.Take(&current, "id = ?", claimed.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Status != common.RUNNING {
		t.Fatalf("claimed task regressed: %s", current.Status)
	}
	if err := archivepublication.CheckActive(t.Context(), dao.DB, claimed); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveCancellationCompensationPreservesSettlement(t *testing.T) {
	for _, userStop := range []bool{true, false} {
		name := "shutdown"
		if userStop {
			name = "user_stop"
		}
		t.Run(name, func(t *testing.T) {
			svc, task := archiveTaskFixture(t)
			svc.SetTaskPublisher(&recordingTaskPublisher{})
			created, err := svc.CreateAndEnqueue(t.Context(), task)
			if err != nil {
				t.Fatal(err)
			}
			running, err := svc.TransitionTaskToRunning(t.Context(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := archivepublication.Acquire(t.Context(), dao.DB, archiveartifact.Scope{DocID: running.DocumentID, KBID: running.DatasetID, TaskID: running.ID, RunID: *running.PipelineLogID}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if userStop {
				if _, err = svc.RequestStop(t.Context(), created.ID); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(archiveartifact.WithScope(t.Context(), scope))
			cancel()
			detached := context.WithoutCancel(ctx)
			for range 2 {
				allowed, err := archivepublication.CanCompensate(detached, dao.DB, scope, ctx.Err())
				if err != nil || !allowed {
					t.Fatalf("cancel compensation: allowed=%t err=%v", allowed, err)
				}
			}
			if err = archivepublication.CheckActive(t.Context(), dao.DB, scope); !errors.Is(err, archivepublication.ErrSuperseded) {
				t.Fatalf("cancelled attempt can still write: %v", err)
			}
			if err = archivepublication.Renew(t.Context(), dao.DB, scope); !errors.Is(err, archivepublication.ErrSuperseded) {
				t.Fatalf("cancelled attempt can renew: %v", err)
			}
			if userStop {
				if err = svc.MarkStopped(detached, created.ID); err != nil {
					t.Fatalf("cannot settle user stop: %v", err)
				}
			} else {
				current, err := svc.GetTask(t.Context(), created.ID)
				if err != nil || current.Status != common.RUNNING {
					t.Fatalf("shutdown must remain redeliverable: task=%+v err=%v", current, err)
				}
				replacement, err := archivepublication.Acquire(t.Context(), dao.DB, scope, time.Minute)
				if err != nil || replacement.AttemptID == scope.AttemptID {
					t.Fatalf("shutdown takeover: scope=%+v err=%v", replacement, err)
				}
				if allowed, err := archivepublication.CanCompensate(detached, dao.DB, scope, ctx.Err()); err != nil || !allowed {
					t.Fatalf("stale cancellation cleanup: %t %v", allowed, err)
				}
				if err = archivepublication.CheckActive(t.Context(), dao.DB, replacement); err != nil {
					t.Fatalf("old cleanup invalidated replacement: %v", err)
				}
			}
			metadata, err := archivepublication.Metadata(t.Context(), dao.DB, []string{scope.DocID})
			if err != nil || metadata[scope.DocID].FailureReason != "" {
				t.Fatalf("cancellation recorded failure: %+v err=%v", metadata, err)
			}
			current, err := svc.GetTask(t.Context(), created.ID)
			if err != nil || userStop && current.Status != common.STOPPED {
				t.Fatalf("user stop final status: %+v err=%v", current, err)
			}
		})
	}
}

func TestArchiveSharedQueueRejectsCustomPipeline(t *testing.T) {
	svc, task := archiveTaskFixture(t)
	publisher := &recordingTaskPublisher{}
	svc.SetTaskPublisher(publisher)
	if err := dao.DB.Model(&entity.Document{}).Where("id = ?", task.DocumentID).Update("pipeline_id", "custom").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAndEnqueue(t.Context(), task); err == nil {
		t.Fatal("custom ZIP pipeline queued")
	}
	if len(publisher.messages) != 0 {
		t.Fatal("invalid task reached broker")
	}
}
