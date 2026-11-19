package entity

// ArchiveDatasetFence serializes archive registration and lifecycle transitions
// with dataset deletion, including documents that did not exist at delete start.
type ArchiveDatasetFence struct {
	KBID     string `gorm:"column:kb_id;primaryKey;size:256"`
	Deleting bool   `gorm:"column:deleting;not null;default:false"`
}

func (ArchiveDatasetFence) TableName() string { return "code_archive_dataset" }
