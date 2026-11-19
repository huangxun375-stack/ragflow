package task

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/ingestion/component"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/server/config"
	"ragflow/internal/service/archivepublication"
	"ragflow/internal/storage"
	"ragflow/internal/tokenizer"
)

func TestArchiveQueuedTargetRatioIgnoresDeploymentChanges(t *testing.T) {
	for _, tc := range []struct {
		frozen     float64
		deployment string
		want       int
	}{{.5, "0", 1}, {0, "0.75", 2}} {
		t.Run(tc.deployment, func(t *testing.T) {
			executor, ctx, backend := archiveSnapshotExecutor(t)
			scope, _ := archiveartifact.ScopeFrom(ctx)
			snapshot, err := archivepublication.LoadSnapshot(ctx, executor.deps.DB, scope)
			if err != nil {
				t.Fatal(err)
			}
			scope.RunID += "-target"
			snapshot.Policy.ChunkTargetRatio = tc.frozen
			if err := archivepublication.SaveSnapshot(ctx, executor.deps.DB, scope, snapshot); err != nil {
				t.Fatal(err)
			}
			ctx = archiveartifact.WithScope(ctx, scope)
			invoke := func() []map[string]any {
				t.Helper()
				prepared, _ := archiveartifact.WithCollector(ctx)
				prepared, err = executor.prepareCodeArchive(prepared)
				if err != nil {
					t.Fatal(err)
				}
				c, err := component.NewCodeArchiveComponent(nil)
				if err != nil {
					t.Fatal(err)
				}
				out, err := c.Invoke(prepared, executor.deps.DB, map[string]any{"doc_id": scope.DocID})
				if err != nil {
					t.Fatal(err)
				}
				chunks := out["chunks"].([]map[string]any)
				count := 0
				for _, chunk := range chunks {
					_, body, _ := strings.Cut(chunk["text"].(string), "\n")
					if !strings.HasPrefix(body, "概览 ") {
						count++
					}
				}
				if count != tc.want {
					t.Fatalf("frozen ratio=%g source chunks=%d want=%d", tc.frozen, count, tc.want)
				}
				return chunks
			}
			before := invoke()
			t.Setenv("RAGFLOW_CODE_ARCHIVE_CHUNK_TARGET_RATIO", tc.deployment)
			deployment := &config.Config{}
			if err := deployment.ParseCodeArchiveConfig(viper.New()); err != nil {
				t.Fatal(err)
			}
			policy := deployment.GetCodeArchivePolicy()
			if policy.ChunkTargetRatio == tc.frozen {
				t.Fatal("deployment did not change")
			}
			blob, err := storage.ReadBounded(ctx, backend, snapshot.Source.Bucket, snapshot.Source.Path, policy.Limits.MaxCompressedBytes)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := codearchive.Process(ctx, blob, codearchive.Options{Policy: &policy, ChunkTokens: 512, CountTokens: tokenizer.NumTokensFromString, FitsEmbedding: func(string) bool { return true }})
			if err != nil || len(fresh.Chunks) == tc.want {
				t.Fatalf("new policy should change fresh parse: %+v %v", fresh, err)
			}
			if after := invoke(); !reflect.DeepEqual(before, after) {
				t.Fatal("queued chunks drifted with deployment")
			}
			loaded, err := archivepublication.LoadSnapshot(ctx, executor.deps.DB, scope)
			if err != nil || loaded.Policy.ChunkTargetRatio != tc.frozen {
				t.Fatalf("snapshot ratio changed: %+v %v", loaded.Policy, err)
			}
		})
	}
}
