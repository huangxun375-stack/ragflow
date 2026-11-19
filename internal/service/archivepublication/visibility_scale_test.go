package archivepublication

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"ragflow/internal/common"
	"ragflow/internal/engine/types"
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
)

func matchVisibility(filter, row map[string]any) bool {
	for key, value := range filter {
		switch key {
		case "and":
			for _, child := range types.FilterClauses(value) {
				if !matchVisibility(child, row) {
					return false
				}
			}
		case "or":
			match := false
			for _, child := range types.FilterClauses(value) {
				match = match || matchVisibility(child, row)
			}
			if !match {
				return false
			}
		case "must_not":
			if matchVisibility(value.(map[string]any), row) {
				return false
			}
		case "exists":
			if row[value.(string)] == nil {
				return false
			}
		default:
			if list, ok := value.([]string); ok {
				match := false
				for _, v := range list {
					match = match || v == row[key]
				}
				if !match {
					return false
				}
			} else if !reflect.DeepEqual(value, row[key]) {
				return false
			}
		}
	}
	return true
}

func TestArchiveVisibilityEquivalentAcrossScopesAndLifecycle(t *testing.T) {
	for _, state := range []string{"unqueued", "pending", "running", "failed", "cancelled", "deleting", "reparse", "stopped", "ready", "deleted"} {
		t.Run(state, func(t *testing.T) {
			db, scope := publicationDB(t)
			current, err := Acquire(t.Context(), db, scope, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "unqueued":
				err = db.Where("doc_id = ?", scope.DocID).Delete(&entity.DocumentCodeArchive{}).Error
			case "reparse":
				err = db.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Update("pipeline_log_id", "next-run").Error
			case "stopped":
				err = db.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Update("status", common.STOPPED).Error
			case "deleted":
				err = db.Where("id = ?", scope.DocID).Delete(&entity.Document{}).Error
			case "ready":
			default:
				err = db.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ?", scope.DocID).Update("status", state).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, docScope := range []bool{false, true} {
				kbIDs, docIDs := []string{"kb"}, []string(nil)
				if docScope {
					kbIDs, docIDs = nil, []string{"doc", "ordinary"}
				}
				generations, err := VisibleGenerations(t.Context(), db, kbIDs, docIDs)
				if err != nil {
					t.Fatal(err)
				}
				filter, err := Filter(t.Context(), db, kbIDs, docIDs, nil)
				if err != nil {
					t.Fatal(err)
				}
				rows := []map[string]any{
					{"doc_id": "ordinary"},
					{"doc_id": "doc", "archive_run_kwd": scope.RunID, "archive_attempt_kwd": current.AttemptID},
					{"doc_id": "doc", "archive_run_kwd": scope.RunID, "archive_attempt_kwd": "old-attempt"},
					{"doc_id": "doc", "archive_run_kwd": "old-run", "archive_attempt_kwd": "old-attempt"},
					{"doc_id": "deleted", "archive_run_kwd": "orphan", "archive_attempt_kwd": "orphan-attempt"},
				}
				if state != "deleted" {
					rows = append(rows, map[string]any{"doc_id": "doc"})
				}
				for _, row := range rows {
					g, archive := generations[row["doc_id"].(string)]
					old := !archive && row["archive_run_kwd"] == nil || archive && g.RunID != "" && g.AttemptID != "" && row["archive_run_kwd"] == g.RunID && row["archive_attempt_kwd"] == g.AttemptID
					if got := matchVisibility(filter, row); got != old {
						t.Fatalf("scope=%v row=%v new=%v old=%v", docScope, row, got, old)
					}
					if row["doc_id"] == "ordinary" && !old {
						t.Fatal("ordinary document excluded")
					}
				}
			}
		})
	}
}

