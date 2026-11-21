package codearchive

import (
	"math"
	"slices"
	"strings"
)

// Policy is the deployment-owned processing policy frozen with each archive run.
// Safety budgets may be tightened, but cannot exceed the supported hard caps.
type Policy struct {
	Limits             Limits   `json:"limits"`
	IgnoredDirs        []string `json:"ignored_dirs"`
	RespectGitignore   bool     `json:"respect_gitignore"`
	SyntaxErrorRatio   float64  `json:"syntax_error_ratio"`
	DefaultChunkTokens int      `json:"default_chunk_token_num"`
	ChunkTargetRatio   float64  `json:"chunk_target_ratio"`
	MaxArtifactBytes   int      `json:"artifact_max_bytes"`
	Languages          []string `json:"languages"`
	Extensions         []string `json:"extensions"`
}

func DefaultPolicy() Policy {
	return Policy{
		Limits:           DefaultLimits(),
		IgnoredDirs:      []string{".git", ".hg", ".idea", ".svn", ".venv", ".vscode", "__pycache__", "build", "dist", "node_modules", "target", "vendor", "venv"},
		RespectGitignore: true, SyntaxErrorRatio: .2, DefaultChunkTokens: 512, ChunkTargetRatio: .5, MaxArtifactBytes: 8 << 20,
		Languages:  []string{"c", "cpp", "go", "java", "javascript", "lua", "python", "tsx", "typescript", "vue"},
		Extensions: []string{".c", ".cc", ".cpp", ".cxx", ".go", ".h", ".hpp", ".hxx", ".java", ".js", ".jsx", ".lh", ".li", ".ls", ".lua", ".py", ".ts", ".tsx", ".vue"},
	}
}

func (p Policy) Validate() error {
	if err := p.Limits.validate(); err != nil {
		return err
	}
	defaults := DefaultPolicy()
	if p.MaxArtifactBytes < 1 || p.MaxArtifactBytes > defaults.MaxArtifactBytes || p.DefaultChunkTokens < 1 || p.DefaultChunkTokens > 2048 || math.IsNaN(p.SyntaxErrorRatio) || p.SyntaxErrorRatio < 0 || p.SyntaxErrorRatio > 1 {
		return fail("ZIP_CONFIG_INVALID")
	}
	if math.IsNaN(p.ChunkTargetRatio) || p.ChunkTargetRatio < 0 || p.ChunkTargetRatio > 1 {
		return fail("ZIP_CONFIG_INVALID")
	}
	for _, dir := range defaults.IgnoredDirs {
		if !slices.Contains(p.IgnoredDirs, dir) {
			return fail("ZIP_CONFIG_INVALID")
		}
	}
	for _, dir := range p.IgnoredDirs {
		clean, err := safePath(dir, 1, false)
		if err != nil || clean != dir || strings.ContainsAny(dir, "/*?[]") {
			return fail("ZIP_CONFIG_INVALID")
		}
	}
	if !supportedSubset(p.Languages, defaults.Languages) || !supportedSubset(p.Extensions, defaults.Extensions) {
		return fail("ZIP_CONFIG_INVALID")
	}
	return nil
}

func supportedSubset(selected, supported []string) bool {
	if len(selected) == 0 {
		return false
	}
	seen := make(map[string]bool, len(selected))
	for _, value := range selected {
		if seen[value] || !slices.Contains(supported, value) {
			return false
		}
		seen[value] = true
	}
	return true
}

func (l Limits) validate() error {
	d := DefaultLimits()
	for _, pair := range [][2]int64{
		{l.MaxCompressedBytes, d.MaxCompressedBytes}, {int64(l.MaxFiles), int64(d.MaxFiles)},
		{int64(l.MaxDepth), int64(d.MaxDepth)}, {l.MaxReadBytes, d.MaxReadBytes},
		{l.MaxFileBytes, d.MaxFileBytes}, {l.MaxGitignoreBytes, d.MaxGitignoreBytes},
		{int64(l.MaxChunks), int64(d.MaxChunks)},
	} {
		if pair[0] < 1 || pair[0] > pair[1] {
			return fail("ZIP_CONFIG_INVALID")
		}
	}
	return nil
}
