package utility

import "testing"

func TestArchiveFilenameClassification(t *testing.T) {
	for _, name := range []string{"code.zip", "code.ZIP", "nested/code.Zip", " code.zip "} {
		if FilenameType(name) != FileTypeCODE || GetFileType(name) != FileTypeCODE || !IsArchiveFilename(name) {
			t.Fatalf("archive not classified: %s", name)
		}
	}
	for _, name := range []string{"code.zip.txt", "code.docx", "zip", "archive.tar.gz"} {
		if IsArchiveFilename(name) || FilenameType(name) == FileTypeCODE || GetFileType(name) == FileTypeCODE {
			t.Fatalf("ordinary file classified as archive: %s", name)
		}
	}
}
