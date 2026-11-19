package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ragflow/internal/common"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/service/archivepublication"
)

func (e *Ingestor) SetArchiveCleanup(interval, grace time.Duration, batchSize int) error {
	if interval <= 0 || grace <= 0 || batchSize < 1 {
		return errors.New("invalid archive cleanup schedule")
	}
	e.archiveCleanupInterval = interval
	e.archiveCleanupOptions = archivepublication.CleanupOptions{Grace: grace, BatchSize: batchSize}
	return nil
}

func (e *Ingestor) startArchiveCleanup() {
	if e.archiveCleanupInterval <= 0 || e.deps.DB == nil || e.deps.Engine == nil {
		return
	}
	e.archiveCleanupWg.Add(1)
	go func() {
		defer e.archiveCleanupWg.Done()
		ticker := time.NewTicker(e.archiveCleanupInterval)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(e.ctx, 45*time.Second)
			_, err := archivepublication.Sweep(ctx, e.deps.DB, e.archiveCleanupOptions, func(ctx context.Context, scope archiveartifact.Scope, tenant string) error {
				_, err := e.deps.Engine.DeleteChunks(ctx, map[string]any{"doc_id": scope.DocID, "kb_id": scope.KBID, "archive_run_kwd": scope.RunID, "archive_attempt_kwd": scope.AttemptID}, "ragflow_"+tenant, scope.KBID)
				return err
			})
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				common.Warn(fmt.Sprintf("archive generation cleanup: %v", err))
			}
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
