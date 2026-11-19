package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/service"
	"ragflow/internal/service/archivepublication"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/permission"
	"ragflow/internal/service/document"
)

func TestArchiveEmbeddingFailureReasonSurvivesAsyncStatusAPI(t *testing.T) {
	for _, compensate := range []bool{false, true} {
		t.Run(fmt.Sprint(compensate), func(t *testing.T) {
			h := setupArchiveDocumentPatchHandler(t, true)
			db := dao.DB
			if err := db.AutoMigrate(&entity.ArchiveDatasetFence{}); err != nil {
				t.Fatal(err)
			}
			scope := archiveartifact.Scope{DocID: "doc-1", KBID: "ds-1", TaskID: "embedding-task", RunID: "embedding-run"}
			if err := db.Create(&entity.IngestionTask{ID: scope.TaskID, DocumentID: scope.DocID, DatasetID: scope.KBID, Status: common.RUNNING, PipelineLogID: &scope.RunID}).Error; err != nil {
				t.Fatal(err)
			}
			if err := archivepublication.Queue(t.Context(), db, scope); err != nil {
				t.Fatal(err)
			}
			current, err := archivepublication.Acquire(t.Context(), db, scope, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if compensate {
				if ok, err := archivepublication.CanCompensate(t.Context(), db, current, nil); err != nil || !ok {
					t.Fatalf("%v %v", ok, err)
				}
			}
			cause := errors.Join(fmt.Errorf("publish: %w", &codearchive.Error{Code: "ZIP_EMBEDDING_CHANGED"}), errors.New("cleanup diagnostic"))
			if err := service.NewIngestionTaskService().MarkFailed(archiveartifact.WithScope(t.Context(), current), scope.TaskID, cause); err != nil {
				t.Fatal(err)
			}
			c, response := setupGinContextWithUser("GET", "/api/v1/documents/doc-1", "")
			c.Request = c.Request.WithContext(permission.WithIdentity(c.Request.Context(), "user-1", "tenant-1"))
			c.Params = gin.Params{{Key: "id", Value: "doc-1"}}
			h.GetDocumentByID(c)
			var body struct {
				Code common.ErrorCode `json:"code"`
				Data map[string]any   `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != common.CodeSuccess || body.Data["archive_failure_reason"] != "ZIP_EMBEDDING_CHANGED" || body.Data["archive_status"] != "failed" {
				t.Fatal(response.Body.String())
			}
		})
	}
}

func TestArchiveTreeHandlerAuthorizesCurrentPublishedGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, user, dataset, archiveStatus, taskStatus string
		ready, success                                 bool
	}{
		{"ready", "user-1", "ds-1", "ready", common.COMPLETED, true, true},
		{"pending", "user-1", "ds-1", "pending", common.RUNNING, false, true},
		{"failed", "user-1", "ds-1", "ready", common.FAILED, false, true},
		{"cancelled", "user-1", "ds-1", "ready", common.STOPPED, false, true},
		{"stale run", "user-1", "ds-1", "ready", common.COMPLETED, false, true},
		{"wrong dataset", "user-1", "another", "ready", common.COMPLETED, false, false},
		{"unauthorized", "outsider", "ds-1", "ready", common.COMPLETED, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupHandlerAccessDB(t)
			old := dao.DB
			dao.DB = db
			t.Cleanup(func() { dao.DB = old })
			if err := db.AutoMigrate(&entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}, &entity.IngestionTask{}); err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&entity.Document{}).Where("id = ?", "doc-1").Updates(map[string]any{"parser_id": "code_archive", "type": "code", "suffix": "zip"}).Error; err != nil {
				t.Fatal(err)
			}
			run := "run-1"
			if tc.name == "stale run" {
				run = "run-2"
			}
			if err := db.Create(&entity.IngestionTask{ID: "task-1", DocumentID: "doc-1", DatasetID: "ds-1", Status: tc.taskStatus, PipelineLogID: &run}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&entity.DocumentCodeArchive{DocID: "doc-1", KBID: "ds-1", TaskID: "task-1", RunID: "run-1", AttemptID: "attempt-1", Status: tc.archiveStatus, RootName: "repo", TreeJSON: `[{"name":"src","path":"src","type":"directory"}]`, StatsJSON: `{"accepted_files":1,"skipped_dir":2,"filtered_entries":[{"path":"node_modules","type":"directory","reason":"dir","file_count":2}],"filtered_entries_truncated":true}`}).Error; err != nil {
				t.Fatal(err)
			}
			r := gin.New()
			h := &DocumentHandler{}
			r.GET("/datasets/:dataset_id/documents/:document_id/archive_tree", func(c *gin.Context) {
				c.Request = c.Request.WithContext(permission.WithIdentity(c.Request.Context(), tc.user, "tenant-1"))
				h.ArchiveTree(c)
			})
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/datasets/"+tc.dataset+"/documents/doc-1/archive_tree", nil))
			var body struct {
				Code common.ErrorCode              `json:"code"`
				Data *document.ArchiveTreeResponse `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if (body.Code == common.CodeSuccess) != tc.success {
				t.Fatalf("response=%s", rec.Body.String())
			}
			if !tc.success {
				if strings.Contains(rec.Body.String(), "node_modules") {
					t.Fatal("unauthorized filtering details leaked")
				}
				return
			}
			if body.Data == nil {
				t.Fatalf("missing response: %s", rec.Body.String())
			}
			var raw struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			if _, exists := raw.Data["archive_run_id"]; !exists {
				t.Fatalf("archive_run_id omitted: %s", rec.Body.String())
			}
			if _, exists := raw.Data["run_id"]; exists {
				t.Fatalf("inconsistent run_id key: %s", rec.Body.String())
			}
			if !tc.ready && string(raw.Data["tree"]) != "[]" {
				t.Fatalf("not_ready must expose tree:[]: %s", rec.Body.String())
			}
			if tc.ready {
				if body.Data.Status != "ready" || body.Data.RunID == nil || *body.Data.RunID != "run-1" || len(body.Data.Tree) != 1 {
					t.Fatalf("ready response=%+v", body.Data)
				}
				stats := body.Data.Stats
				if stats == nil || len(stats.FilteredEntries) != 1 || stats.FilteredEntries[0].Path != "node_modules" || stats.FilteredEntries[0].FileCount != 2 || !stats.FilteredEntriesTruncated {
					t.Fatalf("filter details missing: %+v", stats)
				}
			} else if body.Data.Status != "not_ready" || len(body.Data.Tree) != 0 || body.Data.Stats != nil {
				t.Fatalf("unpublished response=%+v", body.Data)
			}
		})
	}
}

func setupArchiveDocumentPatchHandler(t *testing.T, archive bool) *DocumentHandler {
	t.Helper()
	db := setupHandlerAccessDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	prior := dao.DB
	dao.DB = db
	t.Cleanup(func() { dao.DB = prior })
	if err := db.AutoMigrate(&entity.IngestionTask{}, &entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}); err != nil {
		t.Fatal(err)
	}
	if archive {
		if err := db.Model(&entity.Document{}).Where("id = ?", "doc-1").Updates(map[string]any{
			"name": "repo.zip", "parser_id": "code_archive", "type": "code", "suffix": "zip",
			"parser_config": entity.JSONMap{"CodeArchive:Archive": map[string]any{"chunk_token_num": 123, "code_file_overview": false}},
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	isolateKnowledgeCompilePublisher(t)
	return &DocumentHandler{documentService: document.NewDocumentService()}
}

func patchArchiveDocument(h *DocumentHandler, userID, datasetID, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.PATCH("/api/v1/datasets/:dataset_id/documents/:document_id", func(c *gin.Context) {
		if userID != "" {
			c.Set("user", &entity.User{ID: userID})
			c.Set("user_id", userID)
			setGinContextSubject(c, userID, "tenant-1")
		}
		h.UpdateDatasetDocument(c)
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/datasets/"+datasetID+"/documents/doc-1", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func assertDocumentPatchError(t *testing.T, recorder *httptest.ResponseRecorder, code common.ErrorCode, message, reason string) {
	t.Helper()
	var response struct {
		Code    common.ErrorCode `json:"code"`
		Message string           `json:"message"`
		Data    json.RawMessage  `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || response.Code != code || response.Message != message {
		t.Fatalf("HTTP=%d response=%s, want HTTP=200 code=%d message=%q", recorder.Code, recorder.Body.String(), code, message)
	}
	if reason == "" {
		if len(response.Data) != 0 {
			t.Fatalf("ordinary error must omit data: %s", recorder.Body.String())
		}
		return
	}
	var data map[string]string
	if err := json.Unmarshal(response.Data, &data); err != nil {
		t.Fatalf("archive error must include data.reason_code: %s (%v)", recorder.Body.String(), err)
	}
	if len(data) != 1 || data["reason_code"] != reason {
		t.Fatalf("archive reason contract: %s", recorder.Body.String())
	}
}

func TestUpdateDatasetDocumentArchiveParserLockReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"chunk_method", `{"chunk_method":"general"}`},
		{"parser_id", `{"parser_id":"general"}`},
		{"builtin parser_id", `{"parse_type":1,"parser_id":"general"}`},
		{"builtin alias", `{"chunk_method":"naive"}`},
		{"conflicting chunk_method", `{"parse_type":1,"parser_id":"code_archive","chunk_method":"general"}`},
		{"conflicting parser_id", `{"parser_id":"general","chunk_method":"code_archive"}`},
		{"empty chunk_method", `{"chunk_method":" "}`},
		{"empty parser_id", `{"parser_id":" "}`},
		{"pipeline_id", `{"pipeline_id":"canvas"}`},
		{"pipeline_version_id", `{"pipeline_version_id":"version"}`},
		{"parse_type", `{"parse_type":2}`},
		{"invalid parse_type", `{"parse_type":3}`},
		{"pipeline selection", `{"parse_type":2,"pipeline_id":"canvas","pipeline_version_id":"version"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setupArchiveDocumentPatchHandler(t, true)
			recorder := patchArchiveDocument(h, "user-1", "ds-1", tc.body)
			assertDocumentPatchError(t, recorder, common.CodeDataError, "ZIP_CHUNK_METHOD_LOCKED", "ZIP_CHUNK_METHOD_LOCKED")
		})
	}
}

func TestUpdateDatasetDocumentArchiveConfigReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"flat", `{"parser_config":{"chunk_token_num":512}}`},
		{"mixed flat and scoped", `{"parser_config":{"max_files":1,"CodeArchive:Archive":{"chunk_token_num":512}}}`},
		{"other component", `{"parser_config":{"Tokenizer:Archive":{"chunk_token_num":512}}}`},
		{"other archive node", `{"parser_config":{"CodeArchive:Other":{}}}`},
		{"unknown option", `{"parser_config":{"CodeArchive:Archive":{"max_files":999999}}}`},
		{"invalid node shape", `{"parser_config":{"CodeArchive:Archive":[]}}`},
		{"null node", `{"parser_config":{"CodeArchive:Archive":null}}`},
		{"token minimum", `{"parser_config":{"CodeArchive:Archive":{"chunk_token_num":0}}}`},
		{"token maximum", `{"parser_config":{"CodeArchive:Archive":{"chunk_token_num":2049}}}`},
		{"fractional tokens", `{"parser_config":{"CodeArchive:Archive":{"chunk_token_num":1.5}}}`},
		{"overview type", `{"parser_config":{"CodeArchive:Archive":{"code_file_overview":"true"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setupArchiveDocumentPatchHandler(t, true)
			recorder := patchArchiveDocument(h, "user-1", "ds-1", tc.body)
			assertDocumentPatchError(t, recorder, common.CodeDataError, "ZIP_CONFIG_INVALID", "ZIP_CONFIG_INVALID")
		})
	}
}

func TestUpdateDatasetDocumentNonArchiveCannotSelectArchive(t *testing.T) {
	for _, body := range []string{
		`{"chunk_method":"code_archive"}`,
		`{"parser_id":"code_archive"}`,
		`{"parse_type":1,"parser_id":" code_archive "}`,
		`{"parser_id":"general","chunk_method":"code_archive"}`,
	} {
		t.Run(body, func(t *testing.T) {
			h := setupArchiveDocumentPatchHandler(t, false)
			recorder := patchArchiveDocument(h, "user-1", "ds-1", body)
			assertDocumentPatchError(t, recorder, common.CodeDataError, "ZIP_ONLY", "ZIP_ONLY")
		})
	}
}

func TestUpdateDatasetDocumentArchiveAllowsSupportedConfig(t *testing.T) {
	for _, body := range []string{
		`{"parser_config":{"CodeArchive:Archive":{"code_file_overview":true}}}`,
		`{"chunk_method":"code_archive","parser_config":{"CodeArchive:Archive":{"code_file_overview":true}}}`,
		`{"parse_type":1,"parser_id":"code_archive","pipeline_id":"","pipeline_version_id":"","parser_config":{"CodeArchive:Archive":{"code_file_overview":true}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			h := setupArchiveDocumentPatchHandler(t, true)
			recorder := patchArchiveDocument(h, "user-1", "ds-1", body)
			var response struct {
				Code common.ErrorCode                       `json:"code"`
				Data document.UpdateDatasetDocumentResponse `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			data := response.Data
			if recorder.Code != http.StatusOK || response.Code != common.CodeSuccess || data.ParserID != "code_archive" || data.PipelineID != nil || data.PipelineVersionID != nil {
				t.Fatalf("accepted patch response: %s", recorder.Body.String())
			}
			params, ok := data.ParserConfig["CodeArchive:Archive"].(map[string]any)
			if !ok || len(data.ParserConfig) != 1 || len(params) != 2 || params["chunk_token_num"] != float64(123) || params["code_file_overview"] != true {
				t.Fatalf("patch must preserve existing token budget: %s", recorder.Body.String())
			}
		})
	}
}

func TestUpdateDatasetDocumentOrdinaryErrorsKeepContract(t *testing.T) {
	for _, tc := range []struct {
		name, body, taskStatus, message string
		archive                         bool
		code                            common.ErrorCode
	}{
		{"invalid enabled on ZIP", `{"enabled":2}`, "", "`enabled` value invalid, only accept 0 or 1", true, common.CodeDataError},
		{"invalid enabled on ordinary document", `{"enabled":2}`, "", "`enabled` value invalid, only accept 0 or 1", false, common.CodeDataError},
		{"ordinary error containing ZIP reason", `{"parser_config":{}}`, "ZIP_CHUNK_METHOD_LOCKED", `document has unrecognized task status "ZIP_CHUNK_METHOD_LOCKED"`, true, common.CodeDataError},
		{"missing parse_type", `{"parser_id":"code_archive"}`, "", "parse_type is required", true, common.CodeDataError},
		{"malformed config envelope", `{"parser_config":[]}`, "", "Field: <parser_config> - Message: <Input should be a valid dictionary> - Value: <[]>", true, common.CodeDataError},
		{"invalid JSON", `{`, "", "unexpected end of JSON input", true, common.CodeDataError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setupArchiveDocumentPatchHandler(t, tc.archive)
			if tc.taskStatus != "" {
				if err := dao.DB.Create(&entity.IngestionTask{ID: "task-1", DocumentID: "doc-1", DatasetID: "ds-1", Status: tc.taskStatus}).Error; err != nil {
					t.Fatal(err)
				}
			}
			recorder := patchArchiveDocument(h, "user-1", "ds-1", tc.body)
			assertDocumentPatchError(t, recorder, tc.code, tc.message, "")
		})
	}
}

func TestUpdateDatasetDocumentOrdinaryDatabaseErrorKeepsContract(t *testing.T) {
	h := setupArchiveDocumentPatchHandler(t, true)
	if err := dao.DB.Migrator().DropTable(&entity.IngestionTask{}); err != nil {
		t.Fatal(err)
	}
	ctx := permission.WithIdentity(t.Context(), "user-1", "tenant-1")
	_, code, err := h.documentService.UpdateDatasetDocument(ctx, "ds-1", "doc-1", &document.UpdateDatasetDocumentRequest{ParserConfig: map[string]any{}}, map[string]bool{"parser_config": true})
	if err == nil || code != common.CodeServerError {
		t.Fatalf("missing table must produce a service error: code=%v err=%v", code, err)
	}
	recorder := patchArchiveDocument(h, "user-1", "ds-1", `{"parser_config":{}}`)
	assertDocumentPatchError(t, recorder, common.CodeServerError, err.Error(), "")
}

func TestUpdateDatasetDocumentArchivePermissionErrorsKeepContract(t *testing.T) {
	for _, tc := range []struct {
		name, user, dataset, message string
		code                         common.ErrorCode
	}{
		{"unauthenticated", "", "ds-1", "User not found", common.CodeUnauthorized},
		{"forbidden", "outsider", "ds-1", "Permission denied", common.CodeForbidden},
		{"missing dataset", "user-1", "missing", "Resource not found", common.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setupArchiveDocumentPatchHandler(t, true)
			if tc.user == "outsider" {
				if err := dao.DB.Create(&entity.User{ID: "outsider", Nickname: "outsider", Email: "outsider@test.local"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			recorder := patchArchiveDocument(h, tc.user, tc.dataset, `{"chunk_method":"general"}`)
			assertDocumentPatchError(t, recorder, tc.code, tc.message, "")
		})
	}
}

func TestArchiveTreeHandlerNonArchiveReasonContract(t *testing.T) {
	db := setupHandlerAccessDB(t)
	old := dao.DB
	dao.DB = db
	t.Cleanup(func() { dao.DB = old })
	router := gin.New()
	handler := &DocumentHandler{}
	router.GET("/datasets/:dataset_id/documents/:document_id/archive_tree", func(c *gin.Context) {
		c.Request = c.Request.WithContext(permission.WithIdentity(c.Request.Context(), "user-1", "tenant-1"))
		handler.ArchiveTree(c)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/datasets/ds-1/documents/doc-1/archive_tree", nil))
	var response struct {
		Code    common.ErrorCode `json:"code"`
		Message string           `json:"message"`
		Data    struct {
			ReasonCode string `json:"reason_code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != common.CodeDataError || response.Message != "ZIP_NOT_ARCHIVE" || response.Data.ReasonCode != "ZIP_NOT_ARCHIVE" {
		t.Fatalf("unexpected contract: %s", recorder.Body.String())
	}
}
