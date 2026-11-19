package service

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/service/archivepublication"
)

// claimArchiveAttempt is durable across workers/processes. Broker heartbeats
// alone cannot fence a worker whose delivery is redelivered to another process.
func (e *Ingestor) claimArchiveAttempt(ctx context.Context, task *entity.IngestionTask) (context.Context, func(), error) {
	done := func() {}
	if e.deps == nil || e.deps.DB == nil {
		return ctx, done, errors.New("archive attempt claim requires a database")
	}
	var doc entity.Document
	if err := e.deps.DB.WithContext(ctx).Select("id", "parser_id").Take(&doc, "id = ?", task.DocumentID).Error; err != nil {
		// A missing document is settled by the ordinary run path as FAILED;
		// redelivering it would leave the task RUNNING once delivery is exhausted.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ctx, done, nil
		}
		return ctx, done, err
	}
	if doc.ParserID != codearchive.ParserID {
		return ctx, done, nil
	}
	if task.PipelineLogID == nil {
		return ctx, done, archivepublication.ErrSuperseded
	}
	scope, err := archivepublication.Acquire(ctx, e.deps.DB, archiveartifact.Scope{DocID: task.DocumentID, KBID: task.DatasetID, TaskID: task.ID, RunID: *task.PipelineLogID}, archivepublication.LeaseDuration)
	if err != nil {
		return ctx, done, err
	}
	ctx, cancel := context.WithCancel(archiveartifact.WithScope(ctx, scope))
	stopped := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(archivepublication.LeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopped:
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(ctx, 5*time.Second)
				err := archivepublication.Renew(renewCtx, e.deps.DB, scope)
				if err != nil && archivepublication.Completed(renewCtx, e.deps.DB, scope) == nil {
					renewCancel()
					return
				}
				renewCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { close(stopped); cancel(); <-exited }, nil
}
