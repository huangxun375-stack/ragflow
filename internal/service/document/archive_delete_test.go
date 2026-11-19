package document

import (
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
	"testing"
)

func TestDeleteArchiveDocumentRemovesPublicationInCounterTransaction(t *testing.T) {
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	insertTestKB(t, "kb-archive", "tenant", 1, 3, 2)
	insertTestDoc(t, "doc-archive", "kb-archive", 3, 2)
	if err := db.Model(&entity.Document{}).Where("id = ?", "doc-archive").Update("parser_id", codearchive.ParserID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&entity.DocumentCodeArchive{DocID: "doc-archive", KBID: "kb-archive", TaskID: "task", RunID: "run", AttemptID: "attempt", Status: "ready", TreeJSON: "[]"}).Error; err != nil {
		t.Fatal(err)
	}
	doc, err := dao.NewDocumentDAO().GetByID(t.Context(), db, "doc-archive")
	if err != nil {
		t.Fatal(err)
	}
	svc := testDocumentService(t)
	if err = svc.deleteDocRecordWithCounters(t.Context(), doc, "kb-archive"); err != nil {
		t.Fatal(err)
	}
	if err = svc.deleteDocRecordWithCounters(t.Context(), doc, "kb-archive"); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err = db.Model(&entity.DocumentCodeArchive{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal(count, err)
	}
	var kb entity.Knowledgebase
	if err = db.Take(&kb, "id = ?", "kb-archive").Error; err != nil {
		t.Fatal(err)
	}
	if kb.DocNum != 0 || kb.ChunkNum != 0 || kb.TokenNum != 0 {
		t.Fatalf("counters: %+v", kb)
	}
}
