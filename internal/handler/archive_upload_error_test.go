package handler

import (
	"encoding/json"
	"errors"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/entity"
	"ragflow/internal/service/document"
	"ragflow/internal/storage"
)

func TestArchiveUploadErrorsRetainReasonWithoutFilename(t *testing.T) {
	factory := storage.GetStorageFactory()
	prior := factory.GetStorage()
	factory.SetStorage(storage.NewMemoryStorage())
	t.Cleanup(func() { factory.SetStorage(prior) })
	for _, tc := range []struct{ name, config, reason string }{
		{"invalid archive", "", "ZIP_INVALID_FORMAT"},
		{"malformed config", "{", "ZIP_CONFIG_INVALID"},
		{"nonobject config", "[]", "ZIP_CONFIG_INVALID"},
		{"null config", "null", "ZIP_CONFIG_INVALID"},
		{"unknown component", `{"Parser:Parser":{}}`, "ZIP_CONFIG_INVALID"},
		{"bad tokens", `{"CodeArchive:Archive":{"chunk_token_num":0}}`, "ZIP_CONFIG_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, recorder := setupUploadContext(t, "/api/v1/datasets/ds-1/documents", map[string]string{"parser_config": tc.config}, "private-project.zip", []byte("not a ZIP"))
			handler := &DocumentHandler{documentService: &document.DocumentService{}}
			handler.uploadLocalDocuments(ctx, &entity.Knowledgebase{ID: "ds-1", TenantID: "tenant-1"})
			var response struct {
				Code    common.ErrorCode `json:"code"`
				Message string           `json:"message"`
				Data    struct {
					Reason string `json:"reason_code"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != common.CodeDataError || response.Data.Reason != tc.reason || response.Message != tc.reason {
				t.Fatalf("unexpected archive error: %s", recorder.Body.String())
			}
		})
	}
}

func TestOrdinaryUploadFilenameCannotChangeErrorClassification(t *testing.T) {
	ctx, recorder := setupUploadContext(t, "/api/v1/datasets/ds-1/documents", nil, "ZIP_FAILED.txt", []byte("text"))
	fake := &fakeDocumentService{uploadLocalErrs: []document.UploadFailure{{Filename: "ZIP_FAILED.txt", Err: errors.New("storage unavailable"), Code: common.CodeServerError}}}
	handler := &DocumentHandler{documentService: fake}
	handler.uploadLocalDocuments(ctx, &entity.Knowledgebase{ID: "ds-1"})
	var response struct {
		Code    common.ErrorCode `json:"code"`
		Message string           `json:"message"`
		Data    json.RawMessage  `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != common.CodeServerError || response.Message != "ZIP_FAILED.txt: storage unavailable" || len(response.Data) != 0 {
		t.Fatalf("ordinary response changed: %s", recorder.Body.String())
	}
}
