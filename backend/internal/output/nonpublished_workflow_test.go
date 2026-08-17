package output

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Pages deploy strips private files with a hand-maintained shell list. That
// list silently drifts whenever NonPublishedFiles gains an entry, and the
// consequence is publishing operator-private state to a world-readable site, so
// the two are pinned together here.
func TestDeployWorkflowStripsEveryNonPublishedFile(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".github", "workflows", "reusable-deploy.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the deploy workflow: %v", err)
	}
	workflow := string(data)

	step := "Strip non-published data files"
	if !strings.Contains(workflow, step) {
		t.Fatalf("the deploy workflow no longer has a %q step; this guard needs updating", step)
	}
	for _, name := range NonPublishedFiles {
		if !strings.Contains(workflow, "/"+name) {
			t.Errorf("%s is in NonPublishedFiles but the deploy workflow never removes it, "+
				"so it would be published to the public site", name)
		}
	}
}
