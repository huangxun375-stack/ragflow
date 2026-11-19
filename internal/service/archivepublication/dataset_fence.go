package archivepublication

import (
	"context"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"ragflow/internal/entity"
	"time"
)

// LockDataset must be the first lock in an archive transaction. Existing
// documents then lock document, task, artifact and KB in that order; a new
// registration locks KB before creating its new document. Publish holds the
// fence through its bounded availability synchronization before committing.
func LockDataset(ctx context.Context, tx *gorm.DB, kbID string) error {
	if err := lockDataset(ctx, tx, kbID, false); err != nil {
		return err
	}
	return countOne(tx.WithContext(ctx).Model(&entity.Knowledgebase{}).Where("id = ?", kbID))
}

func lockDataset(ctx context.Context, tx *gorm.DB, kbID string, allowDeleting bool) error {
	if kbID == "" {
		return ErrSuperseded
	}
	row := entity.ArchiveDatasetFence{KBID: kbID}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return err
	}
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "kb_id = ?", kbID).Error; err != nil {
		return err
	}
	if row.Deleting && !allowDeleting {
		return ErrSuperseded
	}
	return nil
}

// WithDatasetCleanup preserves the same fence-first order for final metadata
// removal and permits retries after an earlier deletion was already fenced.
func WithDatasetCleanup(ctx context.Context, db *gorm.DB, kbID string, cleanup func(*gorm.DB) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockDataset(ctx, tx, kbID, true); err != nil {
			return err
		}
		return cleanup(tx)
	})
}
