package service

import "ragflow/internal/service/archivepublication"

// copyArchiveCitationFields accepts either indexed or already-normalized
// provenance and exposes only the public source fields at the citation boundary.
func copyArchiveCitationFields(out, chunk map[string]any) {
	archivepublication.CopySourceFields(out, chunk)
	for _, key := range []string{"source_path", "start_line", "end_line", "code_lang", "archive_run_id"} {
		if value, ok := chunk[key]; ok {
			out[key] = value
		}
	}
	delete(out, "archive_run_kwd")
	delete(out, "archive_attempt_kwd")
}
