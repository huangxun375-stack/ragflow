package elasticsearch

import "testing"

func TestArchiveUpdateRequiresCompleteAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantError  bool
	}{
		{"success", `{"total":2,"updated":2,"version_conflicts":0,"noops":0,"failures":[],"timed_out":false}`, false},
		{"noops", `{"total":2,"updated":0,"noops":2,"failures":[]}`, false},
		{"failed_item", `{"total":2,"updated":1,"failures":[{"cause":{"type":"mapper_parsing_exception"}}]}`, true},
		{"version_conflict", `{"total":2,"updated":1,"version_conflicts":1,"failures":[]}`, true},
		{"timed_out", `{"total":2,"updated":1,"timed_out":true,"failures":[]}`, true},
		{"partial_count", `{"total":2,"updated":1,"failures":[]}`, true},
		{"missing_generation", `{"total":0,"updated":0,"failures":[]}`, true},
		{"missing_ack", `{}`, true},
		{"malformed", `{`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newBulkReplyEngine(t, tc.body, "")
			err := engine.UpdateChunks(t.Context(), map[string]any{"doc_id": "doc", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt"}, map[string]any{"available_int": 0}, "ragflow_tenant", "kb")
			if (err != nil) != tc.wantError {
				t.Fatalf("UpdateChunks error=%v wantError=%v", err, tc.wantError)
			}
		})
	}
}
