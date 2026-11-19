package archiveartifact

import "ragflow/internal/parser/codearchive"

type Source struct {
	Filename string `json:"filename"`
	Bucket   string `json:"bucket"`
	Path     string `json:"path"`
	Hash     string `json:"hash"`
	Size     int64  `json:"size"`
}

// Embedding excludes API credentials. Fingerprint covers the resolved model
// identity, endpoint, tokenizer limits and provider parameters.
type Embedding struct {
	Reference   string `json:"reference"`
	Fingerprint string `json:"fingerprint"`
	Parameters  string `json:"parameters"`
}

type Snapshot struct {
	Config    codearchive.Config `json:"config"`
	Policy    codearchive.Policy `json:"policy"`
	Source    Source             `json:"source"`
	Embedding Embedding          `json:"embedding"`
}

func (s Snapshot) Validate() error {
	if err := s.Policy.Validate(); err != nil {
		return err
	}
	if s.Config.ChunkTokens < 1 || s.Config.ChunkTokens > 2048 || s.Source.Size <= 0 || s.Source.Size > s.Policy.Limits.MaxCompressedBytes || s.Source.Hash == "" || s.Source.Bucket == "" || s.Source.Path == "" || !codearchive.IsArchive(s.Source.Filename) || s.Embedding.Reference == "" || s.Embedding.Fingerprint == "" {
		return &codearchive.Error{Code: "ZIP_SNAPSHOT_INVALID"}
	}
	return nil
}
