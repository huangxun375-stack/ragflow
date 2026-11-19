package document

import (
	"archive/zip"
	"bytes"
	"errors"
	"mime/multipart"
	"strings"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/service"
	"ragflow/internal/storage"
	"ragflow/internal/utility"
)

func TestArchivePatchAndApplyKeepOnlyArchiveOptions(t *testing.T) {
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	if err := db.AutoMigrate(&entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}); err != nil {
		t.Fatal(err)
	}
	insertTestKB(t, "kb-1", "tenant-1", 1, 0, 0)
	insertNamedTestDoc(t, "doc-1", "kb-1", "repo.zip", 0, 0)
	if err := db.Model(&entity.Document{}).Where("id = ?", "doc-1").Updates(map[string]any{
		"parser_id": "code_archive", "type": "code", "parser_config": entity.JSONMap{"CodeArchive:Archive": map[string]any{"chunk_token_num": 123, "code_file_overview": false}},
	}).Error; err != nil {
		t.Fatal(err)
	}
	svc := testDocumentService(t)
	response, code, err := svc.UpdateDatasetDocument(documentTestContext(t.Context()), "kb-1", "doc-1", &UpdateDatasetDocumentRequest{ParserConfig: map[string]any{"CodeArchive:Archive": map[string]any{"code_file_overview": true}}}, map[string]bool{"parser_config": true})
	if err != nil || code != common.CodeSuccess {
		t.Fatalf("patch: code=%v err=%v", code, err)
	}
	params := response.ParserConfig["CodeArchive:Archive"].(map[string]any)
	if len(response.ParserConfig) != 1 || params["chunk_token_num"] != float64(123) || params["code_file_overview"] != true {
		t.Fatalf("patch=%v", response.ParserConfig)
	}
	if response.ArchiveMetadata == nil || response.ArchiveStatus != "unstart" {
		t.Fatalf("metadata=%+v", response.ArchiveMetadata)
	}
	kb, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, "kb-1")
	if err != nil {
		t.Fatal(err)
	}
	kb.ParserID = "general"
	kb.ParserConfig = generalParserConfig()
	kb.ParserConfig["CodeArchive:Archive"] = map[string]any{"chunk_token_num": 256, "code_file_overview": false, "max_files": 999999}
	if err := db.Model(kb).Updates(map[string]any{"parser_id": kb.ParserID, "parser_config": kb.ParserConfig}).Error; err != nil {
		t.Fatal(err)
	}
	doc, err := dao.NewDocumentDAO().GetByID(t.Context(), db, "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.applyKnowledgebaseParserConfig(documentTestContext(t.Context()), doc, kb); err != nil {
		t.Fatal(err)
	}
	params = doc.ParserConfig["CodeArchive:Archive"].(map[string]any)
	if len(doc.ParserConfig) != 1 || len(params) != 2 || params["chunk_token_num"] != 256 || doc.PipelineID != nil || doc.PipelineVersionID != nil {
		t.Fatalf("apply_kb=%+v", doc)
	}
	if _, code, err := svc.ApplyDatasetParserConfig(documentTestContext(t.Context()), kb.ID); err != nil || code != common.CodeSuccess {
		t.Fatalf("batch: code=%v err=%v", code, err)
	}
	doc, err = dao.NewDocumentDAO().GetByID(t.Context(), db, "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	params = doc.ParserConfig["CodeArchive:Archive"].(map[string]any)
	if doc.ParserID != "code_archive" || doc.PipelineID != nil || doc.PipelineVersionID != nil || len(doc.ParserConfig) != 1 || len(params) != 2 || params["chunk_token_num"] != float64(256) {
		t.Fatalf("batch=%+v", doc)
	}
}

func TestArchiveBoundedReadDoesNotTrustHeaderSize(t *testing.T) {
	fh := makeTestFileHeader(t, "file", "repo.zip ", make([]byte, 10*1024*1024+1))
	fh.Size = 1
	if _, err := readFileHeaderBytes(fh); err == nil || !strings.Contains(err.Error(), "ZIP_SIZE_EXCEEDED") {
		t.Fatalf("bounded read: %v", err)
	}
}

