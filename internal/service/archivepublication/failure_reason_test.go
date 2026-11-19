package archivepublication

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"ragflow/internal/common"
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
)

func TestFailureReasonIsFencedAndResetWithNewRun(t *testing.T) {
	db, scope := publicationDB(t)
	first, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	assertReason := func(want string) {
		t.Helper()
		metadata, err := Metadata(t.Context(), db, []string{scope.DocID})
		if err != nil {
			t.Fatal(err)
		}
		if got := metadata[scope.DocID].FailureReason; got != want {
			t.Fatalf("reason=%q want=%q", got, want)
		}
	}
	if err = Terminate(t.Context(), db, first, common.FAILED, fmt.Errorf("component: %w", &codearchive.Error{Code: "ZIP_NO_SUPPORTED_FILES"})); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{nil, errors.New("provider body ZIP_CONFIG_INVALID private/path"), &codearchive.Error{Code: "ZIP_CORRUPTED"}} {
		if err = Terminate(t.Context(), db, first, common.FAILED, cause); err != nil {
			t.Fatal(err)
		}
		assertReason("ZIP_NO_SUPPORTED_FILES")
	}
	if err = Queue(t.Context(), db, scope); err != nil {
		t.Fatal(err)
	}
	assertReason("ZIP_NO_SUPPORTED_FILES")
	scope.RunID = "new-run"
	if err = db.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Updates(map[string]any{"pipeline_log_id": scope.RunID, "status": common.RUNNING}).Error; err != nil {
		t.Fatal(err)
	}
	if err = Queue(t.Context(), db, scope); err != nil {
		t.Fatal(err)
	}
	assertReason("")
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = Terminate(t.Context(), db, first, common.FAILED, &codearchive.Error{Code: "ZIP_CORRUPTED"}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale failure: %v", err)
	}
	assertReason("")
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	if err = Terminate(t.Context(), db, current, common.FAILED, &codearchive.Error{Code: "ZIP_CORRUPTED"}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("published failure: %v", err)
	}
	assertReason("")
}

func TestFailureReasonIgnoresUntypedMessagesAndCancelledRuns(t *testing.T) {
	for _, status := range []string{common.FAILED, common.STOPPED} {
		t.Run(status, func(t *testing.T) {
			db, scope := publicationDB(t)
			current, err := Acquire(t.Context(), db, scope, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err = Terminate(t.Context(), db, current, status, errors.New("ZIP_NO_SUPPORTED_FILES /private/project.go")); err != nil {
				t.Fatal(err)
			}
			metadata, err := Metadata(t.Context(), db, []string{scope.DocID})
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if status == common.FAILED {
				want = "ZIP_PARSE_FAILED"
			}
			if metadata[scope.DocID].FailureReason != want {
				t.Fatalf("unsafe reason: %+v", metadata)
			}
			if status == common.STOPPED {
				if err = Terminate(t.Context(), db, current, common.FAILED, &codearchive.Error{Code: "ZIP_CORRUPTED"}); !errors.Is(err, ErrSuperseded) {
					t.Fatalf("cancelled overwritten: %v", err)
				}
			}
		})
	}
}

func TestTerminateEnrichesOnlyGenericFailureForCurrentTuple(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := CanCompensate(t.Context(), db, current, nil); err != nil || !allowed {
		t.Fatalf("generic compensation: %t %v", allowed, err)
	}
	for _, cause := range []error{&codearchive.Error{Code: "ZIP_CORRUPTED"}, &codearchive.Error{Code: "ZIP_NO_SUPPORTED_FILES"}, nil} {
		if err := Terminate(t.Context(), db, current, common.FAILED, cause); err != nil {
			t.Fatal(err)
		}
		var row entity.DocumentCodeArchive
		if err := db.Take(&row, "doc_id = ?", scope.DocID).Error; err != nil || row.Status != "failed" || row.FailureReason != "ZIP_CORRUPTED" {
			t.Fatalf("typed reason was lost or rewritten: %+v %v", row, err)
		}
	}
	stale := current
	stale.AttemptID = "stale-attempt"
	if err := Terminate(t.Context(), db, stale, common.FAILED, &codearchive.Error{Code: "ZIP_NO_SUPPORTED_FILES"}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale failure enriched current tuple: %v", err)
	}
}

func TestCompensationPersistsCauseBeforeExternalCleanup(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := CanCompensate(t.Context(), db, current, fmt.Errorf("parser: %w", &codearchive.Error{Code: "ZIP_NO_SUPPORTED_FILES"})); err != nil || !allowed {
		t.Fatalf("compensation: %t %v", allowed, err)
	}
	// Even a crash before MarkFailed must leave both state and reason durable.
	rows, err := Metadata(t.Context(), db, []string{scope.DocID})
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[scope.DocID]; got.Status != "failed" || got.FailureReason != "ZIP_NO_SUPPORTED_FILES" {
		t.Fatalf("incomplete terminal state: %+v", got)
	}
	if err = Terminate(t.Context(), db, current, common.FAILED, nil); err != nil {
		t.Fatal(err)
	}
	rows, err = Metadata(t.Context(), db, []string{scope.DocID})
	if err != nil || rows[scope.DocID].FailureReason != "ZIP_NO_SUPPORTED_FILES" {
		t.Fatalf("cause overwritten: %+v %v", rows, err)
	}
}
