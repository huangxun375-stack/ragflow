//go:build integration && e2e && manual

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ragflow/internal/ingestion/testutil"
)

func TestArchiveHTTPWorkerRestartAndDeleteWhileRunning(t *testing.T) {
	if os.Getenv("CODE_ARCHIVE_E2E_RESTART") != "1" {
		t.Skip("set CODE_ARCHIVE_E2E_RESTART=1 with a host-side signal observer")
	}
	if os.Getenv("CODE_ARCHIVE_E2E_SIGNAL_DIR") == "" {
		t.Fatal("CODE_ARCHIVE_E2E_SIGNAL_DIR must name a fresh directory watched by the host")
	}
	blob, sources := archiveRestartSource(t)
	d := newArchiveDeployment(t, 22*time.Minute, "real worker restart, fenced publication, delete during running", "CODE_ARCHIVE_E2E_RESTART_REPORT")
	d.provision()
	// A checkpoint identifies this dataset if the test process is killed.
	d.writeReport()
	sum := sha256.Sum256(blob)
	d.report["zip_sha256"], d.report["zip_bytes"], d.report["candidate_source_files"] = hex.EncodeToString(sum[:]), len(blob), len(sources)
	doc := d.upload("restart-source.zip", blob)
	d.report["restart_document_id"] = doc
	d.writeReport()
	d.start(doc, false)
	d.restartObserveRunning(doc, "a")
	signaled := time.Now()
	d.restartSignal("restart-ingestor", "restarted", map[string]string{"document_id": doc}, 3*time.Minute)
	ctx := d.ctx
	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Minute)
	d.ctx = readyCtx
	view := d.ready(doc)
	readyCancel()
	d.ctx = ctx
	d.report["restart_signal_to_ready_ms"] = time.Since(signaled).Milliseconds()
	ack, err := os.ReadFile(filepath.Join(os.Getenv("CODE_ARCHIVE_E2E_SIGNAL_DIR"), "restarted"))
	var restartAck struct {
		Started int64 `json:"restart_started_unix_ms"`
	}
	if err != nil || json.Unmarshal(ack, &restartAck) != nil || restartAck.Started <= 0 {
		t.Fatal("host restart timestamp missing")
	}
	d.report["restart_to_ready_ms"] = time.Now().UnixMilli() - restartAck.Started
	d.report["published_run_id"] = view.RunID
	d.restartSignal("check-publication", "publication-checked", map[string]string{"document_id": doc, "run_id": view.RunID}, time.Minute)
	d.report["published_run_count"] = 1
	count := d.restartCheckPages(doc, view, sources)
	d.restartCheckVectors(doc, view.RunID, count)
	d.restartRetrieval(doc, view.RunID, false)
	d.report["indexed_chunks"] = count
	d.report["byte_reconstruction_all_accepted_files"] = true
	d.writeReport()
	t.Logf("worker restart signal to ready: %s; published run %s; %d chunks", time.Since(signaled).Round(time.Millisecond), view.RunID, count)

	deleted := d.upload("delete-running-source.zip", blob)
	d.report["deleted_document_id"] = deleted
	d.writeReport()
	d.start(deleted, false)
	d.restartObserveRunning(deleted, "b")
	if n := d.restartDelete(deleted); n != 0 {
		t.Fatalf("running archive deletion removed %d documents, want 0", n)
	}
	if status, _ := d.status(deleted); status != "pending" && status != "running" {
		t.Fatalf("refused deletion changed running archive status to %s", status)
	}
	d.requireHidden(deleted)
	d.report["running_delete_refused"] = true
	var stopped struct {
		Count int `json:"success_count"`
	}
	d.must(http.MethodPost, d.datasetPath()+"/documents/stop", map[string]any{"document_ids": []string{deleted}}, &stopped)
	if stopped.Count != 1 {
		t.Fatalf("stop accepted %d documents, want 1", stopped.Count)
	}
	// archive_status also maps STOPPING to stopped; deletion needs terminal STOPPED.
	d.restartWaitIngestion(deleted, "STOPPED")
	if n := d.restartDelete(deleted); n != 1 {
		t.Fatalf("stopped archive deletion removed %d documents, want 1", n)
	}
	deleteAt := time.Now()
	checks := 0
	for {
		d.restartDeleted(deleted)
		checks++
		if time.Since(deleteAt) >= 95*time.Second {
			break
		}
		d.pause(3 * time.Second)
	}
	d.report["delete_observation_ms"], d.report["delete_observation_checks"] = time.Since(deleteAt).Milliseconds(), checks
	if n := d.restartDelete(doc); n != 1 {
		t.Fatalf("published archive deletion removed %d documents, want 1", n)
	}
	d.waitIndexEmpty(doc)
	d.restartDeleted(doc)
	d.restartDeleted(deleted)
	var result chunkPage
	d.must(http.MethodPost, "/api/v1/retrieval", d.retrievalRequest(""), &result)
	if result.Total != 0 || len(result.Chunks) != 0 {
		t.Fatal("test dataset remained retrievable after cleanup")
	}
	d.report["cleanup_verified"] = true
}

