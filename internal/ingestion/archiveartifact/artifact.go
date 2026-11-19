// Package archiveartifact carries trusted run identity and a side-channel tree
// between the archive component and the ingestion transaction boundary.
package archiveartifact

import (
	"context"
	"encoding/json"
	"errors"
	"ragflow/internal/parser/codearchive"
	"sync"
)

type Scope struct{ DocID, KBID, TaskID, RunID, AttemptID string }
type Artifact struct {
	EmbeddingModelID string                 `json:"embedding_model_id"`
	RootName         string                 `json:"root_name"`
	Tree             []codearchive.TreeNode `json:"tree"`
	Stats            codearchive.Stats      `json:"stats"`
}
type contextKey uint8

const (
	scopeKey contextKey = iota
	collectorKey
)

func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey, scope)
}
func ScopeFrom(ctx context.Context) (Scope, bool) { s, ok := ctx.Value(scopeKey).(Scope); return s, ok }
func WithCollector(ctx context.Context) (context.Context, *Collector) {
	c := &Collector{}
	return context.WithValue(ctx, collectorKey, c), c
}
func CollectorFrom(ctx context.Context) (*Collector, bool) {
	c, ok := ctx.Value(collectorKey).(*Collector)
	return c, ok
}

type Collector struct {
	mu    sync.Mutex
	raw   []byte
	taken bool
}

const maxArtifactBytes = 8 << 20

func treeTextSize(nodes []codearchive.TreeNode, remaining int) (int, bool) {
	for _, node := range nodes {
		remaining -= len(node.Name) + len(node.Path)
		if remaining < 0 {
			return 0, false
		}
		var ok bool
		remaining, ok = treeTextSize(node.Children, remaining)
		if !ok {
			return 0, false
		}
	}
	return remaining, true
}

func (c *Collector) Set(artifact Artifact, limit int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.raw != nil || c.taken {
		return errors.New("archive artifact already collected")
	}
	if artifact.RootName == "" || len(artifact.Tree) == 0 || artifact.Stats.AcceptedFiles <= 0 {
		return errors.New("empty archive artifact")
	}
	if limit <= 0 || limit > maxArtifactBytes {
		limit = maxArtifactBytes
	}
	if _, ok := treeTextSize(artifact.Tree, limit-len(artifact.RootName)); !ok {
		return &codearchive.Error{Code: "ZIP_ARTIFACT_LIMIT_EXCEEDED"}
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	// Diagnostic paths must not make an otherwise publishable archive fail.
	for len(raw) > limit && len(artifact.Stats.FilteredEntries) > 0 {
		artifact.Stats.FilteredEntries = artifact.Stats.FilteredEntries[:len(artifact.Stats.FilteredEntries)/2]
		artifact.Stats.FilteredEntriesTruncated = true
		raw, err = json.Marshal(artifact)
		if err != nil {
			return err
		}
	}
	if len(raw) > limit {
		return &codearchive.Error{Code: "ZIP_ARTIFACT_LIMIT_EXCEEDED"}
	}
	c.raw = raw
	return nil
}
func (c *Collector) Take() (Artifact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var artifact Artifact
	if c.raw == nil || c.taken {
		return artifact, errors.New("archive artifact missing or consumed")
	}
	if err := json.Unmarshal(c.raw, &artifact); err != nil {
		return artifact, err
	}
	c.raw = nil
	c.taken = true
	return artifact, nil
}
