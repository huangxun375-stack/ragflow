package archivepublication

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"ragflow/internal/common"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/parser/codearchive"
)

func publicationDB(t *testing.T) (*gorm.DB, archiveartifact.Scope) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if err = db.AutoMigrate(&entity.Document{}, &entity.Knowledgebase{}, &entity.IngestionTask{}, &entity.DocumentCodeArchive{}, &entity.ArchiveRun{}, &entity.ArchiveAttempt{}, &entity.ArchiveDatasetFence{}); err != nil {
		t.Fatal(err)
	}
	scope := archiveartifact.Scope{DocID: "doc", KBID: "kb", TaskID: "task", RunID: "run"}
	for _, row := range []any{&entity.Knowledgebase{ID: "kb", Name: "kb", TenantID: "tenant"}, &entity.Document{ID: "doc", KbID: "kb", ParserID: codearchive.ParserID, ParserConfig: entity.JSONMap{}}, &entity.IngestionTask{ID: "task", UserID: "user", DocumentID: "doc", DatasetID: "kb", Status: common.RUNNING, PipelineLogID: &scope.RunID}} {
		if err = db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err = Queue(t.Context(), db, scope); err != nil {
		t.Fatal(err)
	}
	return db, scope
}

func artifact() archiveartifact.Artifact {
	return archiveartifact.Artifact{RootName: "src", Tree: []codearchive.TreeNode{{Name: "a.go", Path: "a.go", Type: "file"}}, Stats: codearchive.Stats{AcceptedFiles: 1, SkippedDir: 2, FilteredEntries: []codearchive.FilteredEntry{{Path: "node_modules", Type: "directory", Reason: "dir", FileCount: 2}}}}
}

func TestCountOneRequiresExactlyOneMatchAndPreservesQueryErrors(t *testing.T) {
	db, scope := publicationDB(t)
	if err := db.Create(&entity.Knowledgebase{ID: "other", Name: "other", TenantID: "tenant"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ids  []string
		want error
	}{
		{name: "none", ids: []string{"missing"}, want: ErrSuperseded},
		{name: "one", ids: []string{scope.KBID}},
		{name: "multiple", ids: []string{scope.KBID, "other"}, want: ErrSuperseded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := countOne(db.Model(&entity.Knowledgebase{}).Where("id IN ?", tc.ids)); !errors.Is(err, tc.want) {
				t.Fatalf("countOne: %v, want %v", err, tc.want)
			}
		})
	}
	queryErr := errors.New("count query rejected")
	query := db.Model(&entity.Knowledgebase{})
	query.AddError(queryErr)
	if err := countOne(query); !errors.Is(err, queryErr) {
		t.Fatalf("count error lost: %v", err)
	}
}

