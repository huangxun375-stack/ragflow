//go:build integration

package elasticsearch

import (
	"context"
	"strings"
	"testing"
	"time"

	es "github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"ragflow/internal/common"
	"ragflow/internal/engine/types"
)

// TestArchiveProvenanceAndPublicationRealES exercises the production codec and
// query builder against a real service, including filtering before pagination.
func TestArchiveProvenanceAndPublicationRealES(t *testing.T) {
	if common.GetEnv(common.EnvESTest) != "1" {
		t.Skip("requires ES_TEST=1 and a real Elasticsearch service")
	}
	if err := common.InitLogger("warn", common.FileOutput{}, "archive_integration_test"); err != nil {
		t.Fatal(err)
	}
	cfg := getESTestConfig()
	client, err := es.NewClient(es.Config{Addresses: []string{cfg.Hosts}, Username: cfg.Username, Password: cfg.Password})
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{client: client}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	index := "ragflow_archive_contract_" + common.GenerateUUID()
	check := func(response *esapi.Response, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.IsError() {
			t.Fatalf("Elasticsearch request failed: %s", response.String())
		}
	}
	check(client.Indices.Create(index, client.Indices.Create.WithContext(ctx), client.Indices.Create.WithBody(strings.NewReader(`{
	  "settings":{"number_of_shards":1,"number_of_replicas":0},
	  "mappings":{"properties":{
	    "id":{"type":"keyword"},"doc_id":{"type":"keyword"},"kb_id":{"type":"keyword"},
	    "available_int":{"type":"integer"},"content_with_weight":{"type":"text"},
	    "source_path_kwd":{"type":"keyword"},"code_lang_kwd":{"type":"keyword"},
	    "start_line_int":{"type":"integer"},"end_line_int":{"type":"integer"},
	    "archive_run_kwd":{"type":"keyword"},"archive_attempt_kwd":{"type":"keyword"}
	  }}
	}`))))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		response, err := client.Indices.Delete([]string{index}, client.Indices.Delete.WithContext(cleanupCtx))
		if err != nil {
			t.Error(err)
			return
		}
		defer response.Body.Close()
		if response.IsError() {
			t.Errorf("cleanup failed: %s", response.String())
		}
	})
	source := "// src/a###b.py:L2-L3\ndef visibleArchiveWord():\n    return '<table>literal</table>'\n"
	chunks := []map[string]interface{}{{"id": "a-ordinary", "doc_id": "ordinary", "content_with_weight": "ordinary text", "available_int": 1}}
	for _, row := range []struct{ id, doc, run, attempt string }{
		{"b-ready", "zip-1", "run-new", "attempt-new"},
		{"c-stale", "zip-1", "run-old", "attempt-old"},
		{"d-pending", "zip-2", "run-pending", "attempt-pending"},
		{"e-orphan", "deleted-zip", "run-orphan", "attempt-orphan"},
	} {
		chunks = append(chunks, map[string]interface{}{"id": row.id, "doc_id": row.doc, "available_int": 1, "content_with_weight": source,
			"source_path_kwd": "src/a###b.py", "code_lang_kwd": "python", "start_line_int": 2, "end_line_int": 3, "archive_run_kwd": row.run, "archive_attempt_kwd": row.attempt})
	}
	if _, err := engine.InsertChunks(ctx, chunks, index, "kb", ""); err != nil {
		t.Fatal(err)
	}
	check(client.Indices.Refresh(client.Indices.Refresh.WithIndex(index), client.Indices.Refresh.WithContext(ctx)))
	// More values than the historical per-document Boolean clause ceiling.
	attempts := []string{"attempt-new"}
	for i := 0; i < 2048; i++ {
		attempts = append(attempts, common.GenerateUUID())
	}
	visibility := map[string]interface{}{"or": []map[string]interface{}{
		{"and": []map[string]interface{}{
			{"must_not": map[string]interface{}{"doc_id": []string{"zip-1", "zip-2"}}},
			{"must_not": map[string]interface{}{"exists": "archive_run_kwd"}},
		}},
		{"archive_attempt_kwd": attempts},
	}}
	fields := []string{"id", "doc_id", "content_with_weight", "source_path_kwd", "code_lang_kwd", "start_line_int", "end_line_int", "archive_run_kwd", "archive_attempt_kwd"}
	req := &types.SearchRequest{IndexNames: []string{index}, KbIDs: []string{"kb"}, Limit: 1, SelectFields: fields, Filter: visibility,
		OrderBy: &types.OrderByExpr{Fields: []types.OrderByField{{Field: "id", Type: types.SortAsc}}}}
	for offset, want := range []string{"a-ordinary", "b-ready"} {
		req.Offset = offset
		result, err := engine.Search(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if result.Total != 2 || len(result.Chunks) != 1 || result.Chunks[0]["id"] != want {
			t.Fatalf("page%d: %+v", offset, result)
		}
	}
	stored, err := engine.GetChunk(ctx, index, "b-ready", []string{"kb"})
	if err != nil {
		t.Fatal(err)
	}
	row := stored.(map[string]interface{})
	for field, want := range map[string]interface{}{"source_path_kwd": "src/a###b.py", "code_lang_kwd": "python", "start_line_int": float64(2), "end_line_int": float64(3), "archive_run_kwd": "run-new", "archive_attempt_kwd": "attempt-new", "content_with_weight": source} {
		if row[field] != want {
			t.Fatalf("%s=%#v want=%#v", field, row[field], want)
		}
	}
	req.Offset, req.Limit, req.OrderBy = 0, 10, nil
	req.MatchExprs = []interface{}{&types.MatchTextExpr{Fields: []string{"content_with_weight"}, MatchingText: "visibleArchiveWord", TopN: 10}}
	result, err := engine.Search(ctx, req)
	if err != nil || result.Total != 1 || len(result.Chunks) != 1 || result.Chunks[0]["id"] != "b-ready" {
		t.Fatalf("retrieval=%+v err=%v", result, err)
	}
	// Chunk enable/disable sends flat equality conditions through the
	// single-document path, for ordinary rows and published archive rows.
	for _, tc := range []struct {
		id        string
		condition map[string]interface{}
	}{
		{"a-ordinary", map[string]interface{}{"id": "a-ordinary", "doc_id": "ordinary"}},
		{"b-ready", map[string]interface{}{"id": "b-ready", "doc_id": "zip-1", "archive_run_kwd": "run-new", "archive_attempt_kwd": "attempt-new"}},
	} {
		if err := engine.UpdateChunks(ctx, tc.condition, map[string]interface{}{"id": tc.id, "available_int": 0}, index, "kb"); err != nil {
			t.Fatalf("switch %s: %v", tc.id, err)
		}
		updated, err := engine.GetChunk(ctx, index, tc.id, []string{"kb"})
		if err != nil {
			t.Fatal(err)
		}
		if got := updated.(map[string]interface{})["available_int"]; got != float64(0) {
			t.Fatalf("%s available_int=%#v", tc.id, got)
		}
	}
	if err := engine.UpdateChunks(ctx, map[string]interface{}{"id": "c-stale", "doc_id": "zip-1", "archive_run_kwd": "run-new", "archive_attempt_kwd": "attempt-new"}, map[string]interface{}{"id": "c-stale", "available_int": 0}, index, "kb"); err == nil {
		stale, getErr := engine.GetChunk(ctx, index, "c-stale", []string{"kb"})
		if getErr != nil {
			t.Fatal(getErr)
		}
		if stale.(map[string]interface{})["available_int"] != float64(1) {
			t.Fatal("generation-scoped switch changed a stale row")
		}
	}
}
