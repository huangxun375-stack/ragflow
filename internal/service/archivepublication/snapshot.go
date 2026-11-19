package archivepublication

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/parser/codearchive"
)

var ErrSnapshotExists = errors.New("archive run snapshot already exists")

func SaveSnapshot(ctx context.Context, tx *gorm.DB, scope archiveartifact.Scope, snapshot archiveartifact.Snapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	var prior entity.ArchiveRun
	err = tx.WithContext(ctx).Take(&prior, "run_id = ?", scope.RunID).Error
	if err == nil {
		if prior.DocID == scope.DocID && prior.KBID == scope.KBID && prior.TaskID == scope.TaskID && prior.SnapshotJSON == string(raw) {
			return nil
		}
		return ErrSnapshotExists
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return tx.WithContext(ctx).Create(&entity.ArchiveRun{RunID: scope.RunID, DocID: scope.DocID, KBID: scope.KBID, TaskID: scope.TaskID, SnapshotJSON: string(raw)}).Error
}

func LoadSnapshot(ctx context.Context, db *gorm.DB, scope archiveartifact.Scope) (archiveartifact.Snapshot, error) {
	var row entity.ArchiveRun
	var snapshot archiveartifact.Snapshot
	err := db.WithContext(ctx).Take(&row, "run_id = ? AND doc_id = ? AND kb_id = ? AND task_id = ?", scope.RunID, scope.DocID, scope.KBID, scope.TaskID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return snapshot, &codearchive.Error{Code: "ZIP_SNAPSHOT_MISSING"}
	}
	if err != nil {
		return snapshot, err
	}
	if err = json.Unmarshal([]byte(row.SnapshotJSON), &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, snapshot.Validate()
}
