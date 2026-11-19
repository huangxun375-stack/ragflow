//go:build integration && e2e && manual

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	tspack "github.com/xberg-io/tree-sitter-language-pack/packages/go"

	"ragflow/internal/ingestion/testutil"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/tokenizer"
)

// Provider work is opt-in and bounded by an exact local BGE-M3 preflight.
func TestArchiveHTTPScaleDeployment(t *testing.T) {
	if os.Getenv("CODE_ARCHIVE_E2E_SCALE") != "1" {
		t.Skip("set CODE_ARCHIVE_E2E_SCALE=1 to enable real 10 MiB provider usage")
	}
	d := newArchiveDeployment(t, 30*time.Minute, "10 MiB HTTP upload -> real ingestor -> BGE-M3 -> Elasticsearch -> retrieval", "CODE_ARCHIVE_E2E_SCALE_REPORT")
	blob, files, err := testutil.ArchiveScale()
	if err != nil {
		t.Fatal(err)
	}
	maxChunks := envInt(t, "CODE_ARCHIVE_E2E_SCALE_MAX_CHUNKS", 10000)
	if maxChunks > codearchive.DefaultLimits().MaxChunks {
		t.Fatal("CODE_ARCHIVE_E2E_SCALE_MAX_CHUNKS exceeds server archive limit")
	}
	count, tokens := preflightScale(t, d.ctx, blob, maxChunks)
	t.Logf("10 MiB preflight: %d source files, %d chunks, %d XLM-R tokens; provider chunk budget %d", len(files), count, tokens, maxChunks)
	digest := sha256.Sum256(blob)
	d.report["zip_bytes"], d.report["zip_sha256"], d.report["source_files"] = len(blob), hex.EncodeToString(digest[:]), len(files)
	d.report["preflight_chunks"], d.report["preflight_embedding_tokens"], d.report["provider_chunk_budget"] = count, tokens, maxChunks
	d.provision()
	model, _ := d.report["embedding_model"].(string)
	if model != "BAAI/bge-m3" && !strings.HasPrefix(model, "BAAI/bge-m3@") {
		t.Fatal("scale preflight requires the dataset's registered embedding model to be BAAI/bge-m3; configure the default or CODE_ARCHIVE_E2E_MODEL_REFERENCE")
	}
	started := time.Now()
	doc := d.upload("ten-mib-source.zip", blob)
	d.report["upload_ms"], d.report["document_id"] = time.Since(started).Milliseconds(), doc
	d.requireHidden(doc)
	d.start(doc, false)
	d.observePending(doc)
	view := d.ready(doc)
	d.report["upload_to_ready_ms"], d.report["run_id"] = time.Since(started).Milliseconds(), view.RunID
	if view.Stats.Accepted != len(files) || view.Stats.Total != len(files) {
		t.Fatal("scale tree file counts differ from source")
	}
	treeData, err := json.Marshal(view.Tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(treeData) > 8<<20 {
		t.Fatal("scale tree exceeds artifact size bound")
	}
	d.report["tree_bytes"] = len(treeData)
	paths := map[string]bool{}
	var walk func([]treeNode)
	walk = func(nodes []treeNode) {
		for _, node := range nodes {
			if node.Type == "file" {
				if paths[node.Path] {
					t.Fatal("scale tree duplicated a source file")
				}
				paths[node.Path] = true
			}
			walk(node.Children)
		}
	}
	walk(view.Tree)
	if len(paths) != len(files) {
		t.Fatal("scale tree omitted source files")
	}
	for path := range files {
		if !paths[path] {
			t.Fatalf("tree omitted %s", path)
		}
	}
	d.verifyScalePages(doc, view.RunID, files, count)
	d.verifyScaleVectors(doc, view.RunID, count)
	request := d.retrievalRequest(doc)
	request["question"] = "Checksum19 computes a deterministic repository checksum"
	var retrieved chunkPage
	d.must(http.MethodPost, "/api/v1/retrieval", request, &retrieved)
	if len(retrieved.Chunks) == 0 {
		t.Fatal("scale vector retrieval returned no source")
	}
	for _, item := range retrieved.Chunks {
		if !paths[item.Path] || item.RunID != view.RunID || item.VectorSimilarity <= 0 || item.Start < 1 || item.End < item.Start {
			t.Fatal("scale retrieval returned invalid source or vector similarity")
		}
	}
	d.report["retrieved_chunks"] = len(retrieved.Chunks)
	d.report["all_source_pages_verified"] = true
	d.must(http.MethodDelete, d.datasetPath()+"/documents", map[string]any{"ids": []string{doc}}, nil)
	d.waitIndexEmpty(doc)
	d.requireDeletedTree(doc)
	var empty chunkPage
	d.must(http.MethodPost, "/api/v1/retrieval", d.retrievalRequest(""), &empty)
	if empty.Total != 0 || len(empty.Chunks) != 0 {
		t.Fatal("scale document remained retrievable after deletion")
	}
	d.report["index_cleanup_verified"] = true
}

func preflightScale(t *testing.T, ctx context.Context, blob []byte, maxChunks int) (int, int64) {
	t.Helper()
	policy := codearchive.DefaultPolicy()
	policy.Limits.MaxChunks = maxChunks
	prepareScaleGrammars(t, policy)
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	counter, err := tokenizer.LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	result, err := codearchive.Process(ctx, blob, codearchive.Options{
		Filename: "ten-mib-source.zip", Policy: &policy, ChunkTokens: 512,
		CountTokens:   tokenizer.NumTokensFromString,
		FitsEmbedding: func(text string) bool { return counter.Count(text) <= 8192 },
	})
	if err != nil {
		t.Fatalf("local preflight refused the archive before provider use: %v", err)
	}
	var tokens int64
	for _, item := range result.Chunks {
		tokens += int64(counter.Count(item.Text))
	}
	return len(result.Chunks), tokens
}

func prepareScaleGrammars(t *testing.T, policy codearchive.Policy) {
	t.Helper()
	cache := os.Getenv("RAGFLOW_CODE_ARCHIVE_GRAMMAR_CACHE")
	if cache == "" {
		t.Fatal("RAGFLOW_CODE_ARCHIVE_GRAMMAR_CACHE must name the prepared grammar directory")
	}
	if err := tspack.Configure(tspack.PackConfig{CacheDir: &cache}); err != nil {
		t.Fatalf("configure prepared grammar cache: %v", err)
	}
	if err := codearchive.CheckGrammars(policy); err != nil {
		t.Fatalf("prepared grammars unavailable; run tools/code_archive/prepare.sh before the test: %v", err)
	}
}

func (d *deployment) verifyScalePages(doc, run string, files map[string]string, expected int) {
	d.t.Helper()
	lines := map[string][]string{}
	covered := map[string][]bool{}
	rebuilt := map[string]*strings.Builder{}
	for path, source := range files {
		lines[path] = strings.Split(strings.TrimSuffix(source, "\n"), "\n")
		covered[path] = make([]bool, len(lines[path]))
		rebuilt[path] = &strings.Builder{}
	}
	seen := map[string]bool{}
	for page := 1; len(seen) < expected; page++ {
		var batch chunkPage
		d.must(http.MethodGet, fmt.Sprintf("%s/chunks?page=%d&page_size=100", d.docPath(doc), page), nil, &batch)
		if batch.Total != expected || len(batch.Chunks) == 0 {
			d.t.Fatalf("scale pagination: total=%d expected=%d page=%d returned=%d", batch.Total, expected, page, len(batch.Chunks))
		}
		for _, item := range batch.Chunks {
			if item.ChunkID == "" || seen[item.ChunkID] || item.RunID != run {
				d.t.Fatal("scale chunk ID duplicated or generation incorrect")
			}
			seen[item.ChunkID] = true
			source, ok := lines[item.Path]
			if !ok || item.Start < 1 || item.End < item.Start || item.End > len(source) {
				d.t.Fatal("scale chunk provenance invalid")
			}
			header, body, ok := strings.Cut(item.WeightedContent, "\n")
			prefix := fmt.Sprintf("// %s:L%d-L%d", item.Path, item.Start, item.End)
			if !ok || !strings.HasPrefix(header, prefix) || !strings.Contains(strings.Join(source[item.Start-1:item.End], "\n")+"\n", body) {
				d.t.Fatal("scale source text differs from declared line range")
			}
			rebuilt[item.Path].WriteString(body)
			for line := item.Start - 1; line < item.End; line++ {
				covered[item.Path][line] = true
			}
		}
	}
	if len(seen) != expected {
		d.t.Fatal("scale pagination returned too many chunks")
	}
	for path, marks := range covered {
		for line, present := range marks {
			if !present && strings.TrimSpace(lines[path][line]) != "" {
				d.t.Fatalf("scale source line %s:%d missing from indexed output", path, line+1)
			}
		}
		if rebuilt[path].String() != files[path] {
			d.t.Fatalf("scale chunk bodies do not reconstruct source file %s", path)
		}
	}
	d.report["indexed_chunks"] = len(seen)
}

func (d *deployment) verifyScaleVectors(doc, run string, expected int) {
	d.t.Helper()
	var result struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				Source map[string]json.RawMessage `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	d.decode(d.searchIndex(doc), &result)
	if result.Hits.Total.Value != expected || len(result.Hits.Hits) != min(100, expected) {
		d.t.Fatal("ES source row count differs from HTTP chunks")
	}
	dim := envInt(d.t, "CODE_ARCHIVE_E2E_EMBEDDING_DIMENSIONS", 1024)
	for _, hit := range result.Hits.Hits {
		var gotRun string
		d.decode(hit.Source["archive_run_kwd"], &gotRun)
		if gotRun != run {
			d.t.Fatal("scale ES sample contains an obsolete generation")
		}
		var vector []float64
		d.decode(hit.Source[fmt.Sprintf("q_%d_vec", dim)], &vector)
		var norm float64
		for _, value := range vector {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				d.t.Fatal("scale ES sample has non-finite vector")
			}
			norm += value * value
		}
		if len(vector) != dim || norm == 0 {
			d.t.Fatal("scale ES sample has absent or zero embedding")
		}
	}
	d.report["verified_embedding_samples"] = len(result.Hits.Hits)
	d.report["vector_dimensions"] = dim
}