func archiveRestartSource(t *testing.T) ([]byte, map[string]string) {
	t.Helper()
	files := make(map[string]string, 12)
	for file := range 12 {
		var source strings.Builder
		source.WriteString("package checksum\n\n")
		for function := range 80 {
			fmt.Fprintf(&source, "// AuroraChecksum%d_%d computes a stable repository checksum.\nfunc AuroraChecksum%d_%d(data []byte) uint64 {\n\tvar checksum uint64 = %d\n\tfor index, value := range data {\n\t\tchecksum = checksum*131 + uint64(value) + uint64(index)\n\t\tchecksum ^= checksum >> 13\n\t}\n\treturn checksum\n}\n\n", file, function, file, function, file*80+function+17)
		}
		if source.Len() >= 1<<20 {
			t.Fatal("restart source exceeds per-file bound")
		}
		files[fmt.Sprintf("src/checksum_%02d.go", file)] = source.String()
	}
	blob, err := testutil.ArchiveZIP(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) >= 10<<20 {
		t.Fatal("restart ZIP exceeds upload bound")
	}
	return blob, files
}

func (d *deployment) restartSignal(request, response string, value map[string]string, timeout time.Duration) {
	d.t.Helper()
	dir := os.Getenv("CODE_ARCHIVE_E2E_SIGNAL_DIR")
	for _, name := range []string{request, response} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			d.t.Fatal("signal directory is not fresh")
		}
	}
	data, _ := json.Marshal(value)
	tmp := filepath.Join(dir, request+".tmp")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		d.t.Fatal("cannot write host signal")
	}
	if err := os.Rename(tmp, filepath.Join(dir, request)); err != nil {
		d.t.Fatal("cannot publish host signal")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(dir, response))
		if err == nil {
			var reply struct {
				OK         bool   `json:"ok"`
				DocumentID string `json:"document_id"`
			}
			if json.Unmarshal(data, &reply) != nil || !reply.OK || reply.DocumentID != value["document_id"] {
				d.t.Fatal("host observation/restart failed or mismatched document")
			}
			return
		}
		if !os.IsNotExist(err) {
			d.t.Fatal("cannot read host acknowledgment")
		}
		d.pause(100 * time.Millisecond)
	}
	d.t.Fatal("host signal acknowledgment timed out")
}

func (d *deployment) restartObserveRunning(doc, scenario string) {
	d.t.Helper()
	// The host checks a real publication lease, not merely a queued broker task.
	d.restartSignal("observe-running-"+scenario, "running-"+scenario, map[string]string{"document_id": doc}, 2*time.Minute)
	status, _ := d.status(doc)
	if status != "pending" && status != "running" {
		d.t.Fatalf("archive completed before fault injection: %s", status)
	}
}