func TestArchiveConfigRejectsInvalidValues(t *testing.T) {
	for _, config := range []map[string]any{
		{"chunk_token_num": 512},
		{"Tokenizer:Archive": map[string]any{"chunk_token_num": 512}},
		{"CodeArchive:Other": map[string]any{}},
		{"CodeArchive:Archive": map[string]any{"chunk_token_num": 0}},
		{"CodeArchive:Archive": map[string]any{"chunk_token_num": 2049}},
		{"CodeArchive:Archive": map[string]any{"chunk_token_num": 1.5}},
		{"CodeArchive:Archive": map[string]any{"code_file_overview": "true"}},
		{"CodeArchive:Archive": map[string]any{"unknown": true}},
		{"CodeArchive:Archive": []any{}},
		{"CodeArchive:Archive": nil},
	} {
		err := service.ValidateCodeArchiveConfig(config)
		var archiveErr *codearchive.Error
		if !errors.As(err, &archiveErr) || archiveErr.Code != "ZIP_CONFIG_INVALID" {
			t.Fatalf("config=%v err=%v, want typed ZIP_CONFIG_INVALID", config, err)
		}
	}
}

func TestArchiveConnectorAndAssociationRejectBeforeWrites(t *testing.T) {
	svc, db, store, input := setupSyncDocumentTest(t, false)
	input.SourceDocument.Extension = ".ZIP"
	if _, err := svc.Upsert(t.Context(), input); err == nil || !strings.Contains(err.Error(), "ZIP_CONNECTOR_UNSUPPORTED") {
		t.Fatalf("connector: %v", err)
	}
	if len(store.objects) != 0 {
		t.Fatal("connector stored ZIP")
	}
	insertTestFile(t, "file-zip", "folder-1", "repo.zip", new("repo.zip"))
	// Keep this service test's dependencies local: the public constructor also
	// installs the process-wide compilation publisher, which outlives this DB.
	linker := &File2DocumentService{
		fileDAO: dao.NewFileDAO(), file2DocumentDAO: dao.NewFile2DocumentDAO(),
		kbDAO: dao.NewKnowledgebaseDAO(), userTenantDAO: dao.NewUserTenantDAO(),
		documentDAO: dao.NewDocumentDAO(), documentSvc: svc,
	}
	if err := linker.LinkToDatasets(documentTestContext(t.Context()), &LinkToDatasetsRequest{FileIDs: []string{"file-zip"}, KbIDs: []string{input.TaskContext.Knowledgebase.ID}}, "add"); err != ErrLinkArchiveUnsupported {
		t.Fatalf("association: %v", err)
	}
	var count int64
	if err := db.Model(&entity.Document{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("documents=%d err=%v", count, err)
	}
}

func TestArchiveWebAndEmptyUploadRejectWithoutFetching(t *testing.T) {
	svc := &DocumentService{}
	kb := &entity.Knowledgebase{}
	for _, url := range []string{"https://example.com/archive.ZIP?download=1", "https://example.com/file"} {
		if _, _, err := svc.UploadWebDocument(t.Context(), kb, "repo.zip", url); err == nil || !strings.Contains(err.Error(), "ZIP_LOCAL_UPLOAD_ONLY") {
			t.Fatalf("web: %v", err)
		}
	}
	if _, _, err := svc.UploadEmptyDocument(t.Context(), kb, "repo.zip"); err == nil || !strings.Contains(err.Error(), "ZIP_ONLY") {
		t.Fatalf("empty: %v", err)
	}
}

func TestArchiveDocumentAvailabilityTargetsPublishedAttempt(t *testing.T) {
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	insertTestKB(t, "kb-1", "tenant-1", 1, 0, 0)
	insertNamedTestDoc(t, "doc-1", "kb-1", "repo.zip", 0, 0)
	if err := db.Model(&entity.Document{}).Where("id = ?", "doc-1").Update("parser_id", "code_archive").Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{&entity.IngestionTask{ID: "task", DocumentID: "doc-1", DatasetID: "kb-1", Status: common.COMPLETED, PipelineLogID: new("run")}, &entity.DocumentCodeArchive{DocID: "doc-1", KBID: "kb-1", TaskID: "task", RunID: "run", AttemptID: "attempt", Status: "ready"}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	eng := &sourceAvailabilityDocEngine{}
	svc := testDocumentService(t)
	svc.docEngine = eng
	doc, err := dao.NewDocumentDAO().GetByID(t.Context(), db, "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	kb, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, "kb-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.updateDocumentStatusOnly(t.Context(), doc, kb, 0); err != nil {
		t.Fatal(err)
	}
	if len(eng.updateConditions) != 1 || eng.updateConditions[0]["archive_run_kwd"] != "run" || eng.updateConditions[0]["archive_attempt_kwd"] != "attempt" {
		t.Fatalf("condition=%v", eng.updateConditions)
	}
	if err := db.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ?", "doc-1").Update("status", "pending").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.updateDocumentStatusOnly(t.Context(), doc, kb, 1); err != nil {
		t.Fatal(err)
	}
	if len(eng.updateConditions) != 1 {
		t.Fatal("pending generation must be synchronized at publication")
	}
	if doc.Status == nil || *doc.Status != "1" {
		t.Fatalf("pending enable status=%v", doc.Status)
	}
	if err := db.Model(&entity.ArchiveDatasetFence{}).Where("kb_id = ?", kb.ID).Update("deleting", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.updateDocumentStatusOnly(t.Context(), doc, kb, 0); err == nil {
		t.Fatal("status mutation accepted while dataset deletion is fenced")
	}
	current, err := dao.NewDocumentDAO().GetByID(t.Context(), db, doc.ID)
	if err != nil || current.Status == nil || *current.Status != "1" {
		t.Fatalf("deleting dataset status mutated: doc=%+v err=%v", current, err)
	}
}

func archiveUploadBytes(t *testing.T, path string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("def run():\n    return 1\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestArchiveUploadForcesBuiltinAndClearsCanvasVersion(t *testing.T) {
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	insertTestKB(t, "kb-archive", "tenant-1", 0, 0, 0)
	kb, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, "kb-archive")
	if err != nil {
		t.Fatal(err)
	}
	kb.PipelineID, kb.PipelineVersionID = new("canvas"), new("version")
	kb.ParserConfig = entity.JSONMap{"Parser:Old": map[string]any{"secret": "discard"}}
	factory := storage.GetStorageFactory()
	previous := factory.GetStorage()
	store := newFakeUploadStorage()
	factory.SetStorage(store)
	t.Cleanup(func() { factory.SetStorage(previous) })
	results, errs := testDocumentService(t).UploadLocalDocuments(documentTestContext(t.Context()), kb,
		[]*multipart.FileHeader{makeTestFileHeader(t, "file", "repo.ZIP", archiveUploadBytes(t, "src/a.py"))}, "", nil)
	if len(errs) != 0 || len(results) != 1 {
		t.Fatalf("results=%v errors=%v", results, errs)
	}
	var docs []entity.Document
	if err := db.Where("kb_id = ?", kb.ID).Find(&docs).Error; err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("documents=%d", len(docs))
	}
	doc := docs[0]
	if doc.ParserID != "code_archive" || doc.PipelineID != nil || doc.PipelineVersionID != nil {
		t.Fatalf("selection=%+v", doc)
	}
	if doc.Type != "code" || utility.FilenameType("repo.ZIP") != utility.FileType("code") {
		t.Fatalf("type=%s", doc.Type)
	}
	if len(doc.ParserConfig) != 1 || doc.ParserConfig["CodeArchive:Archive"] == nil {
		t.Fatalf("config=%v", doc.ParserConfig)
	}
}

func TestArchiveUploadRejectsBeforeStorage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blob   []byte
		reason string
	}{
		{"invalid", []byte("not a ZIP"), "ZIP_"},
		{"traversal", archiveUploadBytes(t, "ignored/../../escape.py"), "ZIP_UNSAFE_PATH"},
		{"oversize", make([]byte, 10*1024*1024+1), "ZIP_"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupServiceTestDB(t)
			pushServiceDB(t, db)
			insertTestKB(t, "kb-archive", "tenant-1", 0, 0, 0)
			kb, err := dao.NewKnowledgebaseDAO().GetByID(t.Context(), db, "kb-archive")
			if err != nil {
				t.Fatal(err)
			}
			factory := storage.GetStorageFactory()
			previous := factory.GetStorage()
			store := newFakeUploadStorage()
			factory.SetStorage(store)
			t.Cleanup(func() { factory.SetStorage(previous) })
			results, errs := testDocumentService(t).UploadLocalDocuments(documentTestContext(t.Context()), kb,
				[]*multipart.FileHeader{makeTestFileHeader(t, "file", "repo.zip", tc.blob)}, "", nil)
			if len(results) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.reason) {
				t.Fatalf("results=%v errors=%v", results, errs)
			}
			if len(store.objects) != 0 {
				t.Fatalf("stored rejected ZIP: %v", store.objects)
			}
			var count int64
			db.Model(&entity.Document{}).Count(&count)
			if count != 0 {
				t.Fatalf("registered rejected ZIP: %d", count)
			}
			if err := db.Model(&entity.File{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("file records for rejected ZIP=%d err=%v", count, err)
			}
		})
	}
}

