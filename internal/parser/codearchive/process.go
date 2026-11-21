package codearchive

import (
	"context"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

func Process(ctx context.Context, blob []byte, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	policy := DefaultPolicy()
	if opts.Policy != nil {
		policy = *opts.Policy
	}
	if err := policy.Validate(); err != nil {
		return Result{}, err
	}
	opts.Policy = &policy
	if opts.ChunkTokens == 0 {
		opts.ChunkTokens = policy.DefaultChunkTokens
	}
	if opts.CountTokens == nil || opts.FitsEmbedding == nil || opts.ChunkTokens < 1 || opts.ChunkTokens > 2048 {
		return Result{}, fail("ZIP_CONFIG_INVALID")
	}
	limits := policy.Limits
	entries, err := inspect(ctx, blob, limits)
	if err != nil {
		return Result{}, err
	}
	budget := &readerBudget{ctx: ctx, limit: limits.MaxReadBytes}
	ignore, err := loadIgnore(entries, budget, policy)
	if err != nil {
		return Result{}, err
	}
	result := Result{RootName: archiveRootName(entries, opts.Filename), Tree: []TreeNode{}}
	filtered := filteredRecorder{stats: &result.Stats, policy: policy, ignore: ignore}
	paths := make(map[string]bool)
	directories := make(map[string]bool)
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if e.directory {
			continue
		}
		result.Stats.TotalFiles++
		skip := func(counter *int, reason string) {
			(*counter)++
			filtered.add(e.path, reason)
		}
		switch fixedSkip(e.path, false, policy) {
		case "meta":
			skip(&result.Stats.SkippedMeta, "meta")
			continue
		case "dir":
			skip(&result.Stats.SkippedDir, "dir")
			continue
		}
		if ignore.ignored(e.path, false) {
			skip(&result.Stats.SkippedGitignore, "gitignore")
			continue
		}
		if hidden(e.path) {
			skip(&result.Stats.SkippedHidden, "hidden")
			continue
		}
		extension := strings.ToLower(path.Ext(e.path))
		language, ok := languages[extension]
		if !ok || !slices.Contains(policy.Extensions, extension) || !slices.Contains(policy.Languages, language) {
			skip(&result.Stats.SkippedSuffix, "suffix")
			continue
		}
		if e.file.UncompressedSize64 > uint64(limits.MaxFileBytes) {
			skip(&result.Stats.SkippedLarge, "large")
			continue
		}
		raw, err := budget.readMember(e.file, limits.MaxFileBytes)
		if err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(string(raw)) == "" {
			skip(&result.Stats.SkippedEmpty, "empty")
			continue
		}
		text, ok := decode(raw)
		if !ok {
			skip(&result.Stats.SkippedBinary, "binary")
			continue
		}
		if strings.TrimSpace(text) == "" {
			skip(&result.Stats.SkippedEmpty, "empty")
			continue
		}
		f := SourceFile{Path: e.path, Language: language, Text: text}
		if opts.Transform != nil {
			transformed, err := opts.Transform(ctx, f)
			if err != nil {
				if ctx.Err() != nil {
					return Result{}, ctx.Err()
				}
				return Result{}, fail("ZIP_PARSE_FAILED")
			}
			if err := validateTransform(f, transformed, limits); err != nil {
				return Result{}, err
			}
			f = transformed
		}
		folded := strings.ToLower(f.Path)
		if paths[folded] || directories[folded] {
			return Result{}, fail("ZIP_UNSAFE_PATH")
		}
		for parent := path.Dir(folded); parent != "."; parent = path.Dir(parent) {
			if paths[parent] {
				return Result{}, fail("ZIP_UNSAFE_PATH")
			}
			directories[parent] = true
		}
		paths[folded] = true
		parsed, err := analyze(ctx, f, policy.SyntaxErrorRatio)
		if err != nil {
			return Result{}, err
		}
		if parsed.language != "" {
			f.Language = parsed.language
		}
		builder := chunkBuilder{ctx: ctx, opts: opts, file: f, lines: lineStarts(f.Text), result: &result, symbols: parsed.symbols, syntax: &parsed}
		if parsed.fallback {
			result.Stats.SyntaxFallbacks++
			if opts.OnSyntaxFallback != nil {
				opts.OnSyntaxFallback(SyntaxFallback{
					Path: f.Path, Language: f.Language, ErrorLines: len(parsed.errorLines), TotalLines: len(builder.lines),
					Ratio: float64(len(parsed.errorLines)) / float64(len(builder.lines)), Threshold: policy.SyntaxErrorRatio,
				})
			}
		}
		before := len(result.Chunks)
		if err := func() error {
			defer parsed.close()
			if err := builder.structure(byteSpan{0, len(f.Text)}, parsed.spans, false); err != nil {
				return err
			}
			return builder.finishSource(parsed.spans)
		}(); err != nil {
			return Result{}, err
		}
		if len(result.Chunks) == before {
			return Result{}, fail("ZIP_PARSE_FAILED")
		}
		if opts.Overview && !parsed.fallback && (len(parsed.spans) >= 2 || len(result.Chunks)-before >= 2) {
			codeEnd := len(result.Chunks)
			if err := builder.overview(parsed.symbols); err != nil {
				return Result{}, err
			}
			slices.Reverse(result.Chunks[before:codeEnd])
			slices.Reverse(result.Chunks[codeEnd:])
			slices.Reverse(result.Chunks[before:])
		}
		result.Stats.AcceptedFiles++
		addTree(&result.Tree, f.Path)
	}
	if result.Stats.AcceptedFiles == 0 {
		return Result{}, fail("ZIP_NO_SUPPORTED_FILES")
	}
	result.Stats.ReadBytes = budget.read
	sort.SliceStable(result.Chunks, func(i, j int) bool { return result.Chunks[i].SourcePath < result.Chunks[j].SourcePath })
	for i := range result.Chunks {
		result.Chunks[i].Order = i
	}
	sortTree(result.Tree)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return result, nil
}

