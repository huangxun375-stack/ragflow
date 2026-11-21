package codearchive

import (
	"bytes"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/git-lfs/wildmatch/v2"
)

var languages = map[string]string{
	".py": "python", ".go": "go", ".java": "java",
	".js": "javascript", ".jsx": "javascript", ".ts": "typescript", ".tsx": "tsx",
	".vue": "vue", ".c": "c", ".h": "cpp", ".hpp": "cpp", ".hxx": "cpp",
	".cpp": "cpp", ".cc": "cpp", ".cxx": "cpp",
	".lua": "lua", ".lh": "lua", ".ls": "lua", ".li": "lua",
}

func archiveMetadata(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == "__MACOSX" || part == ".DS_Store" || strings.HasPrefix(part, "._") {
			return true
		}
	}
	return false
}

func fixedSkip(name string, isDir bool, policy Policy) string {
	if archiveMetadata(name) {
		return "meta"
	}
	parts := strings.Split(name, "/")
	if !isDir {
		parts = parts[:len(parts)-1]
	}
	for _, part := range parts {
		if slices.Contains(policy.IgnoredDirs, part) {
			return "dir"
		}
	}
	return ""
}

func hidden(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

type ignoreRule struct {
	dir     string
	pattern *wildmatch.Wildmatch
	include bool
}

type ignoreMatcher struct{ rules []ignoreRule }

// Checking every parent enforces Git's pruning rule: a file negation cannot
// resurrect a file inside a directory that is still excluded.
func (m *ignoreMatcher) ignored(name string, isDir bool) bool {
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		if m.match(strings.Join(parts[:i], "/"), true) {
			return true
		}
	}
	return m.match(name, isDir)
}

func (m *ignoreMatcher) match(name string, isDir bool) bool {
	for i := len(m.rules) - 1; i >= 0; i-- {
		rule := m.rules[i]
		relative := name
		if rule.dir != "" {
			var ok bool
			relative, ok = strings.CutPrefix(name, rule.dir+"/")
			if !ok {
				continue
			}
		}
		if rule.pattern.MatchWithOpts(relative, wildmatch.MatchOpts{IsDirectory: isDir}) {
			return !rule.include
		}
	}
	return false
}

func loadIgnore(entries []entry, budget *readerBudget, policy Policy) (*ignoreMatcher, error) {
	m := &ignoreMatcher{}
	if !policy.RespectGitignore {
		return m, nil
	}
	limits := policy.Limits
	var rules []entry
	for _, e := range entries {
		if !e.directory && path.Base(e.path) == ".gitignore" {
			rules = append(rules, e)
		}
	}
	sort.Slice(rules, func(i, j int) bool {
		a, b := strings.Count(rules[i].path, "/"), strings.Count(rules[j].path, "/")
		if a != b {
			return a < b
		}
		return rules[i].path < rules[j].path
	})
	for _, e := range rules {
		if err := budget.ctx.Err(); err != nil {
			return nil, err
		}
		dir := path.Dir(e.path)
		if fixedSkip(e.path, false, policy) != "" || (dir != "." && (hidden(dir) || m.ignored(dir, true))) {
			continue
		}
		if e.file.UncompressedSize64 > uint64(limits.MaxGitignoreBytes) {
			continue
		}
		raw, err := budget.readMember(e.file, limits.MaxGitignoreBytes)
		if err != nil {
			return nil, err
		}
		if dir == "." {
			dir = ""
		}
		// Git terminates a rule at its first NUL. Decode lines independently
		// so one invalid rule cannot discard unrelated rules or source files.
		for _, rawLine := range bytes.Split(bytes.ReplaceAll(raw, []byte{'\r'}, []byte{'\n'}), []byte{'\n'}) {
			rawLine, _, _ = bytes.Cut(rawLine, []byte{0})
			line, ok := decode(rawLine)
			if !ok {
				continue
			}
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			include := strings.HasPrefix(line, "!")
			if include {
				line = line[1:]
			}
			line = portableIgnorePattern(line)
			if line == "" {
				continue
			}
			if pattern := compileIgnore(line); pattern != nil {
				m.rules = append(m.rules, ignoreRule{dir: dir, pattern: pattern, include: include})
			}
		}
	}
	return m, nil
}

// Wildmatch treats backslashes before ordinary characters as Windows path
// separators. Gitignore treats them as quoting; remove those quotes before
// calling the library while retaining escapes for glob metacharacters.
func portableIgnorePattern(line string) string {
	for strings.HasSuffix(line, " ") {
		backslashes := 0
		for i := len(line) - 2; i >= 0 && line[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			break
		}
		line = strings.TrimSuffix(line, " ")
	}
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' && i+1 < len(line) {
			i++
			if strings.ContainsRune("\\[]*?#", rune(line[i])) {
				b.WriteByte('\\')
			}
			b.WriteByte(line[i])
		} else {
			b.WriteByte(line[i])
		}
	}
	return b.String()
}

func compileIgnore(pattern string) (compiled *wildmatch.Wildmatch) {
	// Malformed patterns do not match in Git. This library represents malformed
	// syntax with a panic; contain that documented parser behavior at this seam.
	defer func() {
		if recover() != nil {
			compiled = nil
		}
	}()
	return wildmatch.NewWildmatch(pattern, wildmatch.Basename)
}