func TestArchivePatchRejectsSelectionAndUnsafeConfig(t *testing.T) {
	for _, tc := range []struct {
		name, field, reason string
		req                 UpdateDatasetDocumentRequest
	}{
		{"parser_id", "parser_id", "ZIP_CHUNK_METHOD_LOCKED", UpdateDatasetDocumentRequest{ParserID: new("general")}},
		{"chunk_method", "chunk_method", "ZIP_CHUNK_METHOD_LOCKED", UpdateDatasetDocumentRequest{ChunkMethod: new("general")}},
		{"pipeline_id", "pipeline_id", "ZIP_CHUNK_METHOD_LOCKED", UpdateDatasetDocumentRequest{PipelineID: new("canvas")}},
		{"pipeline_version_id", "pipeline_version_id", "ZIP_CHUNK_METHOD_LOCKED", UpdateDatasetDocumentRequest{PipelineVersionID: new("version")}},
		{"parse_type", "parse_type", "ZIP_CHUNK_METHOD_LOCKED", UpdateDatasetDocumentRequest{ParseType: new(2)}},
		{"unknown option", "parser_config", "ZIP_CONFIG_INVALID", UpdateDatasetDocumentRequest{ParserConfig: map[string]any{"CodeArchive:Archive": map[string]any{"max_files": 999999}}}},
		{"wrong scope", "parser_config", "ZIP_CONFIG_INVALID", UpdateDatasetDocumentRequest{ParserConfig: map[string]any{"CodeArchive:Other": map[string]any{}}}},
		{"flat config", "parser_config", "ZIP_CONFIG_INVALID", UpdateDatasetDocumentRequest{ParserConfig: map[string]any{"chunk_token_num": 512}}},
		{"dropped flat config", "parser_config", "ZIP_CONFIG_INVALID", UpdateDatasetDocumentRequest{ParserConfig: map[string]any{}, UnscopedParserConfigKeys: []string{"chunk_token_num"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupServiceTestDB(t)
			pushServiceDB(t, db)
			insertTestKB(t, "kb-1", "tenant-1", 1, 0, 0)
			insertNamedTestDoc(t, "doc-1", "kb-1", "repo.zip", 0, 0)
			if err := db.Model(&entity.Document{}).Where("id = ?", "doc-1").Updates(map[string]any{"parser_id": "code_archive", "type": "code"}).Error; err != nil {
				t.Fatal(err)
			}
			response, code, err := testDocumentService(t).UpdateDatasetDocument(documentTestContext(t.Context()), "kb-1", "doc-1", &tc.req, map[string]bool{tc.field: true})
			var archiveErr *codearchive.Error
			if response != nil || code != common.CodeDataError || !errors.As(err, &archiveErr) || archiveErr.Code != tc.reason {
				t.Fatalf("response=%+v code=%v err=%v, want typed %s", response, code, err, tc.reason)
			}
		})
	}
}

func TestNonArchiveCannotSelectArchiveParser(t *testing.T) {
	for _, field := range []string{"chunk_method", "parser_id"} {
		t.Run(field, func(t *testing.T) {
			db := setupServiceTestDB(t)
			pushServiceDB(t, db)
			insertTestKB(t, "kb-1", "tenant-1", 1, 0, 0)
			insertNamedTestDoc(t, "doc-1", "kb-1", "report.txt", 0, 0)
			req := &UpdateDatasetDocumentRequest{}
			if field == "chunk_method" {
				req.ChunkMethod = new("code_archive")
			} else {
				req.ParserID = new("code_archive")
			}
			response, code, err := testDocumentService(t).UpdateDatasetDocument(documentTestContext(t.Context()), "kb-1", "doc-1", req, map[string]bool{field: true})
			var archiveErr *codearchive.Error
			if response != nil || code != common.CodeDataError || !errors.As(err, &archiveErr) || archiveErr.Code != "ZIP_ONLY" {
				t.Fatalf("response=%+v code=%v err=%v, want typed ZIP_ONLY", response, code, err)
			}
		})
	}
}
