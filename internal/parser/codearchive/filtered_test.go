package codearchive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestFilteredEntriesGroupOnlyRuleExcludedDirectories(t *testing.T) {
	files := []member{
		{"repo/.gitignore", "generated/\n*.tmp.py\n!keep.tmp.py\n", 0},
		{"repo/node_modules/pkg/a.js", "ignored", 0},
		{"repo/node_modules/pkg/b.js", "ignored", 0},
		{"repo/node_modules/pkg/._metadata", "ignored", 0},
		{"repo/generated/a.py", "ignored", 0},
		{"repo/generated/.gitignore", "!a.py", 0},
		{"repo/partial/drop.tmp.py", "ignored", 0},
		{"repo/partial/keep.tmp.py", "pass\n", 0},
		{"repo/drop.tmp.py", "ignored", 0},
		{"repo/keep.tmp.py", "pass\n", 0},
		{"repo/src/main.py", "pass\n", 0},
		{"repo/src/README.md", "documentation", 0},
		{"repo/src/empty.py", " \n", 0},
		{"repo/src/binary.py", "a\x00b", 0},
		{"repo/src/large.py", strings.Repeat("x", 201), 0},
		{"repo/.hidden/a.py", "ignored", 0},
		{"repo/.hidden/b.py", "ignored", 0},
		{"repo/__MACOSX/a.py", "ignored", 0},
		{"repo/node_modules_extra/keep.py", "pass\n", 0},
	}
	policy := DefaultPolicy()
	policy.Limits.MaxFileBytes = 200
	opts := options()
	opts.Policy = &policy
	blob := archive(t, files...)
	got, err := Process(t.Context(), blob, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []FilteredEntry{
		{Path: "repo/.gitignore", Type: "file", Reason: "hidden", FileCount: 1},
		{Path: "repo/.hidden", Type: "directory", Reason: "hidden", FileCount: 2},
		{Path: "repo/__MACOSX", Type: "directory", Reason: "meta", FileCount: 1},
		{Path: "repo/drop.tmp.py", Type: "file", Reason: "gitignore", FileCount: 1},
		{Path: "repo/generated", Type: "directory", Reason: "gitignore", FileCount: 2},
		{Path: "repo/node_modules", Type: "directory", Reason: "dir", FileCount: 3},
		{Path: "repo/partial/drop.tmp.py", Type: "file", Reason: "gitignore", FileCount: 1},
		{Path: "repo/src/README.md", Type: "file", Reason: "suffix", FileCount: 1},
		{Path: "repo/src/binary.py", Type: "file", Reason: "binary", FileCount: 1},
		{Path: "repo/src/empty.py", Type: "file", Reason: "empty", FileCount: 1},
		{Path: "repo/src/large.py", Type: "file", Reason: "large", FileCount: 1},
	}
	if !reflect.DeepEqual(got.Stats.FilteredEntries, want) || got.Stats.FilteredEntriesTruncated {
		t.Fatalf("filtered = %+v", got.Stats)
	}
	if got.Stats.SkippedDir != 2 || got.Stats.SkippedMeta != 2 {
		t.Fatalf("directory aggregation changed statistical priority: %+v", got.Stats)
	}
	total := got.Stats.AcceptedFiles
	for _, item := range got.Stats.FilteredEntries {
		total += item.FileCount
	}
	if total != got.Stats.TotalFiles || got.Stats.AcceptedFiles != 4 {
		t.Fatalf("file accounting = %d, stats = %+v", total, got.Stats)
	}
	for _, chunk := range got.Chunks {
		if !strings.HasSuffix(chunk.SourcePath, "keep.py") && !strings.HasSuffix(chunk.SourcePath, "main.py") && !strings.HasSuffix(chunk.SourcePath, "keep.tmp.py") {
			t.Fatalf("filtered source entered chunks: %s", chunk.SourcePath)
		}
	}
	slices.Reverse(files)
	again, err := Process(t.Context(), archive(t, files...), opts)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("archive order changed output: %v", err)
	}
}

