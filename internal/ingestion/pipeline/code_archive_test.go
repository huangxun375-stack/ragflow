package pipeline

import (
	"encoding/json"
	"testing"
)

func TestCodeArchiveTemplateIsRunnableButNotDatasetChoice(t *testing.T) {
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get("code_archive"); !ok {
		t.Fatal("missing code archive runtime template")
	}
	for _, entry := range registry.List().BuiltinPipelines {
		if entry.ParserID == "code_archive" {
			t.Fatal("ZIP-only parser advertised as dataset default")
		}
	}
	dsl, err := LoadBuiltinDSL("code_archive")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components map[string]struct {
			Obj struct {
				ComponentName string `json:"component_name"`
			} `json:"obj"`
		} `json:"components"`
		Graph map[string]any `json:"graph"`
	}
	if err = json.Unmarshal([]byte(dsl), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Components) != 3 || doc.Components["CodeArchive:Archive"].Obj.ComponentName != "CodeArchive" || doc.Components["Tokenizer:Archive"].Obj.ComponentName != "Tokenizer" {
		t.Fatalf("unexpected graph: %s", dsl)
	}
	if doc.Graph == nil {
		t.Fatal("template graph metadata missing")
	}
	for _, c := range doc.Components {
		if c.Obj.ComponentName == "Extractor" || c.Obj.ComponentName == "Compiler" {
			t.Fatal("ZIP must not inherit LLM enhancements")
		}
	}
}