func (d *deployment) restartCheckPages(doc string, tree treeView, sources map[string]string) int {
	d.t.Helper()
	accepted := map[string]string{}
	var walk func([]treeNode)
	walk = func(nodes []treeNode) {
		for _, node := range nodes {
			if node.Type == "file" {
				source, exists := sources[node.Path]
				if _, duplicate := accepted[node.Path]; duplicate || !exists {
					d.t.Fatal("tree has duplicate or non-source path")
				}
				accepted[node.Path] = source
			}
			walk(node.Children)
		}
	}
	walk(tree.Tree)
	if len(accepted) != len(sources) || len(accepted) != tree.Stats.Accepted || tree.Stats.Total != len(sources) {
		d.t.Fatal("accepted source tree count mismatch")
	}
	positions := map[string]int{}
	seen := map[string]bool{}
	total := -1
	for page := 1; total < 0 || len(seen) < total; page++ {
		var batch chunkPage
		d.must(http.MethodGet, fmt.Sprintf("%s/chunks?page=%d&page_size=100", d.docPath(doc), page), nil, &batch)
		if total < 0 {
			total = batch.Total
		}
		if total <= 100 || total > 10000 || batch.Total != total || len(batch.Chunks) == 0 {
			d.t.Fatal("incomplete/unstable or non-multipage chunk pagination")
		}
		for _, c := range batch.Chunks {
			source, exists := accepted[c.Path]
			if !exists || c.ChunkID == "" || seen[c.ChunkID] || c.RunID != tree.RunID || c.Language == "" {
				d.t.Fatal("chunk provenance mismatch or duplicate ID")
			}
			seen[c.ChunkID] = true
			header, body, ok := strings.Cut(c.WeightedContent, "\n")
			pos := positions[c.Path]
			end := pos + len(body)
			if !ok || strings.TrimSpace(body) == "" || end > len(source) || source[pos:end] != body {
				d.t.Fatal("chunk does not reconstruct exact ZIP bytes or has blank body")
			}
			startLine := strings.Count(source[:pos], "\n") + 1
			endLine := startLine + strings.Count(body, "\n")
			if strings.HasSuffix(body, "\n") {
				endLine--
			}
			prefix := fmt.Sprintf("// %s:L%d-L%d", c.Path, startLine, endLine)
			if c.Start != startLine || c.End != endLine || !(header == prefix || strings.HasPrefix(header, prefix+" ") || strings.HasPrefix(header, prefix+"#")) {
				d.t.Fatal("chunk line numbers/header differ from reconstructed byte offsets")
			}
			positions[c.Path] = end
		}
	}
	if len(seen) != total {
		d.t.Fatal("pagination returned excess chunks")
	}
	for name, source := range accepted {
		if positions[name] != len(source) {
			d.t.Fatalf("incomplete byte reconstruction for %s", name)
		}
	}
	var again treeView
	d.must(http.MethodGet, d.docPath(doc)+"/archive_tree", nil, &again)
	if again.Status != "ready" || again.RunID != tree.RunID {
		d.t.Fatal("publication changed during full pagination")
	}
	d.report["accepted_files"] = len(accepted)
	return total
}

