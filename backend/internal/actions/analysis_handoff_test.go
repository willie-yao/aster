package actions

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

func testFixOrigin(identity AnalysisIdentity, analysis *models.AIAnalysis) analysischat.FixOrigin {
	ref := analysisFixTargetRef(identity)
	return analysischat.FixOrigin{Analysis: ref, FixTarget: ref, Original: analysischat.AnalysisSnapshot{
		GeneratedAt: analysis.GeneratedAt, RootCause: analysis.RootCause, Severity: analysis.Severity,
		SuggestedFix: analysis.SuggestedFix, RelevantFiles: analysis.RelevantFiles,
	}}
}

func analysisRequestTestService(t *testing.T) (*Service, models.PatternAnalysis) {
	t.Helper()
	dir := t.TempDir()
	detail := exactJUnitDetail()
	writeJobDetail(t, dir, models.JobDataFilename(detail.JobID), detail)
	service := NewService(exactAnalysisConfig(), dir, AIConfig{})
	service.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{base: ghpr.Base{Branch: "main", HeadSHA: analysisFixRevision, TreeSHA: "tree"}}
	return service, models.PatternAnalysis{}
}

type handoffChatRunner struct{}

func (handoffChatRunner) Reply(context.Context, analysischat.Turn) (analysischat.Reply, error) {
	return analysischat.Reply{
		Answer: "Investigate whether the terminal branch omits Ready.", Assessment: "explains",
		Unverified: true, UnverifiedReason: analysischat.UnverifiedMissing,
		EvidenceWarnings: []string{"The available artifacts do not establish the proposed cause."},
	}, nil
}

