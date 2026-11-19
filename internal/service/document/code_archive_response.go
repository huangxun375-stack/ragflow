package document

import (
	"context"
	"errors"
	"fmt"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/permission"
	permissionresponse "ragflow/internal/permission/response"
	"ragflow/internal/service"
	"ragflow/internal/service/archivepublication"
)

type ArchiveMetadata struct {
	ArchiveStatus        string  `json:"archive_status"`
	ArchiveRunID         *string `json:"archive_run_id"`
	ArchiveFailureReason string  `json:"archive_failure_reason"`
}

func ArchiveMetadataFor(summary archivepublication.Summary) *ArchiveMetadata {
	status := summary.Status
	if status == "" {
		status = "unstart"
	}
	if status == "cancelled" {
		status = "stopped"
	}
	if status == "running" {
		status = "pending"
	}
	var runID *string
	if summary.RunID != "" {
		runID = &summary.RunID
	}
	return &ArchiveMetadata{ArchiveStatus: status, ArchiveRunID: runID, ArchiveFailureReason: summary.FailureReason}
}

func (m *ArchiveMetadata) AddTo(out map[string]any) {
	if m == nil {
		return
	}
	out["archive_status"], out["archive_run_id"], out["archive_failure_reason"] = m.ArchiveStatus, m.ArchiveRunID, m.ArchiveFailureReason
}

func archiveMetadataByDocuments(ctx context.Context, docs []*entity.Document) (map[string]archivepublication.Summary, error) {
	var ids []string
	for _, doc := range docs {
		if isArchiveDocument(doc) {
			ids = append(ids, doc.ID)
		}
	}
	return archivepublication.Metadata(ctx, dao.DB, ids)
}

type ArchiveTreeResponse struct {
	DocumentID string                 `json:"doc_id"`
	Status     string                 `json:"status"`
	RunID      *string                `json:"archive_run_id"`
	RootName   string                 `json:"root_name,omitempty"`
	Tree       []codearchive.TreeNode `json:"tree"`
	Stats      *codearchive.Stats     `json:"stats,omitempty"`
}

var ErrDocumentNotArchive = &codearchive.Error{Code: "ZIP_NOT_ARCHIVE"}

// ReadArchiveTree checks document access and dataset membership before looking
// up a published generation. A non-ready generation never returns tree data.
func ReadArchiveTree(ctx context.Context, datasetID, documentID string) (*ArchiveTreeResponse, common.ErrorCode, error) {
	if err := service.CheckDocumentAccess(ctx, documentID, datasetID, permission.OperationRead); err != nil {
		code, hidden := permissionresponse.NormalizeHidden(err)
		return nil, code, hidden
	}
	doc, err := dao.NewDocumentDAO().GetByDocumentIDAndDatasetID(ctx, dao.DB, documentID, datasetID)
	if err != nil {
		return nil, common.CodeNotFound, fmt.Errorf("document not found")
	}
	if !isArchiveDocument(doc) {
		return nil, common.CodeDataError, ErrDocumentNotArchive
	}
	view, err := archivepublication.Ready(ctx, dao.DB, datasetID, documentID)
	if errors.Is(err, archivepublication.ErrNotReady) {
		return &ArchiveTreeResponse{DocumentID: documentID, Status: "not_ready", Tree: []codearchive.TreeNode{}}, common.CodeSuccess, nil
	}
	if err != nil {
		return nil, common.CodeServerError, err
	}
	return &ArchiveTreeResponse{DocumentID: documentID, Status: "ready", RunID: &view.RunID, RootName: view.RootName, Tree: view.Tree, Stats: &view.Stats}, common.CodeSuccess, nil
}
