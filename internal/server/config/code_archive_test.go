package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"ragflow/internal/parser/codearchive"
)

func TestCodeArchiveConfigDefaultsAndTightenedYAML(t *testing.T) {
	c := &Config{}
	if err := c.ParseCodeArchiveConfig(viper.New()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.GetCodeArchivePolicy(), codearchive.DefaultPolicy()) {
		t.Fatal("missing configuration must use safe defaults")
	}
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`code_archive:
  max_archive_bytes: 1048576
  max_members: 100
  max_depth: 8
  max_uncompressed_bytes: 4194304
  max_file_bytes: 65536
  max_gitignore_bytes: 1024
  max_chunks: 1000
  artifact_max_bytes: 1048576
  ignored_dirs: [generated, vendor]
  respect_gitignore: false
  syntax_error_ratio: 0.1
  default_chunk_token_num: 256
  languages: [go, python]
  extensions: [.go, .py]
`)); err != nil {
		t.Fatal(err)
	}
	if err := c.ParseCodeArchiveConfig(v); err != nil {
		t.Fatal(err)
	}
	got := c.GetCodeArchivePolicy()
	if got.Limits != (codearchive.Limits{MaxCompressedBytes: 1048576, MaxFiles: 100, MaxDepth: 8, MaxReadBytes: 4194304, MaxFileBytes: 65536, MaxGitignoreBytes: 1024, MaxChunks: 1000}) || got.MaxArtifactBytes != 1048576 || got.DefaultChunkTokens != 256 || got.RespectGitignore || got.SyntaxErrorRatio != .1 {
		t.Fatalf("policy = %+v", got)
	}
	if !slices.Contains(got.IgnoredDirs, "generated") || !slices.Contains(got.IgnoredDirs, "node_modules") || !reflect.DeepEqual(got.Languages, []string{"go", "python"}) || !reflect.DeepEqual(got.Extensions, []string{".go", ".py"}) {
		t.Fatalf("selection = %+v", got)
	}
	got.IgnoredDirs[0] = "mutated"
	got.Languages[0] = "mutated"
	got.Extensions[0] = "mutated"
	if slices.Contains(c.GetCodeArchivePolicy().IgnoredDirs, "mutated") || slices.Contains(c.GetCodeArchivePolicy().Languages, "mutated") || slices.Contains(c.GetCodeArchivePolicy().Extensions, "mutated") {
		t.Fatal("configuration getter exposed mutable policy slices")
	}
}

func TestCodeArchiveRetrievalDeploymentConfig(t *testing.T) {
	c := &Config{}
	if got := c.GetCodeArchiveRetrievalConfig(); got.TestSourceWeight != .95 {
		t.Fatal(got)
	}
	v := viper.New()
	v.Set("code_archive", map[string]any{"test_source_weight": .97})
	if err := c.ParseCodeArchiveConfig(v); err != nil {
		t.Fatal(err)
	}
	if got := c.GetCodeArchiveRetrievalConfig(); got.TestSourceWeight != .97 {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(c.GetCodeArchivePolicy(), codearchive.DefaultPolicy()) {
		t.Fatal("retrieval config changed parse snapshot")
	}
	t.Setenv("RAGFLOW_CODE_ARCHIVE_TEST_SOURCE_WEIGHT", "1")
	if err := c.ParseCodeArchiveConfig(v); err != nil {
		t.Fatal(err)
	}
	if got := c.GetCodeArchiveRetrievalConfig(); got.TestSourceWeight != 1 {
		t.Fatal(got)
	}
	for _, value := range []string{"0", "-0.1", "1.01", "null", "NaN", "\"0.95\"", ""} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("RAGFLOW_CODE_ARCHIVE_TEST_SOURCE_WEIGHT", value)
			if err := c.ParseCodeArchiveConfig(v); err == nil {
				t.Fatal("invalid weight accepted")
			}
		})
	}
}

