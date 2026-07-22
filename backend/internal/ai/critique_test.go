package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
)

// TestPuntRE_SanityTable mirrors the Python harness cases so Go and Python
// regex behavior stay aligned.
func TestPuntRE_SanityTable(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantPunt bool
	}{
		{
			name:     "KCP HA punt example",
			text:     "Investigate Azure VM provisioning failures for the third control plane node. Check AzureMachine resource status conditions and ensure proper DNS configuration. Verify Azure quotas and network security rules allowing SSH access.",
			wantPunt: true,
		},
		{
			name:     "Clean Claude-style fix",
			text:     "Update kustomize/cluster-template-prow-azl3-flatcar-private.yaml line 142 to set virtualNetwork.vnetPeerings to match the staging vnet name. Reapply and retry the conformance job.",
			wantPunt: false,
		},
		{
			name:     "Composite verify-by",
			text:     "Bump the kube-vip image to v0.7.2 in templates/addons/kube-vip.yaml, then verify by tailing the kube-vip pod logs for the new image tag.",
			wantPunt: false,
		},
		{
			name:     "Apply then verify-by",
			text:     "Apply the fix; verify by rerunning the e2e suite.",
			wantPunt: false,
		},
		{
			name:     "Should-check punt",
			text:     "You should check whether the controller manager log shows leader election failures.",
			wantPunt: true,
		},
		{
			name:     "No-remediation escape hatch",
			text:     "No remediation possible from available evidence: artifacts show the kubelet failed to register but the journal.log was truncated before the error; would need a fresh build with the journal preserved.",
			wantPunt: false,
		},
		{
			name:     "Recommend punt mid-sentence",
			text:     "We recommend checking the AzureMachine status conditions and reviewing the cloud-init logs.",
			wantPunt: true,
		},
		{
			name:     "Operator should investigate",
			text:     "The operator should investigate why the VMSS failed to scale up.",
			wantPunt: true,
		},
		{
			name:     "Recommend at start",
			text:     "Recommend reviewing the prow-azl3 template diff against main.",
			wantPunt: true,
		},
		{
			name:     "Confirm via dashboard",
			text:     "Patch templates/addons/kube-vip.yaml; confirm via the e2e dashboard that the test passes.",
			wantPunt: false,
		},
		{
			name:     "You should verify by",
			text:     "You should verify by checking the metrics dashboard.",
			wantPunt: false,
		},
		{
			name:     "Mixed investigate-and-fix still punts",
			text:     "Investigate why the AzureMachine controller is logging x509 errors, and update the secret-name in the webhook config to match the new cert.",
			wantPunt: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := critiqueDraft(analysisResponse{SuggestedFix: tc.text}, nil, nil, nil, 0)
			gotPunt := !out.Passed
			if gotPunt != tc.wantPunt {
				t.Errorf("punt=%v, want %v\nmatches=%v\ntext=%q",
					gotPunt, tc.wantPunt, out.Matches(), tc.text)
			}
		})
	}
}

// TestCritiqueDraft_TransientButPersistent verifies the transient-vs-persistent
// contradiction: a draft claiming is_transient=true on a failure that has
// recurred at or above the threshold is rejected with re-investigation feedback.
func TestCritiqueDraft_TransientButPersistent(t *testing.T) {
	transient := analysisResponse{
		IsTransient:  true,
		SuggestedFix: "Re-run the job; this is an infrastructure flake.",
	}

	// Below threshold: a transient verdict on a non-persistent failure passes.
	if out := critiqueDraft(transient, nil, nil, nil, transientPersistThreshold-1); !out.Passed {
		t.Errorf("transient below threshold should pass, got %v", out.Matches())
	}

	// At threshold: contradiction fires.
	out := critiqueDraft(transient, nil, nil, nil, transientPersistThreshold)
	if out.Passed {
		t.Fatal("transient at threshold should fail")
	}
	if out.TransientPersistCount != transientPersistThreshold {
		t.Errorf("TransientPersistCount = %d, want %d", out.TransientPersistCount, transientPersistThreshold)
	}
	if !strings.Contains(out.Feedback, "consecutive builds") {
		t.Errorf("feedback should explain the persistence contradiction, got %q", out.Feedback)
	}
	if !strings.Contains(out.Feedback, "is_transient=false") {
		t.Errorf("feedback should tell the model to flip is_transient, got %q", out.Feedback)
	}
	found := false
	for _, m := range out.Matches() {
		if strings.HasPrefix(m, "transient-but-persistent") {
			found = true
		}
	}
	if !found {
		t.Errorf("Matches should include the transient token, got %v", out.Matches())
	}
}

