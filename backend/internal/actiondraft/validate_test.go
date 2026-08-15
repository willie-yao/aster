package actiondraft

import (
	"strings"
	"testing"
)

func TestValidateBodyRejectsObservedReasoning(t *testing.T) {
	body := `The user wants me to revise this issue.
So I need to preserve the structure.
Let me draft the replacement.
This looks good.

## What happened
The build failed.`
	if err := ValidateBody(body); err == nil {
		t.Fatal("reasoning was accepted")
	}
}

func TestValidateBodyAllowsOrdinaryReasoningPhrasesInDraftContent(t *testing.T) {
	body := "## What happened\nI need to restart the worker, and this looks good after the retry.\n"
	if err := ValidateBody(body); err != nil {
		t.Fatalf("ordinary draft content rejected: %v", err)
	}
	if err := ValidateBody("I need to restart the worker."); err != nil {
		t.Fatalf("single preamble signal rejected: %v", err)
	}
}

func TestValidateBodyRejectsReasoningInsideSection(t *testing.T) {
	body := "## What happened\nThe user wants me to revise this. I need to expose the plan. Let me draft it.\n"
	if err := ValidateBody(body); err == nil {
		t.Fatal("section reasoning was accepted")
	}
}

func TestValidateBodyRejectsRepeatedSections(t *testing.T) {
	body := "## Affected builds\n- 1\n\n## Affected builds\n- 2\n"
	if err := ValidateBody(body); err == nil {
		t.Fatal("repeated section was accepted")
	}
}

func TestValidateBodyRejectsRepeatedAffectedBuild(t *testing.T) {
	body := "## Affected builds\n- build 1\n- build 1\n"
	if err := ValidateBody(body); err == nil {
		t.Fatal("repeated affected build was accepted")
	}
}

func TestValidateBodyRejectsControlAndLimits(t *testing.T) {
	if err := ValidateBody("valid\x00body"); err == nil {
		t.Fatal("control character was accepted")
	}
	if err := ValidateBody(string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
	if err := ValidateBody(strings.Repeat("x", MaxBodyBytes+1)); err == nil {
		t.Fatal("oversized body was accepted")
	}
	if err := ValidateTitleBody(strings.Repeat("x", MaxTitleRunes+1), "body"); err == nil {
		t.Fatal("oversized title was accepted")
	}
	if err := ValidateTitleBody("The user wants me to draft this", "body"); err == nil {
		t.Fatal("reasoning title was accepted")
	}
}

func TestValidateTitleBodyAcceptsNormalDraft(t *testing.T) {
	body := "## What happened\nThe build failed.\n\n## Evidence\n- `build-log.txt:2494`"
	if err := ValidateTitleBody("etcd join failed", body); err != nil {
		t.Fatalf("valid draft rejected: %v", err)
	}
}
