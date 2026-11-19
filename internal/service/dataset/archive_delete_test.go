package dataset

import (
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/storage"
	"testing"
)

func TestDatasetDeleteRemovesOrphanArchivePublication(t *testing.T) {
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	insertDatasetUpdateKB(t, "kb-archive", "tenant", "archive")
	if err := db.Create(&entity.DocumentCodeArchive{DocID: "orphan", KBID: "kb-archive", TaskID: "task", RunID: "run", Status: "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	factory := storage.GetStorageFactory()
	old := factory.GetStorage()
	factory.SetStorage(storage.NewMemoryStorage())
	t.Cleanup(func() { factory.SetStorage(old) })
	kb, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, "kb-archive")
	if err != nil {
		t.Fatal(err)
	}
	if err = testDatasetUpdateService(t).deleteDataset(t.Context(), kb); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err = db.Model(&entity.DocumentCodeArchive{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
