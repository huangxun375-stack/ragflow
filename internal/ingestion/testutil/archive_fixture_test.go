package testutil

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"
)

func TestArchiveZIPDeterministicMembers(t *testing.T) {
	files := map[string]string{"src/z.go": "package z\n", "src/a.py": "print('hello')\n"}
	first, err := ArchiveZIP(files)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ArchiveZIP(files)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("ZIP output is not deterministic: %v", err)
	}
	r, err := zip.NewReader(bytes.NewReader(first), int64(len(first)))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.File) != len(files) {
		t.Fatalf("members=%d want=%d", len(r.File), len(files))
	}
	for i, name := range []string{"src/a.py", "src/z.go"} {
		member := r.File[i]
		if member.Name != name || member.Method != zip.Store {
			t.Fatalf("member=%s method=%d", member.Name, member.Method)
		}
		in, err := member.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(in)
		in.Close()
		if err != nil || string(body) != files[name] {
			t.Fatalf("member %s content changed: %v", name, err)
		}
	}
}
