package document

import (
	"bytes"
	"errors"
	"fmt"
	"mime/multipart"
	"os"
	"path/filepath"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/server"
	"ragflow/internal/server/config"
)

func TestCodeArchiveDeploymentUploadRejectsBeforeReadingOrWriting(t *testing.T) {
	for _, deployment := range []struct{ database, engine string }{
		{"mysql", "infinity"},
		{"vastbase", "elasticsearch"},
	} {
		t.Run(deployment.database+"_"+deployment.engine, func(t *testing.T) {
			useArchiveDeploymentGateConfig(t, deployment.database, deployment.engine)
			pushServiceDB(t, nil)
			store := useFakeStorage(t)
			putCalls := 0
			store.afterPut = func() { putCalls++ }
			kb := &entity.Knowledgebase{ID: "kb-archive", TenantID: "tenant-1", ParserID: "general"}
			// No backing multipart data: opening this header fails, so a typed
			// deployment error proves the gate ran before attempting the read.
			unreadable := &multipart.FileHeader{Filename: "repo.ZIP", Size: 1}
			if f, err := unreadable.Open(); err == nil {
				f.Close()
				t.Fatal("fixture unexpectedly has readable upload bytes")
			}
			for _, fh := range []*multipart.FileHeader{
				unreadable,
				makeTestFileHeader(t, "file", "valid.zip", archiveUploadBytes(t, "src/main.py")),
			} {
				t.Run(fh.Filename, func(t *testing.T) {
					// Nil DAOs/DB also make accidental registration or folder creation fail.
					results, failures := (&DocumentService{}).UploadLocalDocuments(documentTestContext(t.Context()), kb, []*multipart.FileHeader{fh}, "", nil)
					if len(results) != 0 || len(failures) != 1 {
						t.Fatalf("results=%v failures=%v", results, failures)
					}
					var archiveErr *codearchive.Error
					if !errors.As(failures[0], &archiveErr) || archiveErr.Code != "ZIP_UNSUPPORTED_DEPLOYMENT" {
						t.Fatalf("failure=%v, want typed ZIP_UNSUPPORTED_DEPLOYMENT before reading bytes", failures[0])
					}
					if failures[0].Filename != fh.Filename || failures[0].Code != common.CodeDataError || !failures[0].Archive {
						t.Fatalf("incorrect public failure metadata: %+v", failures[0])
					}
					if putCalls != 0 || len(store.objects) != 0 || store.getCalls != 0 {
						t.Fatalf("rejected ZIP touched storage: puts=%d gets=%d objects=%d", putCalls, store.getCalls, len(store.objects))
					}
				})
			}
		})
	}
}

func TestCodeArchiveDeploymentUploadStillAcceptsOrdinaryDocuments(t *testing.T) {
	for _, deployment := range []struct{ database, engine string }{
		{"mysql", "infinity"},
		{"vastbase", "elasticsearch"},
	} {
		t.Run(deployment.database+"_"+deployment.engine, func(t *testing.T) {
			useArchiveDeploymentGateConfig(t, deployment.database, deployment.engine)
			db := setupServiceTestDB(t)
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sqlDB.Close() })
			pushServiceDB(t, db)
			insertTestKB(t, "kb-ordinary", "tenant-1", 0, 0, 0)
			kb, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, "kb-ordinary")
			if err != nil {
				t.Fatal(err)
			}
			kb.ParserID, kb.ParserConfig = "general", generalParserConfig()
			store := useFakeStorage(t)
			putCalls := 0
			store.afterPut = func() { putCalls++ }
			content := []byte("ordinary document remains supported\n")
			results, failures := testDocumentService(t).UploadLocalDocuments(documentTestContext(t.Context()), kb, []*multipart.FileHeader{
				{Filename: "repo.zip", Size: 1},
				makeTestFileHeader(t, "file", "notes.txt", content),
			}, "", nil)
			if len(results) != 1 || len(failures) != 1 {
				t.Fatalf("mixed upload: results=%v failures=%v", results, failures)
			}
			var archiveErr *codearchive.Error
			if failures[0].Filename != "repo.zip" || !errors.As(failures[0], &archiveErr) || archiveErr.Code != "ZIP_UNSUPPORTED_DEPLOYMENT" {
				t.Fatalf("unexpected ZIP rejection: %v", failures[0])
			}
			if results[0]["name"] != "notes.txt" || results[0]["parser_id"] != "general" {
				t.Fatalf("ordinary upload changed parser: %v", results[0])
			}
			var docs []entity.Document
			if err := db.Where("kb_id = ?", kb.ID).Find(&docs).Error; err != nil {
				t.Fatal(err)
			}
			if len(docs) != 1 || docs[0].Name == nil || *docs[0].Name != "notes.txt" || docs[0].Location == nil || docs[0].ParserID != "general" {
				t.Fatalf("persisted documents=%+v", docs)
			}
			if putCalls != 1 || len(store.objects) != 1 || !bytes.Equal(store.objects[store.key(kb.ID, *docs[0].Location)], content) {
				t.Fatalf("ordinary bytes not stored exactly once: puts=%d objects=%d", putCalls, len(store.objects))
			}
			var links int64
			if err := db.Model(&entity.File2Document{}).Where("document_id = ?", docs[0].ID).Count(&links).Error; err != nil || links != 1 {
				t.Fatalf("ordinary file linkage: links=%d err=%v", links, err)
			}
			for _, model := range []any{&entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}} {
				var count int64
				if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("unexpected archive registration in %T: rows=%d err=%v", model, count, err)
				}
			}
			assertKBDocNum(t, kb.ID, 1)
		})
	}
}

func useArchiveDeploymentGateConfig(t *testing.T, database, engine string) {
	t.Helper()
	previous := server.GetConfig()
	var snapshot config.Config
	if previous != nil {
		snapshot = *previous
	}
	dir := t.TempDir()
	restorePath := filepath.Join(dir, "restore.yaml")
	if err := os.WriteFile(restorePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(common.EnvDBType, "")
	t.Setenv(common.EnvDocEngine, "")
	t.Setenv(common.EnvStorageImpl, "")
	// Init replaces the process-wide config. Restore through valid YAML before
	// restoring the saved value; a previously nil config returns to defaults.
	t.Cleanup(func() {
		if err := server.Init(restorePath); err != nil {
			t.Errorf("restore server configuration: %v", err)
			return
		}
		if previous != nil {
			*server.GetConfig() = snapshot
		}
	})
	path := filepath.Join(dir, "service_conf.yaml")
	yaml := fmt.Sprintf("general: {database: %s, doc_engine: %s}\n", database, engine)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.Init(path); err != nil {
		t.Fatalf("initialize deployment configuration: %v", err)
	}
	if server.SupportsCodeArchive() {
		t.Fatal("fixture unexpectedly enables code archives")
	}
}