func capturedHandoff(t *testing.T, service *Service, now func() time.Time) (*analysischat.Service, AnalysisFixInput) {
	t.Helper()
	chat, err := analysischat.NewService(t.Context(), service.dataDir, handoffChatRunner{}, analysischat.Options{
		StateDir: filepath.Join(service.dataDir, ".chat"), SessionTTL: time.Minute, HistoryRetention: time.Minute, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := chat.ConfigureSourceRepository(exactAnalysisRequestInput().SourceRepository); err != nil {
		t.Fatal(err)
	}
	session, err := chat.Create(analysisFixTargetRef(exactIdentity()), "alice", "create-chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.Send(t.Context(), session.ID, "alice", "answer", "Explain the failure."); err != nil {
		t.Fatal(err)
	}
	candidate, err := chat.AnalysisFixCandidate(session.ID, "alice", "answer")
	if err != nil {
		t.Fatal(err)
	}
	input := exactAnalysisRequestInput()
	input.ChatSessionID, input.ChatRequestID, input.ChatResponseHash = candidate.SessionID, candidate.RequestID, candidate.ResponseHash
	input.Origin = analysischat.FixOrigin{Analysis: candidate.Analysis, Original: candidate.Original, FixTarget: candidate.FixTarget}
	input.AssistantAnswer = candidate.AssistantAnswer
	input.AssistantUnverified, input.AssistantUnverifiedReason = candidate.AssistantUnverified, candidate.AssistantUnverifiedReason
	input.ArtifactCitations = nil
	input.EvidenceWarnings = candidate.EvidenceWarnings
	return chat, input
}

func handoffTestPreview(t *testing.T, service *Service, input AnalysisFixInput, owner string, calls *atomic.Int32) (PreviewResult, error) {
	t.Helper()
	token, existing, acquired, err := service.previewStore.reserveIdempotent(owner, input.PreviewRequestHash, input.HandoffHash, time.Minute)
	if err != nil {
		return PreviewResult{}, err
	}
	if !acquired {
		preview, err := validatedPreviewEntry(existing)
		preview.Token = token
		return preview, err
	}
	calls.Add(1)
	subject, err := service.resolveAnalysisFixTarget(input)
	if err != nil {
		return PreviewResult{}, err
	}
	fix := fixpr.RestoreGeneratedFix(&fixpr.GeneratedFixSnapshot{
		Subject: input.Identity.TestName, Rationale: input.AssistantAnswer,
		Diff: "diff --git a/controller.go b/controller.go\n+return nil\n", Files: map[string]string{"controller.go": "package controllers\n"},
		Verify: fixpr.VerifyResult{Status: fixpr.VerifySkipped}, Title: "fix: investigate terminal state", Description: "Investigative patch.", Body: "Investigative patch.",
		Key: "fix-analysis::" + subject.ID, Base: ghpr.Base{Branch: input.SourceBranch, HeadSHA: input.GenerationBaseRevision, TreeSHA: "tree"}, RequireBaseCurrent: true,
	})
	fix.SetWarnings(analysisQualityWarnings(subject.Failure.AIAnalysis, input, subject.SourceHints))
	entry := &previewEntry{
		failureID: subject.ID, patternHash: subject.ContentHash, kind: gfKind, fix: fix,
		targetRepo: input.SourceRepository.Owner + "/" + input.SourceRepository.Name, targetConfig: input.TargetConfig,
		verificationVersion: sourceVerificationVersion, analysisBinding: &AnalysisPreviewBinding{Handoff: cloneAnalysisFixInput(input)},
	}
	if err := service.previewStore.completeIdempotent(owner, token, input.PreviewRequestHash, input.HandoffHash, entry); err != nil {
		return PreviewResult{}, err
	}
	preview, err := validatedPreviewEntry(entry)
	preview.Token = token
	return preview, err
}

func installHandoffTestGenerator(t *testing.T, service *Service, calls *atomic.Int32) {
	t.Helper()
	service.analysisRequestGenerator = func(_ context.Context, input AnalysisFixInput, owner, _, _ string) (PreviewResult, error) {
		return handoffTestPreview(t, service, input, owner, calls)
	}
}

func TestActionOwnedHandoffSurvivesChatLifecycle(t *testing.T) {
	for _, lifecycle := range []string{"delete-after-capture", "delete-pending", "expire-pending", "archive-pending"} {
		t.Run(lifecycle, func(t *testing.T) {
			service, _ := analysisRequestTestService(t)
			var clock atomic.Int64
			clock.Store(time.Now().Unix())
			chat, input := capturedHandoff(t, service, func() time.Time { return time.Unix(clock.Load(), 0) })
			if lifecycle == "delete-after-capture" {
				if err := chat.Delete(input.ChatSessionID, "bob"); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			service.analysisRequestGenerator = func(ctx context.Context, handoff AnalysisFixInput, owner, _, _ string) (PreviewResult, error) {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return PreviewResult{}, ctx.Err()
				}
				return handoffTestPreview(t, service, handoff, owner, &calls)
			}
			request, err := service.CreateAnalysisFixRequest(t.Context(), input, "alice", "write-token", "")
			if err != nil {
				t.Fatal(err)
			}
			<-started
			switch lifecycle {
			case "delete-pending":
				if err := chat.Delete(input.ChatSessionID, "bob"); err != nil {
					t.Fatal(err)
				}
			case "expire-pending":
				clock.Add(120)
			case "archive-pending":
				if err := chat.Archive(input.ChatSessionID, "bob"); err != nil {
					t.Fatal(err)
				}
			}
			chatView, err := chat.Get(input.ChatSessionID, "alice")
			if lifecycle == "archive-pending" {
				if err != nil || !chatView.Archived || !chatView.ReadOnly {
					t.Fatalf("archived chat = %+v, %v", chatView, err)
				}
			} else if !errors.Is(err, analysischat.ErrSessionNotFound) {
				t.Fatalf("chat error = %v", err)
			}
			close(release)
			ready := waitRequest(t, service, request.ID, "alice", RequestReady)
			if err := service.Wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
			reloaded.sourceRevisionClient = service.sourceRevisionClient
			view, found, err := reloaded.FindAnalysisFixRequest(input.ChatSessionID, input.ChatRequestID, "alice", "")
			if err != nil || !found || view.ID != request.ID || view.Preview.Token != ready.Preview.Token || calls.Load() != 1 {
				t.Fatalf("reconnect = %+v %t %v calls=%d", view, found, err, calls.Load())
			}
			record := reloaded.requests.Requests[request.ID]
			handoff := record.AnalysisFix
			if handoff == nil || !handoff.AssistantUnverified || handoff.AssistantUnverifiedReason != input.AssistantUnverifiedReason || len(handoff.ArtifactCitations) != 0 || !slices.Equal(handoff.EvidenceWarnings, input.EvidenceWarnings) {
				t.Fatalf("handoff = %+v", handoff)
			}
			if err := reloaded.validateAnalysisPreview(t.Context(), "alice", AnalysisPreviewBinding{Handoff: handoff}); err != nil {
				t.Fatalf("validation without chat: %v", err)
			}
			if _, err := reloaded.CancelRequest(t.Context(), request.ID, "alice"); err != nil {
				t.Fatal(err)
			}
			if _, err := reloaded.Confirm(t.Context(), ready.Preview.Token, "alice", "token"); !errors.Is(err, ErrPreviewNotFound) {
				t.Fatalf("cancelled preview confirmation = %v", err)
			}
		})
	}
}

func TestAnalysisFixAdmissionPersistenceFailureStartsNothing(t *testing.T) {
	service, _ := analysisRequestTestService(t)
	var calls atomic.Int32
	installHandoffTestGenerator(t, service, &calls)
	service.requestStateWriter = func(string, any) error { return errors.New("disk unavailable") }
	if _, err := service.CreateAnalysisFixRequest(t.Context(), exactAnalysisRequestInput(), "alice", "token", ""); err == nil {
		t.Fatal("admission succeeded")
	}
	if len(service.requests.Requests) != 0 || calls.Load() != 0 {
		t.Fatalf("requests=%d generation=%d", len(service.requests.Requests), calls.Load())
	}
	state, _, err := service.previewStore.load()
	if err != nil || len(state.Previews) != 0 {
		t.Fatalf("preview reservations=%+v err=%v", state, err)
	}
}

func TestAnalysisFixReadyPersistenceFailureRecoversWithoutGeneration(t *testing.T) {
	service, _ := analysisRequestTestService(t)
	var calls atomic.Int32
	installHandoffTestGenerator(t, service, &calls)
	service.requestStateWriter = func(path string, value any) error {
		state := value.(*actionRequestState)
		for _, request := range state.Requests {
			if request.Status == RequestReady {
				return errors.New("ready write failed")
			}
		}
		return statefile.WritePrivateJSONDurable(path, value)
	}
	input := exactAnalysisRequestInput()
	request, err := service.CreateAnalysisFixRequest(t.Context(), input, "alice", "token", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, service, request.ID, "alice", RequestReady)
	if err := service.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	reloaded.sourceRevisionClient = service.sourceRevisionClient
	if reloaded.requests.Requests[request.ID].Status != RequestFailed {
		t.Fatal("unfinished request did not fail closed")
	}
	installHandoffTestGenerator(t, reloaded, &calls)
	retry, err := reloaded.CreateAnalysisFixRequest(t.Context(), input, "alice", "token", "")
	if err != nil {
		t.Fatal(err)
	}
	waitRequest(t, reloaded, retry.ID, "alice", RequestReady)
	if err := reloaded.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("generation calls=%d", calls.Load())
	}
}

func TestAnalysisFixConcurrentAdmissionOwnsIndependentHandoffs(t *testing.T) {
	service, _ := analysisRequestTestService(t)
	var calls atomic.Int32
	installHandoffTestGenerator(t, service, &calls)
	type result struct {
		owner string
		view  ActionRequestView
		err   error
	}
	results := make(chan result, 16)
	for _, owner := range []string{"alice", "bob"} {
		for range 8 {
			go func() {
				view, err := service.CreateAnalysisFixRequest(t.Context(), exactAnalysisRequestInput(), owner, "token", "")
				results <- result{owner, view, err}
			}()
		}
	}
	ids := map[string]string{}
	for range 16 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if id := ids[result.owner]; id != "" && id != result.view.ID {
			t.Fatalf("duplicate request: %+v", result)
		}
		ids[result.owner] = result.view.ID
	}
	for owner, id := range ids {
		waitRequest(t, service, id, owner, RequestReady)
	}
	if err := service.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ids["alice"] == ids["bob"] || calls.Load() != 2 {
		t.Fatalf("ids=%v calls=%d", ids, calls.Load())
	}
	if service.requests.Requests[ids["alice"]].AnalysisFix == service.requests.Requests[ids["bob"]].AnalysisFix {
		t.Fatal("owners share mutable handoff")
	}
}

