//go:build integration

package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"ragflow/internal/ingestion/testutil"
)

func archiveSourceFiles() map[string]string {
	return map[string]string{
		"src/checksum.go":            "package sample\n\n// AuroraChecksum computes a stable checksum for repository search.\nfunc AuroraChecksum(value int) int {\n\treturn value*31 + 17\n}\n",
		"src/greeting.ts":            "export function greet(name: string): string {\n  return `hello ${name}`;\n}\n",
		"ui/Panel.vue":               "<template><p>{{ title }}</p></template>\n<script setup lang=\"ts\">\nconst title: string = 'archive panel';\n</script>\n<style scoped>p { color: blue; }</style>\n",
		"lib/count.lua":              "local function count(values)\n  return #values\nend\nreturn count\n",
		"include/answer.h":           "#pragma once\nstatic inline int answer(void) { return 42; }\n",
		".gitignore":                 "generated/\n",
		"generated/ignored.go":       "package generated\nconst Excluded = 1\n",
		"node_modules/dependency.js": "export const excluded = true;\n",
		".private.go":                "package private\nconst Excluded = true\n",
		"README.md":                  "This fixture exercises the Go ZIP ingestion path.\n",
	}
}

func TestRESTCodeArchiveUploadContract(t *testing.T) {
	blob, err := testutil.ArchiveZIP(archiveSourceFiles())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, config string
		tokens       int
		overview     bool
	}{
		{"default", "", 512, false},
		{"scoped_override", `{"CodeArchive:Archive":{"chunk_token_num":256,"code_file_overview":true}}`, 256, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataset := createRESTDataset(t, nil)
			id := dataset["id"].(string)
			result := restUpload(t, TestConfig, http.MethodPost, documentRESTPath(id), "file", "source.zip", blob, map[string]string{"parser_config": tc.config})
			docs := array(t, restOK(t, result)["data"])
			if len(docs) != 1 {
				t.Fatalf("uploaded documents=%d want=1", len(docs))
			}
			uploaded := object(t, docs[0])
			doc := getRESTDocument(t, id, uploaded["id"].(string))
			wantConfig := apiObject{"CodeArchive:Archive": apiObject{"chunk_token_num": float64(tc.tokens), "code_file_overview": tc.overview}}
			if doc["parser_id"] != "code_archive" || doc["type"] != "code" || doc["suffix"] != "zip" || !reflect.DeepEqual(doc["parser_config"], wantConfig) {
				t.Fatalf("ZIP did not select the fixed parser/config: %v", doc)
			}
			for _, key := range []string{"pipeline_id", "pipeline_version_id"} {
				if doc[key] != "" || uploaded[key] != nil {
					t.Fatalf("ZIP retained an external pipeline selection: %s", key)
				}
			}
			if doc["ingestion_status"] != "UNSTART" || doc["archive_status"] != "unstart" || doc["archive_run_id"] != nil || doc["archive_failure_reason"] != "" {
				t.Fatalf("ZIP upload unexpectedly started or published: %v", doc)
			}
			for _, key := range []string{"id", "parser_id", "parser_config", "archive_status", "archive_run_id"} {
				if !reflect.DeepEqual(uploaded[key], doc[key]) {
					t.Fatalf("upload and persisted %s differ", key)
				}
			}
			tree := object(t, restOK(t, restCall(t, TestConfig, http.MethodGet, documentRESTPath(id)+"/"+doc["id"].(string)+"/archive_tree", nil, nil))["data"])
			if tree["status"] != "not_ready" || tree["archive_run_id"] != nil || len(array(t, tree["tree"])) != 0 || tree["stats"] != nil {
				t.Fatalf("unpublished tree leaked content: %v", tree)
			}
		})
	}
}

