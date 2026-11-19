package archivepublication

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"ragflow/internal/common"
	"ragflow/internal/entity"
)

func TestMetadataDeletionPreservesScopeSequenceAndTombstones(t *testing.T) {
	for _, dataset := range []bool{false, true} {
		name := "documents"
		if dataset {
			name = "dataset"
		}
		t.Run(name, func(t *testing.T) {
			db, scope := publicationDB(t)
			current, err := Acquire(t.Context(), db, scope, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.Create(&entity.ArchiveRun{RunID: scope.RunID, DocID: scope.DocID, KBID: scope.KBID, TaskID: scope.TaskID, SnapshotJSON: "{}"}).Error; err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"same-kb", "other-kb"} {
				kbID := scope.KBID
				if id == "other-kb" {
					kbID = id
					if err := db.Create(&entity.ArchiveDatasetFence{KBID: kbID}).Error; err != nil {
						t.Fatal(err)
					}
				}
				for _, row := range []any{
					&entity.DocumentCodeArchive{DocID: id, KBID: kbID, TaskID: id, RunID: id, AttemptID: id, Status: "ready"},
					&entity.ArchiveRun{DocID: id, KBID: kbID, TaskID: id, RunID: id, SnapshotJSON: "{}"},
					&entity.ArchiveAttempt{DocID: id, KBID: kbID, TaskID: id, RunID: id, AttemptID: id, TenantID: "tenant", State: "published", LeaseUntil: 123},
				} {
					if err := db.Create(row).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			var sequence []string
			recordWrite := func(tx *gorm.DB) { sequence = append(sequence, tx.Statement.Table) }
			if err := db.Callback().Update().After("gorm:update").Register("record_metadata_update", recordWrite); err != nil {
				t.Fatal(err)
			}
			if err := db.Callback().Delete().After("gorm:delete").Register("record_metadata_delete", recordWrite); err != nil {
				t.Fatal(err)
			}
			remove := func(tx *gorm.DB) error {
				if dataset {
					return DeleteDataset(t.Context(), tx, scope.KBID)
				}
				return DeleteDocuments(t.Context(), tx, []string{scope.DocID})
			}
			wantSequence := []string{"archive_attempt", "archive_run", "document_code_archive"}
			if dataset {
				wantSequence = append(wantSequence, "code_archive_dataset")
			}
			for range 2 {
				sequence = nil
				if err := db.Transaction(remove); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(sequence, wantSequence) {
					t.Fatalf("metadata deletion sequence=%v, want %v", sequence, wantSequence)
				}
			}
			for _, id := range []string{scope.DocID, "same-kb", "other-kb"} {
				removed := id == scope.DocID || (dataset && id == "same-kb")
				wantCount := int64(1)
				if removed {
					wantCount = 0
				}
				for _, model := range []any{&entity.ArchiveRun{}, &entity.DocumentCodeArchive{}} {
					var count int64
					if err := db.Model(model).Where("doc_id = ?", id).Count(&count).Error; err != nil || count != wantCount {
						t.Fatalf("%T doc=%s count=%d want=%d: %v", model, id, count, wantCount, err)
					}
				}
				var attempt entity.ArchiveAttempt
				if err := db.Take(&attempt, "doc_id = ?", id).Error; err != nil {
					t.Fatal(err)
				}
				if removed {
					if attempt.State != "revoked" || attempt.LeaseUntil != 0 {
						t.Fatalf("lost cleanup tombstone: %+v", attempt)
					}
				} else if attempt.State != "published" || attempt.LeaseUntil != 123 {
					t.Fatalf("changed unrelated attempt: %+v", attempt)
				}
				if id == scope.DocID && attempt.AttemptID != current.AttemptID {
					t.Fatalf("lost attempt identity: %+v", attempt)
				}
			}
			for _, kbID := range []string{scope.KBID, "other-kb"} {
				want := int64(1)
				if dataset && kbID == scope.KBID {
					want = 0
				}
				var count int64
				if err := db.Model(&entity.ArchiveDatasetFence{}).Where("kb_id = ?", kbID).Count(&count).Error; err != nil || count != want {
					t.Fatalf("dataset fence %s: count=%d want=%d %v", kbID, count, want, err)
				}
			}
		})
	}
}

func TestMetadataDeletionStopsAtFirstError(t *testing.T) {
	for _, failTable := range []string{"archive_attempt", "archive_run", "document_code_archive"} {
		t.Run(failTable, func(t *testing.T) {
			db, scope := publicationDB(t)
			wantErr := errors.New("metadata write rejected")
			var sequence []string
			beforeWrite := func(tx *gorm.DB) {
				sequence = append(sequence, tx.Statement.Table)
				if tx.Statement.Table == failTable {
					tx.AddError(wantErr)
				}
			}
			if err := db.Callback().Update().Before("gorm:update").Register("reject_metadata_update", beforeWrite); err != nil {
				t.Fatal(err)
			}
			if err := db.Callback().Delete().Before("gorm:delete").Register("reject_metadata_delete", beforeWrite); err != nil {
				t.Fatal(err)
			}
			if err := DeleteDataset(t.Context(), db, scope.KBID); !errors.Is(err, wantErr) {
				t.Fatalf("metadata error lost: %v", err)
			}
			wantSequence := []string{"archive_attempt"}
			if failTable != "archive_attempt" {
				wantSequence = append(wantSequence, "archive_run")
			}
			if failTable == "document_code_archive" {
				wantSequence = append(wantSequence, "document_code_archive")
			}
			if !reflect.DeepEqual(sequence, wantSequence) {
				t.Fatalf("writes continued after error: %v, want %v", sequence, wantSequence)
			}
			var fence entity.ArchiveDatasetFence
			if err := db.Take(&fence, "kb_id = ?", scope.KBID).Error; err != nil {
				t.Fatalf("removed fence before metadata deletion completed: %v", err)
			}
		})
	}
	if err := DeleteDocuments(t.Context(), nil, nil); err != nil {
		t.Fatalf("empty document deletion: %v", err)
	}
}

func TestDatasetRevocationCleansWriteThatPassedOwnershipCheck(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	checked := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	var mu sync.Mutex
	indexed := map[string]bool{}
	go func() {
		finished <- WriteBatch(t.Context(), db, current, func(context.Context) error {
			close(checked)
			<-release
			mu.Lock()
			indexed[current.AttemptID] = true
			mu.Unlock()
			return nil
		})
	}()
	<-checked
	if err = RevokeDataset(t.Context(), db, scope.KBID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clear(indexed)
	mu.Unlock() // Dataset's external index cleanup.
	if err = db.Delete(&entity.Document{}, "id = ?", scope.DocID).Error; err != nil {
		t.Fatal(err)
	}
	close(release)
	if err = <-finished; !errors.Is(err, ErrSuperseded) {
		t.Fatalf("late write escaped ownership fence: %v", err)
	}
	allowed, err := CanCompensate(t.Context(), db, current, nil)
	if err != nil || !allowed {
		t.Fatalf("late write cleanup=%v %v", allowed, err)
	}
	mu.Lock()
	delete(indexed, current.AttemptID)
	remaining := len(indexed)
	mu.Unlock()
	if remaining != 0 {
		t.Fatalf("orphan chunks: %d", remaining)
	}
}

func TestDatasetDeletionTombstoneRejectsRequeueAndPublish(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = RevokeDataset(t.Context(), db, scope.KBID); err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("publish after deletion fence: %v", err)
	}
	if err = Renew(t.Context(), db, current); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("renew after deletion fence: %v", err)
	}
	scope.RunID = "replacement"
	if err = db.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Updates(map[string]any{"status": common.CREATED, "pipeline_log_id": scope.RunID}).Error; err != nil {
		t.Fatal(err)
	}
	if err = Queue(t.Context(), db, scope); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("queue erased deletion fence: %v", err)
	}
	if err = RevokeDataset(t.Context(), db, scope.KBID); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	allowed, err := CanCompensate(t.Context(), db, current, nil)
	if err != nil || !allowed {
		t.Fatalf("deletion fence blocked cleanup: %v %v", allowed, err)
	}
}

func TestDeletingDatasetRejectsRegistrationForPreviouslyUnknownDocument(t *testing.T) {
	db, scope := publicationDB(t)
	if err := RevokeDataset(t.Context(), db, scope.KBID); err != nil {
		t.Fatal(err)
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := LockDataset(t.Context(), tx, scope.KBID); err != nil {
			return err
		}
		return tx.Create(&entity.Document{ID: "new-after-delete", KbID: scope.KBID, ParserID: "code_archive", ParserConfig: entity.JSONMap{}}).Error
	})
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("registration after deletion fence: %v", err)
	}
	var count int64
	db.Model(&entity.Document{}).Where("id = ?", "new-after-delete").Count(&count)
	if count != 0 {
		t.Fatal("new document escaped deletion fence")
	}
}
