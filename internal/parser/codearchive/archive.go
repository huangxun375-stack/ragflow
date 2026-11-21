package codearchive

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
)

type entry struct {
	file      *zip.File
	path      string
	directory bool
}

func safePath(name string, depth int, directory bool) (string, error) {
	if !utf8.ValidString(name) {
		return "", fail("ZIP_UNSAFE_PATH")
	}
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return "", fail("ZIP_UNSAFE_PATH")
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return "", fail("ZIP_UNSAFE_PATH")
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fail("ZIP_UNSAFE_PATH")
		}
	}
	clean := path.Clean(name)
	if clean == "." && (!directory || name == "") || clean == "" {
		return "", fail("ZIP_UNSAFE_PATH")
	}
	if len(strings.Split(clean, "/")) > depth {
		return "", fail("ZIP_TOO_DEEP")
	}
	return clean, nil
}

// Decode display/matching names separately from the original member handle.
// File.Open must retain the raw name used to verify the local ZIP header.
func decodedMemberName(f *zip.File) (string, error) {
	name, present, err := unicodeMemberName(f)
	if err != nil {
		return "", err
	}
	if f.Flags&0x800 != 0 {
		if !utf8.ValidString(f.Name) || (present && name != f.Name) {
			return "", fail("ZIP_UNSAFE_PATH")
		}
		return f.Name, nil
	}
	if present {
		return name, nil
	}
	if utf8.ValidString(f.Name) {
		return f.Name, nil
	}
	original, err := charmap.CodePage437.NewDecoder().String(f.Name)
	if err != nil {
		return "", fail("ZIP_UNSAFE_PATH")
	}
	// No flag/extra field can distinguish all CP437 and GBK byte sequences.
	// Keep ordinary CP437 letters (including ü, ß, é, á); repair only names
	// with CP437 box drawing, Greek or math mojibake and a lossless Han decode.
	// Genuine CP437 symbol names and GBK names without these signals remain
	// ambiguous; producers must supply Unicode Path metadata to disambiguate.
	if strings.ContainsFunc(original, func(r rune) bool {
		return r == 'µ' || unicode.Is(unicode.Greek, r) || unicode.Is(unicode.Sm, r) || (r >= 0x2500 && r <= 0x259f)
	}) {
		decoded, err := simplifiedchinese.GBK.NewDecoder().String(f.Name)
		if err == nil && !strings.ContainsRune(decoded, utf8.RuneError) && strings.ContainsFunc(decoded, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
			encoded, err := simplifiedchinese.GBK.NewEncoder().String(decoded)
			if err == nil && encoded == f.Name {
				return decoded, nil
			}
		}
	}
	return original, nil
}

// Info-ZIP Unicode Path (0x7075) is authoritative only for version 1 and a
// matching CRC of the raw central-directory name. Stale/unknown records are
// ignored; contradictory or invalid matching Unicode names fail closed.
func unicodeMemberName(f *zip.File) (string, bool, error) {
	var name string
	present := false
	for extra := f.Extra; len(extra) >= 4; {
		tag, size := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		extra = extra[4:]
		if size > len(extra) {
			break
		}
		data := extra[:size]
		extra = extra[size:]
		if tag != 0x7075 || len(data) < 5 || data[0] != 1 || binary.LittleEndian.Uint32(data[1:5]) != crc32.ChecksumIEEE([]byte(f.Name)) {
			continue
		}
		value := string(data[5:])
		if value == "" || !utf8.ValidString(value) || (present && value != name) {
			return "", false, fail("ZIP_UNSAFE_PATH")
		}
		name, present = value, true
	}
	return name, present, nil
}

// Precheck checks every member's metadata, including members later ignored.
// Payload CRCs are verified when candidates are read by Process.
func Precheck(blob []byte, limits Limits) error {
	if err := limits.validate(); err != nil {
		return err
	}
	_, err := inspect(context.Background(), blob, limits)
	return err
}

