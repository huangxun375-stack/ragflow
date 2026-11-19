package component

import (
	"archive/zip"
	"bytes"
	"context"
	"slices"
	"testing"
	"unicode/utf8"

	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/ingestion/chunkcache"
	"ragflow/internal/ingestion/component/schema"
	"ragflow/internal/parser/codearchive"
)

func prepareArchiveCacheChunks(t *testing.T, run, attempt string, identity archiveartifact.Embedding, model Embedder) (context.Context, []schema.ChunkDoc) {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create("src/a.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("def run():\n    return 1\n")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := archiveartifact.WithScope(cacheTaskCtx("archive_"+attempt), archiveartifact.Scope{
		DocID: "doc", KBID: "kb", TaskID: "task", RunID: run, AttemptID: attempt,
	})
	ctx, collector := archiveartifact.WithCollector(ctx)
	ctx = WithCodeArchiveRuntime(ctx, CodeArchiveRuntime{
		Snapshot: &archiveartifact.Snapshot{
			Config: codearchive.Config{ChunkTokens: 512}, Policy: codearchive.DefaultPolicy(),
			Source:    archiveartifact.Source{Filename: "repo.zip", Bucket: "kb", Path: "original", Hash: "hash", Size: int64(buffer.Len())},
			Embedding: identity,
		},
		Filename: "repo.zip", ModelID: identity.Reference, Embedder: model,
		CountTokens:   utf8.RuneCountInString,
		FitsEmbedding: func(s string) bool { return utf8.RuneCountInString(s) <= 2048 },
		ReadOriginal:  func(context.Context) ([]byte, error) { return buffer.Bytes(), nil },
	})
	parser, err := NewCodeArchiveComponent(nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := parser.Invoke(ctx, nil, map[string]any{"doc_id": "doc"})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := decodeTokenizerFromUpstream(out)
	if err != nil {
		t.Fatal(err)
	}
	chunks := chunksFromTokenizerUpstream(upstream)
	if len(chunks) != 1 || chunks[0].ID == "" || chunks[0].SourcePathKwd != "src/a.py" {
		t.Fatalf("expected one identified code chunk from the real archive parser, got %+v", chunks)
	}
	artifact, err := collector.Take()
	if err != nil {
		t.Fatal(err)
	}
	if artifact.EmbeddingModelID != identity.Reference {
		t.Fatalf("model reference changed: got %q want %q", artifact.EmbeddingModelID, identity.Reference)
	}
	return ctx, chunks
}

func newArchiveCacheEmbedder(vector []float64) configurableTrimmer {
	model := newStubEmbedder(len(vector))
	model.resultsByCall = []embeddingCallResult{
		{vectors: [][]float64{vector}},
		{vectors: [][]float64{vector}},
	}
	return configurableTrimmer{model}
}

