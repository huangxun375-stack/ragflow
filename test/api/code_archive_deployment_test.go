//go:build integration && e2e

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
	"strconv"
	"strings"
	"testing"
	"time"

	"ragflow/internal/ingestion/testutil"
)

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type deployment struct {
	t                           *testing.T
	ctx                         context.Context
	client                      *http.Client // Elasticsearch only; API calls use the shared REST helpers.
	es, esUser, esPassword      string
	tenant, dataset, reportPath string
	secrets                     []string
	report                      map[string]any
	vectorSource                any
}

type treeNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	Type     string     `json:"type"`
	Children []treeNode `json:"children,omitempty"`
}

type treeView struct {
	Status string     `json:"status"`
	RunID  string     `json:"archive_run_id"`
	Tree   []treeNode `json:"tree"`
	Stats  struct {
		Accepted        int `json:"accepted_files"`
		Total           int `json:"total_files"`
		FilteredEntries []struct {
			Path      string `json:"path"`
			Type      string `json:"type"`
			Reason    string `json:"reason"`
			FileCount int    `json:"file_count"`
		} `json:"filtered_entries"`
		FilteredEntriesTruncated bool `json:"filtered_entries_truncated"`
	} `json:"stats"`
}

type chunk struct {
	ID               string  `json:"id"`
	Content          string  `json:"content"`
	ChunkID          string  `json:"chunk_id"`
	WeightedContent  string  `json:"content_with_weight"`
	Path             string  `json:"source_path"`
	Language         string  `json:"code_lang"`
	RunID            string  `json:"archive_run_id"`
	Start            int     `json:"start_line"`
	End              int     `json:"end_line"`
	VectorSimilarity float64 `json:"vector_similarity"`
}

type chunkPage struct {
	Chunks []chunk `json:"chunks"`
	Total  int     `json:"total"`
}

func newArchiveDeployment(t *testing.T, timeout time.Duration, scope, reportEnv string) *deployment {
	t.Helper()
	if value := os.Getenv("CODE_ARCHIVE_E2E_TIMEOUT"); value != "" {
		var err error
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			t.Fatal("CODE_ARCHIVE_E2E_TIMEOUT must be a positive Go duration")
		}
	}
	es := os.Getenv("ES_HOST")
	if es == "" {
		t.Fatal("ES_HOST is required for real index assertions")
	}
	if !strings.Contains(es, "://") {
		es = "http://" + es
		u, err := url.Parse(es)
		if err != nil {
			t.Fatal("invalid ES_HOST")
		}
		if u.Port() == "" {
			u.Host = u.Hostname() + ":9200"
		}
		es = u.String()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	d := &deployment{
		t: t, ctx: ctx, client: &http.Client{Timeout: 90 * time.Second}, es: strings.TrimRight(es, "/"),
		esUser:     os.Getenv("ES_USERNAME"),
		esPassword: os.Getenv("ES_PASSWORD"),
		reportPath: os.Getenv(reportEnv),
		report:     map[string]any{"scope": scope, "started_at": time.Now().UTC().Format(time.RFC3339), "host_address": TestConfig.HostAddress},
	}
	d.secrets = []string{TestConfig.APIKey, TestConfig.AuthToken, TestConfig.Password, d.esPassword}
	t.Cleanup(func() {
		d.report["passed"] = !t.Failed()
		d.writeReport()
	})
	return d
}

func (d *deployment) writeReport() {
	d.t.Helper()
	if d.reportPath == "" {
		return
	}
	data, err := json.MarshalIndent(d.report, "", "  ")
	if err != nil {
		d.t.Error("cannot encode archive report")
		return
	}
	if err := os.WriteFile(d.reportPath, append(data, '\n'), 0600); err != nil {
		d.t.Error("cannot write archive report")
	}
}

