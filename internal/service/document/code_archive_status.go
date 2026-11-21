package document

import (
	"context"
	"fmt"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/service/archivepublication"
)

// setArchiveDocumentStatus serializes status changes with archive publication.
// A pending run observes the committed status when it publishes; an already
// published run is updated by exact generation while the dataset fence and
// document lock are held in the same order as publication and deletion.
func (s *DocumentService) setArchiveDocumentStatus(ctx context.Context, doc *entity.Document, kb *entity.Knowledgebase, status int) error {
	statusText := strconv.Itoa(status)
	err := dao.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := archivepublication.LockDataset(ctx, tx, kb.ID); err != nil {
			return err
		}
		var current entity.Document
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND kb_id = ?", doc.ID, kb.ID).Take(&current).Error; err != nil {
			return err
		}
		switch {
		case current.Status == nil || *current.Status != statusText:
			if err := s.documentDAO.UpdateByID(ctx, tx, doc.ID, map[string]any{"status": statusText}); err != nil {
				return err
			}
		case status == 1:
			// An already-enabled document can contain individually disabled chunks.
			return nil
		}
		// Reassert a disable in ES even when SQL already records it: a prior
		// enable may have reached ES before its acknowledgement was lost.
		generations, err := archivepublication.VisibleGenerations(ctx, tx, []string{kb.ID}, []string{doc.ID})
		if err != nil {
			return err
		}
		generation := generations[doc.ID]
		if generation.RunID == "" || generation.AttemptID == "" {
			return nil
		}
		if s.docEngine == nil {
			return fmt.Errorf("document engine not initialized")
		}
		return s.docEngine.UpdateChunks(ctx, map[string]any{"doc_id": doc.ID, "archive_run_kwd": generation.RunID, "archive_attempt_kwd": generation.AttemptID}, map[string]any{"available_int": status}, fmt.Sprintf("ragflow_%s", kb.TenantID), kb.ID)
	})
	if err == nil {
		doc.Status = &statusText
	}
	return err
}
