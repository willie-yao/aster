package causalfixpreview

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationinvestigation"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeResolver struct {
	subject remediationinvestigation.ActionableSubject
	err     error
	calls   int
}

func (r *fakeResolver) ResolveActionable(context.Context, remediationinvestigation.OperationRef) (remediationinvestigation.ActionableSubject, error) {
	r.calls++
	return r.subject, r.err
}

type fakeAgent struct {
	result engineruntime.GenerateResult
	err    error
	spec   engineruntime.GenerateSpec
	calls  int
}

func (a *fakeAgent) Generate(_ context.Context, spec engineruntime.GenerateSpec) (engineruntime.GenerateResult, error) {
	a.calls++
	a.spec = spec
	return a.result, a.err
}

type fakeValidator struct {
	result engineruntime.Result
	err    error
	calls  int
}

func (v *fakeValidator) Run(context.Context, engineruntime.Spec) (engineruntime.Result, error) {
	v.calls++
	return v.result, v.err
}

func previewSubject() remediationinvestigation.ActionableSubject {
	repo := sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}
	return remediationinvestigation.ActionableSubject{ResultDigest: strings.Repeat("b", 64), Input: remediationinvestigation.FrozenInput{PatternHash: strings.Repeat("c", 64), CausalGroupHash: strings.Repeat("d", 64)}, Proposal: remediationinvestigation.ActionableProposal{
		TargetKind: remediationinvestigation.TargetAddRequiredCall, Repository: repo,
		Target:           models.RemediationTarget{Repository: "example/repo", Revision: repo.Revision, Path: "controller.go", Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "applyFix"},
		ExpectedBehavior: "call applyFix", EvidenceIDs: []string{"private-id"}, AllowedChangedPaths: []string{"controller.go"}, AllowedValidationCommands: []remediationinvestigation.ValidationCommand{{Argv: []string{"go", "test", "./..."}, Timeout: "1m"}},
	}, Evidence: []remediationinvestigation.EvidenceRecord{{ID: "private-id", Kind: remediationinvestigation.EvidenceSource}}}
}
func newTestService(t *testing.T, resolver *fakeResolver, agent *fakeAgent, validator *fakeValidator) *Service {
	t.Helper()
	s, err := New(resolver, Options{Runtime: agent, Validator: validator, ApplyDiff: func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error) {
		return map[string]string{"controller.go": "new"}, "canonical diff", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestPreviewSuccessIsNonConfirmableAndIdempotent(t *testing.T) {
	r := &fakeResolver{subject: previewSubject()}
	a := &fakeAgent{result: engineruntime.GenerateResult{Files: map[string]string{"controller.go": "new"}, Diff: "agent diff"}}
	v := &fakeValidator{result: engineruntime.Result{ExitCode: 0}}
	s := newTestService(t, r, a, v)
	ref := remediationinvestigation.OperationRef{PatternHash: strings.Repeat("c", 64), CausalGroupHash: strings.Repeat("d", 64)}
	first, err := s.Preview(t.Context(), ref, "alice", "id")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Preview(t.Context(), ref, "alice", "id")
	if err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 || first.Diff != "canonical diff" || second.Diff != first.Diff || a.spec.Repo.Token != "" || len(first.ChangedFiles) != 1 {
		t.Fatalf("first=%+v calls=%d spec=%+v", first, a.calls, a.spec)
	}
	raw, _ := json.Marshal(first)
	for _, bad := range []string{"private-id", "confirm", "token", "branch"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("preview leaked %q: %s", bad, raw)
		}
	}
}
func TestPreviewFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		edit func(*fakeResolver, *fakeAgent, *fakeValidator, *Service)
		want error
	}{
		{"not actionable", func(r *fakeResolver, _ *fakeAgent, _ *fakeValidator, _ *Service) {
			r.err = remediationinvestigation.ErrOperationNotActionable
		}, ErrNotActionable},
		{"wrong path", func(_ *fakeResolver, _ *fakeAgent, _ *fakeValidator, s *Service) {
			s.opts.ApplyDiff = func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error) {
				return map[string]string{"other.go": "x"}, "d", nil
			}
		}, ErrRejected},
		{"mismatch", func(_ *fakeResolver, _ *fakeAgent, _ *fakeValidator, s *Service) {
			s.opts.ApplyDiff = func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error) {
				return map[string]string{"controller.go": "other"}, "d", nil
			}
		}, ErrRejected},
		{"validation", func(_ *fakeResolver, _ *fakeAgent, v *fakeValidator, _ *Service) {
			v.result = engineruntime.Result{ExitCode: 1}
		}, ErrValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeResolver{subject: previewSubject()}
			a := &fakeAgent{result: engineruntime.GenerateResult{Files: map[string]string{"controller.go": "new"}, Diff: "diff"}}
			v := &fakeValidator{result: engineruntime.Result{ExitCode: 0}}
			s := newTestService(t, r, a, v)
			tt.edit(r, a, v, s)
			_, err := s.Preview(t.Context(), remediationinvestigation.OperationRef{}, "alice", "id")
			if !errors.Is(err, tt.want) {
				t.Fatalf("err=%v want=%v", err, tt.want)
			}
		})
	}
}