func TestArchiveHTTPDeployment(t *testing.T) {
	d := newArchiveDeployment(t, 10*time.Minute, "HTTP upload -> NATS ingestor -> external embedding -> Elasticsearch -> HTTP retrieval", "CODE_ARCHIVE_E2E_REPORT")
	d.provision()
	files := archiveSourceFiles()
	blob, err := testutil.ArchiveZIP(files)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(blob)
	d.report["zip_bytes"], d.report["zip_sha256"] = len(blob), hex.EncodeToString(digest[:])
	started := time.Now()
	doc := d.upload("deployment-source.zip", blob)
	d.report["document_id"] = doc
	d.requireHidden(doc)
	d.report["unstarted_hidden"] = true
	d.start(doc, false)
	d.observePending(doc)
	first := d.ready(doc)
	d.report["initial_ingestion_ms"] = time.Since(started).Milliseconds()
	d.report["first_run_id"] = first.RunID
	d.checkTree(first, files)
	chunks := d.chunks(doc)
	d.checkChunks(chunks, files, first.RunID)
	d.report["chunks"] = len(chunks.Chunks)
	d.checkStoredVectors(doc, first.RunID, len(chunks.Chunks))
	d.requireRetrieval(doc, first.RunID)
	d.report["external_vectors_and_retrieval"] = true

	// Cancel before a second embedding request can publish the new generation.
	d.start(doc, true)
	d.must(http.MethodPost, d.datasetPath()+"/documents/stop", map[string]any{"document_ids": []string{doc}}, nil)
	d.waitStatus(doc, "stopped")
	d.requireHidden(doc)
	d.report["stopped_hidden"] = true
	d.retryReparse(doc)
	second := d.ready(doc)
	if second.RunID == "" || second.RunID == first.RunID {
		t.Fatal("reparse did not publish a new run")
	}
	d.report["second_run_id"] = second.RunID
	d.checkTree(second, files)
	secondChunks := d.chunks(doc)
	d.checkChunks(secondChunks, files, second.RunID)
	d.checkStoredVectors(doc, second.RunID, len(secondChunks.Chunks))
	d.requireRetrieval(doc, second.RunID)
	d.report["reparse_published_new_generation"] = true

	empty, err := testutil.ArchiveZIP(map[string]string{"README.md": "no supported source files\n"})
	if err != nil {
		t.Fatal(err)
	}
	failedDoc := d.upload("unsupported-source.zip", empty)
	d.start(failedDoc, false)
	d.waitStatus(failedDoc, "failed")
	if _, reason := d.status(failedDoc); reason != "ZIP_NO_SUPPORTED_FILES" {
		t.Fatalf("async archive failure lost reason: %s", d.clean(reason))
	}
	d.report["failure_reason"] = "ZIP_NO_SUPPORTED_FILES"
	d.requireHidden(failedDoc)
	d.report["failed_hidden"] = true

	d.must(http.MethodDelete, d.datasetPath()+"/documents", map[string]any{"ids": []string{doc, failedDoc}}, nil)
	for _, deleted := range []string{doc, failedDoc} {
		d.waitIndexEmpty(deleted)
		d.requireDeletedTree(deleted)
	}
	var retrieval chunkPage
	d.must(http.MethodPost, "/api/v1/retrieval", d.retrievalRequest(""), &retrieval)
	if retrieval.Total != 0 || len(retrieval.Chunks) != 0 {
		t.Fatal("deleted archive remained retrievable")
	}
	d.report["deleted_from_index_and_api"] = true
	d.report["total_scenario_ms"] = time.Since(started).Milliseconds()
}