func causeHandoff(t *testing.T, service *Service) (models.JobDetail, AnalysisFixInput) {
	t.Helper()
	detail := exactJUnitDetail()
	detail.Runs[0].TestCases[0].AIAnalysis.Disposition = models.AnalysisDispositionPreliminary
	other := detail.Runs[0]
	other.BuildID = "124"
	detail.Runs = append(detail.Runs, other)
	pattern := models.PatternAnalysis{
		Subject: "terminal state", JobID: detail.JobID, GeneratedAt: "original publication", Confidence: "high",
		CausalGroups: []models.PatternCausalGroup{{
			Builds: []string{"123", "124"}, RootCause: "terminal branch omits Ready", Confidence: "high",
			Remediation: &models.PatternCausalGroupRemediation{BuildID: "123", SuggestedFix: "Investigate the terminal update."},
		}},
	}
	models.AssignPatternIdentity(&pattern)
	detail.PatternAnalyses = []models.PatternAnalysis{pattern}
	writeJobDetail(t, service.dataDir, models.JobDataFilename(detail.JobID), detail)
	group := pattern.CausalGroups[0]
	input := exactAnalysisRequestInput()
	input.Origin.Analysis = analysischat.AnalysisRef{
		Scope: analysischat.ScopeCause, JobID: detail.JobID, PatternID: pattern.ID, PatternHash: models.PatternHash(pattern),
		CausalGroupID: group.ID, CausalGroupHash: models.PatternCausalGroupHash(group),
	}
	input.Origin.Original = analysischat.AnalysisSnapshot{GeneratedAt: pattern.GeneratedAt, RootCause: group.RootCause, Severity: "High", SuggestedFix: group.Remediation.SuggestedFix}
	input, err := service.prepareAnalysisFix(t.Context(), input, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	return detail, input
}

func TestActionOwnedHandoffRechecksCauseBeyondRepresentativeJUnit(t *testing.T) {
	for _, change := range []struct {
		name            string
		mutate          func(*models.JobDetail)
		unchangedHashes bool
		accept          bool
	}{
		{name: "membership", mutate: func(d *models.JobDetail) { d.PatternAnalyses[0].CausalGroups[0].Builds = []string{"123"} }},
		{name: "missing member", mutate: func(d *models.JobDetail) { d.Runs = d.Runs[:1] }, unchangedHashes: true},
		{name: "root cause", mutate: func(d *models.JobDetail) { d.PatternAnalyses[0].CausalGroups[0].RootCause = "different cause" }},
		{name: "confidence", mutate: func(d *models.JobDetail) { d.PatternAnalyses[0].CausalGroups[0].Confidence = "low" }},
		{name: "remediation", mutate: func(d *models.JobDetail) {
			d.PatternAnalyses[0].CausalGroups[0].Remediation.SuggestedFix = "Investigate something else."
		}, unchangedHashes: true},
		{name: "timestamp only", mutate: func(d *models.JobDetail) { d.PatternAnalyses[0].GeneratedAt = "republished" }, unchangedHashes: true, accept: true},
	} {
		t.Run(change.name, func(t *testing.T) {
			service, _ := analysisRequestTestService(t)
			detail, input := causeHandoff(t, service)
			change.mutate(&detail)
			if models.TestAnalysisContentHash(detail.Runs[0].TestCases[0]) != input.AnalysisContentHash {
				t.Fatal("fixture changed the representative JUnit analysis")
			}
			if change.unchangedHashes && (models.PatternHash(detail.PatternAnalyses[0]) != input.Origin.Analysis.PatternHash || models.PatternCausalGroupHash(detail.PatternAnalyses[0].CausalGroups[0]) != input.Origin.Analysis.CausalGroupHash) {
				t.Fatal("fixture changed cause hashes")
			}
			writeJobDetail(t, service.dataDir, models.JobDataFilename(detail.JobID), detail)
			err := service.validateAnalysisPreview(t.Context(), "alice", AnalysisPreviewBinding{Handoff: &input})
			if change.accept && err != nil || !change.accept && !errors.Is(err, ErrPreviewTargetChanged) {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

func TestActionOwnedHandoffRejectsChangedPublicationAndSource(t *testing.T) {
	for _, change := range []string{"analysis", "repository", "branch", "generation base", "destination"} {
		t.Run(change, func(t *testing.T) {
			service, _ := analysisRequestTestService(t)
			input, err := service.prepareAnalysisFix(t.Context(), exactAnalysisRequestInput(), "alice", "")
			if err != nil {
				t.Fatal(err)
			}
			detail := exactJUnitDetail()
			switch change {
			case "analysis":
				detail.Runs[0].TestCases[0].AIAnalysis.SuggestedFix = "New investigation"
			case "repository":
				detail.Runs[0].RepoRefs = map[string]string{"other/repo": "main:" + analysisFixRevision}
			case "branch":
				detail.Runs[0].RepoRefs = map[string]string{"kubernetes-sigs/cluster-api-provider-azure": "release:" + analysisFixRevision}
			case "generation base":
				service.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{base: ghpr.Base{Branch: "main", HeadSHA: capzGenerationBaseRevision}, contains: true}
			case "destination":
				service.cfg.AI.FixPRs.Repo.Name = "other"
			}
			writeJobDetail(t, service.dataDir, models.JobDataFilename(detail.JobID), detail)
			if err := service.validateAnalysisPreview(t.Context(), "alice", AnalysisPreviewBinding{Handoff: &input}); !errors.Is(err, ErrPreviewTargetChanged) {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

func TestAnalysisFixRejectsStaleResultBeforeReady(t *testing.T) {
	service, _ := analysisRequestTestService(t)
	detail, input := causeHandoff(t, service)
	var calls atomic.Int32
	produced, release := make(chan struct{}), make(chan struct{})
	service.analysisRequestGenerator = func(ctx context.Context, input AnalysisFixInput, owner, _, _ string) (PreviewResult, error) {
		preview, err := handoffTestPreview(t, service, input, owner, &calls)
		close(produced)
		select {
		case <-release:
		case <-ctx.Done():
			return PreviewResult{}, ctx.Err()
		}
		return preview, err
	}
	request, err := service.CreateAnalysisFixRequest(t.Context(), input, "alice", "token", "")
	if err != nil {
		t.Fatal(err)
	}
	<-produced
	detail.PatternAnalyses[0].CausalGroups[0].Remediation.SuggestedFix = "Different proposal"
	writeJobDetail(t, service.dataDir, models.JobDataFilename(detail.JobID), detail)
	close(release)
	view := waitRequest(t, service, request.ID, "alice", RequestFailed)
	if view.Preview != nil {
		t.Fatal("stale result was made ready")
	}
	if _, err := service.Confirm(t.Context(), idempotentPreviewToken("alice", input.PreviewRequestHash), "alice", "token"); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("stale preview confirmation = %v", err)
	}
	if err := service.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAnalysisFixHandoffRejectsTamperingAndIncompleteState(t *testing.T) {
	service, _ := analysisRequestTestService(t)
	input, err := service.prepareAnalysisFix(t.Context(), exactAnalysisRequestInput(), "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*AnalysisFixInput){
		func(i *AnalysisFixInput) { i.Version-- },
		func(i *AnalysisFixInput) { i.GenerationBaseRevision = "" },
		func(i *AnalysisFixInput) { i.SourceBranch = "" },
		func(i *AnalysisFixInput) { i.AssistantAnswer = "Different selected response" },
		func(i *AnalysisFixInput) { i.AssistantUnverified = !i.AssistantUnverified },
		func(i *AnalysisFixInput) { i.ArtifactCitations = nil },
		func(i *AnalysisFixInput) { i.EvidenceWarnings = []string{"different qualification"} },
		func(i *AnalysisFixInput) { i.Owner = "bob" },
		func(i *AnalysisFixInput) { i.Origin.FixTarget.TestName = "Other" },
	} {
		changed := cloneAnalysisFixInput(input)
		mutate(changed)
		if err := validateAnalysisFixHandoff(*changed); err == nil {
			t.Fatalf("accepted changed handoff: %+v", changed)
		}
	}
	if err := validateAnalysisPreviewBinding(&AnalysisPreviewBinding{}); err == nil {
		t.Fatal("accepted incomplete old binding")
	}
}

func TestAnalysisFixLegacyPreviewOnlyReconcilesUnknownWrites(t *testing.T) {
	service, _ := analysisRequestTestService(t)
	input, err := service.prepareAnalysisFix(t.Context(), exactAnalysisRequestInput(), "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	preview, err := handoffTestPreview(t, service, input, "alice", &calls)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := service.previewStore.load()
	if err != nil {
		t.Fatal(err)
	}
	record := state.Previews[tokenHash(preview.Token)]
	record.AnalysisBinding = &AnalysisPreviewBinding{}
	record.Status = previewStatusUnknown
	record.CreatedAt = time.Now().Add(-2 * previewTTL).Format(time.RFC3339Nano)
	if err := statefile.WritePrivateJSONDurable(service.previewStore.path, state); err != nil {
		t.Fatal(err)
	}
	reloaded := NewService(service.cfg, service.dataDir, AIConfig{})
	_, _, attempt, reconcile, err := reloaded.beginConfirm("alice", preview.Token, time.Minute)
	if err != nil || !reconcile {
		t.Fatalf("legacy unknown record reconcile=%t err=%v", reconcile, err)
	}
	if err := reloaded.finishConfirm("alice", preview.Token, attempt, "https://github.com/example/repo/pull/1", nil); err != nil {
		t.Fatal(err)
	}
	url, err := reloaded.Confirm(t.Context(), preview.Token, "alice", "token")
	if err != nil || url == "" {
		t.Fatalf("confirmed receipt = %q %v", url, err)
	}
	state, _, err = service.previewStore.load()
	if err != nil {
		t.Fatal(err)
	}
	record = state.Previews[tokenHash(preview.Token)]
	record.Status, record.ResultURL = previewStatusReady, ""
	if err := statefile.WritePrivateJSONDurable(service.previewStore.path, state); err != nil {
		t.Fatal(err)
	}
	if _, err := reloaded.Confirm(t.Context(), preview.Token, "alice", "token"); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("incomplete ready preview = %v", err)
	}
}

func TestAnalysisFixConfirmationRejectsStaleOrTamperedPreview(t *testing.T) {
	for _, change := range []string{"cause content", "JUnit content", "patch base", "execution results", "missing handoff", "answer qualification"} {
		t.Run(change, func(t *testing.T) {
			service, _ := analysisRequestTestService(t)
			detail, input := causeHandoff(t, service)
			var calls atomic.Int32
			preview, err := handoffTestPreview(t, service, input, "alice", &calls)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "cause content":
				detail.PatternAnalyses[0].CausalGroups[0].Remediation.SuggestedFix = "Changed cause remediation"
				writeJobDetail(t, service.dataDir, models.JobDataFilename(detail.JobID), detail)
			case "JUnit content":
				detail.Runs[0].TestCases[0].AIAnalysis.RootCause = "Changed JUnit diagnosis"
				writeJobDetail(t, service.dataDir, models.JobDataFilename(detail.JobID), detail)
			default:
				state, _, err := service.previewStore.load()
				if err != nil {
					t.Fatal(err)
				}
				record := state.Previews[tokenHash(preview.Token)]
				switch change {
				case "patch base":
					record.Fix.Base.HeadSHA = capzGenerationBaseRevision
				case "execution results":
					record.Fix.ExecutionVerification = &fixpr.ExecutionVerification{BaseSHA: capzGenerationBaseRevision}
				case "missing handoff":
					record.AnalysisBinding = nil
				case "answer qualification":
					record.AnalysisBinding.Handoff.AssistantUnverified = !record.AnalysisBinding.Handoff.AssistantUnverified
				}
				if err := statefile.WritePrivateJSONDurable(service.previewStore.path, state); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.Confirm(t.Context(), preview.Token, "alice", "token"); err == nil {
				t.Fatal("stale or tampered preview was confirmed")
			}
			if calls.Load() != 1 {
				t.Fatalf("confirmation regenerated a patch: %d", calls.Load())
			}
		})
	}
}

func TestAnalysisFixAdmissionRejectsSourcePreflightBeforeGeneration(t *testing.T) {
	for _, change := range []string{"unknown branch", "diverged revision", "wrong destination", "repository access"} {
		t.Run(change, func(t *testing.T) {
			service, _ := analysisRequestTestService(t)
			input := exactAnalysisRequestInput()
			switch change {
			case "unknown branch":
				detail := exactJUnitDetail()
				detail.Runs[0].RepoRefs = map[string]string{"kubernetes-sigs/cluster-api-provider-azure": analysisFixRevision}
				writeJobDetail(t, service.dataDir, models.JobDataFilename(detail.JobID), detail)
				input.SourceBranch = ""
			case "diverged revision":
				service.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{base: ghpr.Base{Branch: "main", HeadSHA: capzGenerationBaseRevision}}
			case "wrong destination":
				service.cfg.AI.FixPRs.Repo.Name = "other"
			case "repository access":
				service.sourceRevisionClient = &fakeAnalysisSourceRevisionClient{resolveErr: errors.New("repository unavailable")}
			}
			var calls atomic.Int32
			installHandoffTestGenerator(t, service, &calls)
			if _, err := service.CreateAnalysisFixRequest(t.Context(), input, "alice", "token", ""); err == nil {
				t.Fatal("invalid source was admitted")
			}
			if len(service.requests.Requests) != 0 || calls.Load() != 0 {
				t.Fatalf("requests=%d calls=%d", len(service.requests.Requests), calls.Load())
			}
		})
	}
}
