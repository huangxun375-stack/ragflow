package testutil

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ArchiveZIP stores members in sorted order without writing temporary files.
func ArchiveZIP(files map[string]string) ([]byte, error) {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			return nil, err
		}
		if _, err = io.WriteString(out, files[name]); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ArchiveScale builds an exact 10 MiB ZIP from generated Go, TypeScript and Lua.
// Store compression keeps the upload boundary independent of compressor versions.
func ArchiveScale() ([]byte, map[string]string, error) {
	const target = 10 << 20
	const count = 192
	files := make(map[string]string, count)
	overhead := 22
	for i := 0; i < count; i++ {
		ext := []string{"go", "ts", "lua"}[i%3]
		name := fmt.Sprintf("module%03d/source.%s", i, ext)
		files[name] = ""
		// Local header, data descriptor, central header, and both names.
		overhead += 30 + 16 + 46 + 2*len(name)
	}
	names := make([]string, 0, count)
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	remaining := target - overhead
	for i, name := range names {
		size := remaining / (count - i)
		remaining -= size
		var b strings.Builder
		if strings.HasSuffix(name, ".go") {
			b.WriteString("package module\n\n")
		}
		for n := 0; ; n++ {
			var declaration string
			switch {
			case strings.HasSuffix(name, ".go"):
				declaration = fmt.Sprintf("// Checksum%d computes a deterministic repository checksum.\nfunc Checksum%d(value int) int {\n\treturn value*31 + %d\n}\n\n", n, n, n+17)
			case strings.HasSuffix(name, ".ts"):
				declaration = fmt.Sprintf("// checksum%d computes a deterministic repository checksum.\nexport function checksum%d(value: number): number {\n  return value * 31 + %d;\n}\n\n", n, n, n+17)
			default:
				declaration = fmt.Sprintf("-- checksum%d computes a deterministic repository checksum.\nfunction checksum%d(value)\n  return value * 31 + %d\nend\n\n", n, n, n+17)
			}
			if b.Len()+len(declaration)+4 > size {
				break
			}
			b.WriteString(declaration)
		}
		prefix := "//"
		if strings.HasSuffix(name, ".lua") {
			prefix = "--"
		}
		b.WriteString(prefix)
		b.WriteString(strings.Repeat("x", size-b.Len()-1))
		b.WriteByte('\n')
		files[name] = b.String()
	}
	blob, err := ArchiveZIP(files)
	if err != nil {
		return nil, nil, err
	}
	if len(blob) != target {
		return nil, nil, fmt.Errorf("fixture ZIP has %d bytes, want %d", len(blob), target)
	}
	return blob, files, nil
}
