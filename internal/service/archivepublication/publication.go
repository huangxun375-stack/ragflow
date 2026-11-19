// Package archivepublication owns archive publication and generation fencing.
// It deliberately depends on neither ingestion components nor parent services.
package archivepublication

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"ragflow/internal/common"
	"ragflow/internal/entity"
	"ragflow/internal/ingestion/archiveartifact"
	"ragflow/internal/parser/codearchive"
)

var (
	ErrNotReady   = errors.New("archive is not published")
	ErrSuperseded = errors.New("archive run or attempt is no longer current")
	ErrLeaseHeld  = errors.New("archive attempt lease is held")
)

const LeaseDuration = 30 * time.Second

type Generation struct {
	RunID     string `json:"run_id"`
	AttemptID string `json:"attempt_id"`
}
type Summary struct {
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
	RootName      string `json:"root_name,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	AttemptID     string `json:"attempt_id,omitempty"`
}
type View struct {
	Summary
	Tree  []codearchive.TreeNode `json:"tree"`
	Stats codearchive.Stats      `json:"stats"`
}
type Counts struct {
	Chunks, Tokens    int
	Duration          float64
	ValidateEmbedding func(context.Context, *gorm.DB) error
}

type AvailabilitySync func(context.Context, bool) error

// Queue is called in the transaction binding ingestion_task.pipeline_log_id,
// before publishing the broker wake-up. Repeated scheduling preserves a claim.
func Queue(ctx context.Context, tx *gorm.DB, s archiveartifact.Scope) error {
	return tx.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if s.DocID == "" || s.KBID == "" || s.TaskID == "" || s.RunID == "" {
			return ErrSuperseded
		}
		if err := LockDataset(ctx, tx, s.KBID); err != nil {
			return err
		}
		if _, _, err := lockCurrent(tx, s, false); err != nil {
			return err
		}
		if err := countOne(taskQuery(tx.WithContext(ctx), s)); err != nil {
			return err
		}
		var prior entity.DocumentCodeArchive
		err := tx.WithContext(ctx).Where("doc_id = ?", s.DocID).Take(&prior).Error
		if err == nil && prior.Status == "deleting" {
			return ErrSuperseded
		}
		if err == nil && prior.RunID == s.RunID && prior.TaskID == s.TaskID {
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		row := entity.DocumentCodeArchive{DocID: s.DocID, KBID: s.KBID, TaskID: s.TaskID, RunID: s.RunID, Status: "pending"}
		if err := tx.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "doc_id"}}, DoUpdates: clause.AssignmentColumns([]string{"kb_id", "task_id", "run_id", "attempt_id", "status", "failure_reason", "lease_until", "root_name", "tree_json", "stats_json"})}).Create(&row).Error; err != nil {
			return err
		}
		if prior.AttemptID != "" && prior.Status == "ready" {
			// Only the committed pointer replacement retires this generation. The
			// transaction rolls retirement back with a failed/rejected enqueue.
			return tx.Model(&entity.ArchiveAttempt{}).Where("attempt_id = ? AND run_id = ? AND doc_id = ? AND state = ?", prior.AttemptID, prior.RunID, prior.DocID, "published").Updates(map[string]any{"state": "retired", "lease_until": 0}).Error
		}
		return nil
	})
}

func taskQuery(db *gorm.DB, s archiveartifact.Scope) *gorm.DB {
	return db.Model(&entity.IngestionTask{}).Where("id = ? AND document_id = ? AND dataset_id = ? AND pipeline_log_id = ?", s.TaskID, s.DocID, s.KBID, s.RunID)
}
func rowQuery(db *gorm.DB, s archiveartifact.Scope) *gorm.DB {
	return db.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ? AND kb_id = ? AND task_id = ? AND run_id = ? AND attempt_id = ?", s.DocID, s.KBID, s.TaskID, s.RunID, s.AttemptID)
}
func one(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrSuperseded
	}
	return nil
}

func countOne(query *gorm.DB) error {
	var n int64
	if err := query.Count(&n).Error; err != nil {
		return err
	}
	if n != 1 {
		return ErrSuperseded
	}
	return nil
}

// lockCurrent serializes publication with queue/reset/delete and validates the
// worker's original run identity. All callers lock document, task, then artifact.
func lockCurrent(tx *gorm.DB, s archiveartifact.Scope, allowDeleting bool) (entity.Document, entity.IngestionTask, error) {
	var doc entity.Document
	var task entity.IngestionTask
	if err := lockDataset(tx.Statement.Context, tx, s.KBID, allowDeleting); err != nil {
		return doc, task, err
	}
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND kb_id = ?", s.DocID, s.KBID).Take(&doc).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = ErrSuperseded
		}
		return doc, task, err
	}
	if err := taskQuery(tx, s).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = ErrSuperseded
		}
		return doc, task, err
	}
	return doc, task, nil
}

func Acquire(ctx context.Context, db *gorm.DB, s archiveartifact.Scope, lease time.Duration) (archiveartifact.Scope, error) {
	attempt := common.GenerateUUID()
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, task, err := lockCurrent(tx, s, false)
		if err != nil {
			return err
		}
		if task.Status != common.RUNNING {
			return ErrSuperseded
		}
		var row entity.DocumentCodeArchive
		if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("doc_id = ? AND run_id = ? AND task_id = ?", s.DocID, s.RunID, s.TaskID).Take(&row).Error; err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		if row.Status == "running" && row.LeaseUntil > now {
			return ErrLeaseHeld
		}
		if row.Status != "pending" && row.Status != "running" {
			return ErrSuperseded
		}
		until := time.Now().Add(lease).UnixMilli()
		if err := one(tx.Model(&entity.DocumentCodeArchive{}).Where("doc_id = ? AND run_id = ? AND attempt_id = ? AND status = ? AND lease_until = ?", s.DocID, s.RunID, row.AttemptID, row.Status, row.LeaseUntil).Updates(map[string]any{"attempt_id": attempt, "status": "running", "lease_until": until})); err != nil {
			return err
		}
		var kb entity.Knowledgebase
		if err := tx.Take(&kb, "id = ?", s.KBID).Error; err != nil {
			return err
		}
		return tx.Create(&entity.ArchiveAttempt{AttemptID: attempt, RunID: s.RunID, DocID: s.DocID, KBID: s.KBID, TaskID: s.TaskID, TenantID: kb.TenantID, State: "running", LeaseUntil: until}).Error
	})
	s.AttemptID = attempt
	return s, err
}

func Renew(ctx context.Context, db *gorm.DB, s archiveartifact.Scope) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		until := time.Now().Add(LeaseDuration).UnixMilli()
		if err := one(rowQuery(tx, s).Where("status = ? AND lease_until > ?", "running", time.Now().UnixMilli()).Update("lease_until", until)); err != nil {
			return err
		}
		return one(attemptQuery(tx, s).Where("state = ?", "running").Update("lease_until", until))
	})
}

// Fence adds an atomic current-generation predicate to a write. It also works
// for terminal run-log writes after Publish or Terminate has settled the row.
func Fence(q *gorm.DB, s archiveartifact.Scope) *gorm.DB {
	return q.Where("EXISTS (SELECT 1 FROM document_code_archive ap JOIN ingestion_task it ON it.id = ap.task_id AND it.pipeline_log_id = ap.run_id WHERE ap.doc_id = ? AND ap.kb_id = ? AND ap.task_id = ? AND ap.run_id = ? AND ap.attempt_id = ?)", s.DocID, s.KBID, s.TaskID, s.RunID, s.AttemptID)
}

func Completed(ctx context.Context, db *gorm.DB, s archiveartifact.Scope) error {
	return countOne(rowQuery(db.WithContext(ctx), s).Where("status = ?", "ready").Where("EXISTS (SELECT 1 FROM ingestion_task it WHERE it.id = ? AND it.pipeline_log_id = ? AND it.status = ?)", s.TaskID, s.RunID, common.COMPLETED))
}

func CheckActive(ctx context.Context, db *gorm.DB, s archiveartifact.Scope) error {
	return countOne(Fence(rowQuery(db.WithContext(ctx), s), s).Where("status = ? AND lease_until > ?", "running", time.Now().UnixMilli()).Where("EXISTS (SELECT 1 FROM ingestion_task it WHERE it.id = ? AND it.status = ?)", s.TaskID, common.RUNNING))
}

// WriteBatch checks both sides of the external write. A deletion can commit
// after the first check while the engine request is in flight; the second
// check forces that worker through compensation instead of publishing it.
func WriteBatch(ctx context.Context, db *gorm.DB, s archiveartifact.Scope, write func(context.Context) error) error {
	if err := CheckActive(ctx, db, s); err != nil {
		return err
	}
	if err := write(ctx); err != nil {
		return err
	}
	return CheckActive(ctx, db, s)
}

// lockPublication follows lockCurrent. Publish deliberately does not use it:
// lease renewal must remain possible during availability synchronization.
func lockPublication(tx *gorm.DB, s archiveartifact.Scope) (entity.DocumentCodeArchive, error) {
	var row entity.DocumentCodeArchive
	err := rowQuery(tx, s).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = ErrSuperseded
	}
	return row, err
}

// WithCurrent serializes side-channel log writes with attempt replacement.
// Terminal messages may be recorded after publication, but an expired running
// attempt may not keep appending progress to its replacement's run log.
func WithCurrent(ctx context.Context, db *gorm.DB, s archiveartifact.Scope, write func(*gorm.DB) error) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, _, err := lockCurrent(tx, s, false); err != nil {
			return err
		}
		row, err := lockPublication(tx, s)
		if err != nil {
			return err
		}
		if row.Status == "running" && row.LeaseUntil <= time.Now().UnixMilli() {
			return ErrSuperseded
		}
		return write(tx)
	})
}

// Progress fences even a final detached-context flush against replacement runs.
func Progress(ctx context.Context, db *gorm.DB, s archiveartifact.Scope, updates map[string]any) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, task, err := lockCurrent(tx, s, false)
		if err != nil {
			return err
		}
		if task.Status != common.RUNNING {
			return ErrSuperseded
		}
		if err = countOne(rowQuery(tx, s).Where("status = ? AND lease_until > ?", "running", time.Now().UnixMilli())); err != nil {
			return err
		}
		return one(tx.Model(&entity.Document{}).Where("id = ? AND kb_id = ?", s.DocID, s.KBID).Updates(updates))
	})
}

// Publish switches the tree and content generation together with task status
// and document counters. Repeating it after an ambiguous commit is harmless.
func Publish(ctx context.Context, db *gorm.DB, s archiveartifact.Scope, a archiveartifact.Artifact, c Counts, synchronize ...AvailabilitySync) error {
	if len(synchronize) > 1 {
		return errors.New("archive publication accepts one availability synchronization")
	}
	if len(a.Tree) == 0 || a.RootName == "" || a.Stats.AcceptedFiles <= 0 || c.Chunks <= 0 {
		return errors.New("archive publication requires a nonempty tree and chunks")
	}
	tree, err := json.Marshal(a.Tree)
	if err != nil {
		return err
	}
	stats, err := json.Marshal(a.Stats)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		doc, task, err := lockCurrent(tx, s, false)
		if err != nil {
			return err
		}
		var row entity.DocumentCodeArchive
		// Document/task locks exclude takeover and compensation. Leave this row
		// unlocked until the final UPDATE so lease renewal can continue during
		// the engine synchronization below.
		if err = rowQuery(tx, s).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSuperseded
			}
			return err
		}
		if row.Status == "ready" && task.Status == common.COMPLETED {
			return nil
		}
		if row.Status != "running" || row.LeaseUntil <= time.Now().UnixMilli() || task.Status != common.RUNNING {
			return ErrSuperseded
		}
		var kb entity.Knowledgebase
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "embd_id").Take(&kb, "id = ?", s.KBID).Error; err != nil {
			return err
		}
		if kb.EmbdID != a.EmbeddingModelID {
			return &codearchive.Error{Code: "ZIP_EMBEDDING_CHANGED"}
		}
		if c.ValidateEmbedding != nil {
			if err := c.ValidateEmbedding(ctx, tx); err != nil {
				return err
			}
		}
		if len(synchronize) == 1 && synchronize[0] != nil {
			enabled := doc.Status == nil || *doc.Status != "0"
			if err := synchronize[0](ctx, enabled); err != nil {
				return err
			}
		}
		if err = one(rowQuery(tx, s).Where("status = ? AND lease_until > ?", "running", time.Now().UnixMilli()).Updates(map[string]any{"status": "ready", "failure_reason": "", "tree_json": string(tree), "stats_json": string(stats), "root_name": a.RootName, "lease_until": 0})); err != nil {
			return err
		}
		if err = one(taskQuery(tx, s).Where("status = ?", common.RUNNING).Update("status", common.COMPLETED)); err != nil {
			return err
		}
		if err = one(attemptQuery(tx, s).Where("state = ?", "running").Updates(map[string]any{"state": "published", "lease_until": 0})); err != nil {
			return err
		}
		if err = one(tx.Model(&entity.Document{}).Where("id = ?", s.DocID).Updates(map[string]any{"chunk_num": c.Chunks, "token_num": c.Tokens, "progress": 1, "process_duration": c.Duration})); err != nil {
			return err
		}
		return tx.Model(&entity.Knowledgebase{}).Where("id = ? AND embd_id = ?", s.KBID, a.EmbeddingModelID).Updates(map[string]any{"chunk_num": gorm.Expr("chunk_num + ?", int64(c.Chunks)-doc.ChunkNum), "token_num": gorm.Expr("token_num + ?", int64(c.Tokens)-doc.TokenNum)}).Error
	})
}

// Terminate never rewrites a published attempt. Empty AttemptID only permits
// a pending run (enqueue failure or cancellation before any worker claim).
func Terminate(ctx context.Context, db *gorm.DB, s archiveartifact.Scope, status string, cause error) error {
	if status != common.FAILED && status != common.STOPPED {
		return errors.New("invalid archive terminal state")
	}
	reason := ""
	if status == common.FAILED {
		reason = codearchive.Reason(cause)
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, task, err := lockCurrent(tx, s, true)
		if err != nil {
			return err
		}
		row, err := lockPublication(tx, s)
		if err != nil {
			return err
		}
		if row.Status == "ready" && task.Status == common.COMPLETED {
			return ErrSuperseded
		}
		target := "failed"
		if status == common.STOPPED {
			target = "cancelled"
		}
		if row.Status == target && task.Status == status {
			// Compensation may revoke the attempt before the worker reports its
			// typed cause. Enrich only this same tuple's generic reason, once.
			if status == common.FAILED && (row.FailureReason == "" || row.FailureReason == "ZIP_PARSE_FAILED") && row.FailureReason != reason {
				return one(rowQuery(tx, s).Update("failure_reason", reason))
			}
			return nil
		}
		if row.Status != "running" && row.Status != "pending" {
			return ErrSuperseded
		}
		if task.Status != common.RUNNING && task.Status != common.STOPPING && task.Status != common.CREATED && task.Status != common.SCHEDULED && task.Status != status {
			return ErrSuperseded
		}
		if err = one(rowQuery(tx, s).Updates(map[string]any{"status": target, "failure_reason": reason, "lease_until": 0, "tree_json": "", "stats_json": "", "root_name": ""})); err != nil {
			return err
		}
		if err = taskQuery(tx, s).Update("status", status).Error; err != nil {
			return err
		}
		if s.AttemptID != "" {
			if err = attemptQuery(tx, s).Where("state <> ?", "published").Updates(map[string]any{"state": "revoked", "lease_until": 0}).Error; err != nil {
				return err
			}
		}
		return one(tx.Model(&entity.Document{}).Where("id = ?", s.DocID).Update("progress", -1))
	})
}

// CanCompensate fails closed on DB errors or a committed publication. Cancelled
// attempts release their lease without choosing a run terminal state. Index
// deletion must still include this exact immutable run and attempt predicate.
func CanCompensate(ctx context.Context, db *gorm.DB, s archiveartifact.Scope, cause error) (bool, error) {
	allowed := false
	var inventory entity.ArchiveAttempt
	if err := attemptQuery(db.WithContext(ctx), s).Take(&inventory).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	if inventory.State == "published" {
		return false, nil
	}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Do not recreate a fence after successful dataset deletion. If the
		// fence is absent, document/attempt checks below still fail closed on a
		// live published generation and allow only the exact stale tuple.
		var fence entity.ArchiveDatasetFence
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&fence, "kb_id = ?", s.KBID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var doc entity.Document
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", s.DocID).Take(&doc).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			allowed = true
			return nil
		}
		if err != nil {
			return err
		}
		var task entity.IngestionTask
		err = taskQuery(tx, s).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&task).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var row entity.DocumentCodeArchive
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("doc_id = ?", s.DocID).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			allowed = true
			return nil
		}
		if err != nil {
			return err
		}
		if row.RunID != s.RunID || row.AttemptID != s.AttemptID {
			allowed = true
			return nil
		}
		if row.Status == "ready" {
			return nil
		}
		// Revoke before deletion: a concurrent completion cannot publish after this
		// transaction releases its locks, even if a caller retries compensation.
		if row.Status == "running" || row.Status == "pending" {
			if errors.Is(cause, context.Canceled) || task.Status == common.STOPPING {
				// Release this attempt without choosing the run's terminal state.
				// The caller settles a user stop; shutdown remains claimable on redelivery.
				if err = one(rowQuery(tx, s).Updates(map[string]any{"status": "pending", "failure_reason": "", "lease_until": 0, "tree_json": "", "stats_json": "", "root_name": ""})); err != nil {
					return err
				}
				allowed = true
				return nil
			}
			if err = one(rowQuery(tx, s).Updates(map[string]any{"status": "failed", "failure_reason": codearchive.Reason(cause), "lease_until": 0})); err != nil {
				return err
			}
			if task.ID != "" && task.Status != common.COMPLETED && task.Status != common.STOPPED && task.Status != common.FAILED {
				if err = one(taskQuery(tx, s).Update("status", common.FAILED)); err != nil {
					return err
				}
				if err = one(tx.Model(&entity.Document{}).Where("id = ?", s.DocID).Update("progress", -1)); err != nil {
					return err
				}
			}
		}
		allowed = true
		return nil
	})
	if err == nil && allowed {
		err = attemptQuery(db.WithContext(ctx), s).Where("state <> ?", "published").Updates(map[string]any{"state": "cleaning", "lease_until": 0}).Error
	}
	return allowed, err
}

func DeleteDocuments(ctx context.Context, tx *gorm.DB, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return deleteMetadata(tx.WithContext(ctx), "doc_id IN ?", ids)
}
func DeleteDataset(ctx context.Context, tx *gorm.DB, kbID string) error {
	if err := deleteMetadata(tx.WithContext(ctx), "kb_id = ?", kbID); err != nil {
		return err
	}
	return tx.WithContext(ctx).Where("kb_id = ?", kbID).Delete(&entity.ArchiveDatasetFence{}).Error
}

// Retain revoked attempts for sweeping late writes after the metadata is gone.
func deleteMetadata(tx *gorm.DB, predicate string, value any) error {
	if err := tx.Model(&entity.ArchiveAttempt{}).Where(predicate, value).Updates(map[string]any{"state": "revoked", "lease_until": 0}).Error; err != nil {
		return err
	}
	if err := tx.Where(predicate, value).Delete(&entity.ArchiveRun{}).Error; err != nil {
		return err
	}
	return tx.Where(predicate, value).Delete(&entity.DocumentCodeArchive{}).Error
}

func readyQuery(ctx context.Context, db *gorm.DB) *gorm.DB {
	return db.WithContext(ctx).Table("document_code_archive AS ap").Joins("JOIN document AS d ON d.id = ap.doc_id AND d.kb_id = ap.kb_id").Joins("JOIN ingestion_task AS it ON it.id = ap.task_id AND it.document_id = ap.doc_id AND it.dataset_id = ap.kb_id AND it.pipeline_log_id = ap.run_id").Where("ap.status = ? AND it.status = ? AND d.parser_id = ?", "ready", common.COMPLETED, codearchive.ParserID)
}
func Ready(ctx context.Context, db *gorm.DB, kbID, docID string) (View, error) {
	var row entity.DocumentCodeArchive
	var view View
	if db == nil {
		return view, errors.New("archive publication database unavailable")
	}
	err := readyQuery(ctx, db).Where("ap.doc_id = ? AND ap.kb_id = ?", docID, kbID).Select("ap.*").Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return view, ErrNotReady
	}
	if err != nil {
		return view, err
	}
	view.Summary = Summary{Status: row.Status, RootName: row.RootName, RunID: row.RunID, AttemptID: row.AttemptID}
	if err = json.Unmarshal([]byte(row.TreeJSON), &view.Tree); err != nil {
		return View{}, err
	}
	err = json.Unmarshal([]byte(row.StatsJSON), &view.Stats)
	return view, err
}
func Metadata(ctx context.Context, db *gorm.DB, ids []string) (map[string]Summary, error) {
	out := make(map[string]Summary)
	if len(ids) == 0 {
		return out, nil
	}
	if db == nil {
		return nil, errors.New("archive publication database unavailable")
	}
	var rows []struct{ DocID, Status, RootName, RunID, AttemptID, TaskStatus, FailureReason string }
	err := db.WithContext(ctx).Table("document_code_archive AS ap").Joins("JOIN ingestion_task AS it ON it.id = ap.task_id AND it.pipeline_log_id = ap.run_id").Where("ap.doc_id IN ?", ids).Select("ap.doc_id, ap.status, ap.root_name, ap.run_id, ap.attempt_id, ap.failure_reason, it.status AS task_status").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		status := r.Status
		if r.TaskStatus == common.STOPPING || r.TaskStatus == common.STOPPED {
			status = "cancelled"
		}
		if r.TaskStatus == common.FAILED {
			status = "failed"
		}
		root := ""
		if status == "ready" && r.TaskStatus == common.COMPLETED {
			root = r.RootName
		} else if status == "ready" {
			status = "pending"
		}
		reason := ""
		if status == "failed" {
			reason = r.FailureReason
			if reason == "" {
				reason = "ZIP_PARSE_FAILED"
			}
		}
		out[r.DocID] = Summary{Status: status, RootName: root, RunID: r.RunID, AttemptID: r.AttemptID, FailureReason: reason}
	}
	return out, nil
}
func VisibleGenerations(ctx context.Context, db *gorm.DB, kbIDs, docIDs []string) (map[string]Generation, error) {
	return visibleGenerations(ctx, db, nil, kbIDs, docIDs)
}

func visibleGenerations(ctx context.Context, db *gorm.DB, tenantIDs, kbIDs, docIDs []string) (map[string]Generation, error) {
	out := make(map[string]Generation)
	// Retrieval tenant lists also accept comma-separated IDs when building indices.
	// Match that normalization, and reject blank tenant scopes instead of widening.
	normalizedTenants := make([]string, 0, len(tenantIDs))
	for _, raw := range tenantIDs {
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				normalizedTenants = append(normalizedTenants, id)
			}
		}
	}
	tenantIDs = normalizedTenants
	if len(tenantIDs) == 0 && len(kbIDs) == 0 && len(docIDs) == 0 {
		return nil, errors.New("archive visibility requires tenant, knowledgebase or document scope")
	}
	if db == nil {
		return nil, errors.New("archive publication database unavailable")
	}
	// Presence identifies an archive even when it has never been queued. A zero
	// generation is deliberately invisible to every content outlet.
	docs := db.WithContext(ctx).Model(&entity.Document{}).Where("parser_id = ?", codearchive.ParserID)
	if len(tenantIDs) > 0 {
		docs = docs.Where("kb_id IN (SELECT id FROM knowledgebase WHERE tenant_id IN ?)", tenantIDs)
	}
	if len(kbIDs) > 0 {
		docs = docs.Where("kb_id IN ?", kbIDs)
	}
	if len(docIDs) > 0 {
		docs = docs.Where("id IN ?", docIDs)
	}
	var archiveIDs []string
	if err := docs.Pluck("id", &archiveIDs).Error; err != nil {
		return nil, err
	}
	for _, id := range archiveIDs {
		out[id] = Generation{}
	}
	if len(archiveIDs) == 0 {
		return out, nil
	}
	q := readyQuery(ctx, db)
	if len(tenantIDs) > 0 {
		q = q.Joins("JOIN knowledgebase AS kb ON kb.id = ap.kb_id").Where("kb.tenant_id IN ?", tenantIDs)
	}
	if len(kbIDs) > 0 {
		q = q.Where("ap.kb_id IN ?", kbIDs)
	}
	if len(docIDs) > 0 {
		q = q.Where("ap.doc_id IN ?", docIDs)
	}
	var rows []struct{ DocID, RunID, AttemptID string }
	if err := q.Select("ap.doc_id, ap.run_id, ap.attempt_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.DocID] = Generation{RunID: r.RunID, AttemptID: r.AttemptID}
	}
	return out, nil
}
