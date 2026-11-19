package service

import (
	"encoding/json"
	"testing"
)

func TestArchiveCitationFormattersPreserveSource(t *testing.T) {
	for _, input := range []map[string]any{
		{"chunk_id": "chunk", "source_path": "src/a###b.py", "start_line": 4, "end_line": 9, "code_lang": "python", "archive_run_id": "run", "archive_attempt_kwd": "attempt"},
		{"chunk_id": "chunk", "source_path_kwd": "src/a###b.py", "start_line_int": 4, "end_line_int": 9, "code_lang_kwd": "python", "archive_run_kwd": "run", "archive_attempt_kwd": "attempt"},
	} {
		maps := []map[string]any{chunksFormat([]map[string]any{input})[0], (&ChatSessionService{}).chunksFormat(map[string]any{"chunks": []map[string]any{input}})[0]}
		encoded, err := json.Marshal(formatChunks([]map[string]any{input})[0])
		if err != nil {
			t.Fatal(err)
		}
		var openAI map[string]any
		if err := json.Unmarshal(encoded, &openAI); err != nil {
			t.Fatal(err)
		}
		maps = append(maps, openAI)
		for _, out := range maps {
			if out["source_path"] != "src/a###b.py" || out["code_lang"] != "python" || out["archive_run_id"] != "run" {
				t.Fatalf("provenance lost: %#v", out)
			}
			if start, ok := asInt64(out["start_line"]); !ok || start != 4 {
				t.Fatalf("start_line=%v", out["start_line"])
			}
			if end, ok := asInt64(out["end_line"]); !ok || end != 9 {
				t.Fatalf("end_line=%v", out["end_line"])
			}
			if out["archive_attempt_kwd"] != nil || out["archive_run_kwd"] != nil {
				t.Fatalf("internal generation exposed: %#v", out)
			}
		}
	}
	ordinary := chunksFormat([]map[string]any{{"chunk_id": "normal"}})[0]
	if _, ok := ordinary["source_path"]; ok {
		t.Fatal("ordinary citation gained archive fields")
	}
}
