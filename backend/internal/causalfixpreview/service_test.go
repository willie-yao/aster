package causalfixpreview

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationinvestigation"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeResolver struct {
	subject remediationinvestigation.ActionableSubject
	next    *remediationinvestigation.ActionableSubject
	err     error
	calls   int
}

func (r *fakeResolver) ResolveActionable(context.Context, remediationinvestigation.OperationRef) (remediationinvestigation.ActionableSubject, error) {
	r.calls++
	if r.next != nil && r.calls > 1 {
		return *r.next, r.err
	}
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

type fakeSource struct{ files map[string]string }

func (f fakeSource) ReadFile(_ context.Context, _ sourceinvestigation.Repository, path string) (string, error) {
	return f.files[path], nil
}
func (f fakeSource) ListFiles(context.Context, sourceinvestigation.Repository) ([]string, error) {
	return []string{"config/jobs/periodic.yaml"}, nil
}

func previewSubject() remediationinvestigation.ActionableSubject {
	repo := sourceinvestigation.Repository{Owner: "kubernetes", Name: "test-infra", Revision: strings.Repeat("a", 40)}
	return remediationinvestigation.ActionableSubject{ResultDigest: strings.Repeat("b", 64), EvidenceCatalogDigest: strings.Repeat("e", 64), Source: fakeSource{files: map[string]string{"config/jobs/periodic.yaml": "periodics:\n- name: periodic\n  spec:\n    containers:\n    - name: test\n"}}, Input: remediationinvestigation.FrozenInput{PatternHash: strings.Repeat("c", 64), CausalGroupHash: strings.Repeat("d", 64)}, Proposal: remediationinvestigation.ActionableProposal{
		TargetKind: remediationinvestigation.TargetSetJobEnvironment, Repository: repo,
		Target:           models.RemediationTarget{Repository: "kubernetes/test-infra", Revision: repo.Revision, Path: "config/jobs/periodic.yaml", Intent: models.RemediationIntentSetJobEnvironment, Job: "periodic", Container: "test", Name: "FLAG", Value: "enabled"},
		ExpectedBehavior: "call applyFix", EvidenceIDs: []string{"private-id"}, AllowedChangedPaths: []string{"config/jobs/periodic.yaml"}, AllowedValidationCommands: []remediationinvestigation.ValidationCommand{{Argv: []string{"go", "test", "./..."}, Timeout: "1m"}},
	}, Evidence: []remediationinvestigation.EvidenceRecord{{ID: "private-id", Kind: remediationinvestigation.EvidenceSource}}}
}
func newTestService(t *testing.T, resolver *fakeResolver, agent *fakeAgent, validator *fakeValidator) *Service {
	t.Helper()
	if agent.result.CommandResults == nil {
		agent.result.CommandResults = []engineruntime.CommandResult{
			{Argv: []string{"go", "test", "./..."}, ExitCode: 0, DurationMs: 1},
			{Argv: []string{"git", "diff", "--cached", "--check"}, ExitCode: 0, DurationMs: 1},
		}
	}
	s, err := New(resolver, Options{Runtime: agent, ApplyDiff: func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error) {
		return map[string]string{"config/jobs/periodic.yaml": "periodics:\n- name: periodic\n  spec:\n    containers:\n    - name: test\n      env:\n      - name: FLAG\n        value: enabled\n"}, "canonical diff", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestPreviewSuccessIsNonConfirmableAndIdempotent(t *testing.T) {
	r := &fakeResolver{subject: previewSubject()}
	a := &fakeAgent{result: engineruntime.GenerateResult{Files: map[string]string{"config/jobs/periodic.yaml": "periodics:\n- name: periodic\n  spec:\n    containers:\n    - name: test\n      env:\n      - name: FLAG\n        value: enabled\n"}, Diff: "agent diff"}}
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
	if a.calls != 1 || v.calls != 0 || first.Diff != "canonical diff" || second.Diff != first.Diff || a.spec.Repo.Token != "" || len(first.ChangedFiles) != 1 || len(first.Validations) != 2 {
		t.Fatalf("first=%+v calls=%d spec=%+v", first, a.calls, a.spec)
	}
	if !slices.Equal(first.Validations[len(first.Validations)-1].Argv, []string{"git", "diff", "--cached", "--check"}) {
		t.Fatalf("final validation = %+v", first.Validations)
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
		{"validation", func(_ *fakeResolver, a *fakeAgent, _ *fakeValidator, _ *Service) {
			a.result.CommandResults[0].ExitCode = 1
		}, ErrValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeResolver{subject: previewSubject()}
			a := &fakeAgent{result: engineruntime.GenerateResult{Files: map[string]string{"config/jobs/periodic.yaml": "periodics:\n- name: periodic\n  spec:\n    containers:\n    - name: test\n      env:\n      - name: FLAG\n        value: enabled\n"}, Diff: "diff"}}
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

func TestPreviewRejectsPostGenerationSubjectDrift(t *testing.T) {
	initial := previewSubject()
	changed := previewSubject()
	changed.Input.ProviderFingerprint = "changed-provider"
	resolver := &fakeResolver{subject: initial, next: &changed}
	agent := &fakeAgent{result: engineruntime.GenerateResult{Files: map[string]string{"config/jobs/periodic.yaml": "periodics:\n- name: periodic\n  spec:\n    containers:\n    - name: test\n      env:\n      - name: FLAG\n        value: enabled\n"}, Diff: "diff"}}
	validator := &fakeValidator{result: engineruntime.Result{ExitCode: 0}}
	service := newTestService(t, resolver, agent, validator)
	if _, err := service.Preview(t.Context(), remediationinvestigation.OperationRef{}, "alice", "id"); !errors.Is(err, ErrNotActionable) {
		t.Fatalf("err=%v", err)
	}
}

func TestSubjectDigestBindsProvenancePolicyAndEvidence(t *testing.T) {
	base := previewSubject()
	for name, edit := range map[string]func(*remediationinvestigation.ActionableSubject){
		"source": func(s *remediationinvestigation.ActionableSubject) {
			s.Input.InvestigationSource.Revision = strings.Repeat("f", 40)
		},
		"provider": func(s *remediationinvestigation.ActionableSubject) { s.Input.ProviderFingerprint = "provider-two" },
		"policy": func(s *remediationinvestigation.ActionableSubject) {
			s.Proposal.AllowedChangedPaths = []string{"other.yaml"}
		},
		"commands": func(s *remediationinvestigation.ActionableSubject) {
			s.Proposal.AllowedValidationCommands[0].Argv = []string{"go", "test", "./config/..."}
		},
		"evidence": func(s *remediationinvestigation.ActionableSubject) { s.EvidenceCatalogDigest = strings.Repeat("1", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			changed.Proposal.AllowedChangedPaths = slices.Clone(base.Proposal.AllowedChangedPaths)
			changed.Proposal.AllowedValidationCommands = cloneValidationCommands(base.Proposal.AllowedValidationCommands)
			edit(&changed)
			if subjectDigest(changed) == subjectDigest(base) {
				t.Fatal("digest did not change")
			}
		})
	}
}
func cloneValidationCommands(in []remediationinvestigation.ValidationCommand) []remediationinvestigation.ValidationCommand {
	out := make([]remediationinvestigation.ValidationCommand, len(in))
	copy(out, in)
	for i := range out {
		out[i].Argv = slices.Clone(in[i].Argv)
	}
	return out
}
