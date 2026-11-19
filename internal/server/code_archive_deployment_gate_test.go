package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/server/config"
)

func TestCodeArchiveDeploymentAllowsNilAndZeroConfig(t *testing.T) {
	previous := globalConfig
	t.Cleanup(func() { globalConfig = previous })

	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{name: "nil"},
		{name: "zero", cfg: &config.Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globalConfig = tc.cfg
			if !SupportsCodeArchive() || !GetConfig().SupportsCodeArchive() {
				t.Fatal("uninitialized configuration must retain the MySQL/Elasticsearch archive default")
			}
		})
	}
}

func TestCodeArchiveDeploymentUsesInitializedConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       bool
	}{
		{name: "defaults", yaml: "{}\n", want: true},
		{name: "mysql_elasticsearch", yaml: "general: {database: mysql, doc_engine: elasticsearch}\n", want: true},
		{name: "mysql_infinity", yaml: "general: {database: mysql, doc_engine: infinity}\n"},
		{name: "mysql_oceanbase", yaml: "general: {database: mysql, doc_engine: oceanbase}\n"},
		{name: "mysql_seekdb", yaml: "general: {database: mysql, doc_engine: seekdb}\n"},
		{name: "mysql_vastbase", yaml: "general: {database: mysql, doc_engine: vastbase}\n"},
		{name: "mysql_serenedb", yaml: "general: {database: mysql, doc_engine: serenedb}\n"},
		{name: "mysql_opensearch", yaml: "general: {database: mysql, doc_engine: opensearch}\n"},
		{name: "oceanbase_elasticsearch", yaml: "general: {database: oceanbase, doc_engine: elasticsearch}\n"},
		{name: "vastbase_elasticsearch", yaml: "general: {database: vastbase, doc_engine: elasticsearch}\n"},
		{name: "vastbase_vastbase", yaml: "general: {database: vastbase, doc_engine: vastbase}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initArchiveDeploymentGateConfig(t, tc.yaml, "", "")
			if got := SupportsCodeArchive(); got != tc.want {
				t.Fatalf("SupportsCodeArchive() = %t, want %t", got, tc.want)
			}
			if got := GetConfig().SupportsCodeArchive(); got != tc.want {
				t.Fatalf("GetConfig().SupportsCodeArchive() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestCodeArchiveDeploymentUsesEffectiveEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, database, engine, envDatabase, envEngine string
		want                                           bool
	}{
		{name: "engine_override_disables", database: "mysql", engine: "elasticsearch", envEngine: "infinity"},
		{name: "oceanbase_override_disables", database: "mysql", engine: "elasticsearch", envDatabase: "oceanbase"},
		{name: "vastbase_override_disables", database: "mysql", engine: "elasticsearch", envDatabase: "vastbase"},
		{name: "engine_override_enables", database: "mysql", engine: "infinity", envEngine: "elasticsearch", want: true},
		{name: "database_override_enables", database: "vastbase", engine: "elasticsearch", envDatabase: "mysql", want: true},
		{name: "both_overrides_enables", database: "vastbase", engine: "infinity", envDatabase: "MYSQL", envEngine: "ELASTICSEARCH", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := fmt.Sprintf("general: {database: %s, doc_engine: %s}\n", tc.database, tc.engine)
			initArchiveDeploymentGateConfig(t, yaml, tc.envDatabase, tc.envEngine)
			wantDatabase, wantEngine := tc.database, tc.engine
			if tc.envDatabase != "" {
				wantDatabase = strings.ToLower(tc.envDatabase)
			}
			if tc.envEngine != "" {
				wantEngine = strings.ToLower(tc.envEngine)
			}
			if got := GetConfig().DatabaseType(); got != wantDatabase {
				t.Fatalf("effective database = %q, want %q", got, wantDatabase)
			}
			if got := GetConfig().DocEngineType(); got != wantEngine {
				t.Fatalf("effective document engine = %q, want %q", got, wantEngine)
			}
			if got := SupportsCodeArchive(); got != tc.want {
				t.Fatalf("effective deployment %s/%s: SupportsCodeArchive() = %t, want %t",
					GetConfig().DatabaseType(), GetConfig().DocEngineType(), got, tc.want)
			}
			if got := GetConfig().SupportsCodeArchive(); got != tc.want {
				t.Fatalf("GetConfig().SupportsCodeArchive() = %t, want %t", got, tc.want)
			}
		})
	}
}

func initArchiveDeploymentGateConfig(t *testing.T, yaml, databaseEnv, engineEnv string) {
	t.Helper()
	previousConfig, previousViper := globalConfig, globalViper
	t.Cleanup(func() { globalConfig, globalViper = previousConfig, previousViper })
	t.Setenv(common.EnvDBType, databaseEnv)
	t.Setenv(common.EnvDocEngine, engineEnv)
	t.Setenv(common.EnvStorageImpl, "")
	path := filepath.Join(t.TempDir(), "service_conf.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("initialize deployment configuration: %v", err)
	}
}
