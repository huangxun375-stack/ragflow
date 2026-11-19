package document

import (
	"strings"

	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/server"
	"ragflow/internal/service"
)

const codeArchiveComponentID = "CodeArchive:Archive"

func isArchiveDocument(doc *entity.Document) bool {
	return doc != nil && (doc.ParserID == codearchive.ParserID || doc.Type == "code" || strings.EqualFold(doc.Suffix, "zip") || doc.Name != nil && codearchive.IsArchive(*doc.Name))
}

func codeArchiveConfig(configs ...entity.JSONMap) entity.JSONMap {
	defaultTokens := server.GetCodeArchivePolicy().DefaultChunkTokens
	defaults, _ := codearchive.ParseConfig(nil, defaultTokens)
	params := map[string]any{"chunk_token_num": defaults.ChunkTokens, "code_file_overview": defaults.Overview}
	for _, config := range configs {
		raw, ok := nodeParams(config[codeArchiveComponentID])
		if !ok {
			continue
		}
		for _, key := range []string{"chunk_token_num", "code_file_overview"} {
			value, exists := raw[key]
			if !exists {
				continue
			}
			if _, err := codearchive.ParseConfig(map[string]any{key: value}, defaultTokens); err == nil {
				params[key] = value
			}
		}
	}
	return entity.JSONMap{codeArchiveComponentID: params}
}

func validateArchiveDocumentUpdate(doc *entity.Document, req *UpdateDatasetDocumentRequest, present map[string]bool) error {
	if !isArchiveDocument(doc) {
		if req.ParserID != nil && strings.TrimSpace(*req.ParserID) == codearchive.ParserID || req.ChunkMethod != nil && strings.TrimSpace(*req.ChunkMethod) == codearchive.ParserID {
			return &codearchive.Error{Code: "ZIP_ONLY"}
		}
		return nil
	}
	for _, id := range []*string{req.ParserID, req.ChunkMethod} {
		if id != nil && strings.TrimSpace(*id) != codearchive.ParserID {
			return &codearchive.Error{Code: "ZIP_CHUNK_METHOD_LOCKED"}
		}
	}
	if req.ParseType != nil && *req.ParseType != int(service.ParseModeBuiltIn) || req.PipelineID != nil && strings.TrimSpace(*req.PipelineID) != "" || req.PipelineVersionID != nil && strings.TrimSpace(*req.PipelineVersionID) != "" {
		return &codearchive.Error{Code: "ZIP_CHUNK_METHOD_LOCKED"}
	}
	if present["parser_config"] {
		if len(req.UnscopedParserConfigKeys) > 0 {
			return &codearchive.Error{Code: "ZIP_CONFIG_INVALID"}
		}
		return service.ValidateCodeArchiveConfig(req.ParserConfig)
	}
	return nil
}
