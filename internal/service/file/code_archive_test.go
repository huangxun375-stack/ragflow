package file

import (
	"mime/multipart"
	"strings"
	"testing"

	"ragflow/internal/entity"
	"ragflow/internal/permission"
	"ragflow/internal/storage"
)

func TestArchiveFileManagerStoreOnlyRemainsAllowed(t *testing.T) {
	db := setupFolderTestDB(t)
	parent := &entity.File{ID: "folder-zip", ParentID: "root-1", TenantID: "tenant-1", CreatedBy: "tenant-1", Name: "root", Type: FileTypeFolder}
	if err := db.Create(parent).Error; err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStorage()
	factory := storage.GetStorageFactory()
	old := factory.GetStorage()
	factory.SetStorage(store)
	t.Cleanup(func() { factory.SetStorage(old) })
	data, err := testFileService().UploadFile(fileTestContext(t, "tenant-1", "tenant-1"), parent.ID, []*multipart.FileHeader{makeUploadInfoHeader(t, "repo.zip", []byte("stored without parsing"))}, 1024)
	if err != nil || len(data) != 1 {
		t.Fatalf("store only: data=%v err=%v", data, err)
	}
	var count int64
	if err := db.Model(&entity.Document{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("document rows=%d err=%v", count, err)
	}
}

func TestArchiveAttachmentUploadAndConsumptionRejected(t *testing.T) {
	store := storage.NewMemoryStorage()
	factory := storage.GetStorageFactory()
	old := factory.GetStorage()
	factory.SetStorage(store)
	t.Cleanup(func() { factory.SetStorage(old) })
	ctx := permission.WithIdentity(t.Context(), "user-1", "tenant-1")
	svc := testFileService()
	fh := makeUploadInfoHeader(t, "repo.ZIP", []byte("zip bytes"))
	if _, err := svc.UploadInfos(ctx, []*multipart.FileHeader{fh}); err == nil || !strings.Contains(err.Error(), "ZIP_ATTACHMENT_UNSUPPORTED") {
		t.Fatalf("upload: %v", err)
	}
	if err := store.Put(ctx, "user-1-downloads", "file-1", []byte("existing archive")); err != nil {
		t.Fatal(err)
	}
	files := []map[string]any{{"id": "file-1", "created_by": "user-1", "name": "repo.ZIP", "mime_type": "image/png"}}
	if _, _, err := svc.GetFileContents(ctx, files); err == nil || !strings.Contains(err.Error(), "ZIP_ATTACHMENT_UNSUPPORTED") {
		t.Fatalf("chat consume: %v", err)
	}
	if _, err := svc.ParseAgentUploads(ctx, files, ""); err == nil || !strings.Contains(err.Error(), "ZIP_ATTACHMENT_UNSUPPORTED") {
		t.Fatalf("agent consume: %v", err)
	}
	if _, err := svc.UploadFromURL(ctx, "https://example.com/repo.zip"); err == nil || !strings.Contains(err.Error(), "ZIP_ATTACHMENT_UNSUPPORTED") {
		t.Fatalf("URL attachment: %v", err)
	}
}