func newArchiveCacheTokenizer(t *testing.T, fallback Embedder) *TokenizerComponent {
	t.Helper()
	raw, err := NewTokenizerComponentWithResolver(map[string]any{"filename_embd_weight": 0},
		func(context.Context, string, string) (Embedder, string, error) {
			return fallback, "same-model", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return raw.(*TokenizerComponent)
}

func requireArchiveCacheVector(t *testing.T, chunks []schema.ChunkDoc, want []float64) {
	t.Helper()
	if len(chunks) != 1 {
		t.Fatalf("expected one embedded chunk, got %d", len(chunks))
	}
	if got := vecOf(t, chunks[0], "q_3_vec"); !slices.Equal(got, want) {
		t.Fatalf("embedding = %v, want %v", got, want)
	}
}

func TestCodeArchiveEmbeddingCacheReusesFrozenFingerprint(t *testing.T) {
	for _, run := range []string{"run-A", "run-B"} {
		t.Run(run, func(t *testing.T) {
			identity := archiveartifact.Embedding{Reference: "same-model", Fingerprint: "fingerprint-A"}
			vector := []float64{1, 0, 0}
			firstModel := newArchiveCacheEmbedder(vector)
			tokenizer := newArchiveCacheTokenizer(t, nil)
			store := newMemCacheStore()
			ctx, chunks := prepareArchiveCacheChunks(t, "run-A", "attempt-A", identity, firstModel)
			first, tokens, err := tokenizer.embedChunks(ctx, "tenant", "kb", "repo.zip", chunks, store)
			if err != nil {
				t.Fatal(err)
			}
			requireArchiveCacheVector(t, first, vector)
			if firstModel.calls.Load() != 1 || tokens <= 0 || store.countEmbKeys() != 1 {
				t.Fatalf("cache was not populated: calls=%d tokens=%d keys=%d", firstModel.calls.Load(), tokens, store.countEmbKeys())
			}

			retryModel := newArchiveCacheEmbedder(vector)
			ctx, chunks = prepareArchiveCacheChunks(t, run, "attempt-B", identity, retryModel)
			second, tokens, err := tokenizer.embedChunks(ctx, "tenant", "kb", "repo.zip", chunks, store)
			if err != nil {
				t.Fatal(err)
			}
			requireArchiveCacheVector(t, second, vector)
			if retryModel.calls.Load() != 0 || tokens != 0 || store.countEmbKeys() != 1 {
				t.Fatalf("same frozen model should reuse cache: calls=%d tokens=%d keys=%d", retryModel.calls.Load(), tokens, store.countEmbKeys())
			}
		})
	}
}

func TestCodeArchiveEmbeddingCacheSeparatesFrozenModels(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity archiveartifact.Embedding
	}{
		{"changed_fingerprint", archiveartifact.Embedding{Reference: "same-model", Fingerprint: "fingerprint-B"}},
		{"changed_reference", archiveartifact.Embedding{Reference: "other-model", Fingerprint: "fingerprint-A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldIdentity := archiveartifact.Embedding{Reference: "same-model", Fingerprint: "fingerprint-A"}
			oldVector, newVector := []float64{1, 0, 0}, []float64{0, 1, 0}
			oldModel := newArchiveCacheEmbedder(oldVector)
			tokenizer := newArchiveCacheTokenizer(t, nil)
			store := newMemCacheStore()
			ctx, chunks := prepareArchiveCacheChunks(t, "run-A", "attempt-A", oldIdentity, oldModel)
			first, _, err := tokenizer.embedChunks(ctx, "tenant", "kb", "repo.zip", chunks, store)
			if err != nil {
				t.Fatal(err)
			}
			requireArchiveCacheVector(t, first, oldVector)

			// A failed purge leaves the prior run's vector under the same chunk ID.
			newModel := newArchiveCacheEmbedder(newVector)
			ctx, chunks = prepareArchiveCacheChunks(t, "run-B", "attempt-B", tc.identity, newModel)
			second, _, err := tokenizer.embedChunks(ctx, "tenant", "kb", "repo.zip", chunks, store)
			if err != nil {
				t.Fatal(err)
			}
			requireArchiveCacheVector(t, second, newVector)
			if newModel.calls.Load() != 1 || store.countEmbKeys() != 2 {
				t.Fatalf("new frozen model must not use the old vector: calls=%d keys=%d", newModel.calls.Load(), store.countEmbKeys())
			}

			controlModel := newArchiveCacheEmbedder(newVector)
			ctx, chunks = prepareArchiveCacheChunks(t, "run-B", "attempt-control", tc.identity, controlModel)
			control, _, err := tokenizer.embedChunks(ctx, "tenant", "kb", "repo.zip", chunks, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireArchiveCacheVector(t, control, newVector)
			if controlModel.calls.Load() != 1 {
				t.Fatalf("no-cache control did not embed: calls=%d", controlModel.calls.Load())
			}

			for _, identity := range []archiveartifact.Embedding{tc.identity, oldIdentity} {
				want := newVector
				if identity == oldIdentity {
					want = oldVector
				}
				retryModel := newArchiveCacheEmbedder(want)
				ctx, chunks = prepareArchiveCacheChunks(t, "run-C", "attempt-C", identity, retryModel)
				retry, tokens, err := tokenizer.embedChunks(ctx, "tenant", "kb", "repo.zip", chunks, store)
				if err != nil {
					t.Fatal(err)
				}
				requireArchiveCacheVector(t, retry, want)
				if retryModel.calls.Load() != 0 || tokens != 0 || store.countEmbKeys() != 2 {
					t.Fatalf("each frozen model must retain its own cache: calls=%d tokens=%d keys=%d", retryModel.calls.Load(), tokens, store.countEmbKeys())
				}
			}
		})
	}
}

func TestCodeArchiveEmbeddingCacheIgnoresReferenceOnlyEntries(t *testing.T) {
	identity := archiveartifact.Embedding{Reference: "same-model", Fingerprint: "fingerprint-A"}
	model := newArchiveCacheEmbedder([]float64{0, 1, 0})
	tokenizer := newArchiveCacheTokenizer(t, nil)
	ctx, chunks := prepareArchiveCacheChunks(t, "run-B", "attempt-B", identity, model)
	store := newMemCacheStore()
	oldKey := chunkcache.Key("emb", identity.Reference, chunks[0].ID, chunks[0].Text)
	chunkcache.Set(ctx, store, oldKey, "[1,0,0]")

	out, _, err := tokenizer.embedChunks(ctx, "tenant", "kb", "repo.zip", chunks, store)
	if err != nil {
		t.Fatal(err)
	}
	requireArchiveCacheVector(t, out, []float64{0, 1, 0})
	if model.calls.Load() != 1 || store.countEmbKeys() != 2 {
		t.Fatalf("reference-only cache must not be reused or overwritten: calls=%d keys=%d", model.calls.Load(), store.countEmbKeys())
	}
	if cached, hit := chunkcache.Get(ctx, store, oldKey); !hit || cached != "[1,0,0]" {
		t.Fatalf("reference-only entry was modified: hit=%v value=%q", hit, cached)
	}
}

func TestCodeArchiveEmbeddingCacheRequiresTrustedFingerprint(t *testing.T) {
	for _, scenario := range []string{"no_runtime", "no_snapshot", "empty_fingerprint", "blank_fingerprint", "mismatched_reference"} {
		t.Run(scenario, func(t *testing.T) {
			identity := archiveartifact.Embedding{Reference: "same-model", Fingerprint: "fingerprint-A"}
			oldModel := newArchiveCacheEmbedder([]float64{1, 0, 0})
			ctx, chunks := prepareArchiveCacheChunks(t, "run-A", "attempt-A", identity, oldModel)
			store := newMemCacheStore()
			if _, _, err := newArchiveCacheTokenizer(t, nil).embedChunks(ctx, "tenant", "kb", "repo.zip", chunks, store); err != nil {
				t.Fatal(err)
			}
			oldKey := chunkcache.Key("emb", identity.Reference, chunks[0].ID, chunks[0].Text)
			chunkcache.Set(ctx, store, oldKey, "[1,0,0]")
			keysBefore := store.countEmbKeys()

			model := newArchiveCacheEmbedder([]float64{0, 1, 0})
			ctx, chunks = prepareArchiveCacheChunks(t, "run-B", "attempt-B", identity, model)
			options, _ := codeArchiveRuntime(ctx)
			// Exercise the tokenizer boundary after losing trusted runtime metadata,
			// while retaining real parser output and an untrusted fingerprint field.
			switch scenario {
			case "no_runtime":
				ctx = cacheTaskCtx("archive_without_runtime")
			case "no_snapshot":
				options.Snapshot = nil
			case "empty_fingerprint":
				options.Snapshot.Embedding.Fingerprint = ""
			case "blank_fingerprint":
				options.Snapshot.Embedding.Fingerprint = " \t\n"
			case "mismatched_reference":
				options.Snapshot.Embedding.Reference = "other-model"
			}
			if scenario != "no_runtime" {
				ctx = WithCodeArchiveRuntime(ctx, options)
			}
			if err := chunks[0].SetExtraValue("embedding_fingerprint", identity.Fingerprint); err != nil {
				t.Fatal(err)
			}
			tokenizer := newArchiveCacheTokenizer(t, model)
			for pass := 1; pass <= 2; pass++ {
				out, _, err := tokenizer.embedChunks(ctx, "tenant", "kb", "repo.zip", cloneChunkDocs(chunks), store)
				if err != nil {
					t.Fatal(err)
				}
				requireArchiveCacheVector(t, out, []float64{0, 1, 0})
				if got := model.calls.Load(); got != int32(pass) {
					t.Fatalf("untrusted identity reused cache on pass %d: calls=%d", pass, got)
				}
			}
			if store.countEmbKeys() != keysBefore {
				t.Fatalf("untrusted identity wrote cache: before=%d after=%d", keysBefore, store.countEmbKeys())
			}
			if cached, hit := chunkcache.Get(ctx, store, oldKey); !hit || cached != "[1,0,0]" {
				t.Fatalf("untrusted identity modified reference-only cache: hit=%v value=%q", hit, cached)
			}
		})
	}
}

func TestCodeArchiveEmbeddingCacheLeavesNonArchiveKeysUnchanged(t *testing.T) {
	ctx := cacheTaskCtx("ordinary-document")
	store := newMemCacheStore()
	model := newArchiveCacheEmbedder([]float64{1, 0, 0})
	chunks := []schema.ChunkDoc{chunkWithID("ordinary-chunk", "ordinary text")}
	out, _, err := newArchiveCacheTokenizer(t, model).embedChunks(ctx, "tenant", "kb", "", cloneChunkDocs(chunks), store)
	if err != nil {
		t.Fatal(err)
	}
	requireArchiveCacheVector(t, out, []float64{1, 0, 0})
	key := chunkcache.Key("emb", "same-model", "ordinary-chunk", "ordinary text")
	if cached, hit := chunkcache.Get(ctx, store, key); !hit || cached != "[1,0,0]" || model.calls.Load() != 1 {
		t.Fatalf("ordinary cache key changed: hit=%v value=%q calls=%d", hit, cached, model.calls.Load())
	}

	retryModel := newArchiveCacheEmbedder([]float64{0, 1, 0})
	out, tokens, err := newArchiveCacheTokenizer(t, retryModel).embedChunks(ctx, "tenant", "kb", "", cloneChunkDocs(chunks), store)
	if err != nil {
		t.Fatal(err)
	}
	requireArchiveCacheVector(t, out, []float64{1, 0, 0})
	if retryModel.calls.Load() != 0 || tokens != 0 || store.countEmbKeys() != 1 {
		t.Fatalf("ordinary cache reuse changed: calls=%d tokens=%d keys=%d", retryModel.calls.Load(), tokens, store.countEmbKeys())
	}
}