func (d *deployment) provision() {
	d.t.Helper()
	var models struct {
		TenantID string `json:"tenant_id"`
	}
	d.must(http.MethodGet, "/api/v1/users/me/models", nil, &models)
	d.tenant = models.TenantID
	if d.tenant == "" {
		d.t.Fatal("current user's model settings omitted tenant_id")
	}
	d.report["tenant_id"] = d.tenant
	fields := apiObject{"name": uniqueName("archive_e2e"), "permission": "me"}
	if model := os.Getenv("CODE_ARCHIVE_E2E_MODEL_REFERENCE"); model != "" {
		fields["embedding_model"] = model
	}
	dataset := createRESTDataset(d.t, fields)
	d.dataset = dataset["id"].(string)
	d.report["dataset_id"], d.report["embedding_model"] = d.dataset, dataset["embedding_model"]
	// Pre-publication retrieval needs the configured engine's tenant index.
	d.must(http.MethodPost, "/api/v1/tenant/chunk-store", map[string]any{"kb_id": d.dataset, "vector_size": envInt(d.t, "CODE_ARCHIVE_E2E_EMBEDDING_DIMENSIONS", 1024)}, nil)
}

func (d *deployment) upload(name string, blob []byte) string {
	d.t.Helper()
	result := restUpload(d.t, TestConfig, http.MethodPost, strings.TrimPrefix(d.datasetPath(), "/api/v1")+"/documents", "file", name, blob,
		map[string]string{"parser_config": `{"CodeArchive:Archive":{"chunk_token_num":512,"code_file_overview":false}}`})
	docs := array(d.t, restOK(d.t, result)["data"])
	if len(docs) != 1 {
		d.t.Fatal("ZIP upload did not create one document")
	}
	doc := object(d.t, docs[0])
	id, ok := doc["id"].(string)
	if !ok || id == "" || doc["parser_id"] != "code_archive" {
		d.t.Fatal("ZIP upload did not create one code_archive document")
	}
	d.t.Cleanup(func() { d.cleanupDocument(id) })
	return id
}

func (d *deployment) cleanupDocument(doc string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	previous := d.ctx
	d.ctx = ctx
	defer func() { d.ctx = previous }()
	var data struct {
		Docs []struct {
			Status string `json:"ingestion_status"`
		} `json:"docs"`
	}
	result, _, err := d.call(http.MethodGet, d.datasetPath()+"/documents?id="+url.QueryEscape(doc), nil)
	if err != nil {
		d.t.Fatal(d.clean(err.Error()))
	}
	if result.Code == 102 {
		d.requireDeletedTree(doc)
		return
	}
	if result.Code != 0 {
		d.t.Fatalf("cleanup document lookup: %s", d.clean(result.Message))
	}
	d.decode(result.Data, &data)
	if len(data.Docs) != 1 {
		d.t.Fatal("cleanup lookup did not uniquely identify the archive")
	}
	switch data.Docs[0].Status {
	case "CREATED", "SCHEDULED", "PENDING", "RUNNING", "STOPPING":
		d.must(http.MethodPost, d.datasetPath()+"/documents/stop", map[string]any{"document_ids": []string{doc}}, nil)
	}
	for {
		var result struct {
			Deleted int `json:"deleted"`
		}
		d.must(http.MethodDelete, d.datasetPath()+"/documents", map[string]any{"ids": []string{doc}}, &result)
		if result.Deleted == 1 {
			d.waitIndexEmpty(doc)
			return
		}
		d.pause(500 * time.Millisecond)
	}
}

func (d *deployment) start(doc string, reparse bool) {
	d.t.Helper()
	if reparse {
		d.must(http.MethodPost, d.datasetPath()+"/chunks", map[string]any{"document_ids": []string{doc}}, nil)
		return
	}
	var result struct {
		Count int `json:"success_count"`
	}
	d.must(http.MethodPost, d.datasetPath()+"/documents/parse", map[string]any{"document_ids": []string{doc}}, &result)
	if result.Count != 1 {
		d.t.Fatalf("ingestion accepted %d documents, want 1", result.Count)
	}
}

