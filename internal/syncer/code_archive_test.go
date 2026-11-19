package syncer

import (
	"ragflow/internal/dao"
	"ragflow/internal/service"
	syncerconnector "ragflow/internal/syncer/connector"
	"testing"
)

func TestArchiveConnectorSkipsBeforeResolutionOrFetch(t *testing.T) {
	runner := &SyncRunner{}
	for _, doc := range []syncerconnector.SourceDocument{
		{SemanticIdentifier: "repo.ZIP"},
		{SemanticIdentifier: "repo", Extension: ".ZIP"},
		{SemanticIdentifier: "repo", Extension: "zip"},
	} {
		result, err := runner.processDocument(t.Context(), dao.SyncTaskContext{}, "connector", nil, doc)
		if err != nil || result.Action != service.DocumentActionSkipped {
			t.Fatalf("doc=%+v result=%+v err=%v", doc, result, err)
		}
	}
}
