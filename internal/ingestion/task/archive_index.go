package task

import (
	"context"
	"time"

	"gorm.io/gorm"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/service/archivepublication"
)

func guardArchiveInsert(insert InsertFunc, db *gorm.DB, scope archiveartifact.Scope) InsertFunc {
	if insert == nil {
		return nil
	}
	return func(ctx context.Context, chunks []map[string]any, baseName, datasetID string) (ids []string, err error) {
		err = archivepublication.WriteBatch(ctx, db, scope, func(writeCtx context.Context) error {
			var writeErr error
			ids, writeErr = insert(writeCtx, chunks, baseName, datasetID)
			return writeErr
		})
		return ids, err
	}
}

// CompensateArchive removes only this attempt's rows after a database
// transaction proves that it is unpublished and revokes future publication.
// An unknown commit outcome or an already published attempt never authorizes
// a delete. The detached timeout also bounds cleanup after cancellation.
func (s *PipelineExecutor) CompensateArchive(ctx context.Context, cause error) error {
	scope, ok := archiveartifact.ScopeFrom(ctx)
	if !ok || s.deleteChunksFunc == nil {
		return nil
	}
	return s.compensateIndexRows(ctx, map[string]any{"doc_id": scope.DocID, "kb_id": scope.KBID}, scope.KBID, 10*time.Second, cause)
}

// Callers retain their deletion scope and timeout; archive authorization must
// commit before any external rows are removed, even after cancellation.
func (s *PipelineExecutor) compensateIndexRows(ctx context.Context, condition map[string]any, kbID string, timeout time.Duration, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if scope, ok := archiveartifact.ScopeFrom(ctx); ok {
		allowed, err := archivepublication.CanCompensate(cleanupCtx, s.deps.DB, scope, cause)
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		}
		condition["archive_run_kwd"] = scope.RunID
		condition["archive_attempt_kwd"] = scope.AttemptID
	}
	_, err := s.deleteChunksFunc(cleanupCtx, condition, s.indexWriter.baseName, kbID)
	return err
}