// TestCritiqueDraft_NonTransientPersistentPasses verifies a persistent failure
// with is_transient=false is not flagged by the contradiction check.
func TestCritiqueDraft_NonTransientPersistentPasses(t *testing.T) {
	real := analysisResponse{
		IsTransient:  false,
		SuggestedFix: "Set the control-plane subnet route table in azurecluster_default.go.",
	}
	if out := critiqueDraft(real, nil, nil, nil, transientPersistThreshold+5); !out.Passed {
		t.Errorf("non-transient persistent failure should pass, got %v", out.Matches())
	}
}

// TestCritiqueDraft_PassedReturnsEmptyFeedback verifies the
// passed-path contract: caller can rely on Feedback being empty so
// the agentic loop doesn't accidentally append a no-op user message.
func TestCritiqueDraft_PassedReturnsEmptyFeedback(t *testing.T) {
	out := critiqueDraft(analysisResponse{
		SuggestedFix: "Update kustomize/cluster-template.yaml line 42 to bump the kube-vip image tag.",
	}, nil, nil, nil, 0)
	if !out.Passed {
		t.Fatalf("expected passed, got %+v", out)
	}
	if out.Feedback != "" {
		t.Errorf("Feedback should be empty when Passed=true, got %q", out.Feedback)
	}
	if len(out.Matches()) != 0 {
		t.Errorf("Matches should be empty when Passed=true, got %v", out.Matches())
	}
}

// TestCritiqueDraft_FeedbackQuotesOffendingText verifies failure feedback quotes
// the draft and lists matched phrases.
func TestCritiqueDraft_FeedbackQuotesOffendingText(t *testing.T) {
	bad := "Check the AzureMachine status. Verify cloud-init."
	out := critiqueDraft(analysisResponse{SuggestedFix: bad}, nil, nil, nil, 0)
	if out.Passed {
		t.Fatalf("expected punt, got passed")
	}
	if !strings.Contains(out.Feedback, bad) {
		t.Errorf("Feedback should quote the offending suggested_fix\nfeedback:\n%s", out.Feedback)
	}
	for _, m := range out.Matches() {
		if !strings.Contains(out.Feedback, m) {
			t.Errorf("Feedback should list matched phrase %q\nfeedback:\n%s", m, out.Feedback)
		}
	}
	// Restate the allowed shapes so retries have a clear target.
	for _, anchor := range []string{
		"CONCRETE remediation",
		"No remediation possible from available evidence",
		"Do NOT re-emit the same draft",
	} {
		if !strings.Contains(out.Feedback, anchor) {
			t.Errorf("Feedback missing anchor %q\nfeedback:\n%s", anchor, out.Feedback)
		}
	}
}

// TestCritiqueDraft_FeedbackDeduplicatesMatches verifies repeated matches are
// listed once in feedback.
func TestCritiqueDraft_FeedbackDeduplicatesMatches(t *testing.T) {
	repeat := "Check A. Check B. Check C. Check D."
	out := critiqueDraft(analysisResponse{SuggestedFix: repeat}, nil, nil, nil, 0)
	if out.Passed {
		t.Fatalf("expected punt")
	}
	// Feedback should contain "Check" but not list it four times.
	matchedSection := out.Feedback[strings.Index(out.Feedback, "(matched:"):]
	if strings.Count(strings.ToLower(matchedSection), `"check"`) > 1 {
		t.Errorf("Feedback should list 'Check' once, got:\n%s", matchedSection)
	}
}

