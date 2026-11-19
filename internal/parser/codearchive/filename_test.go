package codearchive

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func rawNamesArchive(t *testing.T, names ...string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := zip.NewWriter(&buffer)
	for _, name := range names {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, NonUTF8: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte("def run():\n    return 1\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestArchiveDecodesLegacyNamesWithoutChangingMemberHandle(t *testing.T) {
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("目录/文件.py"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, want string }{{string(gbk), "目录/文件.py"}, {"caf\x82.py", "café.py"}} {
		result, err := Process(context.Background(), rawNamesArchive(t, tc.raw), options())
		if err != nil {
			t.Fatalf("decode %q: %v", tc.want, err)
		}
		if len(result.Chunks) != 1 || result.Chunks[0].SourcePath != tc.want {
			t.Fatalf("paths: %+v", result.Chunks)
		}
	}
}

func TestArchivePreservesCP437AndUnflaggedUTF8Names(t *testing.T) {
	for _, name := range []string{"café.py", "grüße.py", "éá.py", "目录/文件.py", "你好.py", "中文.py", "plain.py"} {
		t.Run(name, func(t *testing.T) {
			raws := []string{name}
			if raw, err := charmap.CodePage437.NewEncoder().String(name); err == nil {
				raws = append(raws, raw)
			}
			if name == "目录/文件.py" || name == "你好.py" || name == "中文.py" {
				raw, err := simplifiedchinese.GBK.NewEncoder().String(name)
				if err != nil {
					t.Fatal(err)
				}
				raws = append(raws, raw)
			}
			for _, raw := range raws {
				result, err := Process(t.Context(), rawNamesArchive(t, raw), options())
				if err != nil || len(result.Chunks) != 1 || result.Chunks[0].SourcePath != name {
					t.Fatalf("raw=%x name=%q chunks=%+v err=%v", raw, name, result.Chunks, err)
				}
			}
		})
	}
}

func unicodePathExtra(raw, name string) []byte {
	extra := make([]byte, 9+len(name))
	binary.LittleEndian.PutUint16(extra, 0x7075)
	binary.LittleEndian.PutUint16(extra[2:], uint16(5+len(name)))
	extra[4] = 1
	binary.LittleEndian.PutUint32(extra[5:], crc32.ChecksumIEEE([]byte(raw)))
	copy(extra[9:], name)
	return extra
}

func namedHeaderArchive(t *testing.T, headers ...zip.FileHeader) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := zip.NewWriter(&buffer)
	for _, header := range headers {
		f, err := w.CreateHeader(&header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte("def run():\n    return 1\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestArchiveUnicodePathMetadata(t *testing.T) {
	raw := "\x82\xa0.py" // CP437 éá and GBK 偁 are indistinguishable without metadata.
	badCRC := unicodePathExtra(raw, "其他.py")
	badCRC[5] ^= 1
	badVersion := unicodePathExtra(raw, "其他.py")
	badVersion[4] = 2
	for _, tc := range []struct {
		name, raw, want, reason string
		extra                   []byte
		flags                   uint16
	}{
		{name: "CP437 default", raw: raw, want: "éá.py"},
		{name: "explicit GBK intent", raw: raw, want: "偁.py", extra: unicodePathExtra(raw, "偁.py")},
		{name: "explicit CP437 intent", raw: raw, want: "éá.py", extra: unicodePathExtra(raw, "éá.py")},
		{name: "ASCII raw alias", raw: "alias.py", want: "中文.py", extra: unicodePathExtra("alias.py", "中文.py")},
		{name: "symbol bytes repaired as GBK", raw: "\xd6\xd0\xce\xc4.py", want: "中文.py"},
		{name: "explicit CP437 symbols", raw: "\xd6\xd0\xce\xc4.py", want: "╓╨╬─.py", extra: unicodePathExtra("\xd6\xd0\xce\xc4.py", "╓╨╬─.py")},
		{name: "valid UTF8 preferred", raw: "\xc3\xa9.py", want: "é.py"},
		{name: "explicit CP437 over valid UTF8", raw: "\xc3\xa9.py", want: "├⌐.py", extra: unicodePathExtra("\xc3\xa9.py", "├⌐.py")},
		{name: "CRC mismatch ignored", raw: raw, want: "éá.py", extra: badCRC},
		{name: "unknown version ignored", raw: raw, want: "éá.py", extra: badVersion},
		{name: "short field ignored", raw: raw, want: "éá.py", extra: []byte{0x75, 0x70, 1, 0, 1}},
		{name: "invalid Unicode rejected", raw: raw, reason: "ZIP_UNSAFE_PATH", extra: unicodePathExtra(raw, "\xff.py")},
		{name: "unsafe Unicode rejected", raw: raw, reason: "ZIP_UNSAFE_PATH", extra: unicodePathExtra(raw, "../escape.py")},
		{name: "conflicting Unicode rejected", raw: raw, reason: "ZIP_UNSAFE_PATH", extra: append(unicodePathExtra(raw, "a.py"), unicodePathExtra(raw, "b.py")...)},
		{name: "flagged UTF8", raw: "目录/文件.py", want: "目录/文件.py", flags: 0x800},
		{name: "flagged invalid UTF8", raw: raw, reason: "ZIP_UNSAFE_PATH", flags: 0x800},
		{name: "flag metadata conflict", raw: "原名.py", reason: "ZIP_UNSAFE_PATH", flags: 0x800, extra: unicodePathExtra("原名.py", "其他.py")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blob := namedHeaderArchive(t, zip.FileHeader{Name: tc.raw, Extra: tc.extra, NonUTF8: true, Method: zip.Store})
			if tc.flags != 0 {
				binary.LittleEndian.PutUint16(blob[6:], binary.LittleEndian.Uint16(blob[6:])|tc.flags)
				central := bytes.Index(blob, []byte{'P', 'K', 1, 2})
				binary.LittleEndian.PutUint16(blob[central+8:], binary.LittleEndian.Uint16(blob[central+8:])|tc.flags)
			}
			result, err := Process(t.Context(), blob, options())
			if tc.reason != "" {
				if err == nil || Reason(err) != tc.reason {
					t.Fatalf("want %s, got %v", tc.reason, err)
				}
				return
			}
			if err != nil || len(result.Chunks) != 1 || result.Chunks[0].SourcePath != tc.want {
				t.Fatalf("want %q, got %+v, err=%v", tc.want, result.Chunks, err)
			}
		})
	}
	blob := namedHeaderArchive(t,
		zip.FileHeader{Name: raw, Extra: unicodePathExtra(raw, "same.py"), NonUTF8: true},
		zip.FileHeader{Name: "same.py"},
	)
	if err := Precheck(blob, DefaultLimits()); Reason(err) != "ZIP_UNSAFE_PATH" {
		t.Fatalf("Unicode alias collision accepted: %v", err)
	}
}

func TestArchiveRejectsDecodedFilenameCollision(t *testing.T) {
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("文件.py"))
	if err != nil {
		t.Fatal(err)
	}
	if err = Precheck(rawNamesArchive(t, string(gbk), "文件.py"), DefaultLimits()); Reason(err) != "ZIP_UNSAFE_PATH" {
		t.Fatalf("collision accepted: %v", err)
	}
}
