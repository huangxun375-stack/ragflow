package codearchive

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestArchiveAdmissionRejectsExcludedFormatsBeforeFiltering(t *testing.T) {
	plain := archive(t, member{"main.go", "package main\n", 0}, member{"node_modules/ignored.go", "package ignored\n", 0})
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"self extracting", func(b []byte) []byte { return append([]byte("MZ-review-prefix\n"), b...) }},
		{"PK prefixed", func(b []byte) []byte { return append([]byte{'P', 'K', 3, 4, 0, 0, 0, 0}, b...) }},
		{"disk number", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[len(b)-18:], 1); return b }},
		{"directory disk", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[len(b)-16:], 1); return b }},
		{"disk entry count", func(b []byte) []byte { binary.LittleEndian.PutUint16(b[len(b)-14:], 1); return b }},
		{"member disk", func(b []byte) []byte {
			at := bytes.Index(b, []byte{'P', 'K', 1, 2})
			binary.LittleEndian.PutUint16(b[at+34:], 1)
			return b
		}},
		{"ignored unsupported compression", func(b []byte) []byte {
			at := bytes.LastIndex(b, []byte{'P', 'K', 3, 4})
			binary.LittleEndian.PutUint16(b[at+8:], 99)
			at = bytes.LastIndex(b, []byte{'P', 'K', 1, 2})
			binary.LittleEndian.PutUint16(b[at+10:], 99)
			return b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blob := tc.change(bytes.Clone(plain))
			if err := Precheck(blob, DefaultLimits()); Reason(err) != "ZIP_INVALID_FORMAT" {
				t.Fatalf("upload precheck: %v", err)
			}
			if _, err := Process(t.Context(), blob, options()); Reason(err) != "ZIP_INVALID_FORMAT" {
				t.Fatalf("worker admission: %v", err)
			}
		})
	}
}

func TestArchiveAdmissionSupportsStoreDeflateAndComments(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		if err := w.SetComment("ordinary ZIP with comment PK\x05\x06"); err != nil {
			t.Fatal(err)
		}
		f, err := w.CreateHeader(&zip.FileHeader{Name: "main.go", Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte("package main\n")); err != nil {
			t.Fatal(err)
		}
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
		if err = Precheck(b.Bytes(), DefaultLimits()); err != nil {
			t.Fatal(err)
		}
		if _, err = Process(t.Context(), b.Bytes(), options()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAddTreeReusesExactNamesAndPreservesInsertionOrder(t *testing.T) {
	var nodes []TreeNode
	for _, name := range []string{"z/a.go", "a/b.go", "z/b.go", "z/a.go", "Z/a.go"} {
		addTree(&nodes, name)
	}
	want := []TreeNode{
		{Name: "z", Path: "z", Type: "directory", Children: []TreeNode{
			{Name: "a.go", Path: "z/a.go", Type: "file"},
			{Name: "b.go", Path: "z/b.go", Type: "file"},
		}},
		{Name: "a", Path: "a", Type: "directory", Children: []TreeNode{
			{Name: "b.go", Path: "a/b.go", Type: "file"},
		}},
		{Name: "Z", Path: "Z", Type: "directory", Children: []TreeNode{
			{Name: "a.go", Path: "Z/a.go", Type: "file"},
		}},
	}
	if !reflect.DeepEqual(nodes, want) {
		t.Fatalf("tree=%+v want=%+v", nodes, want)
	}
}

func TestArchiveTreeDirectoriesBeforeFilesRecursively(t *testing.T) {
	r, err := Process(t.Context(), archive(t,
		member{"b.go", "package p\n", 0}, member{"a.go", "package p\n", 0},
		member{"z/b.go", "package p\n", 0}, member{"z/a.go", "package p\n", 0},
		member{"z/z/c.go", "package p\n", 0}, member{"y/d.go", "package p\n", 0}), options())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	var visit func([]TreeNode)
	visit = func(nodes []TreeNode) {
		for _, n := range nodes {
			paths = append(paths, n.Path)
			visit(n.Children)
		}
	}
	visit(r.Tree)
	want := []string{"y", "y/d.go", "z", "z/z", "z/z/c.go", "z/a.go", "z/b.go", "a.go", "b.go"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("tree order: %v", paths)
	}
}

func TestArchiveAdmissionSingleVolumeZIP64(t *testing.T) {
	plain := archive(t, member{"main.go", "package main\n", 0})
	end := len(plain) - 22
	le := binary.LittleEndian
	record := make([]byte, 56)
	le.PutUint32(record, 0x06064b50)
	le.PutUint64(record[4:], 44)
	le.PutUint16(record[12:], 45)
	le.PutUint16(record[14:], 45)
	le.PutUint64(record[24:], 1)
	le.PutUint64(record[32:], 1)
	le.PutUint64(record[40:], uint64(le.Uint32(plain[end+12:])))
	le.PutUint64(record[48:], uint64(le.Uint32(plain[end+16:])))
	locator := make([]byte, 20)
	le.PutUint32(locator, 0x07064b50)
	le.PutUint64(locator[8:], uint64(end))
	le.PutUint32(locator[16:], 1)
	trailer := bytes.Clone(plain[end:])
	le.PutUint16(trailer[8:], 0xffff)
	le.PutUint16(trailer[10:], 0xffff)
	le.PutUint32(trailer[12:], 0xffffffff)
	le.PutUint32(trailer[16:], 0xffffffff)
	blob := append(bytes.Clone(plain[:end]), record...)
	blob = append(blob, locator...)
	blob = append(blob, trailer...)
	if err := Precheck(blob, DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := Process(t.Context(), blob, options()); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{end + 16, end + 20, end + 24, end + 56 + 4, end + 56 + 16} {
		corrupt := bytes.Clone(blob)
		le.PutUint32(corrupt[offset:], 2)
		if err := Precheck(corrupt, DefaultLimits()); Reason(err) != "ZIP_INVALID_FORMAT" {
			t.Fatalf("multi-volume at %d: %v", offset, err)
		}
	}
	// archive/zip ignores a ZIP64 locator unless the end record has sentinels.
	plainTrailer := append(append(append(bytes.Clone(plain[:end]), record...), locator...), plain[end:]...)
	if err := Precheck(plainTrailer, DefaultLimits()); Reason(err) != "ZIP_INVALID_FORMAT" {
		t.Fatalf("locator without ZIP64 sentinels: %v", err)
	}
}

// archive/zip reads the last end-record signature. A forged record inside the
// real record's comment must not leave the checked and the read directories apart.
func TestArchiveAdmissionRejectsForgedEndRecordInComment(t *testing.T) {
	forged := make([]byte, 22)
	binary.LittleEndian.PutUint32(forged, 0x06054b50)
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	if err := w.SetComment(string(forged) + "trailing"); err != nil {
		t.Fatal(err)
	}
	f, err := w.Create("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("package main\n")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Precheck(b.Bytes(), DefaultLimits()); Reason(err) != "ZIP_INVALID_FORMAT" {
		t.Fatalf("forged end record: %v", err)
	}
}
