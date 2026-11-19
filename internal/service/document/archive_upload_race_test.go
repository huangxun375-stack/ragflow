package document

import (
	"mime/multipart"
	"strings"
	"testing"

	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/storage"
)

func TestArchiveUploadRegistrationRejectsDeletedDatasetAndCleansBlob(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		name := "dataset removed"
		if fenced {
			name = "dataset deleting"
		}
		t.Run(name, func(t *testing.T) {
			db := setupServiceTestDB(t)
			pushServiceDB(t, db)
			insertTestKB(t, "kb-archive", "tenant-1", 0, 0, 0)
			kb, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, "kb-archive")
			if err != nil {
				t.Fatal(err)
			}
			store := newFakeUploadStorage()
			factory := storage.GetStorageFactory()
			previous := factory.GetStorage()
			factory.SetStorage(store)
			t.Cleanup(func() { factory.SetStorage(previous) })
			store.afterPut = func() {
				if fenced {
					if err := db.Exec("INSERT INTO code_archive_dataset (kb_id, deleting) VALUES (?, ?)", kb.ID, true).Error; err != nil {
						t.Fatal(err)
					}
				} else if err := db.Where("id = ?", kb.ID).Delete(&entity.Knowledgebase{}).Error; err != nil {
					t.Fatal(err)
				}
			}
			results, failures := testDocumentService(t).UploadLocalDocuments(documentTestContext(t.Context()), kb, []*multipart.FileHeader{makeTestFileHeader(t, "file", "repo.zip", archiveUploadBytes(t, "src/main.py"))}, "", nil)
			if len(results) != 0 || len(failures) != 1 {
				t.Fatalf("results=%v failures=%v", results, failures)
			}
			if len(store.objects) != 0 {
				t.Fatalf("rejected upload left objects: %v", store.objects)
			}
			for _, model := range []any{&entity.Document{}, &entity.File{}, &entity.File2Document{}} {
				var count int64
				if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("partial registration %T count=%d err=%v", model, count, err)
				}
			}
		})
	}
}

func TestArchiveUploadRegistrationRollsBackDocumentCounterAndFileLink(t *testing.T) {
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	insertTestKB(t, "kb-archive", "tenant-1", 0, 0, 0)
	kb, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, "kb-archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_archive_mapping BEFORE INSERT ON file2document BEGIN SELECT RAISE(ABORT, 'mapping rejected'); END").Error; err != nil {
		t.Fatal(err)
	}
	store := newFakeUploadStorage()
	factory := storage.GetStorageFactory()
	previous := factory.GetStorage()
	factory.SetStorage(store)
	t.Cleanup(func() { factory.SetStorage(previous) })
	results, failures := testDocumentService(t).UploadLocalDocuments(documentTestContext(t.Context()), kb, []*multipart.FileHeader{makeTestFileHeader(t, "file", "repo.zip", archiveUploadBytes(t, "src/main.py"))}, "", nil)
	if len(results) != 0 || len(failures) != 1 || !strings.Contains(failures[0].Error(), "mapping rejected") {
		t.Fatalf("results=%v failures=%v", results, failures)
	}
	if len(store.objects) != 0 {
		t.Fatal("failed registration retained blob")
	}
	for _, model := range []any{&entity.Document{}, &entity.File{}, &entity.File2Document{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("partial registration %T count=%d err=%v", model, count, err)
		}
	}
	current, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, kb.ID)
	if err != nil || current.DocNum != 0 {
		t.Fatalf("counter=%+v err=%v", current, err)
	}
}

func TestArchiveConcurrentUploadStagingKeepsSnapshotsIndependent(t *testing.T) {
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	insertTestKB(t, "kb-archive", "tenant-1", 0, 0, 0)
	kb, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, "kb-archive")
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeUploadStorage()
	factory := storage.GetStorageFactory()
	previous := factory.GetStorage()
	factory.SetStorage(store)
	t.Cleanup(func() { factory.SetStorage(previous) })
	svc := testDocumentService(t)
	ctx := documentTestContext(t.Context())
	store.afterPut = func() {
		store.afterPut = nil
		second, failures := svc.UploadLocalDocuments(ctx, kb, []*multipart.FileHeader{makeTestFileHeader(t, "file", "repo.zip", archiveUploadBytes(t, "src/second.py"))}, "", nil)
		if len(second) != 1 || len(failures) != 0 {
			t.Fatalf("interleaved upload=%v failures=%v", second, failures)
		}
	}
	first, failures := svc.UploadLocalDocuments(ctx, kb, []*multipart.FileHeader{makeTestFileHeader(t, "file", "repo.zip", archiveUploadBytes(t, "src/first.py"))}, "", nil)
	if len(first) != 1 || len(failures) != 0 {
		t.Fatalf("first upload=%v failures=%v", first, failures)
	}
	var docs []entity.Document
	if err := db.Where("kb_id = ?", kb.ID).Find(&docs).Error; err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || len(store.objects) != 2 || *docs[0].Location == *docs[1].Location || *docs[0].Name == *docs[1].Name {
		t.Fatalf("snapshots not isolated: docs=%+v objects=%d", docs, len(store.objects))
	}
	for _, doc := range docs {
		blob, err := store.Get(ctx, kb.ID, *doc.Location)
		if err != nil || doc.ContentHash == nil || *doc.ContentHash != contentHashHex(blob) {
			t.Fatalf("snapshot overwritten for doc %s: %v", doc.ID, err)
		}
	}
}