func (d *deployment) restartCheckVectors(doc, run string, want int) {
	d.t.Helper()
	seen := map[string]bool{}
	dim := envInt(d.t, "CODE_ARCHIVE_E2E_EMBEDDING_DIMENSIONS", 1024)
	for offset := 0; offset < want; offset += 100 {
		query := map[string]any{"from": offset, "size": 100, "track_total_hits": true,
			"sort":  []any{map[string]string{"chunk_order_int": "asc"}},
			"query": map[string]any{"term": map[string]any{"doc_id": doc}}, "_source": d.sourceWithVectors()}
		data, _ := json.Marshal(query)
		req, err := http.NewRequestWithContext(d.ctx, http.MethodPost, d.es+"/ragflow_"+d.tenant+"/_search", bytes.NewReader(data))
		if err != nil {
			d.t.Fatal("cannot create ES request")
		}
		req.Header.Set("Content-Type", "application/json")
		if d.esUser != "" {
			req.SetBasicAuth(d.esUser, d.esPassword)
		}
		resp, err := d.client.Do(req)
		if err != nil {
			d.t.Fatal(d.clean(err.Error()))
		}
		var hits struct {
			Hits struct {
				Total struct {
					Value int `json:"value"`
				} `json:"total"`
				Hits []struct {
					ID     string                     `json:"_id"`
					Source map[string]json.RawMessage `json:"_source"`
				} `json:"hits"`
			} `json:"hits"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&hits)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil || hits.Hits.Total.Value != want || len(hits.Hits.Hits) != min(100, want-offset) {
			d.t.Fatal("ES document count/page mismatch")
		}
		for _, hit := range hits.Hits.Hits {
			if hit.ID == "" || seen[hit.ID] {
				d.t.Fatal("duplicate/empty ES row ID")
			}
			seen[hit.ID] = true
			var generation string
			d.decode(hit.Source["archive_run_kwd"], &generation)
			if generation != run {
				d.t.Fatal("ES contains an unpublished run")
			}
			var vector []float64
			d.decode(hit.Source[fmt.Sprintf("q_%d_vec", dim)], &vector)
			var norm float64
			for _, v := range vector {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					d.t.Fatal("non-finite embedding")
				}
				norm += v * v
			}
			if len(vector) != dim || norm == 0 {
				d.t.Fatal("missing or zero embedding")
			}
		}
	}
	if len(seen) != want {
		d.t.Fatal("ES rows missing across pages")
	}
	d.report["all_vector_rows_verified"] = len(seen)
}

func (d *deployment) restartRetrieval(doc, run string, empty bool) {
	d.t.Helper()
	request := d.retrievalRequest(doc)
	request["question"] = "AuroraChecksum computes a stable repository checksum"
	var result chunkPage
	d.must(http.MethodPost, "/api/v1/retrieval", request, &result)
	if empty {
		if result.Total != 0 || len(result.Chunks) != 0 {
			d.t.Fatal("deleted document remained retrievable")
		}
		return
	}
	if len(result.Chunks) == 0 {
		d.t.Fatal("published run returned no retrieval results")
	}
	for _, c := range result.Chunks {
		if c.RunID != run || c.Path == "" || c.Start < 1 || c.End < c.Start || c.VectorSimilarity <= 0 || !strings.Contains(c.Content, "AuroraChecksum") {
			d.t.Fatal("retrieval has obsolete run or invalid source/provenance/vector similarity")
		}
	}
}

func (d *deployment) restartWaitIngestion(doc, want string) {
	d.t.Helper()
	for {
		var data struct {
			Docs []struct {
				ID     string `json:"id"`
				Status string `json:"ingestion_status"`
			} `json:"docs"`
		}
		d.must(http.MethodGet, d.datasetPath()+"/documents?id="+url.QueryEscape(doc), nil, &data)
		if len(data.Docs) != 1 || data.Docs[0].ID != doc {
			d.t.Fatal("document status query did not return exactly the requested archive")
		}
		switch data.Docs[0].Status {
		case want:
			return
		case "COMPLETED", "FAILED", "STOPPED":
			d.t.Fatalf("ingestion reached %s, want %s", data.Docs[0].Status, want)
		}
		d.pause(200 * time.Millisecond)
	}
}

func (d *deployment) restartDelete(doc string) int {
	d.t.Helper()
	var result struct {
		Deleted int `json:"deleted"`
	}
	d.must(http.MethodDelete, d.datasetPath()+"/documents", map[string]any{"ids": []string{doc}}, &result)
	return result.Deleted
}

func (d *deployment) restartDeleted(doc string) {
	d.t.Helper()
	var result struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	d.decode(d.searchIndex(doc), &result)
	if result.Hits.Total.Value != 0 {
		d.t.Fatal("late ES write after document deletion")
	}
	d.requireDeletedTree(doc)
	d.restartRetrieval(doc, "", true)
}