// TestCritiqueDraft_EmptySuggestedFixPasses verifies empty suggested_fix passes
// this punt-pattern check.
func TestCritiqueDraft_EmptySuggestedFixPasses(t *testing.T) {
	out := critiqueDraft(analysisResponse{SuggestedFix: ""}, nil, nil, nil, 0)
	if !out.Passed {
		t.Errorf("empty suggested_fix should pass critique, got %+v", out)
	}
}

// TestNormalizeArtifactCitation covers the shared citation-cleaning rules used
// by read tracking and unread-citation detection.
func TestNormalizeArtifactCitation(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"build-log.txt", "build-log.txt"},
		{"build-log.txt:1720", "build-log.txt"},
		{"manager.log#L42", "manager.log"},
		{"manager.log#L42-L50", "manager.log"},
		{"`Manager.LOG`", "manager.log"},
		{"\"build-log.txt\"", "build-log.txt"},
		{`./artifacts/foo.log`, "artifacts/foo.log"},
		{`/artifacts/foo.log`, "artifacts/foo.log"},
		{`artifacts\machine-a\boot.log`, "artifacts/machine-a/boot.log"},
		{"", ""},
		{"  ", ""},
	}
	for _, tc := range cases {
		got := NormalizeArtifactCitation(tc.in)
		if got != tc.want {
			t.Errorf("NormalizeArtifactCitation(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFindUnreadArtifactCitations covers full-path matches, basename matches,
// source-file exclusions, and nil-map disabling.
func TestFindUnreadArtifactCitations(t *testing.T) {
	t.Run("nil maps disable check", func(t *testing.T) {
		if got := findUnreadArtifactCitations("the manager.log shows an error", nil, nil); got != nil {
			t.Errorf("nil maps should disable check, got %v", got)
		}
	})

	t.Run("basename match against bare citation", func(t *testing.T) {
		base := map[string]bool{"boot.log": true}
		if got := findUnreadArtifactCitations("the boot.log was empty", map[string]bool{}, base); len(got) != 0 {
			t.Errorf("basename match should pass, got %v", got)
		}
	})

	t.Run("basename collision across machines is caught", func(t *testing.T) {
		// Model read machine-a's boot.log, cites machine-b's boot.log.
		full := map[string]bool{"artifacts/machine-a/boot.log": true}
		base := map[string]bool{"boot.log": true}
		got := findUnreadArtifactCitations("artifacts/machine-b/boot.log shows DNS failure", full, base)
		if len(got) != 1 || got[0] != "artifacts/machine-b/boot.log" {
			t.Errorf("expected machine-b unread, got %v", got)
		}
	})

	t.Run("qualified citation matched against full-path reads", func(t *testing.T) {
		full := map[string]bool{"artifacts/manager.log": true}
		base := map[string]bool{"manager.log": true}
		if got := findUnreadArtifactCitations("artifacts/manager.log shows X", full, base); len(got) != 0 {
			t.Errorf("matching full path should pass, got %v", got)
		}
	})

	t.Run("source files not flagged", func(t *testing.T) {
		got := findUnreadArtifactCitations(
			"controllers/azuremachine_controller.go references cluster-template.yaml",
			map[string]bool{}, map[string]bool{})
		if len(got) != 0 {
			t.Errorf(".go and .yaml should not be flagged, got %v", got)
		}
	})

	t.Run("known artifact basenames flagged", func(t *testing.T) {
		got := findUnreadArtifactCitations(
			"checked started.json and build-log.txt and prowjob.json",
			map[string]bool{}, map[string]bool{})
		if len(got) != 3 {
			t.Errorf("expected 3 unread, got %v", got)
		}
	})

	t.Run("line numbers stripped before comparison", func(t *testing.T) {
		base := map[string]bool{"build-log.txt": true}
		if got := findUnreadArtifactCitations("see build-log.txt:1720 for the error",
			map[string]bool{}, base); len(got) != 0 {
			t.Errorf("line-number-suffixed citation should match, got %v", got)
		}
	})

	t.Run("dedup repeated mentions", func(t *testing.T) {
		got := findUnreadArtifactCitations(
			"manager.log shows X. manager.log shows Y. manager.log shows Z.",
			map[string]bool{}, map[string]bool{})
		if len(got) != 1 {
			t.Errorf("expected 1 deduped unread, got %v", got)
		}
	})
}

// TestCritiqueDraft_HallucinationOnly verifies unread artifact citations fail
// critique even when the punt check passes.
func TestCritiqueDraft_HallucinationOnly(t *testing.T) {
	parsed := analysisResponse{
		RootCause:    "The boot.log on the third control plane shows DNS failure.",
		SuggestedFix: "Update kustomize/cluster-template.yaml to match the vnet peering name; reapply.",
	}
	// Empty initialized reads make the unread-citation check fire.
	out := critiqueDraft(parsed, map[string]bool{}, map[string]bool{}, nil, 0)
	if out.Passed {
		t.Fatalf("expected fail on hallucinated citation, got passed: %+v", out)
	}
	if len(out.PuntMatches) != 0 {
		t.Errorf("punt should be clean, got %v", out.PuntMatches)
	}
	if len(out.UnreadCitations) == 0 {
		t.Errorf("expected unread citations, got none")
	}
	if !strings.Contains(out.Feedback, "tool log shows no read_artifact") {
		t.Errorf("Feedback missing hallucination anchor:\n%s", out.Feedback)
	}
	if !strings.Contains(out.Feedback, "Do NOT re-emit the same draft") {
		t.Errorf("Feedback missing closing instruction")
	}
}

// TestArtifactCitationRE_BroadenedCoverage covers qualified .txt/.json artifact
// paths and JUnit filename variants.
func TestArtifactCitationRE_BroadenedCoverage(t *testing.T) {
	cases := []struct {
		text     string
		wantHits int
	}{
		// Bare .json/.txt outside allowlist: not flagged.
		{"see config.json for the value", 0},
		{"check helm/values.yaml", 0},
		// Qualified path with .json: flagged.
		{"artifacts/cluster/events.json shows the issue", 1},
		{"clusters/foo/nodes.json was empty", 1},
		// Qualified path with .txt: flagged.
		{"artifacts/cluster/podinfo.txt has the trace", 1},
		// Different JUnit naming conventions all caught.
		{"junit_runner.xml is empty", 1},
		{"junit.e2e_suite.1.xml shows 3 failures", 1},
		{"junit-conformance.xml passed", 1},
		// Bare known artifacts still caught.
		{"build-log.txt mentions a timeout", 1},
		{"started.json and finished.json bracket the run", 2},
		// .yaml bare is still NOT flagged (source path).
		{"kustomize/cluster-template.yaml needs an update", 0},
		// .go bare is NOT flagged.
		{"controllers/azuremachine_controller.go reconciles X", 0},
	}
	for _, tc := range cases {
		got := artifactCitationRE.FindAllString(tc.text, -1)
		if len(got) != tc.wantHits {
			t.Errorf("text=%q\n  got %d hits %v, want %d", tc.text, len(got), got, tc.wantHits)
		}
	}
}

// TestCritiqueDraft_CitesRealReleaseURL_Passes verifies real upstream release
// URLs are not flagged as fabricated import paths.
func TestCritiqueDraft_CitesRealReleaseURL_Passes(t *testing.T) {
	parsed := analysisResponse{
		RootCause:    "clusterctl failed to apply https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.11.11/core-components.yaml because the management cluster could not reach the registry.",
		SuggestedFix: "Re-run the job; the registry pull is transient.",
	}
	out := critiqueDraft(parsed, map[string]bool{}, map[string]bool{}, nil, 0)
	if !out.Passed {
		t.Fatalf("draft citing a real release URL should pass, got: %+v", out)
	}
}

// loadSkillsForTest writes the given recipes into a temp dir and loads
// them via skills.Load. Returns the loaded set; fails the test on any
// load error so the test body can ignore wiring noise.
func loadSkillsForTest(t *testing.T, recipes map[string]string) *skills.Set {
	t.Helper()
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range recipes {
		p := filepath.Join(skillsDir, name+".yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	set, err := skills.Load(dir)
	if err != nil {
		t.Fatalf("skills.Load: %v", err)
	}
	return set
}

// TestCritiqueDraft_SkillMatchEvidenceSatisfied_Passes verifies matched recipes
// pass when every required-evidence group is satisfied.
func TestCritiqueDraft_SkillMatchEvidenceSatisfied_Passes(t *testing.T) {
	set := loadSkillsForTest(t, map[string]string{
		"webhook-tls": `
id: webhook-tls-failure
triggers:
  - "(?i)x509"
required_evidence:
  - id: cert-config
    description: cert-manager Certificate config
    any_of:
      - "config/certmanager/.*\\.ya?ml"
`,
	})
	parsed := analysisResponse{
		RootCause:    "Webhook x509 failure rooted in misconfigured cert-manager Certificate.",
		SuggestedFix: "Patch config/certmanager/issuer.yaml to set the correct DNS names and reapply.",
	}
	reads := map[string]bool{
		"config/certmanager/issuer.yaml": true,
	}
	out := critiqueDraft(parsed, reads, map[string]bool{"issuer.yaml": true}, set.Match("x509"), 0)
	if !out.Passed {
		t.Fatalf("expected Passed=true with all evidence satisfied, got %+v", out)
	}
	if len(out.MissingSkillEvidence) != 0 {
		t.Errorf("expected no MissingSkillEvidence, got %d entries", len(out.MissingSkillEvidence))
	}
}

// TestCritiqueDraft_SkillMatchMissingEvidence_FailsAndQuotesProcedure verifies
// missing required evidence fails and quotes the recipe procedure.
func TestCritiqueDraft_SkillMatchMissingEvidence_FailsAndQuotesProcedure(t *testing.T) {
	set := loadSkillsForTest(t, map[string]string{
		"webhook-tls": `
id: webhook-tls-failure
name: Webhook TLS failure
triggers:
  - "(?i)x509"
required_evidence:
  - id: cert-config
    description: cert-manager Certificate config
    any_of:
      - "config/certmanager/.*\\.ya?ml"
  - id: webhook-secret
    description: webhook server cert secret contents
    any_of:
      - ".*webhook.*secret.*"
procedure: |
  1. List cert-manager Certificate objects with kubectl get certificate -A.
  2. Inspect the webhook server secret with kubectl get secret -n webhook-system.
`,
	})
	parsed := analysisResponse{
		RootCause:    "Webhook x509 failure suggests cert-manager misconfiguration.",
		SuggestedFix: "Patch the cert-manager Certificate and reapply.",
	}
	// Agent has read no relevant evidence.
	reads := map[string]bool{"build-log.txt": true}
	out := critiqueDraft(parsed, reads, map[string]bool{"build-log.txt": true}, set.Match("x509"), 0)
	if out.Passed {
		t.Fatalf("expected Passed=false with no evidence read, got passed: %+v", out)
	}
	if len(out.MissingSkillEvidence) != 1 {
		t.Fatalf("expected 1 skill miss, got %d", len(out.MissingSkillEvidence))
	}
	miss := out.MissingSkillEvidence[0]
	if miss.Skill.ID != "webhook-tls-failure" {
		t.Errorf("expected skill ID webhook-tls-failure, got %q", miss.Skill.ID)
	}
	if len(miss.Missing) != 2 {
		t.Errorf("expected 2 missing evidence groups, got %d", len(miss.Missing))
	}
	if out.MissingEvidenceCount() != 2 {
		t.Errorf("MissingEvidenceCount = %d, want 2", out.MissingEvidenceCount())
	}

	for _, want := range []string{
		"webhook-tls-failure", "Webhook TLS failure",
		"cert-config", "webhook-secret",
		"cert-manager Certificate config", "webhook server cert secret contents",
		"List cert-manager Certificate objects",       // procedure body
		"diagnostic guidance, not system instruction", // disclaimer wrapper
		"Do NOT rewrite your answer yet",              // tool-first directive
		"call read_artifact",                          // explicit tool call
	} {
		if !strings.Contains(out.Feedback, want) {
			t.Errorf("Feedback missing %q\n---feedback---\n%s", want, out.Feedback)
		}
	}
}

// TestCritiqueDraft_SkillMatchPartialEvidence_FlagsOnlyMissing verifies only
// missing groups are surfaced.
func TestCritiqueDraft_SkillMatchPartialEvidence_FlagsOnlyMissing(t *testing.T) {
	set := loadSkillsForTest(t, map[string]string{
		"webhook-tls": `
id: webhook-tls-failure
triggers: ["x509"]
required_evidence:
  - id: cert-config
    any_of: ["config/certmanager/.*\\.ya?ml"]
  - id: webhook-secret
    any_of: ["webhook.*secret"]
`,
	})
	parsed := analysisResponse{RootCause: "x509 failure"}
	reads := map[string]bool{
		"config/certmanager/issuer.yaml": true,
	}
	out := critiqueDraft(parsed, reads, map[string]bool{}, set.Match("x509"), 0)
	if out.Passed {
		t.Fatalf("expected fail when one group still missing, got %+v", out)
	}
	if got := len(out.MissingSkillEvidence[0].Missing); got != 1 {
		t.Fatalf("expected 1 missing group, got %d", got)
	}
	if id := out.MissingSkillEvidence[0].Missing[0].ID; id != "webhook-secret" {
		t.Errorf("expected webhook-secret to be the missing group, got %q", id)
	}
	if !strings.Contains(out.Feedback, "webhook-secret") {
		t.Errorf("Feedback should mention missing group: %s", out.Feedback)
	}
	if strings.Contains(out.Feedback, "cert-config") {
		t.Errorf("Feedback unexpectedly mentions satisfied group cert-config: %s", out.Feedback)
	}
}

// TestCritiqueDraft_NilSkillsDisablesCheck verifies nil matchedSkills disables
// the skill-evidence check.
func TestCritiqueDraft_NilSkillsDisablesCheck(t *testing.T) {
	parsed := analysisResponse{
		RootCause:    "Webhook x509 failure.",
		SuggestedFix: "Fix the cert and reapply.",
	}
	out := critiqueDraft(parsed, map[string]bool{"build-log.txt": true}, map[string]bool{}, nil, 0)
	if !out.Passed {
		t.Fatalf("expected Passed=true with nil skills, got %+v", out)
	}
	if len(out.MissingSkillEvidence) != 0 {
		t.Errorf("expected no skill misses with nil input, got %d", len(out.MissingSkillEvidence))
	}
}

// TestCritiqueDraft_MultipleSkillsMatchSurfaceAll verifies each matching recipe
// contributes its own miss block.
func TestCritiqueDraft_MultipleSkillsMatchSurfaceAll(t *testing.T) {
	set := loadSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook
triggers: ["x509"]
required_evidence:
  - id: cert-config
    any_of: ["never-matches-pattern"]
`,
		"machine": `
id: machine-bootstrap
triggers: ["cloud-init"]
required_evidence:
  - id: machine-yaml
    any_of: ["never-matches-pattern-2"]
`,
	})
	parsed := analysisResponse{
		RootCause: "x509 webhook failure combined with cloud-init issues.",
	}
	out := critiqueDraft(parsed, map[string]bool{}, map[string]bool{},
		set.Match(parsed.RootCause), 0)
	if out.Passed {
		t.Fatalf("expected fail with two unmatched recipes, got passed")
	}
	if got := len(out.MissingSkillEvidence); got != 2 {
		t.Fatalf("expected 2 skill misses, got %d", got)
	}
	if c := out.MissingEvidenceCount(); c != 2 {
		t.Errorf("MissingEvidenceCount = %d, want 2", c)
	}
	for _, want := range []string{"webhook", "machine-bootstrap"} {
		if !strings.Contains(out.Feedback, want) {
			t.Errorf("Feedback missing recipe %q: %s", want, out.Feedback)
		}
	}
}

// TestCritiqueDraft_SkillCombinesWithPuntAndHallucination verifies punt, unread
// citation, and missing-evidence sections can appear together.
func TestCritiqueDraft_SkillCombinesWithPuntAndHallucination(t *testing.T) {
	set := loadSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook
triggers: ["x509"]
required_evidence:
  - id: cert-config
    any_of: ["never-matches-pattern"]
`,
	})
	parsed := analysisResponse{
		RootCause:    "x509 failure visible in build-log.txt and machine-foo/boot.log.",
		SuggestedFix: "Check the cert and verify the webhook secret.", // bare-imperative punt
	}
	// build-log.txt is read; machine-foo/boot.log is not.
	out := critiqueDraft(parsed,
		map[string]bool{"build-log.txt": true},
		map[string]bool{"build-log.txt": true},
		set.Match("x509"), 0)

	if out.Passed {
		t.Fatalf("expected fail with all issues firing, got passed: %+v", out)
	}
	if len(out.PuntMatches) == 0 {
		t.Errorf("expected PuntMatches non-empty")
	}
	if len(out.UnreadCitations) == 0 {
		t.Errorf("expected UnreadCitations non-empty (machine-foo/boot.log)")
	}
	if len(out.MissingSkillEvidence) == 0 {
		t.Errorf("expected MissingSkillEvidence non-empty (cert-config unread)")
	}

	for _, marker := range []string{
		"diagnostic / information-gathering",     // punt section
		"tool log shows no read_artifact",        // unread section
		"matches one or more diagnostic recipes", // skill section header
	} {
		if !strings.Contains(out.Feedback, marker) {
			t.Errorf("Feedback missing section marker %q", marker)
		}
	}
}

func TestCritiqueDraft_AppliesConditionalSkillEvidence(t *testing.T) {
	set, err := skills.LoadMerged(t.TempDir(), skills.ProfileSelection{Kubernetes: true})
	if err != nil {
		t.Fatal(err)
	}
	parsed := analysisResponse{
		Summary:      "resolver blocked API hostname lookup",
		RootCause:    "the API hostname lookup used a loopback DNS resolver that refused connections",
		Severity:     "High",
		SuggestedFix: "Restore the node resolver configuration and restart kube-proxy.",
	}
	clientPath := "artifacts/clusters/workload/kube-system/kube-proxy-node-1/kube-proxy.log"
	reads := map[string]bool{clientPath: true}
	matches := set.Match(strings.Join(parsed.proseFields(), "\n"))
	out := critiqueDraft(parsed, reads, map[string]bool{"kube-proxy.log": true}, matches, 0)
	if out.Passed || len(out.MissingSkillEvidence) != 1 {
		t.Fatalf("outcome = %+v", out)
	}
	missing := out.MissingSkillEvidence[0].Missing
	if len(missing) != 1 || missing[0].ID != "dns-resolution" {
		t.Fatalf("conditional missing evidence = %+v, want only dns-resolution", missing)
	}
}

// skillMissOutcome builds a critiqueOutcome with one unsatisfied required-
// evidence group, the input pruneAbsentSkillEvidence operates on.
func skillMissOutcome(t *testing.T) (analysisResponse, critiqueOutcome) {
	t.Helper()
	set := loadSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook
triggers: ["x509"]
required_evidence:
  - id: cert-config
    any_of: ["config/webhook/.*\\.yaml"]
`,
	})
	parsed := analysisResponse{
		RootCause:    "x509 webhook validation failed; see build-log.txt.",
		SuggestedFix: "Regenerate the webhook serving certificate and redeploy.",
	}
	// build-log.txt read keeps unread-citation clean; cert-config remains unsatisfied.
	out := critiqueDraft(parsed,
		map[string]bool{"build-log.txt": true},
		map[string]bool{"build-log.txt": true},
		set.Match("x509"), 0)
	if out.Passed || len(out.MissingSkillEvidence) != 1 {
		t.Fatalf("setup: expected one missing skill group, got passed=%v misses=%v", out.Passed, out.MissingSkillEvidence)
	}
	return parsed, out
}

func TestPruneAbsentSkillEvidence_AbsentGroupDroppedAndPasses(t *testing.T) {
	parsed, out := skillMissOutcome(t)
	// Tree has no config/webhook/*.yaml: the recipe is inapplicable.
	tree := map[string]bool{"build-log.txt": true, "artifacts/clusters/x/manager.log": true}
	dropped := pruneAbsentSkillEvidence(parsed, &out, tree)
	if dropped != 1 {
		t.Fatalf("expected 1 absent group dropped, got %d", dropped)
	}
	if !out.Passed {
		t.Errorf("draft should pass once the only failure (absent evidence) is dropped: %+v", out)
	}
	if out.Feedback != "" {
		t.Errorf("Feedback should be cleared on pass, got %q", out.Feedback)
	}
}

func TestPruneAbsentSkillEvidence_PresentButUnreadKept(t *testing.T) {
	parsed, out := skillMissOutcome(t)
	// Tree DOES contain a matching path: evidence exists but was unread, so
	// it stays a genuine miss the agent should have covered.
	tree := map[string]bool{"config/webhook/manifests.yaml": true}
	dropped := pruneAbsentSkillEvidence(parsed, &out, tree)
	if dropped != 0 {
		t.Fatalf("expected 0 dropped (evidence present), got %d", dropped)
	}
	if out.Passed || len(out.MissingSkillEvidence) != 1 {
		t.Errorf("present-but-unread evidence must remain a miss: %+v", out)
	}
}

func TestPruneAbsentSkillEvidence_NilTreeIsNoOp(t *testing.T) {
	parsed, out := skillMissOutcome(t)
	if dropped := pruneAbsentSkillEvidence(parsed, &out, nil); dropped != 0 {
		t.Fatalf("nil tree should be a no-op, got %d dropped", dropped)
	}
	if out.Passed || len(out.MissingSkillEvidence) != 1 {
		t.Errorf("nil tree must leave the outcome unchanged: %+v", out)
	}
}

// TestPruneAbsentSkillEvidence_OtherFailureKeepsFailing verifies that dropping
// an absent skill group does NOT mask an unrelated failing check: a punt in
// suggested_fix must keep the draft failing with regenerated feedback.
func TestPruneAbsentSkillEvidence_OtherFailureKeepsFailing(t *testing.T) {
	set := loadSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook
triggers: ["x509"]
required_evidence:
  - id: cert-config
    any_of: ["config/webhook/.*\\.yaml"]
`,
	})
	parsed := analysisResponse{
		RootCause:    "x509 webhook validation failed.",
		SuggestedFix: "Check the webhook certificate.", // punt
	}
	out := critiqueDraft(parsed,
		map[string]bool{"build-log.txt": true},
		map[string]bool{"build-log.txt": true},
		set.Match("x509"), 0)
	if len(out.PuntMatches) == 0 || len(out.MissingSkillEvidence) != 1 {
		t.Fatalf("setup: expected punt + one skill miss, got %+v", out)
	}
	dropped := pruneAbsentSkillEvidence(parsed, &out, map[string]bool{"build-log.txt": true})
	if dropped != 1 {
		t.Fatalf("expected the absent skill group dropped, got %d", dropped)
	}
	if out.Passed {
		t.Errorf("draft must still fail on the punt, not pass")
	}
	if !strings.Contains(out.Feedback, "diagnostic / information-gathering") {
		t.Errorf("regenerated feedback should retain the punt section, got %q", out.Feedback)
	}
	if strings.Contains(out.Feedback, "matches one or more diagnostic recipes") {
		t.Errorf("regenerated feedback should NOT mention the dropped skill")
	}
}