func inspect(ctx context.Context, blob []byte, limits Limits) ([]entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(blob)) > limits.MaxCompressedBytes {
		return nil, fail("ZIP_SIZE_EXCEEDED")
	}
	count, err := checkArchiveFormat(blob)
	if err != nil {
		return nil, err
	}
	r, err := zip.NewReader(bytes.NewReader(blob), int64(len(blob)))
	if err != nil || uint64(len(r.File)) != count {
		return nil, fail("ZIP_INVALID_FORMAT")
	}
	seen := make(map[string]bool, len(r.File))
	entries := make([]entry, 0, len(r.File))
	files := 0
	for _, f := range r.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if f.Flags&1 != 0 || f.Flags&64 != 0 {
			return nil, fail("ZIP_ENCRYPTED")
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			return nil, fail("ZIP_INVALID_FORMAT")
		}
		decodedName, err := decodedMemberName(f)
		if err != nil {
			return nil, err
		}
		if f.Mode()&os.ModeSymlink != 0 || (!f.Mode().IsRegular() && !f.Mode().IsDir()) {
			return nil, fail("ZIP_UNSAFE_PATH")
		}
		// PowerShell 5.1 emits empty directory entries with backslash names
		// and no directory attribute. Keep the raw ZIP name for header checks.
		directory := f.Mode().IsDir() || strings.HasSuffix(decodedName, "/") || strings.HasSuffix(decodedName, "\\")
		if directory && f.UncompressedSize64 != 0 {
			return nil, fail("ZIP_CORRUPTED")
		}
		name, err := safePath(decodedName, limits.MaxDepth, directory)
		if err != nil {
			return nil, err
		}
		if err := checkLocalHeader(blob, f); err != nil {
			return nil, err
		}
		// A relative root directory is metadata rather than a source-tree node.
		if name == "." {
			continue
		}
		folded := strings.ToLower(name)
		if _, exists := seen[folded]; exists {
			return nil, fail("ZIP_UNSAFE_PATH")
		}
		seen[folded] = directory
		if !directory {
			files++
			if files > limits.MaxFiles {
				return nil, fail("ZIP_TOO_MANY_FILES")
			}
		}
		entries = append(entries, entry{file: f, path: name, directory: directory})
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if isDir, exists := seen[parent]; exists && !isDir {
				return nil, fail("ZIP_UNSAFE_PATH")
			}
		}
	}
	if files == 0 {
		return nil, fail("ZIP_NO_SUPPORTED_FILES")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

func checkLocalHeader(blob []byte, f *zip.File) error {
	offset, err := f.DataOffset()
	if err != nil || offset < 30 || offset > int64(len(blob)) || f.CompressedSize64 > uint64(int64(len(blob))-offset) {
		return fail("ZIP_CORRUPTED")
	}
	// DataOffset is computed from the local header's name and extra lengths.
	// Locate that exact header without trusting either copy of the name alone.
	start := offset - 30 - int64(len(f.Name))
	min := start - 65535
	if min < 0 {
		min = 0
	}
	for at := start; at >= min; at-- {
		h := blob[at : at+30]
		if binary.LittleEndian.Uint32(h) != 0x04034b50 {
			continue
		}
		n := int64(binary.LittleEndian.Uint16(h[26:]))
		e := int64(binary.LittleEndian.Uint16(h[28:]))
		if at+30+n+e != offset {
			continue
		}
		if binary.LittleEndian.Uint16(h[6:])&65 != 0 {
			return fail("ZIP_ENCRYPTED")
		}
		if string(blob[at+30:at+30+n]) != f.Name || binary.LittleEndian.Uint16(h[8:]) != f.Method {
			return fail("ZIP_CORRUPTED")
		}
		return nil
	}
	return fail("ZIP_CORRUPTED")
}

type readerBudget struct {
	ctx   context.Context
	limit int64
	read  int64
}

// readMember always reaches EOF for accepted candidates, so zip's CRC check
// cannot be bypassed by a binary/encoding classification.
func (b *readerBudget) readMember(f *zip.File, memberLimit int64) ([]byte, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	r, err := f.Open()
	if err != nil {
		return nil, fail("ZIP_CORRUPTED")
	}
	defer r.Close()
	var out bytes.Buffer
	buffer := make([]byte, 32<<10)
	for {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		remaining := min(b.limit-b.read, memberLimit-int64(out.Len()))
		if remaining < 0 {
			return nil, fail("ZIP_BOMB_SUSPECTED")
		}
		readSize := int64(len(buffer))
		if remaining < readSize {
			readSize = remaining + 1
		}
		n, readErr := r.Read(buffer[:readSize])
		b.read += int64(n)
		if b.read > b.limit || int64(out.Len())+int64(n) > memberLimit {
			return nil, fail("ZIP_BOMB_SUSPECTED")
		}
		out.Write(buffer[:n])
		if readErr == io.EOF {
			return out.Bytes(), nil
		}
		if readErr != nil {
			return nil, fail("ZIP_CORRUPTED")
		}
	}
}

func decode(raw []byte) (string, bool) {
	if bytes.IndexByte(raw[:min(8192, len(raw))], 0) >= 0 {
		return "", false
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	var text string
	if utf8.Valid(raw) {
		text = string(raw)
	} else {
		var err error
		text, err = simplifiedchinese.GBK.NewDecoder().String(string(raw))
		if err != nil || strings.ContainsRune(text, utf8.RuneError) {
			return "", false
		}
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	return text, true
}
