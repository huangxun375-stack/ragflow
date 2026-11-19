package service

import (
	"strings"
	"testing"

	"ragflow/internal/entity"
	"ragflow/internal/permission"
)

func TestArchiveEmbeddingIdentityPinsParametersButAllowsCredentialRotation(t *testing.T) {
	svc, task := archiveTaskFixture(t)
	svc.SetTaskPublisher(&recordingTaskPublisher{})
	created, err := svc.CreateAndEnqueue(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	_ = created
	db := modelDatabase(t.Context())
	ctx := permission.WithTenantID(t.Context(), "tenant")
	_, original, err := ArchiveEmbeddingModel(ctx, db, "archive-model")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(original.Parameters, "credential-not-in-snapshot") {
		t.Fatal("credential persisted")
	}
	if !strings.Contains(original.Parameters, "max_tokens") || !strings.Contains(original.Parameters, "2048") {
		t.Fatalf("parameters not saved: %s", original.Parameters)
	}
	if err = db.Model(&entity.TenantModelInstance{}).Where("id = ?", "archive-instance").Update("api_key", "rotated-secret").Error; err != nil {
		t.Fatal(err)
	}
	_, rotated, err := ArchiveEmbeddingModel(ctx, db, "archive-model")
	if err != nil || rotated != original {
		t.Fatalf("credential rotation changed inputs: %v", err)
	}
	if err = db.Model(&entity.TenantModel{}).Where("id = ?", "archive-model").Update("extra", `{"max_tokens":1024}`).Error; err != nil {
		t.Fatal(err)
	}
	_, changed, err := ArchiveEmbeddingModel(ctx, db, "archive-model")
	if err != nil || changed == original {
		t.Fatalf("model parameter drift missed: %v", err)
	}
	if err = db.Model(&entity.TenantModelInstance{}).Where("id = ?", "archive-instance").Update("extra", `{"base_url":"https://other.example/v1"}`).Error; err != nil {
		t.Fatal(err)
	}
	_, endpoint, err := ArchiveEmbeddingModel(ctx, db, "archive-model")
	if err != nil || endpoint == changed {
		t.Fatalf("endpoint drift missed: %v", err)
	}
}

func TestArchiveEndpointExcludesURLCredentials(t *testing.T) {
	got := archiveEndpoint("https://username:password@example.com/v1?api_key=secret&deployment=pinned#fragment")
	if got != "https://example.com/v1?deployment=pinned" {
		t.Fatalf("unsafe endpoint: %s", got)
	}
}