func (d *deployment) retryReparse(doc string) {
	d.t.Helper()
	for i := 0; i < 120; i++ {
		resp, _, err := d.call(http.MethodPost, d.datasetPath()+"/chunks", map[string]any{"document_ids": []string{doc}})
		if err != nil {
			d.t.Fatal(d.clean(err.Error()))
		}
		if resp.Code == 0 {
			return
		}
		message := strings.ToLower(resp.Message)
		if !strings.Contains(message, "running") && !strings.Contains(message, "stopping") && !strings.Contains(message, "active") && !strings.Contains(message, "terminal") {
			d.t.Fatalf("reparse failed: %s", d.clean(resp.Message))
		}
		d.pause(500 * time.Millisecond)
	}
	d.t.Fatal("cancelled worker did not become terminal within 60s")
}

func (d *deployment) ready(doc string) treeView {
	d.t.Helper()
	for {
		var tree treeView
		d.must(http.MethodGet, d.docPath(doc)+"/archive_tree", nil, &tree)
		if tree.Status == "ready" {
			if tree.RunID == "" {
				d.t.Fatal("ready tree omitted run ID")
			}
			return tree
		}
		if tree.Status != "not_ready" || len(tree.Tree) != 0 {
			d.t.Fatal("unpublished tree leaked content")
		}
		status, reason := d.status(doc)
		if status == "failed" || status == "stopped" {
			d.t.Fatalf("ingestion %s: %s", status, d.clean(reason))
		}
		d.pause(300 * time.Millisecond)
	}
}

func (d *deployment) status(doc string) (string, string) {
	d.t.Helper()
	var data struct {
		Total int `json:"total"`
		Docs  []struct {
			ID     string `json:"id"`
			Status string `json:"archive_status"`
			Reason string `json:"archive_failure_reason"`
		} `json:"docs"`
	}
	d.must(http.MethodGet, d.datasetPath()+"/documents?id="+url.QueryEscape(doc), nil, &data)
	if data.Total != 1 || len(data.Docs) != 1 || data.Docs[0].ID != doc || data.Docs[0].Status == "" {
		d.t.Fatal("document status query did not return exactly the requested archive")
	}
	return data.Docs[0].Status, data.Docs[0].Reason
}

func (d *deployment) waitStatus(doc, want string) {
	d.t.Helper()
	for {
		status, reason := d.status(doc)
		if status == want {
			return
		}
		if status == "ready" || status == "failed" && want != "failed" {
			d.t.Fatalf("expected %s, got %s (%s)", want, status, d.clean(reason))
		}
		d.pause(200 * time.Millisecond)
	}
}

func (d *deployment) chunks(doc string) chunkPage {
	d.t.Helper()
	var data chunkPage
	d.must(http.MethodGet, d.docPath(doc)+"/chunks?page=1&page_size=100", nil, &data)
	for i := range data.Chunks {
		data.Chunks[i].ID = data.Chunks[i].ChunkID
		data.Chunks[i].Content = data.Chunks[i].WeightedContent
	}
	return data
}

func (d *deployment) requireHidden(doc string) {
	d.t.Helper()
	var tree treeView
	d.must(http.MethodGet, d.docPath(doc)+"/archive_tree", nil, &tree)
	if tree.Status != "not_ready" || len(tree.Tree) != 0 || tree.RunID != "" || len(tree.Stats.FilteredEntries) != 0 {
		d.t.Fatal("unpublished archive leaked tree or run ID")
	}
	chunks := d.chunks(doc)
	if chunks.Total != 0 || len(chunks.Chunks) != 0 {
		d.t.Fatal("unpublished archive leaked chunks")
	}
	var retrieval chunkPage
	d.must(http.MethodPost, "/api/v1/retrieval", d.retrievalRequest(doc), &retrieval)
	if retrieval.Total != 0 || len(retrieval.Chunks) != 0 {
		d.t.Fatal("unpublished archive leaked retrieval results")
	}
}

