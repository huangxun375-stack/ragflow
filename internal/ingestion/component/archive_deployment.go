package component

import (
	"ragflow/internal/parser/codearchive"
	"ragflow/internal/server"
)

// CheckCodeArchiveDeployment verifies the installed grammars before accepting
// archive jobs. It never installs or downloads runtime dependencies.
func CheckCodeArchiveDeployment(policy codearchive.Policy) error {
	if !server.SupportsCodeArchive() {
		return nil
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	if err := EnsureCodeArchiveGrammars(); err != nil {
		return err
	}
	return codearchive.CheckGrammars(policy)
}
