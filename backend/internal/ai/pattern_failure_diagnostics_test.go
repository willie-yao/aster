package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type patternRejectionReplayTransition struct {
	Kind     string                    `json:"kind"`
	Stage    string                    `json:"stage,omitempty"`
	Category patternValidationCategory `json:"category"`
	Code     string                    `json:"code,omitempty"`
	Stats    patternParseStats         `json:"stats,omitempty"`
}

type patternRejectionReplayFixture struct {
	Version                   int                                `json:"version"`
	CaseName                  string                             `json:"case_name"`
	CaseClass                 string                             `json:"case_class"`
	ContractShape             []string                           `json:"contract_shape"`
	ObservedAttempts          int                                `json:"observed_attempts"`
	ObservedRepairs           int                                `json:"observed_repairs"`
	ValidationTransitions     []patternRejectionReplayTransition `json:"validation_transitions"`
	ExpectedPersistedCategory PatternFailureCategory             `json:"expected_persisted_category"`
	EvidenceComplete          bool                               `json:"evidence_complete"`
}

func TestPatternRejectionReplayFixtures(t *testing.T) {
	paths, err := filepath.Glob("testdata/pattern-rejections/*.json")
	if err != nil || len(paths) != 2 {
		t.Fatalf("fixtures=%d err=%v", len(paths), err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fixture patternRejectionReplayFixture
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			if fixture.Version != 1 || fixture.CaseName == "" || fixture.CaseClass == "" || len(fixture.ContractShape) != 7 || fixture.ObservedAttempts != 1 || fixture.ObservedRepairs != 2 || fixture.EvidenceComplete {
				t.Fatalf("invalid fixture: %+v", fixture)
			}
			recorder := &patternFailureDiagnosticsRecorder{}
			var terminal error
			for _, transition := range fixture.ValidationTransitions {
				err := &patternValidationError{category: transition.Category, issue: transition.Code, stats: transition.Stats}
				switch transition.Kind {
				case "validation":
					recorder.recordValidation(transition.Stage, transition.Stats, err)
				case "repair":
					recorder.beginRepair(transition.Stage)
					recorder.recordRepair(transition.Stage, transition.Stats, err)
				case "terminal":
					terminal = err
				default:
					t.Fatalf("unsupported transition: %+v", transition)
				}
			}
			if terminal == nil {
				t.Fatal("fixture omitted terminal transition")
			}
			got := patternFailureCategoryWithDiagnostics(terminal, recorder.snapshot())
			if got != fixture.ExpectedPersistedCategory || recorder.snapshot().RepairCount != 0 {
				t.Fatalf("category=%s diagnostics=%+v", got, recorder.snapshot())
			}
			for _, forbidden := range []string{"prompt", "response", "citation", "artifact", "repository", "source", "prose"} {
				if strings.Contains(strings.ToLower(string(raw)), `"`+forbidden+`"`) {
					t.Fatalf("fixture contains private field %q", forbidden)
				}
			}
		})
	}
}

