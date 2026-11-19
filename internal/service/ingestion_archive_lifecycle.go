package service

import (
	"context"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/ingestion/component"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/permission"
	"ragflow/internal/server"
	"ragflow/internal/service/archivepublication"
	"strings"
)

func queueArchiveSnapshot(ctx context.Context, tx *gorm.DB, scope archiveartifact.Scope, doc *entity.Document) error {
	var count int64
	if err := tx.WithContext(ctx).Model(&entity.ArchiveRun{}).Where("run_id = ? AND doc_id = ? AND kb_id = ? AND task_id = ?", scope.RunID, scope.DocID, scope.KBID, scope.TaskID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		policy := server.GetCodeArchivePolicy()
		params := map[string]any{}
		if raw, ok := doc.ParserConfig["CodeArchive:Archive"].(map[string]any); ok {
			params = raw
		}
		if raw, ok := doc.ParserConfig["CodeArchive:Archive"].(entity.JSONMap); ok {
			params = map[string]any(raw)
		}
		config, err := codearchive.ParseConfig(params, policy.DefaultChunkTokens)
		if err != nil {
			return err
		}
		ref, err := component.ResolveDocumentStorage(ctx, tx, doc.ID)
		if err != nil {
			return err
		}
		var kb entity.Knowledgebase
		if err = tx.Take(&kb, "id = ?", scope.KBID).Error; err != nil {
			return err
		}
		_, embedding, err := ArchiveEmbeddingModel(permission.WithTenantID(ctx, kb.TenantID), tx, kb.EmbdID)
		if err != nil {
			return err
		}
		hash := ""
		if doc.ContentHash != nil {
			hash = *doc.ContentHash
		}
		snapshot := archiveartifact.Snapshot{Config: config, Policy: policy,
			Source: archiveartifact.Source{Filename: ref.Name, Bucket: ref.Bucket, Path: ref.Path, Hash: hash, Size: doc.Size}, Embedding: embedding}
		if err = archivepublication.SaveSnapshot(ctx, tx, scope, snapshot); err != nil {
			return err
		}
	}
	return archivepublication.Queue(ctx, tx, scope)
}

func validateQueuedArchive(doc *entity.Document) error {
	zip := strings.EqualFold(strings.TrimPrefix(doc.Suffix, "."), "zip") || doc.Name != nil && codearchive.IsArchive(*doc.Name)
	if !zip && doc.ParserID != codearchive.ParserID {
		return nil
	}
	if !server.SupportsCodeArchive() {
		return &codearchive.Error{Code: "ZIP_UNSUPPORTED_DEPLOYMENT"}
	}
	if !zip {
		return fmt.Errorf("%w: code_archive requires a ZIP upload", &codearchive.Error{Code: "ZIP_ONLY"})
	}
	if doc.ParserID != codearchive.ParserID || doc.PipelineID != nil && strings.TrimSpace(*doc.PipelineID) != "" || doc.PipelineVersionID != nil && strings.TrimSpace(*doc.PipelineVersionID) != "" {
		return fmt.Errorf("%w: ZIP requires the built-in code_archive pipeline", &codearchive.Error{Code: "ZIP_PARSER_LOCKED"})
	}
	return ValidateCodeArchiveConfig(map[string]any(doc.ParserConfig))
}

// settlePendingArchive is restricted to a captured queued run. An ambiguous
// enqueue acknowledgement cannot fail an attempt already claimed by a worker.
func (s *IngestionTaskService) settlePendingArchive(ctx context.Context, task *entity.IngestionTask, status string) (bool, error) {
	doc, err := s.documentDAO.GetByID(ctx, dao.DB, task.DocumentID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if doc == nil || doc.ParserID != codearchive.ParserID {
		return false, nil
	}
	if task.PipelineLogID == nil {
		return true, archivepublication.ErrSuperseded
	}
	scope := archiveartifact.Scope{DocID: task.DocumentID, KBID: task.DatasetID, TaskID: task.ID, RunID: *task.PipelineLogID}
	return true, archivepublication.Terminate(ctx, dao.DB, scope, status, nil)
}

func (s *IngestionTaskService) emitArchiveTerminal(ctx context.Context, scope archiveartifact.Scope, event, status, reason string) {
	task, err := s.GetTask(ctx, scope.TaskID)
	if err != nil || task.PipelineLogID == nil || *task.PipelineLogID != scope.RunID || task.Status != status {
		return
	}
	s.emitLifecycleEvent(ctx, task, event, status, reason)
}
