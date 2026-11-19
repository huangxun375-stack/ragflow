package service

import (
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/server"
)

// ValidateCodeArchiveConfig restricts overrides to the fixed archive node.
// Resource limits and all other pipeline components remain server-owned.
func ValidateCodeArchiveConfig(config map[string]any) error {
	for id, raw := range config {
		if id != "CodeArchive:Archive" {
			return &codearchive.Error{Code: "ZIP_CONFIG_INVALID"}
		}
		var params map[string]any
		switch value := raw.(type) {
		case map[string]any:
			params = value
		case entity.JSONMap:
			params = map[string]any(value)
		default:
			return &codearchive.Error{Code: "ZIP_CONFIG_INVALID"}
		}
		if _, err := codearchive.ParseConfig(params, server.GetCodeArchivePolicy().DefaultChunkTokens); err != nil {
			return err
		}
	}
	return nil
}
