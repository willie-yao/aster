package evidenceplan

import (
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestRenderCompleteness(t *testing.T) {
	completePlan := []skills.PlannedSkill{{
		ID:               "complete",
		RequiredEvidence: []skills.PlannedEvidenceGroup{{ID: "logs", CandidatePaths: []string{"logs/failure.log"}}},
	}}
	procedureOnly := []skills.PlannedSkill{{ID: "conditional", Procedure: "Inspect the matching subtype."}}
	missingCandidate := []skills.PlannedSkill{{
		ID:               "missing",
		RequiredEvidence: []skills.PlannedEvidenceGroup{{ID: "logs"}},
	}}
	cases := []struct {
		name   string
		plan   []skills.PlannedSkill
		scan   ScanStatus
		want   bool
		marker string
	}{
		{name: "complete", plan: completePlan, want: true},
		{name: "missing candidate", plan: missingCandidate, want: false, marker: "none found"},
		{name: "truncated", plan: completePlan, scan: ScanStatus{Truncated: true}, want: false, marker: "scan was truncated"},
		{name: "failed with no applicable groups", plan: procedureOnly, scan: ScanStatus{Failed: true}, want: false, marker: "scan failed"},
		{name: "unavailable with no applicable groups", plan: procedureOnly, scan: ScanStatus{Unavailable: true}, want: false, marker: "scan was unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt, got := Render(tc.plan, tc.scan)
			if got != tc.want {
				t.Fatalf("complete = %t, want %t: %s", got, tc.want, prompt)
			}
			if tc.marker != "" && !strings.Contains(prompt, tc.marker) {
				t.Errorf("prompt missing %q: %s", tc.marker, prompt)
			}
		})
	}
}

func TestFailureSignalIsBoundedAndInstructionFree(t *testing.T) {
	body := strings.Repeat("b", FailureBodyBytes+100) + "body-tail"
	signal := FailureSignal(models.TestCase{
		Name: "worker node timeout", FailureLocation: "test/e2e.go:42", JUnitFile: "junit.xml",
		FailureMessage: "MachineDeployment did not create a node", FailureBody: body,
	})
	for _, want := range []string{"worker node timeout", "test/e2e.go:42", "junit.xml", "MachineDeployment", "body-tail"} {
		if !strings.Contains(signal, want) {
			t.Errorf("failure signal missing %q: %s", want, signal)
		}
	}
	for _, forbidden := range []string{"classify transient", "Investigate the build", "CI test FAILED"} {
		if strings.Contains(signal, forbidden) {
			t.Errorf("failure signal contains backend instruction %q: %s", forbidden, signal)
		}
	}
}