func (d *deployment) requireDeletedTree(doc string) {
	d.t.Helper()
	r := restCallContext(d.t, d.ctx, TestConfig, http.MethodGet, strings.TrimPrefix(d.docPath(doc), "/api/v1")+"/archive_tree", nil, nil)
	if r.Status == http.StatusNotFound || r.Status == http.StatusForbidden {
		return
	}
	if r.Status != http.StatusOK || r.JSON["code"] != float64(403) && r.JSON["code"] != float64(404) {
		d.t.Fatal("deleted archive tree remained accessible")
	}
}

// Two reads distinguish pending leaks from publication during the chunk request.
func (d *deployment) observePending(doc string) {
	d.t.Helper()
	var before, after treeView
	d.must(http.MethodGet, d.docPath(doc)+"/archive_tree", nil, &before)
	if before.Status == "ready" {
		return
	}
	page := d.chunks(doc)
	d.must(http.MethodGet, d.docPath(doc)+"/archive_tree", nil, &after)
	if after.Status == "not_ready" {
		if page.Total != 0 || len(page.Chunks) != 0 {
			d.t.Fatal("pending archive leaked chunks before publication")
		}
		d.report["pending_hidden_observed"] = true
	}
}

func (d *deployment) checkTree(tree treeView, files map[string]string) {
	d.t.Helper()
	paths := map[string]bool{}
	var walk func([]treeNode)
	walk = func(nodes []treeNode) {
		for _, node := range nodes {
			if node.Type == "file" {
				paths[node.Path] = true
			}
			walk(node.Children)
		}
	}
	walk(tree.Tree)
	for _, path := range []string{"src/checksum.go", "src/greeting.ts", "ui/Panel.vue", "lib/count.lua", "include/answer.h"} {
		if !paths[path] || files[path] == "" {
			d.t.Fatalf("source tree omitted %s", path)
		}
	}
	if len(paths) != 5 || tree.Stats.Accepted != 5 || tree.Stats.Total != len(files) {
		d.t.Fatal("tree filtering or file statistics are incorrect")
	}
	want := map[string]string{".gitignore": "hidden", ".private.go": "hidden", "README.md": "suffix", "generated": "gitignore", "node_modules": "dir"}
	if tree.Stats.FilteredEntriesTruncated || len(tree.Stats.FilteredEntries) != len(want) {
		d.t.Fatal("filtered path diagnostics missing or unexpectedly truncated")
	}
	for _, item := range tree.Stats.FilteredEntries {
		kind := "file"
		if item.Path == "generated" || item.Path == "node_modules" {
			kind = "directory"
		}
		if want[item.Path] != item.Reason || item.FileCount != 1 || item.Type != kind {
			d.t.Fatalf("incorrect filtering diagnostic: %+v", item)
		}
		delete(want, item.Path)
	}
	if len(want) != 0 {
		d.t.Fatal("filter diagnostics omitted paths")
	}
	d.report["filtered_path_diagnostics"] = true
}

func (d *deployment) checkChunks(page chunkPage, files map[string]string, run string) {
	d.t.Helper()
	if len(page.Chunks) < 5 || page.Total != len(page.Chunks) {
		d.t.Fatalf("incomplete chunk list: total=%d, returned=%d", page.Total, len(page.Chunks))
	}
	seen := map[string]bool{}
	rebuilt := map[string]*strings.Builder{}
	for _, item := range page.Chunks {
		source, ok := files[item.Path]
		if !ok || item.RunID != run || item.ID == "" || item.Language == "" {
			d.t.Fatal("chunk omitted or changed provenance")
		}
		lines := strings.Split(strings.TrimSuffix(source, "\n"), "\n")
		if item.Start < 1 || item.End < item.Start || item.End > len(lines) {
			d.t.Fatal("chunk source lines out of bounds")
		}
		header, body, found := strings.Cut(item.Content, "\n")
		prefix := fmt.Sprintf("// %s:L%d-L%d", item.Path, item.Start, item.End)
		if !found || !strings.HasPrefix(header, prefix) || !strings.Contains(strings.Join(lines[item.Start-1:item.End], "\n")+"\n", body) {
			d.t.Fatal("chunk content differs from source line range")
		}
		if rebuilt[item.Path] == nil {
			rebuilt[item.Path] = &strings.Builder{}
		}
		rebuilt[item.Path].WriteString(body)
		seen[item.Path] = true
	}
	if len(seen) != 5 {
		d.t.Fatalf("chunk output covers %d source files, want 5", len(seen))
	}
	for path, body := range rebuilt {
		if body.String() != files[path] {
			d.t.Fatalf("chunk bodies do not reconstruct source file %s", path)
		}
	}
}