func TestProcessSkipPrecedenceAndReadAccounting(t *testing.T) {
	large := strings.Repeat("x", 17)
	for _, tc := range []struct {
		name, path, text, ignore string
		stats                    Stats
		filtered                 FilteredEntry
		read                     int64
	}{
		{"metadata before directory and ignore", "node_modules/._skip.py", large, "node_modules/\n", Stats{SkippedMeta: 1}, FilteredEntry{Path: "node_modules", Type: "directory", Reason: "dir", FileCount: 1}, 0},
		{"directory before ignore", "node_modules/.skip.txt", large, "node_modules/\n", Stats{SkippedDir: 1}, FilteredEntry{Path: "node_modules", Type: "directory", Reason: "dir", FileCount: 1}, 0},
		{"ignore before hidden", ".skip.txt", large, ".skip.txt\n", Stats{SkippedGitignore: 1}, FilteredEntry{Path: ".skip.txt", Type: "file", Reason: "gitignore", FileCount: 1}, 0},
		{"hidden before suffix", ".skip.txt", large, "", Stats{SkippedHidden: 1}, FilteredEntry{Path: ".skip.txt", Type: "file", Reason: "hidden", FileCount: 1}, 0},
		{"suffix before size", "skip.txt", large, "", Stats{SkippedSuffix: 1}, FilteredEntry{Path: "skip.txt", Type: "file", Reason: "suffix", FileCount: 1}, 0},
		{"size before empty", "skip.py", strings.Repeat(" ", 17), "", Stats{SkippedLarge: 1}, FilteredEntry{Path: "skip.py", Type: "file", Reason: "large", FileCount: 1}, 0},
		{"raw empty", "skip.py", " \r\n\t", "", Stats{SkippedEmpty: 1}, FilteredEntry{Path: "skip.py", Type: "file", Reason: "empty", FileCount: 1}, 4},
		{"decoded empty", "skip.py", "\xef\xbb\xbf\r\n", "", Stats{SkippedEmpty: 1}, FilteredEntry{Path: "skip.py", Type: "file", Reason: "empty", FileCount: 1}, 5},
		{"binary before decoded empty", "skip.py", "\x00 \n", "", Stats{SkippedBinary: 1}, FilteredEntry{Path: "skip.py", Type: "file", Reason: "binary", FileCount: 1}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []member{{"good.py", "pass\n", 0}, {tc.path, tc.text, 0}, {"empty/", "", 0}}
			want := tc.stats
			want.TotalFiles, want.AcceptedFiles = 2, 1
			want.ReadBytes = 5 + tc.read
			if tc.ignore != "" {
				files = append(files, member{".gitignore", tc.ignore, 0})
				want.TotalFiles++
				want.SkippedHidden++
				want.ReadBytes += int64(len(tc.ignore))
				want.FilteredEntries = append(want.FilteredEntries, FilteredEntry{Path: ".gitignore", Type: "file", Reason: "hidden", FileCount: 1})
			}
			want.FilteredEntries = append(want.FilteredEntries, tc.filtered)
			policy := DefaultPolicy()
			policy.Limits.MaxFileBytes = 16
			opts := options()
			opts.Policy = &policy
			got, err := Process(t.Context(), archive(t, files...), opts)
			if err != nil || !reflect.DeepEqual(got.Stats, want) {
				t.Fatalf("stats=%+v want=%+v error=%v", got.Stats, want, err)
			}
			wantChunks := []Chunk{{Text: "// good.py:L1-L1\npass\n", SourcePath: "good.py", Language: "python", StartLine: 1, EndLine: 1}}
			wantTree := []TreeNode{{Name: "good.py", Path: "good.py", Type: "file"}}
			if !reflect.DeepEqual(got.Chunks, wantChunks) || !reflect.DeepEqual(got.Tree, wantTree) || got.RootName != "snapshot" {
				t.Fatalf("skipped members changed accepted output: %+v", got)
			}
		})
	}
}

func TestFilteredEntriesDoNotReadExcludedPayload(t *testing.T) {
	blob := archive(t, member{"good.py", "pass\n", 0}, member{"node_modules/a.py", "corrupt-excluded-payload", 0})
	blob[bytes.Index(blob, []byte("corrupt-excluded-payload"))] ^= 0xff
	got, err := Process(t.Context(), blob, options())
	if err != nil {
		t.Fatal(err)
	}
	if got.Stats.ReadBytes != 5 || len(got.Stats.FilteredEntries) != 1 || got.Stats.FilteredEntries[0].Path != "node_modules" {
		t.Fatalf("unexpected reads/details: %+v", got.Stats)
	}
}

func TestFilteredEntriesCapKeepsCountsAndExistingAggregates(t *testing.T) {
	var stats Stats
	r := filteredRecorder{stats: &stats, policy: DefaultPolicy(), ignore: &ignoreMatcher{}}
	r.add("node_modules/first.py", "dir")
	for i := 0; i < maxFilteredEntries+10; i++ {
		r.add(fmt.Sprintf("src/%04d.txt", i), "suffix")
	}
	r.add("node_modules/last.py", "dir")
	if len(stats.FilteredEntries) != maxFilteredEntries || !stats.FilteredEntriesTruncated || stats.FilteredEntries[0].FileCount != 2 {
		t.Fatalf("cap/aggregation: %+v", stats)
	}
	files := []member{{"good.py", "pass\n", 0}}
	for i := 0; i < maxFilteredEntries+10; i++ {
		files = append(files, member{fmt.Sprintf("src/%04d.txt", i), "ignored", 0})
	}
	got, err := Process(t.Context(), archive(t, files...), options())
	if err != nil || got.Stats.TotalFiles != len(files) || got.Stats.SkippedSuffix != len(files)-1 || !got.Stats.FilteredEntriesTruncated {
		t.Fatalf("totals must survive truncation: %+v %v", got.Stats, err)
	}
}

func TestFilteredEntriesByteCapIncludesJSONEscaping(t *testing.T) {
	var stats Stats
	r := filteredRecorder{stats: &stats, policy: DefaultPolicy(), ignore: &ignoreMatcher{}}
	for i := 0; i < 40; i++ {
		r.add(fmt.Sprintf("%02d%s.txt", i, strings.Repeat("<", 2000)), "suffix")
	}
	raw, err := json.Marshal(stats.FilteredEntries)
	if err != nil || len(raw) > maxFilteredBytes || !stats.FilteredEntriesTruncated || len(stats.FilteredEntries) == 0 {
		t.Fatalf("bytes=%d entries=%d truncated=%t error=%v", len(raw), len(stats.FilteredEntries), stats.FilteredEntriesTruncated, err)
	}
}
