package component

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"ragflow/internal/common"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/ingestion/component/schema"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/tokenizer"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestCodeArchiveRefusesUntrustedInvocationBeforeStorage(t *testing.T) {
	c, err := NewCodeArchiveComponent(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Invoke(context.Background(), nil, map[string]any{"binary": []byte("anything"), "name": "x.zip"}); codearchive.Reason(err) != "ZIP_NOT_ALLOWED_HERE" {
		t.Fatalf("unscoped invocation: %v", err)
	}
}

func TestCodeArchiveEmitsChunksAndSeparateTree(t *testing.T) {
	var buffer bytes.Buffer
	w := zip.NewWriter(&buffer)
	f, err := w.Create("src/a.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("def run():\n    return 1\n\ndef version():\n    return 5\n")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := archiveartifact.WithScope(context.Background(), archiveartifact.Scope{DocID: "doc", KBID: "kb", TaskID: "task", RunID: "run", AttemptID: "attempt"})
	ctx, collector := archiveartifact.WithCollector(ctx)
	ctx = WithCodeArchiveRuntime(ctx, CodeArchiveRuntime{
		Snapshot: &archiveartifact.Snapshot{Config: codearchive.Config{ChunkTokens: 512}, Policy: codearchive.DefaultPolicy(), Source: archiveartifact.Source{Filename: "snapshot.zip", Bucket: "kb", Path: "source", Hash: "hash", Size: int64(buffer.Len())}, Embedding: archiveartifact.Embedding{Reference: "model", Fingerprint: "identity"}},
		Filename: "snapshot.zip", ModelID: "model", CountTokens: utf8.RuneCountInString,
		FitsEmbedding: func(s string) bool { return utf8.RuneCountInString(s) <= 1024 },
		ReadOriginal:  func(context.Context) ([]byte, error) { return buffer.Bytes(), nil },
		Transform: func(_ context.Context, source codearchive.SourceFile) (codearchive.SourceFile, error) {
			source.Path = "renamed/a.py"
			return source, nil
		},
	})
	c, err := NewCodeArchiveComponent(map[string]any{"chunk_token_num": 512})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Invoke(ctx, nil, map[string]any{"doc_id": "doc", "bucket": "untrusted", "path": "ignored.zip"})
	if err != nil {
		t.Fatal(err)
	}
	if out["output_format"] != "chunks" {
		t.Fatalf("format: %v", out)
	}
	upstream, err := decodeTokenizerFromUpstream(out)
	if err != nil {
		t.Fatal(err)
	}
	chunks := chunksFromTokenizerUpstream(upstream)
	if len(chunks) != 1 || chunks[0].SourcePathKwd != "renamed/a.py" || chunks[0].StartLineInt == nil || *chunks[0].StartLineInt != 1 || *chunks[0].EndLineInt != 5 {
		t.Fatalf("chunks: %+v", chunks)
	}
	if !strings.Contains(chunks[0].Text, "return 1") || chunks[0].CodeLangKwd != "python" {
		t.Fatalf("source: %+v", chunks[0])
	}
	if !reflect.DeepEqual(chunks[0].CodeSymbols, []string{"run", "version"}) {
		t.Fatalf("symbols lost across component wire: %+v", chunks[0])
	}
	if err := tokenizer.Init(&tokenizer.PoolConfig{MinSize: 1, MaxSize: 2}); err != nil {
		t.Fatal(err)
	}
	if err := tokenizeChunks(chunks, "archive", "English"); err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := json.Unmarshal(chunks[0].Extra["important_kwd"], &names); err != nil || !reflect.DeepEqual(names, []string{"run", "version"}) {
		t.Fatalf("important keywords: %v %v", names, err)
	}
	var importantTokens string
	if err := json.Unmarshal(chunks[0].Extra["important_tks"], &importantTokens); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.Contains(importantTokens, name) {
			t.Fatalf("merged symbol missing from important_tks: %s in %q", name, importantTokens)
		}
	}
	if _, exists := out["tree"]; exists {
		t.Fatal("tree leaked into component payload")
	}
	artifact, err := collector.Take()
	if err != nil {
		t.Fatal(err)
	}
	if artifact.EmbeddingModelID != "model" || len(artifact.Tree) != 1 || artifact.Tree[0].Path != "renamed" {
		t.Fatalf("artifact: %+v", artifact)
	}
}

func TestCodeArchiveSyntaxFallbackWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, source, language string
		threshold              float64
		errorLines, totalLines int
	}{
		{"at-threshold.py", "private_source = 'never-log-me'\npass\npass\npass\n@ @\n", "python", .2, 0, 5},
		{"above-threshold.py", "private_source = 'never-log-me'\npass\npass\npass\n@ @\n", "python", .15, 1, 5},
		{"rescued.h", "int class;\nint template;\nint namespace;\n", "c", .2, 0, 3},
		{"broken.h", "@@@\n@@@\n", "cpp", .2, 2, 2},
		{"broken.vue", "<script>let first = ;</script><script>let second = ;</script>\n", "vue", .2, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			previous := common.Logger
			common.Logger = zap.New(core)
			t.Cleanup(func() { common.Logger = previous })

			var buffer bytes.Buffer
			w := zip.NewWriter(&buffer)
			f, err := w.Create("src/" + tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write([]byte(tc.source)); err != nil {
				t.Fatal(err)
			}
			if err = w.Close(); err != nil {
				t.Fatal(err)
			}
			policy := codearchive.DefaultPolicy()
			policy.SyntaxErrorRatio = tc.threshold
			ctx := archiveartifact.WithScope(t.Context(), archiveartifact.Scope{DocID: "doc", KBID: "kb", TaskID: "task", RunID: "run", AttemptID: "attempt"})
			ctx, collector := archiveartifact.WithCollector(ctx)
			ctx = WithCodeArchiveRuntime(ctx, CodeArchiveRuntime{
				Snapshot: &archiveartifact.Snapshot{Config: codearchive.Config{ChunkTokens: 512}, Policy: policy,
					Source:    archiveartifact.Source{Filename: "snapshot.zip", Bucket: "kb", Path: "source", Hash: "hash", Size: int64(buffer.Len())},
					Embedding: archiveartifact.Embedding{Reference: "model", Fingerprint: "identity"}},
				Filename: "snapshot.zip", ModelID: "model", CountTokens: utf8.RuneCountInString,
				FitsEmbedding: func(s string) bool { return utf8.RuneCountInString(s) <= 1024 },
				ReadOriginal:  func(context.Context) ([]byte, error) { return buffer.Bytes(), nil },
			})
			c, err := NewCodeArchiveComponent(nil)
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Invoke(ctx, nil, map[string]any{"doc_id": "doc"})
			if err != nil || out["output_format"] != "chunks" {
				t.Fatalf("output=%v error=%v", out, err)
			}
			artifact, err := collector.Take()
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if tc.errorLines != 0 {
				wantCount = 1
			}
			if logs.Len() != wantCount || artifact.Stats.SyntaxFallbacks != wantCount {
				t.Fatalf("warnings=%v stats=%+v", logs.All(), artifact.Stats)
			}
			if wantCount == 0 {
				return
			}
			entry := logs.All()[0]
			want := map[string]any{
				"doc_id": "doc", "kb_id": "kb", "task_id": "task", "archive_run_id": "run", "archive_attempt_id": "attempt",
				"source_path": "src/" + tc.name, "code_lang": tc.language, "error_lines": int64(tc.errorLines), "total_lines": int64(tc.totalLines),
				"error_ratio": float64(tc.errorLines) / float64(tc.totalLines), "threshold": tc.threshold,
			}
			if entry.Level != zap.WarnLevel || !reflect.DeepEqual(entry.ContextMap(), want) || !strings.Contains(entry.Message, "falling back to line splitting") {
				t.Fatalf("warning=%+v fields=%v, want=%v", entry.Entry, entry.ContextMap(), want)
			}
			encoded, err := json.Marshal(entry.ContextMap())
			if err != nil || strings.Contains(string(encoded)+entry.Message, "never-log-me") || strings.Contains(string(encoded)+entry.Message, tc.source) {
				t.Fatalf("source leaked into warning: %s %v", encoded, err)
			}
		})
	}
}