func TestPatternFailureDiagnosticsPreferSpecificValidation(t *testing.T) {
	recorder := &patternFailureDiagnosticsRecorder{}
	recorder.recordValidation("grounded", patternParseStats{}, &patternValidationError{category: patternValidationMissing, issue: "no_contract"})
	recorder.recordValidation("extraction", patternParseStats{CandidateCount: 1, ContractLikeRejectedCount: 1}, &patternValidationError{category: patternValidationSchema, issue: "unsafe_conversion_remediation"})
	recorder.beginRepair("validation")
	recorder.recordRepair("validation", patternParseStats{}, &patternValidationError{category: patternValidationMissing, issue: "no_contract"})
	diagnostics := recorder.snapshot()
	if diagnostics.Stage != "extraction" || diagnostics.ValidationCategory != "schema" || diagnostics.ValidationCode != "unsafe_conversion_remediation" || diagnostics.RepairValidationCode != "no_contract" || diagnostics.RepairCount != 1 {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	if got := patternFailureCategoryWithDiagnostics(&patternValidationError{category: patternValidationMissing, issue: "no_contract"}, diagnostics); got != PatternFailureSchema {
		t.Fatalf("category=%s", got)
	}
}

func TestPatternFailureDiagnosticsKeepHigherRankedCategoryAfterMalformedRepair(t *testing.T) {
	recorder := &patternFailureDiagnosticsRecorder{}
	recorder.recordValidation("grounded", patternParseStats{CandidateCount: 1, ContractLikeRejectedCount: 1}, &patternValidationError{category: patternValidationSchema, issue: "unsafe_conversion_remediation"})
	recorder.beginRepair("validation")
	recorder.recordRepair("validation", patternParseStats{IncompleteCount: 1}, &patternValidationError{category: patternValidationJSON, issue: "invalid_json"})
	if got := patternFailureCategoryWithDiagnostics(&patternValidationError{category: patternValidationJSON, issue: "invalid_json"}, recorder.snapshot()); got != PatternFailureSchema {
		t.Fatalf("category=%s diagnostics=%+v", got, recorder.snapshot())
	}
}

func TestPatternFailureDiagnosticsValidationClasses(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		category patternValidationCategory
		code     string
		want     PatternFailureCategory
	}{
		{name: "malformed", category: patternValidationJSON, code: "invalid_json", want: PatternFailureJSON},
		{name: "unsafe conversion", category: patternValidationSchema, code: "unsafe_conversion_remediation", want: PatternFailureSchema},
		{name: "incomplete target", category: patternValidationSchema, code: "remediation_target", want: PatternFailureSchema},
		{name: "invalid shared build", category: patternValidationBuilds, code: "shared_builds", want: PatternFailureBuilds},
		{name: "missing fallback", category: patternValidationMissing, code: "no_contract", want: PatternFailureMissing},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := &patternFailureDiagnosticsRecorder{}
			recorder.recordValidation("grounded", patternParseStats{CandidateCount: 1, ContractLikeRejectedCount: 1}, &patternValidationError{category: testCase.category, issue: testCase.code})
			diagnostics := recorder.snapshot()
			if got := patternFailureCategoryWithDiagnostics(&patternValidationError{category: patternValidationMissing, issue: "no_contract"}, diagnostics); got != testCase.want {
				t.Fatalf("category=%s diagnostics=%+v", got, diagnostics)
			}
		})
	}
}

func TestPatternFailureDiagnosticsSanitizeMalformedMetadata(t *testing.T) {
	got := sanitizePatternFailureDiagnostics(PatternFailureDiagnostics{
		Stage: "private stage text", ValidationCategory: "secret", ValidationCode: "privatecredentialvalue",
		CandidateCount: -1, ValidCount: maxPatternDiagnosticCount + 1,
		RepairStage: "private repair", RepairValidationCode: "private/path", RepairCount: -5,
	})
	if got != (PatternFailureDiagnostics{}) {
		t.Fatalf("diagnostics=%+v", got)
	}
}

func TestReadPatternFailureDiagnosticsSupportsLegacyAndBoundsOutput(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	legacy := patternFailureCacheData{Version: patternFailureCacheVersion, Category: PatternFailureMissing, FailedAt: now.Add(-time.Hour), RetryAfter: now.Add(time.Hour)}
	current := patternFailureCacheData{
		Version: patternFailureCacheVersion, JobID: "periodic-safe", Category: PatternFailureSchema,
		FailedAt: now.Add(-time.Minute), RetryAfter: now.Add(time.Hour),
		PatternFailureDiagnostics: PatternFailureDiagnostics{
			Stage: "extraction", ValidationCategory: "schema", ValidationCode: "unsafe_conversion_remediation",
			CandidateCount: 1, ContractLikeRejectedCount: 1, RepairStage: "validation", RepairValidationCode: "no_contract", RepairCount: 1,
		},
	}
	entries := map[string]CacheEntry{}
	for key, value := range map[string]patternFailureCacheData{"pattern-failure:legacy": legacy, "pattern-failure:current": current} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		entries[key] = CacheEntry{Key: key, CreatedAt: now.Add(-time.Minute), Data: raw}
	}
	path := filepath.Join(t.TempDir(), CacheFilename)
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadPatternFailureDiagnostics(path, now)
	if err != nil || len(snapshot.Entries) != 2 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if snapshot.Entries[0].Identity != "current" || snapshot.Entries[0].JobID != "periodic-safe" || snapshot.Entries[0].ValidationCode != "unsafe_conversion_remediation" {
		t.Fatalf("current=%+v", snapshot.Entries[0])
	}
	if snapshot.Entries[1].Identity != "legacy" || snapshot.Entries[1].PatternFailureDiagnostics != (PatternFailureDiagnostics{}) {
		t.Fatalf("legacy=%+v", snapshot.Entries[1])
	}
}

