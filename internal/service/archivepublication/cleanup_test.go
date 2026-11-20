package archivepublication

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
)

func TestQueueRetiresOnlyCommittedPreviousPublication(t *testing.T) {
	db, scope := publicationDB(t)
	previous, err := Acquire(t.Context(), db, scope, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, previous, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	replacement := scope
	replacement.RunID = "replacement"
	rollback := errors.New("rollback enqueue")
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Updates(map[string]any{"pipeline_log_id": replacement.RunID, "status": common.RUNNING}).Error; err != nil {
			return err
		}
		if err := Queue(t.Context(), tx, replacement); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var inventory entity.ArchiveAttempt
	if err = db.Take(&inventory, "attempt_id = ?", previous.AttemptID).Error; err != nil || inventory.State != "published" {
		t.Fatalf("rollback retired publication: %+v %v", inventory, err)
	}
	if _, err = Ready(t.Context(), db, scope.KBID, scope.DocID); err != nil {
		t.Fatalf("rollback hid published data: %v", err)
	}
	if err = Queue(t.Context(), db, replacement); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("unbound replacement accepted: %v", err)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Updates(map[string]any{"pipeline_log_id": replacement.RunID, "status": common.RUNNING}).Error; err != nil {
			return err
		}
		return Queue(t.Context(), tx, replacement)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Take(&inventory, "attempt_id = ?", previous.AttemptID).Error; err != nil || inventory.State != "retired" {
		t.Fatalf("replacement retained old publication: %+v %v", inventory, err)
	}
	current, err := Acquire(t.Context(), db, replacement, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 2}); err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&entity.ArchiveAttempt{}).Where("attempt_id = ?", previous.AttemptID).UpdateColumn("updated_at", time.Now().Add(-time.Hour).UnixMilli()).Error; err != nil {
		t.Fatal(err)
	}
	deleted := []archiveartifact.Scope{}
	_, err = Sweep(t.Context(), db, CleanupOptions{Grace: time.Millisecond, BatchSize: 10}, func(_ context.Context, scope archiveartifact.Scope, _ string) error {
		deleted = append(deleted, scope)
		return nil
	})
	if err != nil || len(deleted) != 1 || deleted[0] != previous {
		t.Fatalf("wrong generation reclaimed: %+v %v", deleted, err)
	}
	view, err := Ready(t.Context(), db, scope.KBID, scope.DocID)
	if err != nil || view.AttemptID != current.AttemptID {
		t.Fatalf("new publication damaged: %+v %v", view, err)
	}
}

func TestSweepKeepsDeletedDatasetTombstoneForLateWrites(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	if err = RevokeDataset(t.Context(), db, scope.KBID); err != nil {
		t.Fatal(err)
	}
	if err = WithDatasetCleanup(t.Context(), db, scope.KBID, func(tx *gorm.DB) error {
		if err := DeleteDataset(t.Context(), tx, scope.KBID); err != nil {
			return err
		}
		if err := tx.Where("id = ?", scope.DocID).Delete(&entity.Document{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", scope.KBID).Delete(&entity.Knowledgebase{}).Error
	}); err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&entity.ArchiveAttempt{}).Where("attempt_id = ?", current.AttemptID).UpdateColumn("updated_at", time.Now().Add(-time.Hour).UnixMilli()).Error; err != nil {
		t.Fatal(err)
	}
	calls := 0
	remove := func(_ context.Context, got archiveartifact.Scope, tenant string) error {
		if got != current || tenant != "tenant" {
			t.Fatalf("lost deleted identity: %+v %s", got, tenant)
		}
		calls++
		return nil
	}
	for i := 0; i < 2; i++ {
		if _, err = Sweep(t.Context(), db, CleanupOptions{Grace: time.Millisecond, BatchSize: 10}, remove); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("late writes no longer covered: %d", calls)
	}
	var fences int64
	if err = db.Model(&entity.ArchiveDatasetFence{}).Count(&fences).Error; err != nil || fences != 0 {
		t.Fatalf("cleanup resurrected dataset fence: %d %v", fences, err)
	}
}

func TestSweepSkipsAmbiguousPublishedPointerAndMakesProgress(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).UnixMilli()
	// Even inconsistent inventory cannot authorize deleting the current
	// publication. This is the conservative side of an unknown commit outcome.
	if err = db.Model(&entity.ArchiveAttempt{}).Where("attempt_id = ?", current.AttemptID).Updates(map[string]any{"state": "cleaning", "lease_until": old, "updated_at": old, "last_cleanup_at": -1}).Error; err != nil {
		t.Fatal(err)
	}
	orphan := entity.ArchiveAttempt{AttemptID: "orphan", RunID: "orphan-run", DocID: "gone", KBID: scope.KBID, TaskID: "gone-task", TenantID: "tenant", State: "revoked", UpdatedAt: old, LeaseUntil: old}
	if err = db.Create(&orphan).Error; err != nil {
		t.Fatal(err)
	}
	deleted := []string{}
	remove := func(_ context.Context, got archiveartifact.Scope, _ string) error {
		deleted = append(deleted, got.AttemptID)
		return nil
	}
	for i := 0; i < 2; i++ {
		if _, err = Sweep(t.Context(), db, CleanupOptions{Grace: time.Millisecond, BatchSize: 1}, remove); err != nil {
			t.Fatal(err)
		}
	}
	if len(deleted) != 1 || deleted[0] != "orphan" {
		t.Fatalf("ambiguous candidate starved or deleted: %v", deleted)
	}
}

