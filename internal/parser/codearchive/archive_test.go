package codearchive

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

type member struct {
	name string
	text string
	mode os.FileMode
}

func archive(t *testing.T, files ...member) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, f := range files {
		h := &zip.FileHeader{Name: f.name, Method: zip.Store}
		if f.mode != 0 {
			h.SetMode(f.mode)
		}
		out, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = out.Write([]byte(f.text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func options() Options {
	return Options{Filename: "snapshot.zip", ChunkTokens: 512,
		CountTokens: utf8.RuneCountInString, FitsEmbedding: func(s string) bool { return utf8.RuneCountInString(s) <= 1024 }}
}

func TestPortableDirectoryEntries(t *testing.T) {
	const source = "def main():\n    return 1\n"
	for _, directories := range [][]member{
		{{name: "src\\"}, {name: "src\\empty\\"}},
		{{name: "./"}, {name: "./src/"}},
		{{name: ".\\"}, {name: ".\\src\\"}},
	} {
		t.Run(directories[0].name, func(t *testing.T) {
			members := append(directories, member{name: "./src/main.py", text: source})
			blob := archive(t, members...)
			if err := Precheck(blob, DefaultLimits()); err != nil {
				t.Fatal(err)
			}
			opts := options()
			result, err := Process(t.Context(), blob, opts)
			if err != nil {
				t.Fatal(err)
			}
			assertLosslessNonblankChunks(t, result, source, opts)
			if result.Stats.TotalFiles != 1 || result.Stats.AcceptedFiles != 1 || result.Chunks[0].SourcePath != "src/main.py" {
				t.Fatalf("directory entries became files: %+v", result)
			}
		})
	}
}

func TestPortableDirectoriesStillRequireSafeMetadata(t *testing.T) {
	for _, tc := range []struct {
		member
		reason string
	}{
		{member{name: "..\\"}, "ZIP_UNSAFE_PATH"},
		{member{name: ".\\..\\"}, "ZIP_UNSAFE_PATH"},
		{member{name: "\\src\\"}, "ZIP_UNSAFE_PATH"},
		{member{name: "./", mode: os.ModeSymlink | 0777}, "ZIP_UNSAFE_PATH"},
		{member{name: "src\\", text: "payload"}, "ZIP_CORRUPTED"},
		{member{name: ""}, "ZIP_UNSAFE_PATH"},
	} {
		blob := archive(t, tc.member, member{name: "main.py", text: "pass\n"})
		if err := Precheck(blob, DefaultLimits()); Reason(err) != tc.reason {
			t.Errorf("directory %q: %v, want %s", tc.name, err, tc.reason)
		}
	}
}

func TestGitignoreNULTerminatesOnlyItsRule(t *testing.T) {
	members := []member{
		{name: ".gitignore", text: "drop.py\nIcon\x00\x00\nignored.py\x00junk\n\xff\n"},
		{name: "main.py", text: "pass\n"},
		{name: "drop.py", text: "pass\n"},
		{name: "ignored.py", text: "pass\n"},
	}
	result, err := Process(t.Context(), archive(t, members...), options())
	if err != nil {
		t.Fatal(err)
	}
	if result.Stats.AcceptedFiles != 1 || result.Stats.SkippedGitignore != 2 || len(result.Chunks) != 1 || result.Chunks[0].SourcePath != "main.py" {
		t.Fatalf("invalid line discarded valid rules: %+v", result)
	}
}

func TestPrecheckRejectsUnsafeMembersBeforeFiltering(t *testing.T) {
	for _, name := range []string{"ignored/../../escape.py", "node_modules/../x.py", `..\x.py`, "/x.py", `C:\x.py`, `\\host\x.py`, "a\x00.py", "src/a\nb.py", "x:stream.py"} {
		t.Run(name, func(t *testing.T) {
			blob := archive(t, member{"good.py", "pass", 0}, member{name, "pass", 0})
			if err := Precheck(blob, DefaultLimits()); Reason(err) != "ZIP_UNSAFE_PATH" {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, files := range [][]member{
		{{"a.py", "pass", 0}, {"A.PY", "pass", 0}},
		{{"src/a.py", "pass", 0}, {`src\a.py`, "pass", 0}},
		{{"a.py", "pass", 0}, {"./a.py", "pass", 0}},
		{{"a.py", "pass", 0}, {"a.py/b.py", "pass", 0}},
		{{"node_modules/link.py", "target", os.ModeSymlink | 0777}},
	} {
		if err := Precheck(archive(t, files...), DefaultLimits()); Reason(err) != "ZIP_UNSAFE_PATH" {
			t.Fatalf("got %v for %+v", err, files)
		}
	}
}

func TestPrecheckFormatsEncryptionAndLimits(t *testing.T) {
	if err := Precheck([]byte("not a zip"), DefaultLimits()); Reason(err) != "ZIP_INVALID_FORMAT" {
		t.Fatal(err)
	}
	if err := Precheck(archive(t), DefaultLimits()); Reason(err) != "ZIP_NO_SUPPORTED_FILES" {
		t.Fatal(err)
	}
	blob := archive(t, member{"a.py", "pass", 0})
	for _, central := range []bool{false, true} {
		copyBlob := bytes.Clone(blob)
		offset := 6
		if central {
			offset = bytes.Index(copyBlob, []byte{'P', 'K', 1, 2}) + 8
		}
		binary.LittleEndian.PutUint16(copyBlob[offset:], binary.LittleEndian.Uint16(copyBlob[offset:])|1)
		if err := Precheck(copyBlob, DefaultLimits()); Reason(err) != "ZIP_ENCRYPTED" {
			t.Fatalf("central=%t: %v", central, err)
		}
	}
	limits := DefaultLimits()
	limits.MaxCompressedBytes = int64(len(blob) - 1)
	if err := Precheck(blob, limits); Reason(err) != "ZIP_SIZE_EXCEEDED" {
		t.Fatal(err)
	}
	limits = DefaultLimits()
	limits.MaxFiles = 1
	if err := Precheck(archive(t, member{"a.py", "pass", 0}, member{"b.py", "pass", 0}), limits); Reason(err) != "ZIP_TOO_MANY_FILES" {
		t.Fatal(err)
	}
	limits = DefaultLimits()
	limits.MaxDepth = 2
	if err := Precheck(archive(t, member{"a/b/c.py", "pass", 0}), limits); Reason(err) != "ZIP_TOO_DEEP" {
		t.Fatal(err)
	}
}

func TestProcessSourceLinesOrderAndCoverage(t *testing.T) {
	blob := archive(t, member{"src/b.py", "# 中文\r\ndef b():\r\n    return '世界'\r\n", 0}, member{"src/a.py", "def run():\n    return 1\n", 0})
	got, err := Process(context.Background(), blob, options())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Chunks) != 2 {
		t.Fatalf("chunks: %+v", got.Chunks)
	}
	first := got.Chunks[0]
	if first.SourcePath != "src/a.py" || first.StartLine != 1 || first.EndLine != 2 || first.Order != 0 || first.Language != "python" {
		t.Fatalf("first: %+v", first)
	}
	if first.Text != "// src/a.py:L1-L2 run\ndef run():\n    return 1\n" {
		t.Fatalf("text: %q", first.Text)
	}
	if got.Stats.TotalFiles != 2 || got.Stats.AcceptedFiles != 2 || got.RootName != "src" {
		t.Fatalf("result: %+v", got)
	}
	if got.Tree[0].Name != "src" || len(got.Tree[0].Children) != 2 {
		t.Fatalf("tree: %+v", got.Tree)
	}
	again, err := Process(context.Background(), blob, options())
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("nondeterministic: %v", err)
	}
}

func TestFiltersGitignoreAndStatistics(t *testing.T) {
	files := []member{
		{".gitignore", "/root.py\ncache/\n*.tmp.py\n!keep.tmp.py\n\\#literal.py\nescaped\\ file.py\na/**/drop.py\n", 0},
		{"root.py", "pass", 0}, {"sub/root.py", "pass", 0},
		{"cache/.gitignore", "!keep.py", 0}, {"cache/keep.py", "pass", 0},
		{"sub/.gitignore", "local.py\n!keep.tmp.py\n", 0}, {"sub/local.py", "pass", 0},
		{"gone.tmp.py", "pass", 0}, {"keep.tmp.py", "pass", 0},
		{"#literal.py", "pass", 0}, {"escaped file.py", "pass", 0}, {"a/b/drop.py", "pass", 0},
		{"__MACOSX/x.py", "pass", 0}, {"node_modules/a.py", "pass", 0},
		{".hidden.py", "pass", 0}, {"note.txt", "not code", 0},
		{"empty.py", " \n\t", 0}, {"binary.py", "a\x00b", 0},
	}
	got, err := Process(context.Background(), archive(t, files...), options())
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for _, c := range got.Chunks {
		paths = append(paths, c.SourcePath)
	}
	if !reflect.DeepEqual(paths, []string{"keep.tmp.py", "sub/root.py"}) {
		t.Fatalf("paths: %v", paths)
	}
	s := got.Stats
	if s.TotalFiles != len(files) || s.AcceptedFiles != 2 || s.SkippedMeta != 1 || s.SkippedDir != 1 || s.SkippedHidden != 3 || s.SkippedGitignore != 8 || s.SkippedSuffix != 1 || s.SkippedEmpty != 1 || s.SkippedBinary != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestCRCReadBudgetLargeFileAndNoSupportedFiles(t *testing.T) {
	blob := archive(t, member{"a.py", "pass\n", 0})
	broken := bytes.Clone(blob)
	broken[bytes.Index(broken, []byte("pass\n"))] ^= 0xff
	if _, err := Process(context.Background(), broken, options()); Reason(err) != "ZIP_CORRUPTED" {
		t.Fatal(err)
	}
	opts := options()
	policy := DefaultPolicy()
	opts.Policy = &policy
	policy.Limits.MaxReadBytes = 4
	if _, err := Process(context.Background(), blob, opts); Reason(err) != "ZIP_BOMB_SUSPECTED" {
		t.Fatal(err)
	}
	opts = options()
	policy = DefaultPolicy()
	opts.Policy = &policy
	policy.Limits.MaxFileBytes = 4
	if _, err := Process(context.Background(), blob, opts); Reason(err) != "ZIP_NO_SUPPORTED_FILES" {
		t.Fatal(err)
	}
	opts = options()
	policy = DefaultPolicy()
	opts.Policy = &policy
	policy.Limits.MaxGitignoreBytes = 4
	if _, err := Process(context.Background(), archive(t, member{".gitignore", "long-pattern", 0}, member{"a.py", "pass", 0}), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := Process(context.Background(), archive(t, member{"a.txt", "hello", 0}), options()); Reason(err) != "ZIP_NO_SUPPORTED_FILES" {
		t.Fatal(err)
	}
}

func TestGBKAndTransformLineMap(t *testing.T) {
	source := "# 中文\nvalue = 1\n"
	gbk, err := simplifiedchinese.GBK.NewEncoder().String(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Process(context.Background(), archive(t, member{"a.py", gbk, 0}), options())
	if err != nil || !strings.Contains(got.Chunks[0].Text, source) {
		t.Fatalf("%+v %v", got, err)
	}
	opts := options()
	opts.Transform = func(_ context.Context, f SourceFile) (SourceFile, error) {
		f.Text = "value = 2\n"
		f.LineMap = []LineRange{{1, 2}}
		return f, nil
	}
	got, err = Process(context.Background(), archive(t, member{"a.py", source, 0}), opts)
	if err != nil || got.Chunks[0].StartLine != 1 || got.Chunks[0].EndLine != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	opts.Transform = func(_ context.Context, f SourceFile) (SourceFile, error) { f.Text += "x = 3\n"; return f, nil }
	if _, err = Process(context.Background(), archive(t, member{"a.py", source, 0}), opts); Reason(err) != "ZIP_PARSE_FAILED" {
		t.Fatal(err)
	}
}

func TestStrictFinalBudgetAndLongLineCoverage(t *testing.T) {
	source := "value = '" + strings.Repeat("中文🙂", 90) + "'\n"
	opts := options()
	opts.ChunkTokens = 55
	opts.FitsEmbedding = func(s string) bool { return utf8.RuneCountInString(s) <= 48 }
	got, err := Process(context.Background(), archive(t, member{"a.py", source, 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	var covered strings.Builder
	for _, c := range got.Chunks {
		if opts.CountTokens(c.Text) > opts.ChunkTokens || !opts.FitsEmbedding(c.Text) || !utf8.ValidString(c.Text) {
			t.Fatalf("oversize: %q", c.Text)
		}
		_, body, ok := strings.Cut(c.Text, "\n")
		if !ok {
			t.Fatal(c)
		}
		covered.WriteString(body)
		if c.StartLine != 1 || c.EndLine != 1 {
			t.Fatal(c)
		}
	}
	if covered.String() != source {
		t.Fatalf("coverage mismatch: %q", covered.String())
	}
	opts.ChunkTokens = 1
	if _, err = Process(context.Background(), archive(t, member{"a.py", "pass", 0}), opts); Reason(err) != "ZIP_CONFIG_INVALID" {
		t.Fatal(err)
	}
	opts.ChunkTokens = 55
	policy := DefaultPolicy()
	opts.Policy = &policy
	policy.Limits.MaxChunks = 2
	if _, err = Process(context.Background(), archive(t, member{"a.py", source, 0}), opts); Reason(err) != "ZIP_TOO_MANY_CHUNKS" {
		t.Fatal(err)
	}
}

func TestCancellationAndCountersRequired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Process(ctx, archive(t, member{"a.py", "pass", 0}), options()); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := Process(context.Background(), archive(t, member{"a.py", "pass", 0}), Options{}); Reason(err) != "ZIP_CONFIG_INVALID" {
		t.Fatal(err)
	}
}

func TestGitignoreRecursiveBacktrackingAndLiteralEscapes(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		ignored       bool
	}{
		{"a/**/b/c.py", "a/b/d/b/c.py", true},
		{"a/**/b/c.py", "a/b/c.py", true},
		{"a/**/b/c.py", "x/a/b/c.py", false},
		{"foo**bar.py", "fooxyzbar.py", true},
		{"\\!literal.py", "!literal.py", true},
		{"escaped\\ file.py", "escaped file.py", true},
		{"space.py\\ ", "space.py ", true},
		{"space.py ", "space.py", true},
		{"folder/", "folder", false},
		{"folder/", "folder/a.py", true},
		{"[[:digit:]].py", "7.py", true},
	} {
		t.Run(tc.pattern+"/"+tc.name, func(t *testing.T) {
			entries, err := inspect(context.Background(), archive(t, member{".gitignore", tc.pattern + "\n", 0}), DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			matcher, err := loadIgnore(entries, &readerBudget{ctx: context.Background(), limit: 1 << 20}, DefaultPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if got := matcher.ignored(tc.name, false); got != tc.ignored {
				t.Fatalf("ignored=%t want=%t", got, tc.ignored)
			}
		})
	}
}

func TestLocalHeaderMismatchAndForgedSmallSizeFail(t *testing.T) {
	original := archive(t, member{"a.py", "value = 123\n", 0})
	for _, mutate := range []func([]byte){
		func(blob []byte) { blob[30] = 'b' },
		func(blob []byte) { binary.LittleEndian.PutUint16(blob[8:], zip.Deflate) },
		func(blob []byte) {
			central := bytes.Index(blob, []byte{'P', 'K', 1, 2})
			binary.LittleEndian.PutUint32(blob[central+24:], 1)
		},
	} {
		blob := bytes.Clone(original)
		mutate(blob)
		if _, err := Process(context.Background(), blob, options()); Reason(err) != "ZIP_CORRUPTED" {
			t.Fatal(err)
		}
	}
}

func TestTransformRejectsUnsafeConflictingPathsAndMaps(t *testing.T) {
	blob := archive(t, member{"a.py", "value = 1\n", 0}, member{"b.py", "value = 2\n", 0})
	for _, transform := range []SourceTransform{
		func(_ context.Context, f SourceFile) (SourceFile, error) { f.Path = "../bad.py"; return f, nil },
		func(_ context.Context, f SourceFile) (SourceFile, error) { f.Path = "same.py"; return f, nil },
		func(_ context.Context, f SourceFile) (SourceFile, error) {
			if f.Path == "b.py" {
				f.Path = "a.py/b.py"
			}
			return f, nil
		},
		func(_ context.Context, f SourceFile) (SourceFile, error) {
			f.LineMap = []LineRange{{0, 2}}
			return f, nil
		},
	} {
		opts := options()
		opts.Transform = transform
		if _, err := Process(context.Background(), blob, opts); err == nil {
			t.Fatal("invalid transform accepted")
		}
	}
}

func TestTransformedPathsDetermineFinalOrder(t *testing.T) {
	opts := options()
	opts.Transform = func(_ context.Context, f SourceFile) (SourceFile, error) {
		if f.Path == "a.py" {
			f.Path = "z.py"
		} else {
			f.Path = "a.py"
		}
		return f, nil
	}
	got, err := Process(context.Background(), archive(t, member{"a.py", "a = 1", 0}, member{"b.py", "b = 2", 0}), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Chunks[0].SourcePath != "a.py" || got.Chunks[1].SourcePath != "z.py" || got.Chunks[1].Order != 1 {
		t.Fatalf("%+v", got.Chunks)
	}
}