func TestPublicationHelpersPreserveMySQLQueries(t *testing.T) {
	fixture, scope := publicationDB(t)
	sqlDB, err := fixture.DB()
	if err != nil {
		t.Fatal(err)
	}
	// DryRun uses the MySQL dialect without connecting to an external service.
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	scope.AttemptID = "attempt"
	var sql string
	var vars []any
	if err := db.Callback().Query().After("gorm:query").Register("capture_publication_query", func(tx *gorm.DB) {
		sql = tx.Statement.SQL.String()
		vars = append([]any(nil), tx.Statement.Vars...)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := lockPublication(db.Session(&gorm.Session{DryRun: true}), scope); err != nil {
		t.Fatal(err)
	}
	wantSQL := "SELECT * FROM `document_code_archive` WHERE doc_id = ? AND kb_id = ? AND task_id = ? AND run_id = ? AND attempt_id = ? LIMIT ? FOR UPDATE"
	wantVars := []any{scope.DocID, scope.KBID, scope.TaskID, scope.RunID, scope.AttemptID, 1}
	if sql != wantSQL || !reflect.DeepEqual(vars, wantVars) {
		t.Fatalf("locked publication query: %s %#v", sql, vars)
	}
	if err := countOne(taskQuery(db.Session(&gorm.Session{DryRun: true}), scope)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("dry-run count: %v", err)
	}
	wantSQL = "SELECT count(*) FROM `ingestion_task` WHERE id = ? AND document_id = ? AND dataset_id = ? AND pipeline_log_id = ?"
	wantVars = []any{scope.TaskID, scope.DocID, scope.KBID, scope.RunID}
	if sql != wantSQL || !reflect.DeepEqual(vars, wantVars) {
		t.Fatalf("task count query: %s %#v", sql, vars)
	}
	for _, tc := range []struct {
		predicate string
		value     any
		where     string
		vars      []any
	}{
		{predicate: "doc_id IN ?", value: []string{scope.DocID, "other"}, where: "doc_id IN (?,?)", vars: []any{scope.DocID, "other"}},
		{predicate: "kb_id = ?", value: scope.KBID, where: "kb_id = ?", vars: []any{scope.KBID}},
	} {
		t.Run(tc.predicate, func(t *testing.T) {
			var statements []string
			var bindings [][]any
			capture := func(tx *gorm.DB) {
				statements = append(statements, tx.Statement.SQL.String())
				bindings = append(bindings, append([]any(nil), tx.Statement.Vars...))
			}
			if err := db.Callback().Update().After("gorm:update").Register("capture_metadata_update", capture); err != nil {
				t.Fatal(err)
			}
			defer db.Callback().Update().Remove("capture_metadata_update")
			if err := db.Callback().Delete().After("gorm:delete").Register("capture_metadata_delete", capture); err != nil {
				t.Fatal(err)
			}
			defer db.Callback().Delete().Remove("capture_metadata_delete")
			if err := deleteMetadata(db.Session(&gorm.Session{DryRun: true, SkipDefaultTransaction: true}), tc.predicate, tc.value); err != nil {
				t.Fatal(err)
			}
			want := []string{
				"UPDATE `archive_attempt` SET `lease_until`=?,`state`=?,`updated_at`=? WHERE " + tc.where,
				"DELETE FROM `archive_run` WHERE " + tc.where,
				"DELETE FROM `document_code_archive` WHERE " + tc.where,
			}
			if !reflect.DeepEqual(statements, want) {
				t.Fatalf("metadata SQL: %v, want %v", statements, want)
			}
			if len(bindings[0]) != len(tc.vars)+3 || bindings[0][0] != 0 || bindings[0][1] != "revoked" || !reflect.DeepEqual(bindings[0][3:], tc.vars) {
				t.Fatalf("attempt update bindings: %#v", bindings[0])
			}
			for _, got := range bindings[1:] {
				if !reflect.DeepEqual(got, tc.vars) {
					t.Fatalf("delete bindings: %#v, want %#v", got, tc.vars)
				}
			}
		})
	}
}

func TestPublicationReadLockOrderKeepsPublishRowUnlocked(t *testing.T) {
	for _, operation := range []string{"with-current", "terminate", "publish"} {
		t.Run(operation, func(t *testing.T) {
			db, scope := publicationDB(t)
			current, err := Acquire(t.Context(), db, scope, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			type read struct {
				table  string
				locked bool
			}
			var reads []read
			if err := db.Callback().Query().Before("gorm:query").Register("record_publication_locks", func(tx *gorm.DB) {
				_, locked := tx.Statement.Clauses["FOR"]
				reads = append(reads, read{table: tx.Statement.Table, locked: locked})
			}); err != nil {
				t.Fatal(err)
			}
			want := []read{{"code_archive_dataset", true}, {"document", true}, {"ingestion_task", true}, {"document_code_archive", operation != "publish"}}
			switch operation {
			case "with-current":
				err = WithCurrent(t.Context(), db, current, func(*gorm.DB) error { return nil })
			case "terminate":
				err = Terminate(t.Context(), db, current, common.FAILED, nil)
			case "publish":
				want = append(want, read{"knowledgebase", true})
				called := false
				err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}, func(context.Context, bool) error {
					called = true
					if !reflect.DeepEqual(reads, want) {
						t.Errorf("read locks before synchronization=%v, want %v", reads, want)
					}
					return nil
				})
				if !called {
					t.Fatal("availability synchronization not called")
				}
			}
			if err != nil || !reflect.DeepEqual(reads, want) {
				t.Fatalf("publication read locks=%v want=%v error=%v", reads, want, err)
			}
		})
	}
}

func TestWithCurrentPreservesLeaseAndTerminalRules(t *testing.T) {
	for _, state := range []string{"pending", "running", "expired", "ready", "failed", "deleting"} {
		t.Run(state, func(t *testing.T) {
			db, scope := publicationDB(t)
			if state != "pending" {
				var err error
				scope, err = Acquire(t.Context(), db, scope, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "expired":
				if err := rowQuery(db, scope).Update("lease_until", time.Now().Add(-time.Minute).UnixMilli()).Error; err != nil {
					t.Fatal(err)
				}
			case "ready":
				if err := Publish(t.Context(), db, scope, artifact(), Counts{Chunks: 1}); err != nil {
					t.Fatal(err)
				}
			case "failed":
				if err := Terminate(t.Context(), db, scope, common.FAILED, nil); err != nil {
					t.Fatal(err)
				}
			case "deleting":
				if err := RevokeDataset(t.Context(), db, scope.KBID); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err := WithCurrent(t.Context(), db, scope, func(tx *gorm.DB) error {
				called = true
				return tx.Model(&entity.Document{}).Where("id = ?", scope.DocID).Update("process_duration", 7).Error
			})
			blocked := state == "expired" || state == "deleting"
			if blocked {
				if !errors.Is(err, ErrSuperseded) || called {
					t.Fatalf("invalid log write: called=%t error=%v", called, err)
				}
			} else if err != nil || !called {
				t.Fatalf("current log write rejected: called=%t error=%v", called, err)
			}
		})
	}
}

func TestLockedPublicationRejectsMissingOrMismatchedTuple(t *testing.T) {
	for _, operation := range []string{"with-current", "terminate"} {
		for _, field := range []string{"missing", "kb_id", "task_id", "run_id", "attempt_id"} {
			t.Run(operation+"/"+field, func(t *testing.T) {
				db, scope := publicationDB(t)
				var err error
				if field == "missing" {
					err = db.Where("doc_id = ?", scope.DocID).Delete(&entity.DocumentCodeArchive{}).Error
				} else {
					err = rowQuery(db, scope).Update(field, "replacement").Error
				}
				if err != nil {
					t.Fatal(err)
				}
				if operation == "with-current" {
					err = WithCurrent(t.Context(), db, scope, func(*gorm.DB) error {
						t.Fatal("wrote to missing or mismatched publication")
						return nil
					})
				} else {
					err = Terminate(t.Context(), db, scope, common.FAILED, nil)
				}
				if !errors.Is(err, ErrSuperseded) {
					t.Fatalf("missing or mismatched publication: %v", err)
				}
				var task entity.IngestionTask
				if err := db.Take(&task, "id = ?", scope.TaskID).Error; err != nil || task.Status != common.RUNNING {
					t.Fatalf("rejected operation changed task: %+v %v", task, err)
				}
			})
		}
	}
}

func TestPublicationIsAtomicAndRejectsStaleAttempt(t *testing.T) {
	db, scope := publicationDB(t)
	first, err := Acquire(t.Context(), db, scope, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Ready(t.Context(), db, "kb", "doc"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("pending visible: %v", err)
	}
	if _, err = Acquire(t.Context(), db, scope, time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("duplicate acquired: %v", err)
	}
	db.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ?", "doc").Update("lease_until", time.Now().Add(-time.Second).UnixMilli())
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, first, artifact(), Counts{Chunks: 1}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale publish: %v", err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 2, Tokens: 9, Duration: 3}); err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 2, Tokens: 9}); err != nil {
		t.Fatalf("idempotent publish: %v", err)
	}
	view, err := Ready(t.Context(), db, "kb", "doc")
	if err != nil || view.AttemptID != current.AttemptID || len(view.Tree) != 1 || len(view.Stats.FilteredEntries) != 1 || view.Stats.FilteredEntries[0] != artifact().Stats.FilteredEntries[0] {
		t.Fatalf("view: %+v %v", view, err)
	}
	var doc entity.Document
	db.Take(&doc, "id = ?", "doc")
	if doc.ChunkNum != 2 || doc.TokenNum != 9 || doc.Progress != 1 {
		t.Fatalf("counts: %+v", doc)
	}
	var kb entity.Knowledgebase
	db.Take(&kb, "id = ?", "kb")
	if kb.ChunkNum != 2 || kb.TokenNum != 9 {
		t.Fatalf("aggregate: %+v", kb)
	}
	allowed, err := CanCompensate(t.Context(), db, current, nil)
	if err != nil || allowed {
		t.Fatalf("published cleanup allowed: %v %v", allowed, err)
	}
	if err = Terminate(t.Context(), db, current, common.FAILED, nil); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("published failure accepted: %v", err)
	}
	if err = Terminate(t.Context(), db, first, common.FAILED, nil); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale failure: %v", err)
	}
}

func TestRunReplacementAndCancellationStayInvisible(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = Terminate(t.Context(), db, current, common.STOPPED, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = Ready(t.Context(), db, "kb", "doc"); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	scope.RunID = "replacement"
	db.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Updates(map[string]any{"pipeline_log_id": scope.RunID, "status": common.RUNNING})
	if err = Queue(t.Context(), db, scope); err != nil {
		t.Fatal(err)
	}
	if err = Progress(t.Context(), db, current, map[string]any{"progress": .9}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale progress: %v", err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale run published: %v", err)
	}
	if err = DeleteDocuments(t.Context(), db, []string{scope.DocID}); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&entity.DocumentCodeArchive{}).Count(&count)
	if count != 0 {
		t.Fatalf("rows after deletion: %d", count)
	}
}

