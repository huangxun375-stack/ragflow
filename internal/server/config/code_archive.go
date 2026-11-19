package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/viper"
	"ragflow/internal/parser/codearchive"
)

// CodeArchiveRetrievalConfig is deployment-only and never frozen into a parse policy.
type CodeArchiveRetrievalConfig struct {
	TestSourceWeight float64 `json:"test_source_weight"`
}

func (c *Config) GetCodeArchiveRetrievalConfig() CodeArchiveRetrievalConfig {
	if c == nil || c.codeArchiveRetrieval == nil {
		return CodeArchiveRetrievalConfig{TestSourceWeight: 0.95}
	}
	return *c.codeArchiveRetrieval
}

// Deployment YAML flattens the resource limits into the code_archive section.
type codeArchiveSettings struct {
	codearchive.Limits
	IgnoredDirs        []string `json:"ignored_dirs"`
	RespectGitignore   bool     `json:"respect_gitignore"`
	SyntaxErrorRatio   float64  `json:"syntax_error_ratio"`
	DefaultChunkTokens int      `json:"default_chunk_token_num"`
	ChunkTargetRatio   float64  `json:"chunk_target_ratio"`
	MaxArtifactBytes   int      `json:"artifact_max_bytes"`
	Languages          []string `json:"languages"`
	Extensions         []string `json:"extensions"`
	CodeArchiveRetrievalConfig
}

func (c *Config) ParseCodeArchiveConfig(v *viper.Viper) error {
	defaults := codearchive.DefaultPolicy()
	settings := codeArchiveSettings{
		CodeArchiveRetrievalConfig: CodeArchiveRetrievalConfig{TestSourceWeight: 0.95},
		Limits:                     defaults.Limits, RespectGitignore: defaults.RespectGitignore,
		SyntaxErrorRatio: defaults.SyntaxErrorRatio, DefaultChunkTokens: defaults.DefaultChunkTokens, ChunkTargetRatio: defaults.ChunkTargetRatio,
		MaxArtifactBytes: defaults.MaxArtifactBytes, Languages: defaults.Languages, Extensions: defaults.Extensions,
	}
	values := make(map[string]json.RawMessage)
	if v.IsSet("code_archive") {
		raw := v.Get("code_archive")
		encoded, err := json.Marshal(raw)
		if err != nil {
			return fmt.Errorf("code_archive: invalid configuration")
		}
		if err := json.Unmarshal(encoded, &values); err != nil || values == nil {
			return fmt.Errorf("code_archive: configuration must be an object")
		}
	}
	// LookupEnv applies nested overrides and preserves explicitly empty values;
	// AutomaticEnv on the parent Viper map does neither. Decode the same typed
	// JSON representation for YAML and environment values, without coercion.
	knownJSON, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(knownJSON, &known); err != nil {
		return err
	}
	for key := range known {
		if value, exists := os.LookupEnv("RAGFLOW_CODE_ARCHIVE_" + strings.ToUpper(key)); exists {
			values[key] = json.RawMessage(value)
		}
	}
	for key, value := range values {
		if !json.Valid(value) || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("code_archive.%s: expected a non-null typed value", key)
		}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("code_archive: invalid configuration")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return fmt.Errorf("code_archive: %w", err)
	}
	ignored := append(slices.Clone(defaults.IgnoredDirs), settings.IgnoredDirs...)
	slices.Sort(ignored)
	ignored = slices.Compact(ignored)
	policy := codearchive.Policy{
		Limits: settings.Limits, IgnoredDirs: ignored, RespectGitignore: settings.RespectGitignore,
		SyntaxErrorRatio: settings.SyntaxErrorRatio, DefaultChunkTokens: settings.DefaultChunkTokens, ChunkTargetRatio: settings.ChunkTargetRatio,
		MaxArtifactBytes: settings.MaxArtifactBytes, Languages: settings.Languages, Extensions: settings.Extensions,
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("code_archive: %w", err)
	}
	if settings.TestSourceWeight <= 0 || settings.TestSourceWeight > 1 {
		return fmt.Errorf("code_archive.test_source_weight: must be in (0, 1]")
	}
	c.codeArchive = &policy
	c.codeArchiveRetrieval = &settings.CodeArchiveRetrievalConfig
	return nil
}

func (c *Config) GetCodeArchivePolicy() codearchive.Policy {
	if c == nil || c.codeArchive == nil {
		return codearchive.DefaultPolicy()
	}
	policy := *c.codeArchive
	policy.IgnoredDirs = slices.Clone(policy.IgnoredDirs)
	policy.Languages = slices.Clone(policy.Languages)
	policy.Extensions = slices.Clone(policy.Extensions)
	return policy
}
