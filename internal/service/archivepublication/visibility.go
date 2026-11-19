package archivepublication

import (
	"context"
	"fmt"
	"maps"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/server"
	"sort"

	"gorm.io/gorm"
)

var ErrNotPublished = &codearchive.Error{Code: "ZIP_NOT_PUBLISHED"}

// Filter applies publication visibility before the index computes pages,
// totals, KNN candidates or aggregations. Ordinary documents keep their filter.
func Filter(ctx context.Context, db *gorm.DB, kbIDs, docIDs []string, original map[string]any) (map[string]any, error) {
	if len(ReadSourceFields()) == 0 {
		return original, nil
	}
	generations, err := VisibleGenerations(ctx, db, kbIDs, docIDs)
	if err != nil {
		return nil, err
	}
	return visibilityFilter(generations, original), nil
}

// FilterForTenants scopes ID-only reads to the same tenants as their indices.
func FilterForTenants(ctx context.Context, db *gorm.DB, tenantIDs []string, original map[string]any) (map[string]any, error) {
	if len(ReadSourceFields()) == 0 {
		return original, nil
	}
	generations, err := visibleGenerations(ctx, db, tenantIDs, nil, nil)
	if err != nil {
		return nil, err
	}
	return visibilityFilter(generations, original), nil
}

func visibilityFilter(generations map[string]Generation, original map[string]any) map[string]any {
	if len(generations) == 0 {
		// An index row may outlive deletion of its SQL document. Such a row
		// still carries its generation marker and is never an ordinary row.
		ordinary := map[string]any{"must_not": map[string]any{"exists": "archive_run_kwd"}}
		if len(original) == 0 {
			return ordinary
		}
		if _, hasAnd := original["and"]; hasAnd {
			return map[string]any{"and": []map[string]any{original, ordinary}}
		}
		filtered := maps.Clone(original)
		filtered["and"] = []map[string]any{ordinary}
		return filtered
	}
	ids := make([]string, 0, len(generations))
	for id := range generations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	branches := []map[string]any{{"and": []map[string]any{
		{"must_not": map[string]any{"doc_id": ids}},
		{"must_not": map[string]any{"exists": "archive_run_kwd"}},
	}}}
	attempts := make([]string, 0, len(ids))
	for _, id := range ids {
		g := generations[id]
		if g.RunID != "" && g.AttemptID != "" {
			attempts = append(attempts, g.AttemptID)
		}
	}
	// Acquire assigns a globally unique UUID to one immutable doc/run tuple.
	// A terms predicate keeps the Boolean clause count independent of ZIP count;
	// CheckRows still validates the complete tuple after the index read.
	if len(attempts) > 0 {
		branches = append(branches, map[string]any{"archive_attempt_kwd": attempts})
	}
	visibility := map[string]any{"or": branches}
	if len(original) == 0 {
		return visibility
	}
	return map[string]any{"and": []map[string]any{original, visibility}}
}

// CheckRows rechecks publication after the index read. A concurrent reparse
// invalidates the whole response instead of returning a short page or stale total.
func CheckRows(ctx context.Context, db *gorm.DB, kbIDs []string, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	if db == nil {
		return fmt.Errorf("archive visibility database is not initialized")
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if id := scalar(row["doc_id"]); id != "" {
			ids = append(ids, id)
		} else if scalar(row["archive_run_kwd"]) != "" {
			return ErrNotPublished
		}
	}
	if len(ids) == 0 {
		return nil
	}
	generations, err := VisibleGenerations(ctx, db, kbIDs, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id := scalar(row["doc_id"])
		g, isArchive := generations[id]
		if !isArchive {
			if scalar(row["archive_run_kwd"]) != "" || scalar(row["archive_run_id"]) != "" {
				return ErrNotPublished
			}
			continue
		}
		run := scalar(row["archive_run_kwd"])
		if run == "" {
			run = scalar(row["archive_run_id"])
		}
		if g.RunID == "" || g.AttemptID == "" || run != g.RunID || scalar(row["archive_attempt_kwd"]) != g.AttemptID {
			return ErrNotPublished
		}
	}
	return nil
}

func scalar(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []string:
		if len(v) == 1 {
			return v[0]
		}
	case []any:
		if len(v) == 1 {
			return scalar(v[0])
		}
	}
	return ""
}

// SourceFields are indexed fields required for provenance and race checks.
var SourceFields = []string{"source_path_kwd", "start_line_int", "end_line_int", "code_lang_kwd", "archive_run_kwd", "archive_attempt_kwd"}

// ReadSourceFields follows the document engine, independently of whether new
// ZIP ingestion is enabled. ES reads must retain generation checks and orphan
// exclusion; other engines must not project unsupported archive columns.
func ReadSourceFields() []string {
	if cfg := server.GetConfig(); cfg != nil && cfg.DocEngineType() != "" && cfg.DocEngineType() != "elasticsearch" {
		return nil
	}
	return SourceFields
}

func IsSourceField(key string) bool {
	for _, field := range SourceFields {
		if key == field {
			return true
		}
	}
	return false
}

// CopySourceFields preserves scalar strings, including literal ### in paths.
// Generation fields stay available internally until all reads are checked.
func CopySourceFields(out, row map[string]any) {
	for _, pair := range [][2]string{{"source_path_kwd", "source_path"}, {"code_lang_kwd", "code_lang"}, {"archive_run_kwd", "archive_run_id"}} {
		if value, ok := row[pair[0]]; ok {
			out[pair[1]] = scalar(value)
		}
	}
	for _, pair := range [][2]string{{"start_line_int", "start_line"}, {"end_line_int", "end_line"}} {
		if value, ok := row[pair[0]]; ok {
			out[pair[1]] = value
		}
	}
	for _, key := range []string{"archive_run_kwd", "archive_attempt_kwd"} {
		if value, ok := row[key]; ok {
			out[key] = scalar(value)
		}
	}
}