func archiveRootName(entries []entry, filename string) string {
	common := ""
	for _, e := range entries {
		if archiveMetadata(e.path) {
			continue
		}
		top, _, nested := strings.Cut(e.path, "/")
		if (!nested && !e.directory) || (common != "" && common != top) {
			common = ""
			break
		}
		common = top
	}
	if common != "" {
		return common
	}
	name := path.Base(strings.ReplaceAll(filename, "\\", "/"))
	name = strings.TrimSuffix(name, path.Ext(name))
	if name == "" || name == "." {
		return "archive"
	}
	return name
}

func validateTransform(original, transformed SourceFile, limits Limits) error {
	clean, err := safePath(transformed.Path, limits.MaxDepth, false)
	if err != nil || clean != transformed.Path {
		return fail("ZIP_UNSAFE_PATH")
	}
	if transformed.Language != original.Language || !utf8.ValidString(transformed.Text) || strings.ContainsAny(transformed.Text, "\r\x00") || strings.TrimSpace(transformed.Text) == "" {
		return fail("ZIP_PARSE_FAILED")
	}
	if int64(len(transformed.Text)) > limits.MaxFileBytes {
		return fail("ZIP_BOMB_SUSPECTED")
	}
	originalLines, transformedLines := len(lineStarts(original.Text)), len(lineStarts(transformed.Text))
	if transformed.LineMap == nil {
		if originalLines != transformedLines {
			return fail("ZIP_PARSE_FAILED")
		}
		return nil
	}
	if len(transformed.LineMap) != transformedLines {
		return fail("ZIP_PARSE_FAILED")
	}
	previous := 0
	for _, line := range transformed.LineMap {
		if line.Start < 1 || line.End < line.Start || line.End > originalLines || line.Start < previous {
			return fail("ZIP_PARSE_FAILED")
		}
		previous = line.Start
	}
	return nil
}

func lineStarts(text string) []int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' && i+1 < len(text) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func addTree(nodes *[]TreeNode, name string) {
	parts := strings.Split(name, "/")
	prefix := ""
	for i, part := range parts {
		if prefix != "" {
			prefix += "/"
		}
		prefix += part
		index := slices.IndexFunc(*nodes, func(node TreeNode) bool { return node.Name == part })
		if index == -1 {
			kind := "directory"
			if i == len(parts)-1 {
				kind = "file"
			}
			*nodes = append(*nodes, TreeNode{Name: part, Path: prefix, Type: kind})
			index = len(*nodes) - 1
		}
		nodes = &(*nodes)[index].Children
	}
}

func sortTree(nodes []TreeNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Type != nodes[j].Type {
			return nodes[i].Type == "directory"
		}
		return nodes[i].Name < nodes[j].Name
	})
	for i := range nodes {
		sortTree(nodes[i].Children)
	}
}
