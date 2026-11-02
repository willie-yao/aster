package actions

import (
	"errors"
	"testing"

	"github.com/willie-yao/aster/backend/internal/actiondraft"
	fixpr "github.com/willie-yao/aster/backend/internal/fix/pr"
)

func TestValidatedPreviewEntryChecksAddedDiffContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		diff    string
		blocked bool
	}{
		{
			name: "unchanged strategy",
			diff: "diff --git a/crd.yaml b/crd.yaml\n--- a/crd.yaml\n+++ b/crd.yaml\n@@ -1,2 +1,2 @@\n   strategy: None\n-old: value\n+new: value\n",
		},
		{
			name: "removed suspicious text",
			diff: "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-Remove the conversion webhook before upgrade.\n+Update the controller timeout.\n",
		},
		{
			name:    "harmful addition",
			diff:    "diff --git a/crd.yaml b/crd.yaml\n--- a/crd.yaml\n+++ b/crd.yaml\n@@ -1 +1 @@\n-  strategy: Webhook\n+  strategy: None\n",
			blocked: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix := fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{
				Title: "Update controller timeout", Description: "Adjust the configured timeout.",
				Diff: tc.diff, Files: map[string]string{"crd.yaml": "updated"},
			})
			if err := actiondraft.ValidateTitleBody(fix.Title, fix.Description); err != nil {
				t.Fatalf("fixture title/body did not reach diff policy: %v", err)
			}
			if err := fix.ValidateExecutionVerification(); err != nil {
				t.Fatalf("fixture execution verification did not reach diff policy: %v", err)
			}
			preview, err := validatedPreviewEntry(&previewEntry{kind: gfKind, fix: fix})
			if tc.blocked {
				if !errors.Is(err, ErrPreviewRejected) || ReasonCodeOf(err) != ReasonUnsafeRemediation {
					t.Fatalf("unsafe added content error = %v", err)
				}
				return
			}
			if err != nil || preview.Diff != tc.diff {
				t.Fatalf("safe added content preview = %+v, error = %v", preview, err)
			}
		})
	}
}
