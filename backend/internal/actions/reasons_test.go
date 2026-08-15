package actions

import "testing"

func TestReasonCodeContractIsStableAndComplete(t *testing.T) {
	required := map[string]bool{
		"recovered": true, "observing": true, "verified_fixed": true, "retained_stale": true,
		"non_systemic": true, "evidence_unavailable": true, "investigation_required": true,
		"contract_generation_failed": true, "unsafe_remediation": true, "already_present": true,
		"source_verification_inconclusive": true,
	}
	seen := map[string]bool{}
	for _, code := range ReasonCodes() {
		if seen[code] || ReasonMessage(ReasonCode(code)) == "This action is unavailable." {
			t.Fatalf("invalid reason code %q", code)
		}
		seen[code] = true
		delete(required, code)
	}
	if len(required) != 0 {
		t.Fatalf("missing required codes: %v", required)
	}
}
