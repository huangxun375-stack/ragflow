package entity

// ArchiveRun retains the immutable inputs of one pipeline run, independently
// of the document's replaceable publication pointer.
type ArchiveRun struct {
	RunID        string `gorm:"column:run_id;primaryKey;size:32"`
	DocID        string `gorm:"column:doc_id;size:32;not null;index"`
	KBID         string `gorm:"column:kb_id;size:256;not null;index"`
	TaskID       string `gorm:"column:task_id;size:32;not null"`
	SnapshotJSON string `gorm:"column:snapshot_json;type:longtext;not null"`
	CreatedAt    int64  `gorm:"column:created_at;not null;autoCreateTime:milli"`
}

func (ArchiveRun) TableName() string { return "archive_run" }

// ArchiveAttempt is the durable index-generation inventory. Cleanup tombstones
// survive document/dataset deletion to catch writes that were already in flight.
type ArchiveAttempt struct {
	AttemptID     string `gorm:"column:attempt_id;primaryKey;size:32"`
	RunID         string `gorm:"column:run_id;size:32;not null;index"`
	DocID         string `gorm:"column:doc_id;size:32;not null;index"`
	KBID          string `gorm:"column:kb_id;size:256;not null;index"`
	TaskID        string `gorm:"column:task_id;size:32;not null"`
	TenantID      string `gorm:"column:tenant_id;size:32;not null"`
	State         string `gorm:"column:state;size:16;not null;index:idx_archive_cleanup,priority:1"`
	LeaseUntil    int64  `gorm:"column:lease_until;not null"`
	UpdatedAt     int64  `gorm:"column:updated_at;not null;autoUpdateTime:milli"`
	LastCleanupAt int64  `gorm:"column:last_cleanup_at;not null;default:0;index:idx_archive_cleanup,priority:2"`
}

func (ArchiveAttempt) TableName() string { return "archive_attempt" }
