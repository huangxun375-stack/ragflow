package handler

import (
	"errors"
	"net/http"
	"strings"

	"ragflow/internal/common"
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/service/document"

	"github.com/gin-gonic/gin"
)

func respondArchiveError(c *gin.Context, code common.ErrorCode, err error) {
	reason := codearchive.Reason(err)
	common.Respond(c, http.StatusOK, code, gin.H{"reason_code": reason}, reason)
}

func (h *DocumentHandler) ArchiveTree(c *gin.Context) {
	datasetID, documentID := strings.TrimSpace(c.Param("dataset_id")), strings.TrimSpace(c.Param("document_id"))
	if datasetID == "" || documentID == "" {
		common.RespondError(c, common.CodeArgumentError, "dataset_id and document_id are required")
		return
	}
	data, code, err := document.ReadArchiveTree(c.Request.Context(), datasetID, documentID)
	if err != nil {
		if errors.Is(err, document.ErrDocumentNotArchive) {
			common.Respond(c, http.StatusOK, code, gin.H{"reason_code": "ZIP_NOT_ARCHIVE"}, err.Error())
			return
		}
		common.RespondError(c, code, err.Error())
		return
	}
	common.RespondSuccess(c, data, "success")
}
