package analysischat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

const (
	// PreparedCauseFindingsFilename is the private fetcher-owned cause finding cache.
	PreparedCauseFindingsFilename = "prepared_cause_findings.json"
	preparedCauseFindingsVersion  = 1
	PreparedCauseQuestion         = "Investigate this causal group across its member builds. Determine what the artifacts prove about the root cause and the concrete repository change that should fix it. Cite the strongest direct evidence. If the published cause or remediation is wrong, challenge it and propose a revision."
)

// PreparedCauseFinding is one engine-generated first answer for a cause chat.
type PreparedCauseFinding struct {
	Ref        AnalysisRef `json:"ref"`
	Reply      Reply       `json:"reply"`
	PreparedAt string      `json:"prepared_at"`
}

// PreparedCauseFindings is the private cache for one analysis configuration.
type PreparedCauseFailure struct {
	AttemptedAt string `json:"attempted_at"`
}

type PreparedCauseFindings struct {
	Version    int                             `json:"version"`
	Generation string                          `json:"generation"`
	Findings   map[string]PreparedCauseFinding `json:"findings"`
	Failures   map[string]PreparedCauseFailure `json:"failures,omitempty"`
}

// PreparedCauseGeneration identifies model, prompt, and cache policy inputs.
func PreparedCauseGeneration(runtimeFingerprint string) string {
	payload, _ := json.Marshal([]string{strings.TrimSpace(runtimeFingerprint), PreparedCauseQuestion})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// PreparedCauseKey returns the stable identity for one cause reference.
func PreparedCauseKey(ref AnalysisRef) (string, error) {
	ref, err := normalizeAnalysisRef(ref)
	if err != nil || ref.Scope != ScopeCause {
		return "", fmt.Errorf("%w: prepared finding requires cause scope", ErrInvalidRequest)
	}
	return hashAnalysisRef(ref)
}

// LoadPreparedCauseFindings loads the bounded private cache.
func LoadPreparedCauseFindings(path, generation string) (PreparedCauseFindings, error) {
	state := PreparedCauseFindings{Version: preparedCauseFindingsVersion, Generation: generation, Findings: map[string]PreparedCauseFinding{}, Failures: map[string]PreparedCauseFailure{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(data) > 16<<20 {
		return state, fmt.Errorf("prepared cause findings exceed 16 MiB")
	}
	var loaded PreparedCauseFindings
	if err := json.Unmarshal(data, &loaded); err != nil {
		return state, err
	}
	if loaded.Version != preparedCauseFindingsVersion || loaded.Generation != generation || loaded.Findings == nil {
		return state, nil
	}
	if loaded.Failures == nil {
		loaded.Failures = map[string]PreparedCauseFailure{}
	}
	return loaded, nil
}

// SavePreparedCauseFindings atomically writes the private cache.
func SavePreparedCauseFindings(path string, state PreparedCauseFindings) error {
	state.Version = preparedCauseFindingsVersion
	if state.Findings == nil {
		state.Findings = map[string]PreparedCauseFinding{}
	}
	if state.Failures == nil {
		state.Failures = map[string]PreparedCauseFailure{}
	}
	return statefile.WritePrivateJSONDurable(path, state)
}

// PreparedCauseTurn builds the stateless first turn for one actionable cause.
func PreparedCauseTurn(ref AnalysisRef, detail models.JobDetail) (Turn, error) {
	ref, err := normalizeAnalysisRef(ref)
	if err != nil || ref.Scope != ScopeCause {
		return Turn{}, fmt.Errorf("%w: prepared finding requires cause scope", ErrInvalidRequest)
	}
	resolved, err := resolveCauseAnalysis(ref, detail)
	if err != nil {
		return Turn{}, err
	}
	if resolved.fixTarget == nil {
		return Turn{}, fmt.Errorf("%w: cause has no eligible failed-test Fix target", ErrInvalidRequest)
	}
	return Turn{
		Scope: resolved.ref.Scope, JobID: resolved.jobID, BuildPrefix: resolved.buildPrefix,
		Build: cloneBuildInfo(resolved.build), TestCase: cloneTestCase(resolved.testCase),
		Pattern: clonePattern(resolved.pattern), EvidenceBuilds: cloneArtifactBuilds(resolved.evidenceBuilds),
		Question: PreparedCauseQuestion,
	}, nil
}

func preparedRequestID(key string) string {
	if len(key) > 32 {
		key = key[:32]
	}
	return "prepared-" + key
}

func preparedMessage(finding PreparedCauseFinding, key string, now time.Time) (Message, persistedRequest) {
	stamp := strings.TrimSpace(finding.PreparedAt)
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		stamp = now.UTC().Format(time.RFC3339)
	}
	requestID := preparedRequestID(key)
	return Message{
		Role: "assistant", RequestID: requestID, Content: finding.Reply.Answer, Assessment: finding.Reply.Assessment, Prepared: true,
		Citations: append([]Citation(nil), finding.Reply.Citations...), ProposedRevision: cloneRevision(finding.Reply.ProposedRevision),
		EvidenceWarnings: append([]string(nil), finding.Reply.EvidenceWarnings...), ToolCalls: finding.Reply.ToolCalls,
		GCSBytes: finding.Reply.GCSBytes, ElapsedMs: finding.Reply.ElapsedMs, ProviderMs: finding.Reply.ProviderMs,
		ValidationRetries: finding.Reply.ValidationRetries, CreatedAt: stamp,
	}, persistedRequest{QuestionHash: hashText(PreparedCauseQuestion), Question: PreparedCauseQuestion, Status: requestSucceeded, Prepared: true, CreatedAt: stamp, UpdatedAt: stamp}
}

func preparedFindingPath(dataDir string) string {
	return filepath.Join(dataDir, PreparedCauseFindingsFilename)
}