func (d *deployment) retrievalRequest(doc string) map[string]any {
	request := map[string]any{"dataset_ids": []string{d.dataset}, "question": "AuroraChecksum stable repository checksum", "page_size": 20,
		"similarity_threshold": 0, "vector_similarity_weight": 1, "keyword": false, "highlight": false, "include_knowledge_compilation": false}
	if doc != "" {
		request["document_ids"] = []string{doc}
	}
	return request
}

func (d *deployment) requireRetrieval(doc, run string) {
	d.t.Helper()
	var result chunkPage
	d.must(http.MethodPost, "/api/v1/retrieval", d.retrievalRequest(doc), &result)
	found := false
	for _, item := range result.Chunks {
		if item.RunID != run {
			d.t.Fatal("retrieval returned an obsolete generation")
		}
		if item.Path == "src/checksum.go" && strings.Contains(item.Content, "AuroraChecksum") && item.Start > 0 && item.End >= item.Start && item.VectorSimilarity > 0 {
			found = true
		}
	}
	if !found {
		d.t.Fatal("vector retrieval did not return checksum source with valid provenance")
	}
}

func (d *deployment) checkStoredVectors(doc, run string, want int) {
	d.t.Helper()
	var hits struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				Source map[string]json.RawMessage `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	d.decode(d.searchIndex(doc), &hits)
	if hits.Hits.Total.Value != want || len(hits.Hits.Hits) != want {
		d.t.Fatalf("ES contains %d rows, want %d", hits.Hits.Total.Value, want)
	}
	dim := envInt(d.t, "CODE_ARCHIVE_E2E_EMBEDDING_DIMENSIONS", 1024)
	for _, hit := range hits.Hits.Hits {
		var gotRun string
		d.decode(hit.Source["archive_run_kwd"], &gotRun)
		if gotRun != run {
			d.t.Fatal("ES row has wrong archive generation")
		}
		var vector []float64
		d.decode(hit.Source[fmt.Sprintf("q_%d_vec", dim)], &vector)
		var norm float64
		for _, value := range vector {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				d.t.Fatal("ES vector is non-finite")
			}
			norm += value * value
		}
		if len(vector) != dim || norm == 0 {
			d.t.Fatal("ES row does not contain the real nonzero embedding vector")
		}
	}
	d.report["vector_dimensions"] = dim
}

func (d *deployment) searchIndex(doc string) []byte {
	d.t.Helper()
	body, _ := json.Marshal(map[string]any{"size": 100, "track_total_hits": true, "query": map[string]any{"term": map[string]any{"doc_id": doc}}, "_source": d.sourceWithVectors()})
	req, err := http.NewRequestWithContext(d.ctx, http.MethodPost, d.es+"/ragflow_"+d.tenant+"/_search", bytes.NewReader(body))
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if d.esUser != "" {
		req.SetBasicAuth(d.esUser, d.esPassword)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.t.Fatal(d.clean(err.Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		d.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		d.t.Fatalf("ES search returned HTTP %d: %s", resp.StatusCode, d.clean(string(data)))
	}
	return data
}

// ES 9.2+ excludes vectors from _source by default; older ES rejects the option.
func (d *deployment) sourceWithVectors() any {
	d.t.Helper()
	if d.vectorSource != nil {
		return d.vectorSource
	}
	req, err := http.NewRequestWithContext(d.ctx, http.MethodGet, d.es+"/", nil)
	if err != nil {
		d.t.Fatal(err)
	}
	if d.esUser != "" {
		req.SetBasicAuth(d.esUser, d.esPassword)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		d.t.Fatal(d.clean(err.Error()))
	}
	defer resp.Body.Close()
	var info struct {
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info) != nil {
		d.t.Fatalf("cannot read Elasticsearch version: HTTP %d", resp.StatusCode)
	}
	var major, minor int
	if _, err := fmt.Sscanf(info.Version.Number, "%d.%d", &major, &minor); err != nil {
		d.t.Fatalf("unrecognized Elasticsearch version %q", info.Version.Number)
	}
	d.vectorSource = true
	if major > 9 || major == 9 && minor >= 2 {
		d.vectorSource = map[string]any{"exclude_vectors": false}
	}
	d.report["elasticsearch_version"] = info.Version.Number
	return d.vectorSource
}

func (d *deployment) waitIndexEmpty(doc string) {
	d.t.Helper()
	for i := 0; i < 120; i++ {
		var result struct {
			Hits struct {
				Total struct {
					Value int `json:"value"`
				} `json:"total"`
			} `json:"hits"`
		}
		d.decode(d.searchIndex(doc), &result)
		if result.Hits.Total.Value == 0 {
			return
		}
		d.pause(500 * time.Millisecond)
	}
	d.t.Fatal("deleted document retained index rows after 60s")
}

func (d *deployment) datasetPath() string       { return "/api/v1/datasets/" + d.dataset }
func (d *deployment) docPath(doc string) string { return d.datasetPath() + "/documents/" + doc }

func (d *deployment) must(method, path string, body, out any) {
	d.t.Helper()
	resp, _, err := d.call(method, path, body)
	if err != nil {
		d.t.Fatal(d.clean(err.Error()))
	}
	if resp.Code != 0 {
		d.t.Fatalf("%s %s: code %d: %s", method, path, resp.Code, d.clean(resp.Message))
	}
	if out != nil {
		if len(resp.Data) == 0 || bytes.Equal(resp.Data, []byte("null")) {
			d.t.Fatalf("%s %s: omitted expected response data", method, path)
		}
		d.decode(resp.Data, out)
	}
}

func (d *deployment) call(method, path string, body any) (envelope, http.Header, error) {
	d.t.Helper()
	u, err := url.Parse(strings.TrimPrefix(path, "/api/v1"))
	if err != nil {
		return envelope{}, nil, err
	}
	query := apiObject{}
	for key, values := range u.Query() {
		if len(values) != 1 {
			return envelope{}, nil, fmt.Errorf("unexpected repeated query parameter %s", key)
		}
		query[key] = values[0]
	}
	r := restCallContext(d.t, d.ctx, TestConfig, method, u.Path, body, query)
	if r.Status < 200 || r.Status >= 300 {
		return envelope{}, r.Header, fmt.Errorf("%s %s returned HTTP %d", method, path, r.Status)
	}
	var result envelope
	if err := json.Unmarshal(r.Body, &result); err != nil {
		return envelope{}, r.Header, fmt.Errorf("%s %s returned invalid JSON: %w", method, path, err)
	}
	return result, r.Header, nil
}

func (d *deployment) decode(data []byte, out any) {
	d.t.Helper()
	if err := json.Unmarshal(data, out); err != nil {
		d.t.Fatalf("response decoding failed: %v", err)
	}
}

func (d *deployment) pause(duration time.Duration) {
	d.t.Helper()
	select {
	case <-d.ctx.Done():
		d.t.Fatal("deployment test deadline exceeded")
	case <-time.After(duration):
	}
}

func (d *deployment) clean(text string) string {
	for _, secret := range d.secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	if len(text) > 2000 {
		text = text[:2000] + "..."
	}
	return text
}

func envInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	if value := os.Getenv(name); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			t.Fatalf("%s must be a positive integer", name)
		}
		return n
	}
	return fallback
}
