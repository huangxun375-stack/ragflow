// Package codearchive validates and processes source archives entirely in memory.
// Grammar installation and token counting belong to the application composition root.
package codearchive

import (
	"context"
	"errors"
	"path"
	"strings"
)

const ParserID = "code_archive"

type Limits struct {
	MaxCompressedBytes int64 `json:"max_archive_bytes"`
	MaxFiles           int   `json:"max_members"`
	MaxDepth           int   `json:"max_depth"`
	MaxReadBytes       int64 `json:"max_uncompressed_bytes"`
	MaxFileBytes       int64 `json:"max_file_bytes"`
	MaxGitignoreBytes  int64 `json:"max_gitignore_bytes"`
	MaxChunks          int   `json:"max_chunks"`
}

func DefaultLimits() Limits {
	return Limits{10 << 20, 5000, 20, 100 << 20, 1 << 20, 64 << 10, 100000}
}

type Options struct {
	Filename    string
	Policy      *Policy
	ChunkTokens int
	Overview    bool
	// CountTokens counts cl100k (or a counter bounded above by UTF-8 byte length).
	CountTokens      func(string) int
	FitsEmbedding    func(string) bool
	Transform        SourceTransform
	OnSyntaxFallback func(SyntaxFallback)
}

// SyntaxFallback reports the final file-level decision after language retries and
// embedded-script error lines have been merged. It never contains source text.
type SyntaxFallback struct {
	Path       string
	Language   string
	ErrorLines int
	TotalLines int
	Ratio      float64
	Threshold  float64
}

// SourceTransform may transform a decoded file before syntax parsing. A non-nil
// LineMap maps each transformed line to an inclusive original line interval.
// A transform that changes line count must supply a map. Paths remain relative,
// unique archive paths; Language must retain the original language selection.
type SourceTransform func(context.Context, SourceFile) (SourceFile, error)

type SourceFile struct {
	Path     string
	Language string
	Text     string
	LineMap  []LineRange
}

type LineRange struct {
	Start int
	End   int
}

type Chunk struct {
	Symbols    []string `json:"symbols,omitempty"`
	Text       string   `json:"text"`
	SourcePath string   `json:"source_path"`
	Language   string   `json:"code_lang"`
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	Order      int      `json:"order"`
}

type TreeNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	Type     string     `json:"type"`
	Children []TreeNode `json:"children,omitempty"`
}

// FilteredEntry describes excluded members, never their contents. A directory
// groups every skipped file below a rule-excluded prefix, including files whose
// individual statistical category differs from the directory's reason.
type FilteredEntry struct {
	Path      string `json:"path"`
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	FileCount int    `json:"file_count"`
}

// Stats counts each file in exactly one accepted or skipped category.
// ReadBytes includes all bytes read from code candidates and ignore files.
type Stats struct {
	TotalFiles               int             `json:"total_files"`
	AcceptedFiles            int             `json:"accepted_files"`
	SkippedMeta              int             `json:"skipped_meta"`
	SkippedDir               int             `json:"skipped_dir"`
	SkippedGitignore         int             `json:"skipped_gitignore"`
	SkippedHidden            int             `json:"skipped_hidden"`
	SkippedSuffix            int             `json:"skipped_suffix"`
	SkippedLarge             int             `json:"skipped_large"`
	SkippedEmpty             int             `json:"skipped_empty"`
	SkippedBinary            int             `json:"skipped_binary"`
	SyntaxFallbacks          int             `json:"syntax_fallbacks"`
	ReadBytes                int64           `json:"read_bytes"`
	FilteredEntries          []FilteredEntry `json:"filtered_entries,omitempty"`
	FilteredEntriesTruncated bool            `json:"filtered_entries_truncated,omitempty"`
}

type Result struct {
	RootName string     `json:"root_name"`
	Tree     []TreeNode `json:"tree"`
	Stats    Stats      `json:"stats"`
	Chunks   []Chunk    `json:"chunks"`
}

// Error deliberately exposes only a stable reason, never an archive path or source.
type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }

func fail(code string) error { return &Error{Code: code} }

func Reason(err error) string {
	var archiveErr *Error
	if errors.As(err, &archiveErr) {
		return archiveErr.Code
	}
	return "ZIP_PARSE_FAILED"
}

func IsArchive(name string) bool { return strings.EqualFold(path.Ext(strings.TrimSpace(name)), ".zip") }