func TestCodeArchiveChunkTargetRatio(t *testing.T) {
	if got := (&Config{}).GetCodeArchivePolicy().ChunkTargetRatio; got != .5 {
		t.Fatalf("default ratio = %g", got)
	}
	for _, value := range []string{"0", "0.25", "0.5", "0.75", "1"} {
		t.Run(value, func(t *testing.T) {
			v := viper.New()
			v.SetConfigType("yaml")
			if err := v.ReadConfig(strings.NewReader("code_archive:\n  chunk_target_ratio: " + value + "\n")); err != nil {
				t.Fatal(err)
			}
			c := &Config{}
			if err := c.ParseCodeArchiveConfig(v); err != nil {
				t.Fatal(err)
			}
			var want float64
			if err := json.Unmarshal([]byte(value), &want); err != nil {
				t.Fatal(err)
			}
			if c.GetCodeArchivePolicy().ChunkTargetRatio != want {
				t.Fatal("YAML target ratio ignored")
			}
			t.Setenv("RAGFLOW_CODE_ARCHIVE_CHUNK_TARGET_RATIO", value)
			v.Set("code_archive.chunk_target_ratio", 1-want)
			if err := c.ParseCodeArchiveConfig(v); err != nil || c.GetCodeArchivePolicy().ChunkTargetRatio != want {
				t.Fatalf("environment override ignored: %+v %v", c.GetCodeArchivePolicy(), err)
			}
		})
	}
	for _, value := range []string{"-0.01", "1.01", "NaN", "+Inf", "null", "\"0.5\"", "true", ""} {
		t.Run("invalid="+value, func(t *testing.T) {
			t.Setenv("RAGFLOW_CODE_ARCHIVE_CHUNK_TARGET_RATIO", value)
			if err := (&Config{}).ParseCodeArchiveConfig(viper.New()); err == nil {
				t.Fatal("invalid environment target ratio accepted")
			}
		})
	}
}

func TestCodeArchiveConfigRejectsUnsafeOrMistypedSettings(t *testing.T) {
	for _, yaml := range []string{
		"chunk_target_ratio: -0.01", "chunk_target_ratio: 1.01", "chunk_target_ratio: .nan", "chunk_target_ratio: .inf", "chunk_target_ratio: null", "chunk_target_ratio: '0.5'", "chunk_target_ratio: true",
		"test_source_weight: 0", "test_source_weight: -0.01", "test_source_weight: 1.01", "test_source_weight: .nan", "test_source_weight: null", "test_source_weight: wrong",
		"max_archive_bytes: 10485761", "max_members: 5001", "max_depth: 21", "max_uncompressed_bytes: 104857601", "max_file_bytes: 1048577", "max_gitignore_bytes: 65537", "max_chunks: 100001", "artifact_max_bytes: 8388609", "max_members: 0", "max_depth: -1", "max_file_bytes: 1.5", "default_chunk_token_num: 0", "syntax_error_ratio: 1.1", "respect_gitignore: typo", "max_depth: wrong", "ignored_dirs: [../secrets]", "languages: [rust]", "extensions: [.txt]", "languages: []", "extensions: []", "max_memebers: 12",
	} {
		t.Run(yaml, func(t *testing.T) {
			v := viper.New()
			v.SetConfigType("yaml")
			if err := v.ReadConfig(strings.NewReader("code_archive:\n  " + yaml + "\n")); err != nil {
				t.Fatal(err)
			}
			if err := (&Config{}).ParseCodeArchiveConfig(v); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestCodeArchiveEnvironmentOverridesAreTypedAndValidated(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	v.SetEnvPrefix("RAGFLOW")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := v.ReadConfig(strings.NewReader("code_archive:\n  max_archive_bytes: 10485760\n  respect_gitignore: true\n")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAGFLOW_CODE_ARCHIVE_MAX_ARCHIVE_BYTES", "1048576")
	t.Setenv("RAGFLOW_CODE_ARCHIVE_RESPECT_GITIGNORE", "false")
	t.Setenv("RAGFLOW_CODE_ARCHIVE_SYNTAX_ERROR_RATIO", "0.1")
	t.Setenv("RAGFLOW_CODE_ARCHIVE_LANGUAGES", `["go"]`)
	t.Setenv("RAGFLOW_CODE_ARCHIVE_EXTENSIONS", `[".go"]`)
	c := &Config{}
	if err := c.ParseCodeArchiveConfig(v); err != nil {
		t.Fatal(err)
	}
	got := c.GetCodeArchivePolicy()
	if got.Limits.MaxCompressedBytes != 1048576 || got.RespectGitignore || got.SyntaxErrorRatio != .1 || !reflect.DeepEqual(got.Languages, []string{"go"}) || !reflect.DeepEqual(got.Extensions, []string{".go"}) {
		t.Fatalf("environment overrides ignored: %+v", got)
	}
	for name, value := range map[string]string{
		"MAX_ARCHIVE_BYTES": "10485761", "MAX_MEMBERS": "0", "MAX_DEPTH": "", "MAX_CHUNKS": "1.5", "RESPECT_GITIGNORE": "flase", "SYNTAX_ERROR_RATIO": "null", "LANGUAGES": "go", "EXTENSIONS": `[".txt"]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("RAGFLOW_CODE_ARCHIVE_"+name, value)
			if err := (&Config{}).ParseCodeArchiveConfig(v); err == nil {
				t.Fatal("invalid environment override accepted")
			}
		})
	}
}
