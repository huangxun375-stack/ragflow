package document

import (
	"context"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/service/archivepublication"
)

// registerArchiveDocument commits the new document and its file-manager links
// under the same dataset fence used by archive deletion. No existing document
// lock is acquired: the order is fence -> dataset -> new rows.
func (s *DocumentService) registerArchiveDocument(ctx context.Context, kb *entity.Knowledgebase, doc *entity.Document, userID string) error {
	return dao.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := archivepublication.LockDataset(ctx, tx, kb.ID); err != nil {
			return fmt.Errorf("%w: %w", &codearchive.Error{Code: "ZIP_DATASET_UNAVAILABLE"}, err)
		}
		var current entity.Knowledgebase
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND tenant_id = ? AND status = ?", kb.ID, kb.TenantID, string(entity.StatusValid)).Take(&current).Error; err != nil {
			if dao.IsNotFoundErr(err) {
				return fmt.Errorf("%w: dataset no longer exists", &codearchive.Error{Code: "ZIP_DATASET_UNAVAILABLE"})
			}
			return err
		}
		folder, err := s.ensureKBFolderInTransaction(ctx, tx, &current, userID)
		if err != nil {
			return err
		}
		if doc.UserID == "" {
			doc.UserID = doc.CreatedBy
		}
		if doc.Name != nil {
			name, err := common.UniqueFileName(*doc.Name, common.NameLimit255, func(candidate string) (bool, error) {
				return s.documentDAO.NameExistsInKB(ctx, tx, current.ID, candidate)
			})
			if err != nil {
				return err
			}
			doc.Name = &name
		}
		if err := s.documentDAO.Create(ctx, tx, doc); err != nil {
			return err
		}
		updated := tx.Model(&entity.Knowledgebase{}).Where("id = ?", current.ID).Update("doc_num", gorm.Expr("doc_num + 1"))
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return fmt.Errorf("%w: dataset no longer exists", &codearchive.Error{Code: "ZIP_DATASET_UNAVAILABLE"})
		}
		if err := grantDocumentOwnerEE(ctx, tx, doc); err != nil {
			return err
		}
		name, location := "", ""
		if doc.Name != nil {
			name = *doc.Name
		}
		if doc.Location != nil {
			location = *doc.Location
		}
		fileID := common.GenerateToken()
		file := &entity.File{ID: fileID, ParentID: folder.ID, UserID: current.TenantID, TenantID: current.TenantID, CreatedBy: current.TenantID, Name: name, Type: doc.Type, Size: doc.Size, Location: &location, SourceType: string(entity.FileSourceKnowledgebase)}
		if err := s.fileDAO.Create(ctx, tx, file); err != nil {
			return err
		}
		return s.file2DocumentDAO.Create(ctx, tx, &entity.File2Document{ID: common.GenerateToken(), FileID: &fileID, DocumentID: &doc.ID})
	})
}
