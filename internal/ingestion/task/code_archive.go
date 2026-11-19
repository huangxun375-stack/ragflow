package task

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
	componentpkg "ragflow/internal/ingestion/component"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/permission"
	"ragflow/internal/service"
	"ragflow/internal/service/archivepublication"
	"ragflow/internal/storage"
	"ragflow/internal/tokenizer"

	"github.com/zeebo/xxh3"
)

func (s *PipelineExecutor) prepareCodeArchive(ctx context.Context) (context.Context, error) {
	if s.taskCtx.Doc.ParserID != codearchive.ParserID {
		return ctx, nil
	}
	scope, ok := archiveartifact.ScopeFrom(ctx)
	if !ok || scope.DocID != s.taskCtx.Doc.ID || scope.KBID != s.taskCtx.KB.ID {
		return ctx, &codearchive.Error{Code: "ZIP_NOT_ALLOWED_HERE"}
	}
	snapshot, err := archivepublication.LoadSnapshot(ctx, s.deps.DB, scope)
	if err != nil {
		return ctx, err
	}
	if s.taskCtx.KB.EmbdID != snapshot.Embedding.Reference {
		return ctx, fmt.Errorf("%w: dataset model binding changed; start a new run", &codearchive.Error{Code: "ZIP_EMBEDDING_CHANGED"})
	}
	model, identity, err := service.ArchiveEmbeddingModel(permission.WithTenantID(ctx, s.taskCtx.Tenant.ID), s.deps.DB, snapshot.Embedding.Reference)
	if err != nil {
		return ctx, err
	}
	if identity != snapshot.Embedding {
		return ctx, fmt.Errorf("%w: embedding model parameters changed; start a new run", &codearchive.Error{Code: "ZIP_EMBEDDING_CHANGED"})
	}
	e := &embedder{model: model, strictArchive: true}
	limiter := e.limiter()
	if e.limiterErr != nil {
		return ctx, e.limiterErr
	}
	if !limiter.Counter().Available() {
		return ctx, &codearchive.Error{Code: "ZIP_UNSUPPORTED_DEPLOYMENT"}
	}
	backend := storage.GetStorageFactory().GetStorage()
	if backend == nil {
		return ctx, &codearchive.Error{Code: "ZIP_UNSUPPORTED_DEPLOYMENT"}
	}
	limit := snapshot.Policy.Limits.MaxCompressedBytes
	expectedHash := snapshot.Source.Hash
	expectedSize := snapshot.Source.Size
	s.taskCtx.Doc.Name = new(snapshot.Source.Filename)
	s.taskCtx.Doc.ParserConfig = entity.JSONMap{"CodeArchive:Archive": map[string]any{"chunk_token_num": snapshot.Config.ChunkTokens, "code_file_overview": snapshot.Config.Overview}}
	options := componentpkg.CodeArchiveRuntime{
		Filename: snapshot.Source.Filename, ModelID: snapshot.Embedding.Reference, Embedder: e, CountTokens: tokenizer.NumTokensFromString,
		Snapshot:      &snapshot,
		Transform:     archiveartifact.SourceTransformFrom(ctx),
		FitsEmbedding: e.FitsInput,
		ReadOriginal: func(readCtx context.Context) ([]byte, error) {
			blob, err := storage.ReadBounded(readCtx, backend, snapshot.Source.Bucket, snapshot.Source.Path, limit)
			if errors.Is(err, storage.ErrObjectTooLarge) {
				return nil, &codearchive.Error{Code: "ZIP_SIZE_EXCEEDED"}
			}
			if err != nil {
				return nil, err
			}
			hash := xxh3.Hash128(blob).Bytes()
			if int64(len(blob)) != expectedSize || hex.EncodeToString(hash[:]) != expectedHash {
				return nil, &codearchive.Error{Code: "ZIP_SOURCE_CHANGED"}
			}
			return blob, nil
		},
	}
	return componentpkg.WithCodeArchiveRuntime(ctx, options), nil
}
