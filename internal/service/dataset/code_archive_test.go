package dataset

import (
	"ragflow/internal/common"
	"ragflow/internal/entity"
	"testing"
)

func TestArchiveParserCannotBeDatasetDefault(t *testing.T) {
	if err := validateParserID("code_archive"); err == nil {
		t.Fatal("archive accepted as dataset default")
	}
}

func TestArchiveMetadataConfigCannotEnableExtractor(t *testing.T) {
	db := setupServiceTestDB(t)
	pushServiceDB(t, db)
	insertCreateDatasetTenant(t, "user-1")
	insertDatasetMetadataConfigKB(t, "kb-1", "user-1")
	config := entity.JSONMap{"CodeArchive:Archive": map[string]any{"chunk_token_num": 512, "code_file_overview": false}}
	if err := db.Create(&entity.Document{ID: "doc-1", KbID: "kb-1", ParserID: "code_archive", Type: "code", Name: new("repo.zip"), ParserConfig: config, CreatedBy: "user-1", Status: new("1")}).Error; err != nil {
		t.Fatal(err)
	}
	_, code, err := testDatasetServiceForDocumentMetadataConfig(t).UpdateDocumentMetadataConfig(withDatasetTestContext(t.Context(), "user-1", "user-1"), "kb-1", "doc-1", map[string]any{"metadata": map[string]any{"enabled": true}})
	if err == nil || code == common.CodeSuccess {
		t.Fatal("ZIP metadata extractor configuration accepted")
	}
}
