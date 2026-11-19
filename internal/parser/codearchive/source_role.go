package codearchive

import (
	"path"
	"strings"
)

// IsTestSource recognizes language-specific conventions, not arbitrary substrings.
func IsTestSource(sourcePath string) bool {
	parts := strings.Split(strings.ReplaceAll(sourcePath, "\\", "/"), "/")
	base := parts[len(parts)-1]
	ext := strings.ToLower(path.Ext(base))
	dirs := parts[:len(parts)-1]
	hasDir := func(names ...string) bool {
		for _, dir := range dirs {
			for _, name := range names {
				if dir == name {
					return true
				}
			}
		}
		return false
	}
	switch ext {
	case ".go":
		return strings.HasSuffix(base, "_test.go")
	case ".py":
		return hasDir("test", "tests") || base == "conftest.py" || strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")
	case ".js", ".ts", ".jsx", ".tsx", ".vue":
		return hasDir("test", "tests", "__tests__", "__mocks__") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
	case ".java":
		for i := 1; i < len(dirs); i++ {
			if dirs[i-1] == "src" && dirs[i] == "test" {
				return true
			}
		}
		return strings.HasSuffix(base, "Test.java") || strings.HasSuffix(base, "Tests.java") || strings.HasSuffix(base, "IT.java")
	case ".c", ".cc", ".cpp", ".cxx", ".c++", ".h", ".hh", ".hpp", ".hxx":
		stem := strings.TrimSuffix(base, path.Ext(base))
		return hasDir("test", "tests") || (strings.HasPrefix(ext, ".c") && strings.HasSuffix(stem, "_test")) || strings.HasSuffix(base, "_unittest.cc") || (ext == ".c" && strings.HasPrefix(base, "test_"))
	case ".lua":
		return hasDir("spec", "tests") || strings.HasSuffix(base, "_spec.lua") || strings.HasPrefix(base, "test_")
	}
	return false
}
