package orka_test

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

func TestAnalysisManifestIsNotPublished(t *testing.T) {
	for _, name := range output.NonPublishedFiles {
		if name == orka.AnalysisManifestFile {
			return
		}
	}
	t.Fatalf("%s is missing from output.NonPublishedFiles", orka.AnalysisManifestFile)
}