func TestSweepReclaimsReleasedAttemptWithoutFailingRetryableRun(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := CanCompensate(t.Context(), db, current, context.Canceled); err != nil || !allowed {
		t.Fatalf("release cancellation: %t %v", allowed, err)
	}
	deleted := 0
	for range 2 {
		if _, err := Sweep(t.Context(), db, CleanupOptions{Grace: time.Minute, BatchSize: 10}, func(_ context.Context, got archiveartifact.Scope, _ string) error {
			if got != current {
				t.Fatalf("unexpected generation: %+v", got)
			}
			deleted++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var task entity.IngestionTask
	if err := db.Take(&task, "id = ?", scope.TaskID).Error; err != nil || task.Status != common.RUNNING {
		t.Fatalf("sweep failed retryable task: %+v %v", task, err)
	}
	if deleted != 2 {
		t.Fatalf("released attempt was not revisited: %d", deleted)
	}
	replacement, err := Acquire(t.Context(), db, scope, time.Hour)
	if err != nil || replacement.AttemptID == current.AttemptID {
		t.Fatalf("released run cannot resume: %+v %v", replacement, err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("released worker can publish: %v", err)
	}
	if err = Publish(t.Context(), db, replacement, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestSweepSettlesRequestedStop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		released bool
	}{
		{name: "expired", status: common.STOPPING},
		{name: "released", status: common.STOPPING, released: true},
		{name: "redelivered", status: common.STOPPED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, scope := publicationDB(t)
			if err := db.AutoMigrate(&entity.PipelineOperationLog{}, &entity.IngestionTaskLog{}); err != nil {
				t.Fatal(err)
			}
			log := entity.PipelineOperationLog{ID: scope.RunID, DocumentID: scope.DocID, KbID: scope.KBID, OperationStatus: string(entity.TaskStatusRunning)}
			if err := db.Create(&log).Error; err != nil {
				t.Fatal(err)
			}
			attempt, err := Acquire(ctx, db, scope, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err = taskQuery(db, scope).Update("status", tc.status).Error; err != nil {
				t.Fatal(err)
			}
			if tc.released {
				if allowed, err := CanCompensate(ctx, db, attempt, context.Canceled); err != nil || !allowed {
					t.Fatalf("release attempt: allowed=%t err=%v", allowed, err)
				}
			} else {
				expired := time.Now().Add(-time.Hour).UnixMilli()
				if err = rowQuery(db, attempt).Update("lease_until", expired).Error; err != nil {
					t.Fatal(err)
				}
				if err = attemptQuery(db, attempt).UpdateColumns(map[string]any{"lease_until": expired, "updated_at": expired}).Error; err != nil {
					t.Fatal(err)
				}
			}
			remove := func(_ context.Context, got archiveartifact.Scope, _ string) error {
				if got != attempt {
					t.Fatalf("wrong attempt to clean: %+v", got)
				}
				var task entity.IngestionTask
				if err := taskQuery(db, scope).Take(&task).Error; err != nil || task.Status != common.STOPPED {
					t.Fatalf("task before index cleanup: status=%s err=%v", task.Status, err)
				}
				var publication entity.DocumentCodeArchive
				if err := rowQuery(db, attempt).Take(&publication).Error; err != nil || publication.Status != "cancelled" || publication.FailureReason != "" || publication.LeaseUntil != 0 {
					t.Fatalf("publication before index cleanup: %+v err=%v", publication, err)
				}
				return nil
			}
			for range 2 {
				if cleaned, err := Sweep(ctx, db, CleanupOptions{Grace: time.Minute, BatchSize: 10}, remove); err != nil || cleaned != 1 {
					t.Fatalf("sweep: cleaned=%d err=%v", cleaned, err)
				}
			}
			if err = db.Take(&log, "id = ?", scope.RunID).Error; err != nil || log.OperationStatus != string(entity.TaskStatusCancel) {
				t.Fatalf("run log: status=%s err=%v", log.OperationStatus, err)
			}
			var events int64
			if err = db.Model(&entity.IngestionTaskLog{}).Where("pipeline_log_id = ? AND event_type = ?", scope.RunID, dao.EventTypeTerminal).Count(&events).Error; err != nil || events != 1 {
				t.Fatalf("terminal event count=%d err=%v", events, err)
			}
		})
	}
}

func TestSweepProtectsLiveAndPublishedAttempts(t *testing.T) {
	db, scope := publicationDB(t)
	if err := db.AutoMigrate(&entity.ArchiveAttempt{}); err != nil {
		t.Fatal(err)
	}
	current, err := Acquire(t.Context(), db, scope, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	deleted := 0
	remove := func(context.Context, archiveartifact.Scope, string) error { deleted++; return nil }
	if _, err = Sweep(t.Context(), db, CleanupOptions{Grace: time.Millisecond, BatchSize: 10}, remove); err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatal("live attempt deleted")
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = Sweep(t.Context(), db, CleanupOptions{Grace: time.Millisecond, BatchSize: 10}, remove); err != nil || deleted != 0 {
		t.Fatalf("published deleted: %d %v", deleted, err)
	}
}

func TestSweepRetriesFailuresAndLateWritesWithoutPublishing(t *testing.T) {
	db, scope := publicationDB(t)
	if err := db.AutoMigrate(&entity.ArchiveAttempt{}); err != nil {
		t.Fatal(err)
	}
	stale, err := Acquire(t.Context(), db, scope, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).UnixMilli()
	if err = db.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ?", scope.DocID).Update("lease_until", old).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&entity.ArchiveAttempt{}).Where("attempt_id = ?", stale.AttemptID).Updates(map[string]any{"lease_until": old, "updated_at": old}).Error; err != nil {
		t.Fatal(err)
	}
	physicalRows := true
	calls := 0
	remove := func(ctx context.Context, got archiveartifact.Scope, tenant string) error {
		calls++
		if got != stale || tenant != "tenant" {
			t.Fatalf("unsafe delete scope: %+v %s", got, tenant)
		}
		if err := Publish(ctx, db, stale, artifact(), Counts{Chunks: 1}); !errors.Is(err, ErrSuperseded) {
			t.Fatalf("cleanup did not revoke publication: %v", err)
		}
		if calls == 1 {
			return errors.New("engine unavailable")
		}
		physicalRows = false
		return nil
	}
	opts := CleanupOptions{Grace: time.Millisecond, BatchSize: 10}
	if _, err = Sweep(t.Context(), db, opts, remove); err == nil || !physicalRows {
		t.Fatalf("delete failure lost: %v", err)
	}
	if _, err = Sweep(t.Context(), db, opts, remove); err != nil || physicalRows {
		t.Fatalf("retry failed: %v", err)
	}
	physicalRows = true // A process resumes an already in-flight external write.
	if _, err = Sweep(t.Context(), db, opts, remove); err != nil || physicalRows || calls != 3 {
		t.Fatalf("late write orphaned: calls=%d err=%v", calls, err)
	}
}
