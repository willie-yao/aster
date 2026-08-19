package prcomment

import (
	"slices"
	"testing"

	"github.com/willie-yao/aster/backend/internal/output"
)

// TestStateFilenameIsNotPublished pins the tracking file to the deny list that
// keeps operator state off the public site. output uses a string literal to
// avoid depending on this package, so the two are tied together here instead.
func TestStateFilenameIsNotPublished(t *testing.T) {
	if !slices.Contains(output.NonPublishedFiles, StateFilename) {
		t.Fatalf("%s is missing from output.NonPublishedFiles, so it would be published to the public site: %v",
			StateFilename, output.NonPublishedFiles)
	}
}