func TestPatternFailureDiagnosticsPersistSpecificReasonAfterRepairFailure(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	unsafe := `{"systemic":true,"confidence":"high","shared_root_cause":"conversion unavailable","shared_builds":["abuild","bbuild"],"suggested_fix":"Delete admission webhooks so CRD conversion no longer calls ASO.","remediation_targets":[{"intent":"modify_symbol","symbol":"preUpgrade","required_call":"example/asomigration.DeleteWebhookConfigurations","path":"upgrade.go"}],"summary":"unsafe conversion recommendation"}`
	srv.push(200, chatRespFinal(unsafe))
	for range 3 {
		srv.push(200, chatRespFinal("PRIVATE_REPAIR_OUTPUT without a contract"))
	}
	cacheDir := t.TempDir()
	now := time.Now().UTC()
	service := newPatternBackoffService(t, srv.URL, cacheDir, "claude-test")
	service.patternNow = func() time.Time { return now }
	_, err := service.AnalyzePatternWithOptions(t.Context(), "periodic-api-upgrade", "periodic-api-upgrade", patternFailures(3), PatternAnalyzeOptions{AllowValidationRepair: true})
	if PatternFailureCategoryOf(err) != PatternFailureSchema {
		t.Fatalf("error=%v category=%s", err, PatternFailureCategoryOf(err))
	}
	if got := atomic.LoadInt32(&srv.calls); got != 4 {
		t.Fatalf("model calls=%d, want 4", got)
	}
	if err := service.client.Cache().Save(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadPatternFailureDiagnostics(filepath.Join(cacheDir, CacheFilename), now)
	if err != nil || len(snapshot.Entries) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	entry := snapshot.Entries[0]
	if entry.JobID != "periodic-api-upgrade" || entry.Category != PatternFailureSchema || entry.Stage != "tool_free" || entry.ValidationCode != "unsafe_conversion_remediation" || entry.RepairStage != "validation" || entry.RepairValidationCode != "repair_no_contract" || entry.RepairCount != 1 {
		t.Fatalf("entry=%+v", entry)
	}
	beforeSuppression, err := os.ReadFile(filepath.Join(cacheDir, CacheFilename))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.AnalyzePattern(t.Context(), "periodic-api-upgrade", "periodic-api-upgrade", patternFailures(3))
	if !IsPatternFailureSuppressed(err) || PatternFailureCategoryOf(err) != PatternFailureSchema || atomic.LoadInt32(&srv.calls) != 4 {
		t.Fatalf("suppressed error=%v category=%s calls=%d", err, PatternFailureCategoryOf(err), srv.calls)
	}
	if err := service.client.Cache().Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cacheDir, CacheFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(beforeSuppression) {
		t.Fatal("suppressed pass changed exact persisted diagnostics")
	}
	for _, private := range []string{"PRIVATE_REPAIR_OUTPUT", "Delete admission webhooks", "unsafe conversion recommendation"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("cache leaked private output %q", private)
		}
	}
}

func TestPatternFailureDiagnosticsPositiveResultDoesNotPersistCooldown(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"systemic":false,"confidence":"low","shared_root_cause":"","shared_builds":[],"suggested_fix":"","remediation_targets":[],"summary":"independent"}`))
	cacheDir := t.TempDir()
	service := newPatternBackoffService(t, srv.URL, cacheDir, "claude-test")
	if _, err := service.AnalyzePattern(t.Context(), "job", "job", patternFailures(3)); err != nil {
		t.Fatal(err)
	}
	if err := service.client.Cache().Save(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadPatternFailureDiagnostics(filepath.Join(cacheDir, CacheFilename), time.Now().UTC())
	if err != nil || len(snapshot.Entries) != 0 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestPatternFailureDiagnosticsDoNotPersistPrivateContent(t *testing.T) {
	value := patternFailureCacheData{
		Version: patternFailureCacheVersion, JobID: "periodic-safe", Category: PatternFailureSchema,
		FailedAt: time.Now().UTC(), RetryAfter: time.Now().UTC().Add(time.Hour),
		PatternFailureDiagnostics: sanitizePatternFailureDiagnostics(PatternFailureDiagnostics{
			Stage: "grounded", ValidationCategory: "schema", ValidationCode: "required_fields", RepairCount: 1,
		}),
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"PRIVATE_PATTERN_OUTPUT", "Job: periodic-safe", "artifact/path", "repository source", "model prose", "credential"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("cache contains private value %q", private)
		}
	}
}
