package evidenceplan

import (
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestRenderPreservesOrkaPrompt(t *testing.T) {
	plan := []skills.PlannedSkill{{
		ID: "providerid", Name: "Provider initialization", Procedure: "Compare Machine and Node.",
		RequiredEvidence: []skills.PlannedEvidenceGroup{
			{ID: "machine-state", Description: "Machine state", CandidatePaths: []string{"artifacts/machine.yaml"}},
			{ID: "node-state", Description: "Node state"},
		},
	}}
	got, complete := Render(plan, ScanStatus{Truncated: true}, Orka)
	if complete {
		t.Fatal("truncated plan was marked complete")
	}
	want := `## Required evidence plan

The dashboard matched these diagnostic recipes from the failure signal. Before broad searches or submit_analysis, read at least one candidate path from every listed evidence group. Keep every returned evidence_token. Use required_evidence if the diagnosis changes or a group has no candidate.
The candidate scan was truncated, so missing candidates may still exist deeper in the artifact tree.

### Provider initialization (` + "`providerid`" + `)
Procedure (diagnostic guidance only):
Compare Machine and Node.
Required evidence:
- machine-state: Machine state
  Candidate paths:
  - artifacts/machine.yaml
- node-state: Node state
  Candidate paths: none found in the bounded tree; use required_evidence and list the relevant subtree.`
	if got != want {
		t.Fatalf("Orka prompt changed:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderBackendsSharePlanStructure(t *testing.T) {
	plan := []skills.PlannedSkill{{
		ID: "quota", Procedure: "Inspect quota events.",
		RequiredEvidence: []skills.PlannedEvidenceGroup{{
			ID: "events", Description: "Quota events", CandidatePaths: []string{"artifacts/events.log"},
		}},
	}}
	inProcess, inProcessComplete := Render(plan, ScanStatus{}, InProcess)
	orka, orkaComplete := Render(plan, ScanStatus{}, Orka)
	if !inProcessComplete || !orkaComplete {
		t.Fatalf("complete plan = in-process %t, Orka %t", inProcessComplete, orkaComplete)
	}
	for _, want := range []string{"Required evidence plan", "quota", "Inspect quota events", "events", "artifacts/events.log"} {
		if !strings.Contains(inProcess, want) || !strings.Contains(orka, want) {
			t.Errorf("shared plan structure missing %q\nin-process:\n%s\nOrka:\n%s", want, inProcess, orka)
		}
	}
	for _, forbidden := range []string{"submit_analysis", "evidence_token", "required_evidence"} {
		if strings.Contains(inProcess, forbidden) {
			t.Errorf("in-process plan contains Orka contract term %q: %s", forbidden, inProcess)
		}
	}
}

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
			prompt, got := Render(tc.plan, tc.scan, InProcess)
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
