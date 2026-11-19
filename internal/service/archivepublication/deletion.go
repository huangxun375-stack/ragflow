package archivepublication

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"ragflow/internal/common"
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
)

// RevokeDataset durably fences workers before any external objects are removed.
// Tombstones survive a failed deletion so a queued retry cannot restart midway
// through cleanup. The final metadata deletion removes them with the documents.
func RevokeDataset(ctx context.Context, db *gorm.DB, kbID string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockDataset(ctx, tx, kbID, true); err != nil {
			return err
		}
		if err := tx.Model(&entity.ArchiveDatasetFence{}).Where("kb_id = ?", kbID).Update("deleting", true).Error; err != nil {
			return err
		}
		var docs []entity.Document
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("kb_id = ?", kbID).Order("id").Find(&docs).Error; err != nil {
			return err
		}
		for _, doc := range docs {
			if doc.ParserID != codearchive.ParserID {
				continue
			}
			var tasks []entity.IngestionTask
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("document_id = ? AND dataset_id = ?", doc.ID, kbID).Order("id").Find(&tasks).Error; err != nil {
				return err
			}
			if err := tx.Model(&entity.IngestionTask{}).Where("document_id = ? AND dataset_id = ? AND status IN ?", doc.ID, kbID, []string{common.CREATED, common.SCHEDULED, common.RUNNING, common.STOPPING}).Update("status", common.STOPPED).Error; err != nil {
				return err
			}
			row := entity.DocumentCodeArchive{DocID: doc.ID, KBID: kbID, Status: "deleting"}
			if len(tasks) > 0 {
				row.TaskID = tasks[0].ID
				if tasks[0].PipelineLogID != nil {
					row.RunID = *tasks[0].PipelineLogID
				}
			}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "doc_id"}}, DoUpdates: clause.Assignments(map[string]any{"status": "deleting", "lease_until": 0, "tree_json": "", "stats_json": "", "root_name": ""})}).Create(&row).Error; err != nil {
				return err
			}
		}
		// Rows whose document is already absent cannot be published. Their
		// cleanup still belongs to the independently deletable dataset.
		return tx.Model(&entity.DocumentCodeArchive{}).Where("kb_id = ?", kbID).Updates(map[string]any{"status": "deleting", "lease_until": 0, "tree_json": "", "stats_json": "", "root_name": ""}).Error
	})
}
