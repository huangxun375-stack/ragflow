package service

import (
	"fmt"
	"ragflow/internal/parser/codearchive"
	"testing"
	"time"

	"ragflow/internal/dao"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/service/archivepublication"
)

func TestArchiveTaskFailurePersistsSpecificReasonAfterCompensation(t *testing.T) {
	for _, compensate := range []bool{false, true} {
		name := "direct"
		if compensate {
			name = "after compensation"
		}
		t.Run(name, func(t *testing.T) {
			svc, input := archiveTaskFixture(t)
			svc.SetTaskPublisher(&recordingTaskPublisher{})
			created, err := svc.CreateAndEnqueue(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			running, err := svc.TransitionTaskToRunning(t.Context(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := archivepublication.Acquire(t.Context(), dao.DB, archiveartifact.Scope{DocID: running.DocumentID, KBID: running.DatasetID, TaskID: running.ID, RunID: *running.PipelineLogID}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if compensate {
				if allowed, err := archivepublication.CanCompensate(t.Context(), dao.DB, scope, nil); err != nil || !allowed {
					t.Fatalf("compensation: %t %v", allowed, err)
				}
			}
			const reason = "ZIP_NO_SUPPORTED_FILES"
			if err = svc.MarkFailed(archiveartifact.WithScope(t.Context(), scope), created.ID, fmt.Errorf("pipeline: %w", &codearchive.Error{Code: reason})); err != nil {
				t.Fatal(err)
			}
			rows, err := archivepublication.Metadata(t.Context(), dao.DB, []string{scope.DocID})
			if err != nil {
				t.Fatal(err)
			}
			if got := rows[scope.DocID]; got.Status != "failed" || got.FailureReason != reason {
				t.Fatalf("reason lost: %+v", got)
			}
		})
	}
}
