package entity

// DocumentCodeArchive is the sole publication pointer for a ZIP document. Its
// generation is owned by a pipeline run and a renewable worker attempt.
type DocumentCodeArchive struct {
	DocID         string `gorm:"column:doc_id;primaryKey;size:32"`
	KBID          string `gorm:"column:kb_id;size:256;not null;index"`
	TaskID        string `gorm:"column:task_id;size:32;not null"`
	RunID         string `gorm:"column:run_id;size:32;not null"`
	AttemptID     string `gorm:"column:attempt_id;size:32;not null;default:''"`
	Status        string `gorm:"column:status;size:32;not null"`
	FailureReason string `gorm:"column:failure_reason;size:64;not null;default:''"`
	LeaseUntil    int64  `gorm:"column:lease_until;not null;default:0"`
	RootName      string `gorm:"column:root_name;size:255;not null;default:''"`
	TreeJSON      string `gorm:"column:tree_json;type:longtext"`
	StatsJSON     string `gorm:"column:stats_json;type:longtext"`
}

func (DocumentCodeArchive) TableName() string { return "document_code_archive" }