func TestCodeChunkFieldsSurviveTypedSerialization(t *testing.T) {
	start, end := 4, 8
	chunk := schema.ChunkDoc{Text: "code", SourcePathKwd: "src/a.go", StartLineInt: &start, EndLineInt: &end, CodeLangKwd: "go"}
	items := schema.ChunkDocsToMaps([]schema.ChunkDoc{chunk})
	if len(items) != 1 || items[0]["source_path_kwd"] != "src/a.go" || items[0]["code_lang_kwd"] != "go" {
		t.Fatalf("lost source fields: %v", items)
	}
}

func TestCodeIdentifierTermsPreserveCamelAcronymAndDigits(t *testing.T) {
	terms := codeIdentifierTerms("buildZipFileTree HTTPServer2 parse_json")
	for _, word := range []string{"build", "zip", "file", "tree", "http", "server", "2", "parse", "json"} {
		if !strings.Contains(" "+terms+" ", " "+word+" ") {
			t.Fatalf("missing %q in %q", word, terms)
		}
	}
}

func TestCodeEmbeddingPreservesHTMLLiterals(t *testing.T) {
	stub := newStubEmbedder(4)
	raw, err := NewTokenizerComponentWithResolver(map[string]any{"filename_embd_weight": 0}, func(context.Context, string, string) (Embedder, string, error) {
		return configurableTrimmer{stub}, "model", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := raw.(*TokenizerComponent)
	text := "// src/a.ts:L1-L1\nconst html = '<table><td>keep</td></table>';"
	_, _, err = c.embedChunks(t.Context(), "tenant", "kb", "snapshot.zip", []schema.ChunkDoc{{Text: text, SourcePathKwd: "src/a.ts"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.callInputs) != 1 || len(stub.callInputs[0]) != 1 || stub.callInputs[0][0] != text {
		t.Fatalf("altered embedding input: %v", stub.callInputs)
	}
}

func TestCodeSymbolsUseRealTokenizerAndKeywordBounds(t *testing.T) {
	if err := tokenizer.Init(&tokenizer.PoolConfig{MinSize: 1, MaxSize: 2}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("函", esKeywordMaxTermBytes)
	chunks := []schema.ChunkDoc{
		{SourcePathKwd: "src/a.go", Text: "source remains unchanged", CodeSymbols: []string{"Listener.parseHTTPTimeout2", "parse_json", "Listener.parseHTTPTimeout2", long}, Keywords: "unrelated"},
		{SourcePathKwd: "src/a.go", Text: "// src/a.go:L1-L2\n概览 a.go [go]\n", Keywords: "must-not-promote-overview"},
	}
	wire, err := json.Marshal(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(wire, &chunks); err != nil {
		t.Fatal(err)
	}
	if err = tokenizeChunks(chunks, "snapshot", "English"); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err = json.Unmarshal(chunks[0].Extra["important_kwd"], &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"Listener.parseHTTPTimeout2", "parse_json", sanitizeKeywordTerm(long)}
	if !reflect.DeepEqual(got, want) || len(got[2]) > esKeywordMaxTermBytes || !utf8.ValidString(got[2]) {
		t.Fatalf("keyword bounds/dedup failed: %v", got[:2])
	}
	names := strings.Join(want, " ")
	expected, err := tokenizer.New("English").Tokenize(names + " " + codeIdentifierTerms(names))
	if err != nil {
		t.Fatal(err)
	}
	var actual string
	if err = json.Unmarshal(chunks[0].Extra["important_tks"], &actual); err != nil || actual != expected || actual == "" {
		t.Fatalf("symbol tokenization differs: %v", err)
	}
	for _, word := range []string{"listener", "parse", "http", "timeout", "2", "json"} {
		if !strings.Contains(codeIdentifierTerms(names), word) {
			t.Fatalf("missing identifier term %s", word)
		}
	}
	if chunks[0].Text != "source remains unchanged" {
		t.Fatal("display/embedding text changed")
	}
	if err = json.Unmarshal(chunks[1].Extra["important_kwd"], &got); err != nil || len(got) != 0 {
		t.Fatal("overview received important keywords")
	}
}

func TestCodeEmbeddingRejectsTruncation(t *testing.T) {
	stub := newStubEmbedder(4)
	raw, err := NewTokenizerComponentWithResolver(map[string]any{"filename_embd_weight": 0}, func(context.Context, string, string) (Embedder, string, error) {
		return emptyingTrimmer{stub}, "model", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = raw.(*TokenizerComponent).embedChunks(t.Context(), "tenant", "kb", "snapshot.zip", []schema.ChunkDoc{{Text: "complete code", SourcePathKwd: "src/a.py"}}, nil)
	if codearchive.Reason(err) != "ZIP_TOKEN_LIMIT_EXCEEDED" || stub.calls.Load() != 0 {
		t.Fatalf("silent code truncation: %v", err)
	}
}