func TestRESTCodeArchiveRestrictions(t *testing.T) {
	dataset := createRESTDataset(t, nil)
	id := dataset["id"].(string)
	blob, err := testutil.ArchiveZIP(archiveSourceFiles())
	if err != nil {
		t.Fatal(err)
	}
	docs := array(t, restOK(t, restUpload(t, TestConfig, http.MethodPost, documentRESTPath(id), "file", "readonly.zip", blob, nil))["data"])
	if len(docs) != 1 {
		t.Fatalf("uploaded documents=%d want=1", len(docs))
	}
	doc := object(t, docs[0])["id"].(string)
	path := documentRESTPath(id) + "/" + doc
	for _, tc := range []struct {
		name, method, suffix, reason string
		body                         apiObject
		status, code                 int
	}{
		{"parser_locked", http.MethodPatch, "", "ZIP_CHUNK_METHOD_LOCKED", apiObject{"parse_type": 1, "parser_id": "general"}, 200, 102},
		{"pipeline_locked", http.MethodPatch, "", "ZIP_CHUNK_METHOD_LOCKED", apiObject{"parse_type": 2, "pipeline_id": "other"}, 200, 102},
		{"unscoped_config", http.MethodPatch, "", "ZIP_CONFIG_INVALID", apiObject{"parser_config": apiObject{"chunk_token_num": 256}}, 200, 102},
		{"invalid_tokens", http.MethodPatch, "", "ZIP_CONFIG_INVALID", apiObject{"parser_config": apiObject{"CodeArchive:Archive": apiObject{"chunk_token_num": 0}}}, 200, 102},
		{"add_chunk", http.MethodPost, "/chunks", "ZIP_CHUNKS_READ_ONLY", apiObject{"content": "manual source"}, 200, 102},
		{"edit_chunk", http.MethodPatch, "/chunks/" + TestConfig.InvalidID, "ZIP_CHUNKS_READ_ONLY", apiObject{"content": "manual source"}, 500, 500},
		{"delete_chunks", http.MethodDelete, "/chunks", "ZIP_CHUNKS_READ_ONLY", apiObject{"delete_all": true}, 500, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := restCall(t, TestConfig, tc.method, path+tc.suffix, tc.body, nil)
			restCode(t, r, tc.status, tc.code)
			if !strings.Contains(r.JSON["message"].(string), tc.reason) {
				t.Fatalf("reason=%v want=%s", r.JSON["message"], tc.reason)
			}
			if tc.suffix == "" && object(t, r.JSON["data"])["reason_code"] != tc.reason {
				t.Fatalf("archive reason envelope=%v", r.JSON["data"])
			}
		})
	}
	for _, query := range []apiObject{{"page": 0}, {"page_size": 0}, {"available": "invalid"}} {
		restCode(t, restCall(t, TestConfig, http.MethodGet, path+"/chunks", nil, query), 400, 101)
	}
	after := getRESTDocument(t, id, doc)
	if after["parser_id"] != "code_archive" || after["archive_status"] != "unstart" || after["archive_run_id"] != nil {
		t.Fatalf("rejected mutation changed archive identity/state: %v", after)
	}
	ordinary := object(t, uploadRESTDocuments(t, TestConfig, id, []documentUpload{{Name: "notes.txt", Content: []byte("short original notes\n")}})[0])
	ordinaryPath := documentRESTPath(id) + "/" + ordinary["id"].(string)
	for _, tc := range []struct {
		method, suffix, reason string
		body                   apiObject
	}{
		{http.MethodGet, "/archive_tree", "ZIP_NOT_ARCHIVE", nil},
		{http.MethodPatch, "", "ZIP_ONLY", apiObject{"parse_type": 1, "parser_id": "code_archive"}},
	} {
		r := restCall(t, TestConfig, tc.method, ordinaryPath+tc.suffix, tc.body, nil)
		restCode(t, r, 200, 102)
		if object(t, r.JSON["data"])["reason_code"] != tc.reason {
			t.Fatalf("ordinary document archive restriction=%v", r.JSON)
		}
	}
}

func TestRESTCodeArchiveUploadRejections(t *testing.T) {
	dataset := createRESTDataset(t, nil)
	id := dataset["id"].(string)
	valid, err := testutil.ArchiveZIP(archiveSourceFiles())
	if err != nil {
		t.Fatal(err)
	}
	unsafe, err := testutil.ArchiveZIP(map[string]string{"../escape.go": "package escape\n"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, filename, config, reason string
		content                        []byte
		code                           int
	}{
		{"not_zip", "invalid.zip", "", "ZIP_INVALID_FORMAT", []byte("not a ZIP"), 102},
		{"unsafe_path", "unsafe.zip", "", "ZIP_UNSAFE_PATH", unsafe, 102},
		{"malformed_config", "config.zip", "{", "ZIP_CONFIG_INVALID", valid, 102},
		{"unknown_option", "config.zip", `{"CodeArchive:Archive":{"max_files":1}}`, "ZIP_CONFIG_INVALID", valid, 102},
		{"non_zip_extension", "source.tar.gz", "", "not been supported", valid, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := restUpload(t, TestConfig, http.MethodPost, documentRESTPath(id), "file", tc.filename, tc.content, map[string]string{"parser_config": tc.config})
			restCode(t, r, 200, tc.code)
			message, _ := r.JSON["message"].(string)
			if !strings.Contains(message, tc.reason) {
				t.Fatalf("archive rejection=%v want=%s", r.JSON, tc.reason)
			}
			if strings.HasPrefix(tc.reason, "ZIP_") && object(t, r.JSON["data"])["reason_code"] != tc.reason {
				t.Fatalf("archive reason envelope=%v", r.JSON["data"])
			}
			data, docs := listRESTDocuments(t, id, nil)
			if data["total"] != float64(0) || len(docs) != 0 {
				t.Fatalf("rejected upload persisted a document: %v", data)
			}
		})
	}
}
