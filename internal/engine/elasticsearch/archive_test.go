package elasticsearch

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestArchivePublicationNestedNegation(t *testing.T) {
	filter := map[string]interface{}{"and": []map[string]interface{}{
		{"kb_id": "kb"},
		{"or": []map[string]interface{}{
			{"must_not": map[string]interface{}{"doc_id": []string{"zip"}}},
			{"doc_id": "zip", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt"},
		}},
	}}
	query := buildBoolQueryFromConditionRaw(filter, nil, false, false)
	encoded, _ := json.Marshal(query)
	if !strings.Contains(string(encoded), `"must_not"`) || !strings.Contains(string(encoded), `"terms":{"doc_id":["zip"]}`) {
		t.Fatalf("lost archive exclusion: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"archive_attempt_kwd":"attempt"`) {
		t.Fatalf("lost attempt: %s", encoded)
	}
}

func TestArchiveAttemptSetConstantClauseFilter(t *testing.T) {
	filter := map[string]any{"and": []map[string]any{
		{"kb_id": []string{"kb"}},
		{"or": []map[string]any{
			{"and": []map[string]any{{"must_not": map[string]any{"doc_id": []string{"zip-1", "zip-2"}}}, {"must_not": map[string]any{"exists": "archive_run_kwd"}}}},
			{"archive_attempt_kwd": []string{"attempt-1", "attempt-2"}},
		}},
	}}
	query := buildBoolQueryFromConditionRaw(filter, nil, false, false)
	encoded, _ := json.Marshal(query)
	for _, want := range []string{`"terms":{"archive_attempt_kwd":["attempt-1","attempt-2"]}`, `"terms":{"doc_id":["zip-1","zip-2"]}`, `"exists":{"field":"archive_run_kwd"}`, `"must_not"`, `"should"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("missing %s: %s", want, encoded)
		}
	}
}

func TestArchiveEmptyBooleanFiltersFailClosed(t *testing.T) {
	query := buildBoolQueryFromConditionRaw(map[string]interface{}{"or": []map[string]interface{}{}}, nil, false, false)
	encoded, _ := json.Marshal(query)
	if !strings.Contains(string(encoded), `"match_none"`) {
		t.Fatalf("empty OR widened scope: %s", encoded)
	}
}
