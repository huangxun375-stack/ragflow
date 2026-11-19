package document

import (
	"errors"

	"ragflow/internal/common"
	"ragflow/internal/parser/codearchive"
)

// UploadFailure retains a per-file error until the HTTP boundary selects the
// public response. Err may contain internal details and is not a ZIP message.
type UploadFailure struct {
	Filename string
	Err      error
	Code     common.ErrorCode
	Archive  bool
}

func (f UploadFailure) Error() string {
	if f.Filename == "" {
		return f.Err.Error()
	}
	return f.Filename + ": " + f.Err.Error()
}

func (f UploadFailure) Unwrap() error { return f.Err }

func uploadFailure(filename string, err error) UploadFailure {
	code := common.CodeServerError
	var archiveErr *codearchive.Error
	if errors.As(err, &archiveErr) {
		code = common.CodeDataError
	}
	return UploadFailure{Filename: filename, Err: err, Code: code, Archive: codearchive.IsArchive(filename)}
}
