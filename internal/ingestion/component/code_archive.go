package component

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"ragflow/internal/agent/runtime"
	"ragflow/internal/common"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/ingestion/component/globals"
	"ragflow/internal/ingestion/component/schema"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/server"

	tspack "github.com/xberg-io/tree-sitter-language-pack/packages/go"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// CodeArchiveRuntime is trusted, per-execution wiring. It is never decoded
// from the DSL or an HTTP request. Transform is the future preprocessing seam.
type CodeArchiveRuntime struct {
	Snapshot      *archiveartifact.Snapshot
	Filename      string
	ModelID       string
	ReadOriginal  func(context.Context) ([]byte, error)
	CountTokens   func(string) int
	FitsEmbedding func(string) bool
	Transform     codearchive.SourceTransform
	Embedder      Embedder
}

type archiveRuntimeKey struct{}

func WithCodeArchiveRuntime(ctx context.Context, options CodeArchiveRuntime) context.Context {
	return context.WithValue(ctx, archiveRuntimeKey{}, options)
}

func codeArchiveRuntime(ctx context.Context) (CodeArchiveRuntime, bool) {
	options, ok := ctx.Value(archiveRuntimeKey{}).(CodeArchiveRuntime)
	return options, ok
}

var archiveGrammarOnce sync.Once
var archiveGrammarError error

// EnsureCodeArchiveGrammars configures a deployment-prepared cache. It never
// downloads a grammar; Process checks each needed language before GetLanguage.
func EnsureCodeArchiveGrammars() error {
	archiveGrammarOnce.Do(func() {
		cache := os.Getenv("RAGFLOW_CODE_ARCHIVE_GRAMMAR_CACHE")
		if cache == "" {
			root := os.Getenv("RAG_PROJECT_BASE")
			if root == "" {
				root, _ = os.Getwd()
				for {
					if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
						break
					}
					parent := filepath.Dir(root)
					if parent == root {
						break
					}
					root = parent
				}
			}
			cache = filepath.Join(root, "deps", "code_archive", "grammars")
		}
		if info, err := os.Stat(cache); err != nil || !info.IsDir() {
			archiveGrammarError = &codearchive.Error{Code: "ZIP_UNSUPPORTED_DEPLOYMENT"}
			return
		}
		if err := tspack.Configure(tspack.PackConfig{CacheDir: &cache}); err != nil {
			archiveGrammarError = &codearchive.Error{Code: "ZIP_UNSUPPORTED_DEPLOYMENT"}
		}
	})
	return archiveGrammarError
}

type CodeArchiveComponent struct{}

func NewCodeArchiveComponent(params map[string]any) (runtime.Component, error) {
	_, err := codearchive.ParseConfig(params, server.GetCodeArchivePolicy().DefaultChunkTokens)
	if err != nil {
		return nil, err
	}
	return &CodeArchiveComponent{}, nil
}

func (*CodeArchiveComponent) Inputs() map[string]string {
	return map[string]string{"doc_id": "Document identity supplied by File."}
}
func (*CodeArchiveComponent) Outputs() map[string]string {
	return map[string]string{"output_format": "Always chunks.", "chunks": "Code chunks with original source locations."}
}

func (c *CodeArchiveComponent) Invoke(ctx context.Context, _ *gorm.DB, inputs map[string]any) (map[string]any, error) {
	if !server.SupportsCodeArchive() {
		return nil, &codearchive.Error{Code: "ZIP_UNSUPPORTED_DEPLOYMENT"}
	}
	scope, ok := archiveartifact.ScopeFrom(ctx)
	if !ok || scope.DocID == "" || scope.RunID == "" || scope.AttemptID == "" {
		return nil, &codearchive.Error{Code: "ZIP_NOT_ALLOWED_HERE"}
	}
	if id, ok := inputs["doc_id"].(string); ok && id != "" && id != scope.DocID {
		return nil, &codearchive.Error{Code: "ZIP_NOT_ALLOWED_HERE"}
	}
	options, ok := codeArchiveRuntime(ctx)
	if !ok || options.Snapshot == nil || options.ReadOriginal == nil || options.CountTokens == nil || options.FitsEmbedding == nil || !codearchive.IsArchive(options.Filename) {
		return nil, &codearchive.Error{Code: "ZIP_UNSUPPORTED_DEPLOYMENT"}
	}
	if err := options.Snapshot.Validate(); err != nil {
		return nil, err
	}
	collector, ok := archiveartifact.CollectorFrom(ctx)
	if !ok {
		return nil, &codearchive.Error{Code: "ZIP_PARSE_FAILED"}
	}
	if err := EnsureCodeArchiveGrammars(); err != nil {
		return nil, err
	}
	data, err := options.ReadOriginal(ctx)
	if err != nil {
		return nil, err
	}
	result, err := codearchive.Process(ctx, data, codearchive.Options{
		Filename: options.Filename, Policy: &options.Snapshot.Policy, ChunkTokens: options.Snapshot.Config.ChunkTokens, Overview: options.Snapshot.Config.Overview,
		CountTokens: options.CountTokens, FitsEmbedding: options.FitsEmbedding, Transform: options.Transform,
		OnSyntaxFallback: func(f codearchive.SyntaxFallback) {
			common.Warn("code archive syntax error ratio exceeded threshold; falling back to line splitting",
				zap.String("doc_id", scope.DocID), zap.String("kb_id", scope.KBID), zap.String("task_id", scope.TaskID),
				zap.String("archive_run_id", scope.RunID), zap.String("archive_attempt_id", scope.AttemptID),
				zap.String("source_path", f.Path), zap.String("code_lang", f.Language),
				zap.Int("error_lines", f.ErrorLines), zap.Int("total_lines", f.TotalLines),
				zap.Float64("error_ratio", f.Ratio), zap.Float64("threshold", f.Threshold))
		},
	})
	if err != nil {
		return nil, err
	}
	chunks := make([]schema.ChunkDoc, 0, len(result.Chunks))
	for _, source := range result.Chunks {
		start, end := source.StartLine, source.EndLine
		chunks = append(chunks, schema.ChunkDoc{ID: common.ChunkID(scope.DocID, source.Text), Text: source.Text, DocType: "text", SourcePathKwd: source.SourcePath, StartLineInt: &start, EndLineInt: &end, CodeLangKwd: source.Language, CodeSymbols: source.Symbols})
	}
	if len(chunks) == 0 {
		return nil, &codearchive.Error{Code: "ZIP_NO_SUPPORTED_FILES"}
	}
	artifact := archiveartifact.Artifact{RootName: result.RootName, Tree: result.Tree, Stats: result.Stats, EmbeddingModelID: options.ModelID}
	if err = collector.Set(artifact, options.Snapshot.Policy.MaxArtifactBytes); err != nil {
		return nil, &codearchive.Error{Code: codearchive.Reason(err)}
	}
	globals.PublishGlobals(ctx, map[string]any{"name": options.Filename})
	return map[string]any{"output_format": "chunks", "chunks": schema.ChunkDocsToMaps(chunks)}, nil
}

func init() {
	c := &CodeArchiveComponent{}
	runtime.MustRegister(common.ComponentNameCodeArchive, runtime.CategoryIngestion,
		func(_ string, params map[string]any) (runtime.Component, error) {
			return NewCodeArchiveComponent(params)
		},
		runtime.Metadata{Version: "1.0.0", Inputs: c.Inputs(), Outputs: c.Outputs()})
}
