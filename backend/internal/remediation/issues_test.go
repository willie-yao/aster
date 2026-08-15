package remediation

import (
	"context"
	"testing"
)

type fakeIssueLifecycle struct {
	comments []string
	closed   []int
	reopened []int
}

func (f *fakeIssueLifecycle) CommentIssue(_ context.Context, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}
func (f *fakeIssueLifecycle) CloseIssue(_ context.Context, number int) error {
	f.closed = append(f.closed, number)
	return nil
}
func (f *fakeIssueLifecycle) ReopenIssue(_ context.Context, number int) error {
	f.reopened = append(f.reopened, number)
	return nil
}

func TestReconcileIssueClosesVerifiedFix(t *testing.T) {
	client := &fakeIssueLifecycle{}
	remediation := &Remediation{Issue: &IssueRef{Number: 9}}
	attempt := &Attempt{Status: StatusVerifiedFixed, URL: "https://github.com/o/r/pull/7", LastTransition: "observing->verified_fixed"}
	if err := reconcileIssue(context.Background(), client, remediation, attempt); err != nil {
		t.Fatal(err)
	}
	if len(client.comments) != 1 || len(client.closed) != 1 || remediation.Issue.State != "closed" {
		t.Fatalf("comments=%v closed=%v issue=%+v", client.comments, client.closed, remediation.Issue)
	}
	if err := reconcileIssue(context.Background(), client, remediation, attempt); err != nil {
		t.Fatal(err)
	}
	if len(client.comments) != 1 {
		t.Fatal("duplicate transition comment")
	}
}

func TestReconcileLinkedIssuesWaitsForEveryFinding(t *testing.T) {
	client := &fakeIssueLifecycle{}
	state := NewState()
	state.Remediations["old"] = &Remediation{
		UpdatedAt: "2026-07-20T01:00:00Z", Issue: &IssueRef{Number: 9, Repo: "o/r"},
		Attempts: []Attempt{{Status: StatusVerifiedFixed, LastTransition: "observing->verified_fixed"}},
	}
	state.Remediations["new"] = &Remediation{
		UpdatedAt: "2026-07-20T02:00:00Z", Issue: &IssueRef{Number: 9, Repo: "o/r"},
		Attempts: []Attempt{{Status: StatusObserving, LastTransition: "merged->observing"}},
	}
	if err := reconcileLinkedIssues(context.Background(), client, "o/r", state, nil); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 0 {
		t.Fatalf("issue closed while a finding was pending: %v", client.closed)
	}
	state.Remediations["new"].Attempts[0].Status = StatusVerifiedFixed
	state.Remediations["new"].Attempts[0].LastTransition = "observing->verified_fixed"
	if err := reconcileLinkedIssues(context.Background(), client, "o/r", state, nil); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 1 {
		t.Fatalf("issue close calls = %v", client.closed)
	}
}

func TestReconcileLinkedIssuesClosedUnmergedBlocksClosure(t *testing.T) {
	client := &fakeIssueLifecycle{}
	state := NewState()
	state.Remediations["verified"] = &Remediation{
		Issue:    &IssueRef{Number: 9, Repo: "o/r"},
		Attempts: []Attempt{{Status: StatusVerifiedFixed, LastTransition: "observing->verified_fixed"}},
	}
	state.Remediations["closed"] = &Remediation{
		Issue:    &IssueRef{Number: 9, Repo: "o/r"},
		Attempts: []Attempt{{Status: StatusClosedUnmerged, LastTransition: "open->closed_unmerged"}},
	}
	if err := reconcileLinkedIssues(context.Background(), client, "o/r", state, nil); err != nil {
		t.Fatal(err)
	}
	if len(client.closed) != 0 {
		t.Fatalf("issue closed with unresolved remediation: %v", client.closed)
	}
}

func TestReconcileLinkedIssuesReopensForPendingFinding(t *testing.T) {
	client := &fakeIssueLifecycle{}
	state := NewState()
	state.Remediations["verified"] = &Remediation{
		Issue:    &IssueRef{Number: 9, Repo: "o/r", State: "closed"},
		Attempts: []Attempt{{Status: StatusVerifiedFixed, LastTransition: "observing->verified_fixed"}},
	}
	state.Remediations["pending"] = &Remediation{
		Issue:    &IssueRef{Number: 9, Repo: "o/r", State: "closed"},
		Attempts: []Attempt{{Status: StatusAwaitingPresubmit, LastTransition: "open->awaiting_presubmit"}},
	}
	if err := reconcileLinkedIssues(context.Background(), client, "o/r", state, nil); err != nil {
		t.Fatal(err)
	}
	if len(client.reopened) != 1 {
		t.Fatalf("reopen calls = %v", client.reopened)
	}
	for id, entry := range state.Remediations {
		if entry.Issue.State != "open" {
			t.Fatalf("%s issue = %+v", id, entry.Issue)
		}
	}
}

func TestReconcileLinkedIssuesKeepsOpenForUnremediatedPattern(t *testing.T) {
	client := &fakeIssueLifecycle{}
	state := NewState()
	state.Remediations["verified"] = &Remediation{
		JobID: "job", Issue: &IssueRef{Number: 9, Repo: "o/r", State: "closed"},
		Attempts: []Attempt{{Status: StatusVerifiedFixed, LastTransition: "observing->verified_fixed"}},
	}
	if err := reconcileLinkedIssues(context.Background(), client, "o/r", state, map[string]bool{"job": true}); err != nil {
		t.Fatal(err)
	}
	if len(client.reopened) != 1 || len(client.closed) != 0 || state.Remediations["verified"].Issue.State != "open" {
		t.Fatalf("reopened=%v closed=%v issue=%+v", client.reopened, client.closed, state.Remediations["verified"].Issue)
	}
}

func TestReconcileIssueReopensOnSameCauseRecurrence(t *testing.T) {
	client := &fakeIssueLifecycle{}
	remediation := &Remediation{Issue: &IssueRef{
		Number: 9, State: "closed", LastTransition: "observing->verified_fixed",
	}}
	attempt := &Attempt{
		Status: StatusStillFailingSameCause, URL: "https://github.com/o/r/pull/7",
		LastTransition: "verified_fixed->still_failing_same_cause",
	}
	if err := reconcileIssue(context.Background(), client, remediation, attempt); err != nil {
		t.Fatal(err)
	}
	if len(client.reopened) != 1 || len(client.comments) != 1 || remediation.Issue.State != "open" {
		t.Fatalf("reopened=%v comments=%v issue=%+v", client.reopened, client.comments, remediation.Issue)
	}
}