func TestStopRequestWinsOverFailureDuringCompensation(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&entity.IngestionTask{}).Where("id = ?", scope.TaskID).Update("status", common.STOPPING).Error; err != nil {
		t.Fatal(err)
	}
	if allowed, err := CanCompensate(t.Context(), db, current, errors.New("provider failure after stop")); err != nil || !allowed {
		t.Fatalf("stop compensation: %t %v", allowed, err)
	}
	if err = Terminate(t.Context(), db, current, common.STOPPED, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := Metadata(t.Context(), db, []string{scope.DocID})
	if err != nil || rows[scope.DocID].Status != "cancelled" || rows[scope.DocID].FailureReason != "" {
		t.Fatalf("stop lost to failure: %+v %v", rows, err)
	}
	if allowed, err := CanCompensate(t.Context(), db, current, context.Canceled); err != nil || !allowed {
		t.Fatalf("stopped generation cleanup: %t %v", allowed, err)
	}
	if _, err = Acquire(t.Context(), db, scope, time.Minute); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stopped run reacquired: %v", err)
	}
}

func TestEmptyPublicationCannotCompleteTask(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, current, archiveartifact.Artifact{}, Counts{}); err == nil {
		t.Fatal("empty publication accepted")
	}
	var task entity.IngestionTask
	db.Take(&task, "id = ?", scope.TaskID)
	if task.Status != common.RUNNING {
		t.Fatal(task.Status)
	}
}

