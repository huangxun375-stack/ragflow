package archivepublication

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/entity"
	"ragflow/internal/server"
	"ragflow/internal/server/config"
)

func TestCodeArchiveDeploymentUnsupportedPreservesOrdinaryFilters(t *testing.T) {
	for _, deployment := range []struct{ database, engine string }{
		{"mysql", "infinity"},
		{"mysql", "oceanbase"},
		{"mysql", "seekdb"},
		{"mysql", "serenedb"},
		{"mysql", "vastbase"},
	} {
		t.Run(deployment.database+"_"+deployment.engine, func(t *testing.T) {
			useArchiveDeploymentGateConfig(t, deployment.database, deployment.engine)
			if server.SupportsCodeArchive() {
				t.Fatal("fixture unexpectedly enables code archives")
			}
			if fields := ReadSourceFields(); fields != nil {
				t.Fatalf("unsupported engine received ZIP-only source fields: %v", fields)
			}
			for _, filter := range archiveDeploymentGateFilters() {
				t.Run(filter.name, func(t *testing.T) {
					for _, shape := range []string{"nil", "empty", "nested"} {
						t.Run(shape, func(t *testing.T) {
							original := archiveDeploymentOrdinaryFilter(shape)
							want := archiveDeploymentOrdinaryFilter(shape)
							got, err := filter.apply(t.Context(), original)
							if err != nil {
								t.Fatalf("ordinary filter must not require SQL: %v", err)
							}
							if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(original, want) {
								t.Fatalf("ordinary filter changed or gained archive predicates: got=%#v original=%#v want=%#v", got, original, want)
							}
							if original != nil {
								got["identity_probe"] = true
								if original["identity_probe"] != true {
									t.Fatal("unsupported deployment must return the original map, not a copy")
								}
								delete(got, "identity_probe")
							}
						})
					}
				})
			}
		})
	}
}

func TestCodeArchiveDeploymentSupportedStillFailsClosedWithoutDatabase(t *testing.T) {
	useArchiveDeploymentGateConfig(t, "mysql", "elasticsearch")
	if !server.SupportsCodeArchive() {
		t.Fatal("fixture must enable code archives")
	}
	wantFields := []string{"source_path_kwd", "start_line_int", "end_line_int", "code_lang_kwd", "archive_run_kwd", "archive_attempt_kwd"}
	if got := ReadSourceFields(); !reflect.DeepEqual(got, wantFields) {
		t.Fatalf("supported deployment source fields = %v, want %v", got, wantFields)
	}
	for _, filter := range archiveDeploymentGateFilters() {
		t.Run(filter.name, func(t *testing.T) {
			original := archiveDeploymentOrdinaryFilter("nested")
			got, err := filter.apply(t.Context(), original)
			if err == nil || got != nil {
				t.Fatalf("missing database must fail closed: filter=%#v err=%v", got, err)
			}
			if !reflect.DeepEqual(original, archiveDeploymentOrdinaryFilter("nested")) {
				t.Fatal("failed visibility check mutated the caller's filter")
			}
		})
	}
}

func TestCodeArchiveDeploymentElasticsearchReadsRetainPublicationChecks(t *testing.T) {
	for _, database := range []string{"oceanbase", "vastbase"} {
		t.Run(database, func(t *testing.T) {
			useArchiveDeploymentGateConfig(t, database, "elasticsearch")
			if server.SupportsCodeArchive() {
				t.Fatal("new ZIP ingestion must be disabled")
			}
			if len(ReadSourceFields()) != len(SourceFields) {
				t.Fatal("ES generation markers removed from projections")
			}
			for _, filter := range archiveDeploymentGateFilters() {
				if _, err := filter.apply(t.Context(), map[string]any{"doc_id": "doc"}); err == nil {
					t.Fatal("ES read bypassed publication database")
				}
			}
			db, scope := publicationDB(t)
			if err := db.Delete(&entity.Document{}, "id = ?", scope.DocID).Error; err != nil {
				t.Fatal(err)
			}
			got, err := Filter(t.Context(), db, []string{scope.KBID}, nil, nil)
			if err != nil || !reflect.DeepEqual(got, map[string]any{"must_not": map[string]any{"exists": "archive_run_kwd"}}) {
				t.Fatalf("orphan exclusion = %#v, %v", got, err)
			}
			if err := CheckRows(t.Context(), db, []string{scope.KBID}, []map[string]any{{"doc_id": scope.DocID, "archive_run_kwd": "run", "archive_attempt_kwd": "attempt"}}); !errors.Is(err, ErrNotPublished) {
				t.Fatalf("orphan accepted: %v", err)
			}
		})
	}
}

func archiveDeploymentGateFilters() []struct {
	name  string
	apply func(context.Context, map[string]any) (map[string]any, error)
} {
	return []struct {
		name  string
		apply func(context.Context, map[string]any) (map[string]any, error)
	}{
		{"Filter", func(ctx context.Context, original map[string]any) (map[string]any, error) {
			return Filter(ctx, nil, []string{"kb-ordinary"}, []string{"doc-ordinary"}, original)
		}},
		{"FilterForTenants", func(ctx context.Context, original map[string]any) (map[string]any, error) {
			return FilterForTenants(ctx, nil, []string{"tenant-ordinary"}, original)
		}},
	}
}

func archiveDeploymentOrdinaryFilter(shape string) map[string]any {
	switch shape {
	case "nil":
		return nil
	case "empty":
		return map[string]any{}
	default:
		return map[string]any{
			"doc_id":        []string{"doc-ordinary"},
			"available_int": 1,
			"and": []map[string]any{
				{"or": []map[string]any{{"category_kwd": "guide"}, {"category_kwd": "notes"}}},
				{"must_not": map[string]any{"exists": "compile_kwd"}},
			},
		}
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
}
