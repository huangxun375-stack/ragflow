package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/server"
	"ragflow/internal/server/config"
)

func TestArchiveQueueRejectsUnsupportedBackendBeforePublishingTask(t *testing.T) {
	svc, task := archiveTaskFixture(t)
	publisher := &recordingTaskPublisher{}
	svc.SetTaskPublisher(publisher)
	previous := server.GetConfig()
	var saved config.Config
	if previous != nil {
		saved = *previous
	}
	path := filepath.Join(t.TempDir(), "service.yaml")
	if err := os.WriteFile(path, []byte("general: {database: mysql, doc_engine: infinity}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(common.EnvDBType, "")
	t.Setenv(common.EnvDocEngine, "")
	if err := server.Init(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if previous != nil {
			*server.GetConfig() = saved
			return
		}
		defaults := &config.Config{}
		if err := defaults.ParseGeneralConfig(viper.New()); err != nil {
			t.Error(err)
		}
		*server.GetConfig() = *defaults
	})
	name := "snapshot.zip"
	if err := validateQueuedArchive(&entity.Document{Name: &name, Suffix: "zip", ParserID: codearchive.ParserID}); codearchive.Reason(err) != "ZIP_UNSUPPORTED_DEPLOYMENT" {
		t.Fatalf("queue validation: %v", err)
	}
	if _, err := svc.CreateAndEnqueue(t.Context(), task); codearchive.Reason(err) != "ZIP_UNSUPPORTED_DEPLOYMENT" {
		t.Fatalf("enqueue: %v", err)
	}
	if len(publisher.messages) != 0 {
		t.Fatal("unsupported ZIP reached the task broker")
	}
	for _, model := range []any{&entity.IngestionTask{}, &entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}} {
		var count int64
		if err := dao.DB.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("rejected task left %T records: %d %v", model, count, err)
		}
	}
}