func TestCompensationAndPublishCannotBothWin(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var publishErr, cleanupErr error
	var cleanupAllowed bool
	go func() {
		defer wg.Done()
		<-start
		publishErr = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1})
	}()
	go func() {
		defer wg.Done()
		<-start
		cleanupAllowed, cleanupErr = CanCompensate(t.Context(), db, current, nil)
	}()
	close(start)
	wg.Wait()
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if publishErr == nil && cleanupAllowed {
		t.Fatal("published generation authorized for deletion")
	}
	if publishErr != nil && !errors.Is(publishErr, ErrSuperseded) {
		t.Fatal(publishErr)
	}
	if cleanupAllowed {
		if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); !errors.Is(err, ErrSuperseded) {
			t.Fatalf("revoked generation published: %v", err)
		}
	}
}

func TestPublicationRollsBackIfCounterOwnerMissing(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Delete(&entity.Knowledgebase{}, "id = ?", scope.KBID).Error; err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err == nil {
		t.Fatal("publication without KB accepted")
	}
	var task entity.IngestionTask
	db.Take(&task, "id = ?", scope.TaskID)
	if task.Status != common.RUNNING {
		t.Fatalf("partial completion: %s", task.Status)
	}
	if _, err = Ready(t.Context(), db, scope.KBID, scope.DocID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("partial tree: %v", err)
	}
}

