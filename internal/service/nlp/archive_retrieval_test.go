package nlp

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/spf13/viper"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/server/config"
	"ragflow/internal/service/archivepublication"
)

func archiveRetrievalFixture(t *testing.T) {
	t.Helper()
	db := newRetrievalTestDB(t, "zip", "ordinary")
	if err := db.AutoMigrate(&entity.IngestionTask{}, &entity.DocumentCodeArchive{}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&entity.Knowledgebase{ID: "kb-1", TenantID: "tenant-1"},
		&entity.IngestionTask{ID: "task", DocumentID: "zip", DatasetID: "kb-1", Status: common.COMPLETED, PipelineLogID: new("run")},
		&entity.DocumentCodeArchive{DocID: "zip", KBID: "kb-1", TaskID: "task", RunID: "run", AttemptID: "attempt", Status: "ready"},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&entity.Document{}).Where("id = ?", "zip").Update("parser_id", "code_archive").Error; err != nil {
		t.Fatal(err)
	}
	prior := globalQueryBuilder
	globalQueryBuilder = NewQueryBuilder()
	t.Cleanup(func() { globalQueryBuilder = prior })
}

func archiveRow(id, path string, score float64) map[string]any {
	return map[string]any{"id": id, "doc_id": "zip", "kb_id": "kb-1", "source_path_kwd": path, "archive_run_kwd": "run", "archive_attempt_kwd": "attempt", "content_ltks": "alpha", "content_with_weight": "alpha", "_score": score}
}

func retrievalDeployment(t *testing.T, weight float64) *config.Config {
	t.Helper()
	c := &config.Config{}
	v := viper.New()
	v.Set("code_archive", map[string]any{"test_source_weight": weight})
	if err := c.ParseCodeArchiveConfig(v); err != nil {
		t.Fatal(err)
	}
	return c
}

func archiveRequest(question string) *RetrievalRequest {
	return &RetrievalRequest{Question: question, TenantIDs: []string{"tenant-1"}, KbIDs: []string{"kb-1"}, Page: 1, PageSize: 10, RerankCandidatesCount: new(10), VectorSimilarityWeight: new(1.0), SimilarityThreshold: new(.89), RankFeature: &map[string]float64{}, Aggs: new(false)}
}

type archiveInfinityEngine struct{ retrievalCountEngine }

func (*archiveInfinityEngine) GetType() string { return "infinity" }

func TestArchiveRetrievalSourcePriorUsesOriginalThresholdAndComponents(t *testing.T) {
	archiveRetrievalFixture(t)
	for _, backend := range []string{"elasticsearch", "infinity"} {
		for _, tc := range []struct {
			question string
			weight   float64
			penalize bool
		}{
			{"alpha implementation", .95, true}, {"alpha implementation", 1, false},
			{"alpha 测试用例", .95, false}, {"alpha 单测", .95, false}, {"alpha 用例", .95, false},
			{"alpha TEST", .95, false}, {"alpha tests", .95, false}, {"alpha spec", .95, false}, {"alpha mock", .95, false}, {"alpha benchmark", .95, false},
			{"alpha latest contest specification mockery", .95, true},
		} {
			t.Run(backend+tc.question+string(rune(int(tc.weight*100))), func(t *testing.T) {
				rows := []map[string]any{archiveRow("test", "driver_test.go", .91), archiveRow("prod", "driver.go", .90), {"id": "normal", "doc_id": "ordinary", "kb_id": "kb-1", "docnm_kwd": "driver_test.go", "content_ltks": "alpha", "content_with_weight": "alpha", "_score": .90}}
				eng := &retrievalCountEngine{rows: rows}
				svc := NewRetrievalService(eng, dao.NewDocumentDAO())
				if backend == "infinity" {
					svc.docEngine = &archiveInfinityEngine{retrievalCountEngine{rows: rows}}
				}
				svc.deployment = retrievalDeployment(t, tc.weight)
				result, err := svc.Retrieval(t.Context(), archiveRequest(tc.question))
				if err != nil {
					t.Fatal(err)
				}
				if result.Total != 3 || len(result.Chunks) != 3 {
					t.Fatalf("threshold changed count: %+v", result)
				}
				wantFirst, wantScore := "test", .91
				if tc.penalize {
					wantFirst, wantScore = "prod", .91*tc.weight
				}
				if result.Chunks[0]["chunk_id"] != wantFirst {
					t.Fatal(result.Chunks)
				}
				for _, row := range result.Chunks {
					if row["chunk_id"] == "test" {
						if math.Abs(row["similarity"].(float64)-wantScore) > 1e-12 || row["vector_similarity"] != .91 {
							t.Fatal(row)
						}
						if backend == "infinity" && row["term_similarity"] != .91 {
							t.Fatal("aliased component mutated", row)
						}
					} else if row["similarity"] != .90 {
						t.Fatal("ordinary or production score changed", row)
					}
				}
			})
		}
	}
}

func TestArchiveVectorHydrationTenantOnlyScope(t *testing.T) {
	archiveRetrievalFixture(t)
	row := archiveRow("chunk", "a.go", .91)
	row["q_2_vec"] = []float64{.1, .2}
	eng := &retrievalCountEngine{rows: []map[string]any{row}}
	svc := NewRetrievalService(eng, dao.NewDocumentDAO())
	got, err := svc.FetchChunkVectors(t.Context(), []string{"chunk"}, []string{" tenant-1,unused "}, nil, 2)
	if err != nil || !reflect.DeepEqual(got["chunk"], []float64{.1, .2}) {
		t.Fatalf("%v %v", got, err)
	}
	row["archive_attempt_kwd"] = "old"
	if _, err := svc.FetchChunkVectors(t.Context(), []string{"chunk"}, []string{"tenant-1"}, nil, 2); !errors.Is(err, archivepublication.ErrNotPublished) {
		t.Fatal(err)
	}
}

func TestArchiveTestIntentWordBoundaries(t *testing.T) {
	for _, q := range []string{"测试驱动", "做单测", "用例如何写", "TEST?", "(tests)", "spec/mock", "benchmark性能", "性能test怎么写", "unit tests", "__mocks__ mock"} {
		if !hasTestIntent(q) {
			t.Fatal(q)
		}
	}
	for _, q := range []string{"contest", "latest", "mockery", "specification", "test_connection", "mytest", "test2", "测试"[:0]} {
		if hasTestIntent(q) {
			t.Fatal(q)
		}
	}
}
