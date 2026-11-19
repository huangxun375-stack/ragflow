package component

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"ragflow/internal/common"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/server"
	"ragflow/internal/server/config"
)

func TestCodeArchiveUnsupportedBackendDoesNotRequireGrammarsOrReadStorage(t *testing.T) {
	old := server.GetConfig()
	var saved config.Config
	if old != nil {
		saved = *old
	}
	path := filepath.Join(t.TempDir(), "service.yaml")
	if err := os.WriteFile(path, []byte("general: {database: mysql, doc_engine: infinity}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(common.EnvDBType, "")
	t.Setenv(common.EnvDocEngine, "")
	t.Setenv("RAGFLOW_CODE_ARCHIVE_GRAMMAR_CACHE", filepath.Join(t.TempDir(), "missing"))
	if err := server.Init(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if old != nil {
			*server.GetConfig() = saved
			return
		}
		defaults := &config.Config{}
		if err := defaults.ParseGeneralConfig(viper.New()); err != nil {
			t.Error(err)
		}
		*server.GetConfig() = *defaults
	})
	if err := CheckCodeArchiveDeployment(codearchive.DefaultPolicy()); err != nil {
		t.Fatalf("ordinary deployment required ZIP grammars: %v", err)
	}
	_, err := (&CodeArchiveComponent{}).Invoke(context.Background(), nil, nil)
	if codearchive.Reason(err) != "ZIP_UNSUPPORTED_DEPLOYMENT" {
		t.Fatalf("archive invocation: %v", err)
	}
}