func TestArchiveTenantScopeAndNoUnboundedLookup(t *testing.T) {
	db, scope := publicationDB(t)
	for _, row := range []any{&entity.Knowledgebase{ID: "other-kb", TenantID: "other-tenant"}, &entity.Document{ID: "other-zip", KbID: "other-kb", ParserID: codearchive.ParserID, ParserConfig: entity.JSONMap{}}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	g, err := visibleGenerations(t.Context(), db, []string{"tenant"}, nil, nil)
	if err != nil || len(g) != 1 {
		t.Fatalf("%v %v", g, err)
	}
	if _, ok := g[scope.DocID]; !ok {
		t.Fatal(g)
	}
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	g, err = visibleGenerations(t.Context(), db, []string{"tenant"}, nil, nil)
	if err != nil || g[scope.DocID].AttemptID != current.AttemptID || len(g) != 1 {
		t.Fatalf("published tenant query: %v %v", g, err)
	}
	filter, err := FilterForTenants(t.Context(), db, []string{"tenant"}, map[string]any{"id": "chunk"})
	if err != nil {
		t.Fatal(err)
	}
	if matchVisibility(filter, map[string]any{"id": "chunk", "doc_id": "doc"}) {
		t.Fatal("unqueued archive visible")
	}
	if matchVisibility(filter, map[string]any{"id": "chunk", "doc_id": "orphan", "archive_run_kwd": "gone"}) {
		t.Fatal("deleted archive visible")
	}
	if _, err = Filter(t.Context(), db, nil, nil, nil); err == nil {
		t.Fatal("unbounded filter accepted")
	}
	if _, err = VisibleGenerations(t.Context(), db, nil, nil); err == nil {
		t.Fatal("unbounded lookup accepted")
	}
	for _, tenants := range [][]string{nil, {""}, {" , "}} {
		if _, err = FilterForTenants(t.Context(), db, tenants, nil); err == nil {
			t.Fatal("empty tenant scope accepted")
		}
	}
}

func TestArchiveAttemptsAreGloballyUniqueAndBindOneTuple(t *testing.T) {
	db, first := publicationDB(t)
	a, err := Acquire(t.Context(), db, first, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(t.Context(), db, a, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	second := first
	second.DocID, second.TaskID, second.RunID = "doc2", "task2", "run2"
	for _, row := range []any{&entity.Document{ID: second.DocID, KbID: second.KBID, ParserID: codearchive.ParserID, ParserConfig: entity.JSONMap{}}, &entity.IngestionTask{ID: second.TaskID, DocumentID: second.DocID, DatasetID: second.KBID, Status: common.RUNNING, PipelineLogID: &second.RunID}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := Queue(t.Context(), db, second); err != nil {
		t.Fatal(err)
	}
	b, err := Acquire(t.Context(), db, second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if a.AttemptID == b.AttemptID || len(a.AttemptID) != 32 || len(b.AttemptID) != 32 {
		t.Fatal("invalid attempt identity", a, b)
	}
	if err := Publish(t.Context(), db, b, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&entity.ArchiveAttempt{AttemptID: a.AttemptID, DocID: b.DocID, RunID: b.RunID}).Error; err == nil {
		t.Fatal("attempt reused across tuples")
	}
	for _, docIDs := range [][]string{nil, {a.DocID}, {b.DocID}} {
		filter, err := Filter(t.Context(), db, []string{"kb"}, docIDs, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range []struct{ doc, run, attempt string }{{a.DocID, a.RunID, a.AttemptID}, {b.DocID, b.RunID, b.AttemptID}} {
			want := len(docIDs) == 0 || docIDs[0] == scope.doc
			if got := matchVisibility(filter, map[string]any{"doc_id": scope.doc, "archive_run_kwd": scope.run, "archive_attempt_kwd": scope.attempt}); got != want {
				t.Fatalf("scope=%v tuple=%v got=%v", docIDs, scope, got)
			}
		}
	}
	// Corrupt index tuples are outside the writer invariant; the mandatory
	// post-read check still rejects a stolen attempt attached to another doc/run.
	for _, row := range []map[string]any{{"doc_id": a.DocID, "archive_run_kwd": b.RunID, "archive_attempt_kwd": a.AttemptID}, {"doc_id": b.DocID, "archive_run_kwd": a.RunID, "archive_attempt_kwd": a.AttemptID}} {
		if err := CheckRows(t.Context(), db, []string{"kb"}, []map[string]any{row}); err == nil {
			t.Fatal("corrupt tuple accepted", row)
		}
	}
}

func TestArchiveVisibilityClauseCountIndependentOfArchiveCount(t *testing.T) {
	for _, n := range []int{1, 100, 10000} {
		g := map[string]Generation{}
		for i := 0; i < n; i++ {
			g[fmt.Sprint(i)] = Generation{RunID: fmt.Sprint("run", i), AttemptID: common.GenerateUUID()}
		}
		filter := visibilityFilter(g, nil)
		branches := filter["or"].([]map[string]any)
		if len(branches) != 2 || len(branches[0]["and"].([]map[string]any)) != 2 || len(branches[1]) != 1 || len(branches[1]["archive_attempt_kwd"].([]string)) != n {
			t.Fatalf("n=%d shape=%v", n, filter)
		}
	}
}
