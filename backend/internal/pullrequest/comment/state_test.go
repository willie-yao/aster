package comment

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/output"
)

func TestStateFilenameIsNotPublished(t *testing.T) {
	for _, name := range []string{StateFilename, StateFilename + ".lock"} {
		if output.IsPublicDataPath(name) {
			t.Fatalf("tracking state %q is public", name)
		}
	}
}
