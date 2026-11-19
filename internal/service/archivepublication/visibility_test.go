package archivepublication

import (
	"errors"
	"ragflow/internal/entity"
	"reflect"
	"testing"
	"time"
)

func TestVisibilityFilterKeepsOrdinaryAndOnlyPublishedAttempt(t *testing.T) {
	db, scope := publicationDB(t)
	original := map[string]any{"available_int": 1, "or": []map[string]any{{"category_kwd": "a"}, {"category_kwd": "b"}}}
	filter, err := Filter(t.Context(), db, []string{"kb"}, nil, original)
	if err != nil {
		t.Fatal(err)
	}
	and, ok := filter["and"].([]map[string]any)
	if !ok || len(and) != 2 || !reflect.DeepEqual(and[0], original) {
		t.Fatalf("filter=%#v", filter)
	}
	if branches := and[1]["or"].([]map[string]any); len(branches) != 1 {
		t.Fatalf("unpublished admitted: %#v", branches)
	} else if ordinary := branches[0]["and"].([]map[string]any); len(ordinary) != 2 || ordinary[1]["must_not"].(map[string]any)["exists"] != "archive_run_kwd" {
		t.Fatalf("orphan archive row admitted as ordinary: %#v", ordinary)
	}
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	filter, err = Filter(t.Context(), db, []string{"kb"}, nil, original)
	if err != nil {
		t.Fatal(err)
	}
	branches := filter["and"].([]map[string]any)[1]["or"].([]map[string]any)
	if len(branches) != 2 || !reflect.DeepEqual(branches[1]["archive_attempt_kwd"], []string{current.AttemptID}) || len(branches[1]) != 1 {
		t.Fatalf("ready filter=%#v", filter)
	}
	rows := []map[string]any{{"doc_id": "doc", "archive_run_kwd": scope.RunID, "archive_attempt_kwd": current.AttemptID}, {"doc_id": "ordinary"}}
	if err := CheckRows(t.Context(), db, []string{"kb"}, rows); err != nil {
		t.Fatal(err)
	}
	db.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ?", "doc").Update("status", "failed")
	if err := CheckRows(t.Context(), db, []string{"kb"}, rows); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("stale rows accepted: %v", err)
	}
}

func TestVisibilityFilterExcludesOrphanRowsWhenNoArchiveDocumentsRemain(t *testing.T) {
	db, _ := publicationDB(t)
	if err := db.Where("id = ?", "doc").Delete(&entity.Document{}).Error; err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][]string{{"absent-kb"}, {"kb"}} {
		original := map[string]any{"doc_id": "other", "must_not": map[string]any{"exists": "compile_kwd"}}
		filter, err := Filter(t.Context(), db, scope, nil, original)
		if err != nil {
			t.Fatal(err)
		}
		and, ok := filter["and"].([]map[string]any)
		if !ok || len(and) != 1 || and[0]["must_not"].(map[string]any)["exists"] != "archive_run_kwd" {
			t.Fatalf("orphan exclusion missing: %#v", filter)
		}
		if filter["doc_id"] != "other" || filter["must_not"].(map[string]any)["exists"] != "compile_kwd" {
			t.Fatalf("original filter changed: %#v", filter)
		}
		if original["and"] != nil {
			t.Fatal("original filter mutated")
		}
	}
}

func TestVisibilityFailsClosedWithoutDatabase(t *testing.T) {
	if _, err := Filter(t.Context(), nil, []string{"kb"}, nil, nil); err == nil {
		t.Fatal("missing DB allowed")
	}
}

func TestSourceMappingPreservesLiteralPaths(t *testing.T) {
	row := map[string]any{"source_path_kwd": []string{"src/a###b.ts"}, "start_line_int": float64(4), "end_line_int": float64(9), "code_lang_kwd": "typescript", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt"}
	out := map[string]any{}
	CopySourceFields(out, row)
	if out["source_path"] != "src/a###b.ts" || out["code_lang"] != "typescript" || out["archive_run_id"] != "run" || out["start_line"] != float64(4) {
		t.Fatalf("source=%#v", out)
	}
}
