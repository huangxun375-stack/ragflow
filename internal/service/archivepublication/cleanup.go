package archivepublication

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"ragflow/internal/common"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
)

type CleanupOptions struct {
	Grace     time.Duration
	BatchSize int
}

type GenerationDelete func(context.Context, archiveartifact.Scope, string) error

// Sweep retains cleanup tombstones and revisits them on later passes. A worker
// may die after submitting an engine request, which can land after deletion.
// Keeping the exact tuple inventory makes those late writes recoverable.
func Sweep(ctx context.Context, db *gorm.DB, opts CleanupOptions, remove GenerationDelete) (int, error) {
	if opts.Grace <= 0 || opts.BatchSize < 1 || remove == nil {
		return 0, errors.New("invalid archive cleanup configuration")
	}
	cutoff := time.Now().Add(-opts.Grace).UnixMilli()
	var attempts []entity.ArchiveAttempt
	if err := db.WithContext(ctx).Where("state <> ? AND ((updated_at <= ? AND lease_until <= ?) OR state IN ?)", "published", cutoff, cutoff, []string{"cleaning", "cleaned"}).Order("last_cleanup_at, attempt_id").Limit(opts.BatchSize).Find(&attempts).Error; err != nil {
		return 0, err
	}
	cleaned := 0
	var failures []error
	for _, attempt := range attempts {
		if err := ctx.Err(); err != nil {
			return cleaned, errors.Join(append(failures, err)...)
		}
		scope := archiveartifact.Scope{DocID: attempt.DocID, KBID: attempt.KBID, TaskID: attempt.TaskID, RunID: attempt.RunID, AttemptID: attempt.AttemptID}
		allowed, err := claimExpired(ctx, db, scope, cutoff)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !allowed {
			// A renewed publication can disagree with this scan's older candidate
			// snapshot. Advance its scan position so it cannot starve other rows.
			if err := attemptQuery(db.WithContext(ctx), scope).UpdateColumn("last_cleanup_at", time.Now().UnixMilli()).Error; err != nil {
				failures = append(failures, err)
			}
			continue
		}
		err = remove(ctx, scope, attempt.TenantID)
		state := "cleaned"
		if err != nil {
			state = "cleaning"
			failures = append(failures, err)
		} else {
			cleaned++
		}
		if updateErr := db.WithContext(ctx).Model(&entity.ArchiveAttempt{}).Where("attempt_id = ? AND state IN ?", attempt.AttemptID, []string{"cleaning", "cleaned"}).Updates(map[string]any{"state": state, "last_cleanup_at": time.Now().UnixMilli()}).Error; updateErr != nil {
			failures = append(failures, updateErr)
		}
	}
	return cleaned, errors.Join(failures...)
}

func attemptQuery(db *gorm.DB, scope archiveartifact.Scope) *gorm.DB {
	return db.Model(&entity.ArchiveAttempt{}).Where("attempt_id = ? AND run_id = ? AND doc_id = ? AND kb_id = ? AND task_id = ?", scope.AttemptID, scope.RunID, scope.DocID, scope.KBID, scope.TaskID)
}

func claimExpired(ctx context.Context, db *gorm.DB, scope archiveartifact.Scope, cutoff int64) (bool, error) {
	allowed := false
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Do not recreate a deleted dataset's fence. An absent fence is safe only
		// for a durable attempt whose current publication is independently checked.
		var fence entity.ArchiveDatasetFence
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&fence, "kb_id = ?", scope.KBID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var doc entity.Document
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&doc, "id = ?", scope.DocID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var task entity.IngestionTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&task, "id = ?", scope.TaskID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var publication entity.DocumentCodeArchive
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&publication, "doc_id = ?", scope.DocID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var attempt entity.ArchiveAttempt
		if err := attemptQuery(tx, scope).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&attempt).Error; err != nil {
			return err
		}
		if attempt.State == "published" {
			return nil
		}
		current := publication.RunID == scope.RunID && publication.AttemptID == scope.AttemptID
		if current && publication.Status == "ready" {
			return nil
		}
		if attempt.State != "cleaning" && attempt.State != "cleaned" {
			if attempt.LeaseUntil > cutoff || attempt.UpdatedAt > cutoff {
				return nil
			}
			if current && publication.Status == "running" && publication.LeaseUntil > cutoff {
				return nil
			}
		}
		// A pending pointer with an old attempt belongs to a released execution;
		// reclaim its rows without terminally failing the run awaiting redelivery.
		if current && publication.Status == "running" {
			if err := one(rowQuery(tx, scope).Updates(map[string]any{"status": "failed", "lease_until": 0})); err != nil {
				return err
			}
			if task.PipelineLogID != nil && *task.PipelineLogID == scope.RunID && task.Status != common.COMPLETED && task.Status != common.STOPPED && task.Status != common.FAILED {
				if err := one(taskQuery(tx, scope).Update("status", common.FAILED)); err != nil {
					return err
				}
				if doc.ID != "" {
					if err := tx.Model(&entity.Document{}).Where("id = ?", doc.ID).Update("progress", -1).Error; err != nil {
						return err
					}
				}
			}
		}
		if err := one(attemptQuery(tx, scope).Updates(map[string]any{"state": "cleaning", "lease_until": 0, "last_cleanup_at": time.Now().UnixMilli()})); err != nil {
			return err
		}
		allowed = true
		return nil
	})
	return allowed, err
}