func TestPublicationRollsBackTreeAndCompletionOnDocumentWriteFailure(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("CREATE TRIGGER reject_archive_completion BEFORE UPDATE ON document WHEN NEW.progress = 1 BEGIN SELECT RAISE(ABORT, 'document write rejected'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err == nil {
		t.Fatal("expected transactional write failure")
	}
	var task entity.IngestionTask
	if err = db.Take(&task, "id = ?", scope.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != common.RUNNING {
		t.Fatalf("partial completion: %s", task.Status)
	}
	var row entity.DocumentCodeArchive
	if err = db.Take(&row, "doc_id = ?", scope.DocID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "running" || row.TreeJSON != "" {
		t.Fatalf("partial tree: %+v", row)
	}
}

func TestChangedEmbeddingConfigurationCannotPublish(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&entity.Knowledgebase{}).Where("id = ?", scope.KBID).Update("embd_id", "replacement-model").Error; err != nil {
		t.Fatal(err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err == nil {
		t.Fatal("published vectors from an obsolete embedding configuration")
	}
	if _, err = Ready(t.Context(), db, scope.KBID, scope.DocID); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}

func TestAvailabilitySyncUsesLockedCurrentStatusBeforePublishing(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(2)
	if err = db.Model(&entity.Document{}).Where("id = ?", scope.DocID).Update("status", "0").Error; err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("index synchronization failed")
	called := false
	syncAvailability := func(ctx context.Context, enabled bool) error {
		called = true
		if enabled {
			t.Error("publication used stale enabled status")
		}
		if _, err := Ready(ctx, db, scope.KBID, scope.DocID); !errors.Is(err, ErrNotReady) {
			t.Errorf("tree visible before availability sync: %v", err)
		}
		return wantErr
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}, syncAvailability); !errors.Is(err, wantErr) || !called {
		t.Fatalf("sync callback: %v called=%v", err, called)
	}
	var task entity.IngestionTask
	db.Take(&task, "id = ?", scope.TaskID)
	if task.Status != common.RUNNING {
		t.Fatal("sync failure completed task")
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}, func(context.Context, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestPendingTerminationCannotKillClaimedWorker(t *testing.T) {
	db, scope := publicationDB(t)
	current, err := Acquire(t.Context(), db, scope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = Terminate(t.Context(), db, scope, common.FAILED, nil); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("late enqueue failure killed worker: %v", err)
	}
	if err = Publish(t.Context(), db, current, artifact(), Counts{Chunks: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = Ready(t.Context(), db, "other-kb", scope.DocID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("cross-KB tree: %v", err)
	}
}

func TestVisibilityIncludesUnstartedArchiveButNotOrdinaryDocument(t *testing.T) {
	db, scope := publicationDB(t)
	if err := db.Create(&entity.Document{ID: "never-queued", KbID: scope.KBID, ParserID: codearchive.ParserID, ParserConfig: entity.JSONMap{}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&entity.Document{ID: "ordinary", KbID: scope.KBID, ParserID: "general", ParserConfig: entity.JSONMap{}}).Error; err != nil {
		t.Fatal(err)
	}
	generations, err := VisibleGenerations(t.Context(), db, []string{scope.KBID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 2 || generations[scope.DocID].RunID != "" {
		t.Fatalf("generations: %+v", generations)
	}
	if _, ok := generations["ordinary"]; ok {
		t.Fatal("ordinary marked archive")
	}
	if err = DeleteDataset(t.Context(), db, scope.KBID); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&entity.DocumentCodeArchive{}).Count(&count)
	if count != 0 {
		t.Fatal(count)
	}
}
