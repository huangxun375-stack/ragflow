//go:build manual

package codearchive_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"ragflow/internal/ingestion/testutil"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/tokenizer"
)

func TestArchiveTenMiBScale(t *testing.T) {
	if err := tokenizer.InitCL100KEncoder(); err != nil {
		t.Fatal(err)
	}
	counter, err := tokenizer.LoadXLMRSentencePieceCounter()
	if err != nil {
		t.Fatal(err)
	}
	blob, files, err := testutil.ArchiveScale()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("upload_boundary", func(t *testing.T) {
		limits := codearchive.DefaultLimits()
		if len(blob) != 10<<20 || int64(len(blob)) != limits.MaxCompressedBytes {
			t.Fatalf("archive has %d bytes, want the 10 MiB upload limit", len(blob))
		}
		if err := codearchive.Precheck(blob, limits); err != nil {
			t.Fatal(err)
		}
		limits.MaxCompressedBytes--
		if err := codearchive.Precheck(blob, limits); codearchive.Reason(err) != "ZIP_SIZE_EXCEEDED" {
			t.Fatalf("archive above upload limit: %v", err)
		}
	})
	t.Run("source_coverage", func(t *testing.T) {
		result, err := codearchive.Process(t.Context(), blob, codearchive.Options{
			Filename: "ten-mib-source.zip", ChunkTokens: 512,
			CountTokens:   tokenizer.NumTokensFromString,
			FitsEmbedding: func(text string) bool { return counter.Count(text) <= 8192 },
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Stats.AcceptedFiles != len(files) || result.Stats.TotalFiles != len(files) || result.Stats.SyntaxFallbacks != 0 {
			t.Fatalf("source counts or syntax fallback: %+v", result.Stats)
		}
		if len(result.Chunks) < len(files) || len(result.Chunks) > codearchive.DefaultLimits().MaxChunks {
			t.Fatalf("invalid chunk count: %d", len(result.Chunks))
		}
		tree, err := json.Marshal(result.Tree)
		if err != nil || len(tree) > codearchive.DefaultPolicy().MaxArtifactBytes {
			t.Fatalf("tree exceeds artifact bound: bytes=%d error=%v", len(tree), err)
		}
		rebuilt := make(map[string]*strings.Builder, len(files))
		var sourceBytes int64
		for name, source := range files {
			sourceBytes += int64(len(source))
			rebuilt[name] = &strings.Builder{}
		}
		if result.Stats.ReadBytes != sourceBytes {
			t.Fatalf("read %d source bytes, want %d", result.Stats.ReadBytes, sourceBytes)
		}
		for i, chunk := range result.Chunks {
			source, ok := files[chunk.SourcePath]
			if !ok {
				t.Fatalf("chunk %d has unknown source %q", i, chunk.SourcePath)
			}
			lineCount := strings.Count(strings.TrimSuffix(source, "\n"), "\n") + 1
			if chunk.Order != i || chunk.StartLine < 1 || chunk.EndLine < chunk.StartLine || chunk.EndLine > lineCount {
				t.Fatalf("chunk %d has invalid provenance: %+v", i, chunk)
			}
			header := fmt.Sprintf("// %s:L%d-L%d", chunk.SourcePath, chunk.StartLine, chunk.EndLine)
			if !strings.HasPrefix(chunk.Text, header) {
				t.Fatalf("chunk %d has invalid source header", i)
			}
			_, body, ok := strings.Cut(chunk.Text, "\n")
			if !ok || strings.TrimSpace(body) == "" || !utf8.ValidString(chunk.Text) {
				t.Fatalf("chunk %d is blank or invalid UTF-8", i)
			}
			builder := rebuilt[chunk.SourcePath]
			start, end := builder.Len(), builder.Len()+len(body)
			if end > len(source) || source[start:end] != body {
				t.Fatalf("chunk %d changed or duplicated source bytes", i)
			}
			if chunk.StartLine != strings.Count(source[:start], "\n")+1 || chunk.EndLine != strings.Count(source[:end-1], "\n")+1 {
				t.Fatalf("chunk %d has inaccurate line numbers", i)
			}
			builder.WriteString(body)
			if tokenizer.NumTokensFromString(chunk.Text) > 512 || counter.Count(chunk.Text) > 8192 {
				t.Fatalf("chunk %d exceeds tokenizer budget", i)
			}
		}
		for name, source := range files {
			if rebuilt[name].String() != source {
				t.Fatalf("chunk bodies do not reconstruct %s", name)
			}
		}
	})
}
