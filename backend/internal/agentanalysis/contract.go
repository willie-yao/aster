// Package agentanalysis defines the private experimental failure-analysis adapter.
package agentanalysis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

var (
	// ErrInvalidBundle marks malformed or inconsistent frozen evidence.
	ErrInvalidBundle = errors.New("invalid agent analysis evidence bundle")
	// ErrInvalidResult marks malformed or ungrounded agent output.
	ErrInvalidResult = errors.New("invalid agent analysis result")
)

var immutableSourceRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

const (
	BundleSchemaVersion = 1
	ResultSchemaVersion = 1
	ContractVersion     = "agent-analysis-v1"
	OutputPath          = ".prow-ai-dashboard/analysis.json"
	SkillName           = "failure-analysis"

	maxBundleBytes        = 104 << 10
	maxPlanBytes          = 64 << 10
	maxExcerptBytes       = 64 << 10
	maxExcerptTotalBytes  = 96 << 10
	maxExcerpts           = 16
	maxArtifactPathCount  = 5000
	maxFailureStringBytes = 32 << 10
)

// ArtifactScan records the bounded artifact-tree snapshot used to rank evidence.
type ArtifactScan struct {
	PathCount int    `json:"path_count"`
	Truncated bool   `json:"truncated,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

// EvidenceExcerpt is one immutable artifact excerpt exposed to the Agent.
type EvidenceExcerpt struct {
	ID            string `json:"id"`
	Path          string `json:"path"`
	Kind          string `json:"kind"`
	Content       string `json:"content"`
	ContentSHA256 string `json:"content_sha256"`
	Truncated     bool   `json:"truncated,omitempty"`
}

// EvidenceBundle is the complete bounded failure and artifact context.
type EvidenceBundle struct {
	SchemaVersion   int                            `json:"schema_version"`
	ContractVersion string                         `json:"contract_version"`
	Hash            string                         `json:"hash"`
	Request         ai.FailureAnalysisRequest      `json:"request"`
	Source          sourceinvestigation.Repository `json:"source"`
	Scan            ArtifactScan                   `json:"scan"`
	Plan            []skills.PlannedSkill          `json:"plan,omitempty"`
	Excerpts        []EvidenceExcerpt              `json:"excerpts"`
	SkillSetHash    string                         `json:"skill_set_hash,omitempty"`
}

// NewEvidenceBundle canonicalizes, seals, and validates one evidence bundle.
func NewEvidenceBundle(
	request ai.FailureAnalysisRequest,
	source sourceinvestigation.Repository,
	scan ArtifactScan,
	plan []skills.PlannedSkill,
	excerpts []EvidenceExcerpt,
	skillSetHash string,
) (EvidenceBundle, error) {
	source.Owner = strings.TrimSpace(source.Owner)
	source.Name = strings.TrimSpace(source.Name)
	source.Revision = strings.TrimSpace(source.Revision)
	bundle := EvidenceBundle{
		SchemaVersion: BundleSchemaVersion, ContractVersion: ContractVersion,
		Request: canonicalRequest(request), Source: source, Scan: scan,
		Plan: clonePlan(plan), Excerpts: slices.Clone(excerpts), SkillSetHash: strings.TrimSpace(skillSetHash),
	}
	for i := range bundle.Excerpts {
		excerpt := &bundle.Excerpts[i]
		excerpt.Path = strings.TrimSpace(excerpt.Path)
		excerpt.Kind = strings.TrimSpace(excerpt.Kind)
		excerpt.Content = strings.ReplaceAll(excerpt.Content, "\r\n", "\n")
		excerpt.ContentSHA256 = hashString(excerpt.Content)
		if strings.TrimSpace(excerpt.ID) == "" {
			excerpt.ID = excerptID(*excerpt)
		}
	}
	sort.Slice(bundle.Excerpts, func(i, j int) bool { return bundle.Excerpts[i].ID < bundle.Excerpts[j].ID })
	hash, err := bundleDigest(bundle)
	if err != nil {
		return EvidenceBundle{}, err
	}
	bundle.Hash = hash
	if err := ValidateEvidenceBundle(bundle); err != nil {
		return EvidenceBundle{}, err
	}
	return bundle, nil
}

// ValidateEvidenceBundle verifies canonical identity, bounds, paths, and hashes.
func ValidateEvidenceBundle(bundle EvidenceBundle) error {
	if bundle.SchemaVersion != BundleSchemaVersion || bundle.ContractVersion != ContractVersion {
		return fmt.Errorf("%w: unsupported schema or contract version", ErrInvalidBundle)
	}
	if !immutableSourceRevision.MatchString(bundle.Source.Revision) {
		return fmt.Errorf("%w: source revision must be a lowercase 40-character commit SHA", ErrInvalidBundle)
	}
	if err := sourceinvestigation.ValidateRepository(bundle.Source); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	if bundle.Request.JobID == "" || bundle.Request.BuildPrefix == "" || bundle.Request.Build.BuildID == "" || bundle.Request.TestCase.Name == "" {
		return fmt.Errorf("%w: request identity is incomplete", ErrInvalidBundle)
	}
	if !requestStringsValid(bundle.Request) {
		return fmt.Errorf("%w: request contains invalid or oversized text", ErrInvalidBundle)
	}
	if got := canonicalRequest(bundle.Request); !requestsEqual(got, bundle.Request) {
		return fmt.Errorf("%w: request is not canonical", ErrInvalidBundle)
	}
	if bundle.Scan.PathCount < 0 || bundle.Scan.PathCount > maxArtifactPathCount {
		return fmt.Errorf("%w: artifact path count is out of range", ErrInvalidBundle)
	}
	if bundle.Scan.Digest != "" && !validSHA256(bundle.Scan.Digest) {
		return fmt.Errorf("%w: artifact scan digest is invalid", ErrInvalidBundle)
	}
	if bundle.SkillSetHash != "" && !validSHA256(bundle.SkillSetHash) {
		return fmt.Errorf("%w: skill set hash is invalid", ErrInvalidBundle)
	}
	planData, err := json.Marshal(bundle.Plan)
	if err != nil || len(planData) > maxPlanBytes {
		return fmt.Errorf("%w: evidence plan exceeds %d bytes", ErrInvalidBundle, maxPlanBytes)
	}
	if len(bundle.Excerpts) > maxExcerpts {
		return fmt.Errorf("%w: evidence excerpts exceed %d", ErrInvalidBundle, maxExcerpts)
	}
	seen := map[string]bool{}
	total := 0
	previous := ""
	for i, excerpt := range bundle.Excerpts {
		if excerpt.ID == "" || excerpt.ID != excerptID(excerpt) {
			return fmt.Errorf("%w: excerpt %d has an invalid id", ErrInvalidBundle, i)
		}
		if previous != "" && excerpt.ID < previous {
			return fmt.Errorf("%w: excerpts are not sorted", ErrInvalidBundle)
		}
		previous = excerpt.ID
		if seen[excerpt.ID] {
			return fmt.Errorf("%w: duplicate excerpt id %q", ErrInvalidBundle, excerpt.ID)
		}
		seen[excerpt.ID] = true
		clean, err := artifacts.SafePath(excerpt.Path)
		if err != nil || clean == "" || clean != excerpt.Path {
			return fmt.Errorf("%w: excerpt %d has unsafe path %q", ErrInvalidBundle, i, excerpt.Path)
		}
		switch excerpt.Kind {
		case "read", "tail", "grep", "failure":
		default:
			return fmt.Errorf("%w: excerpt %d has unsupported kind %q", ErrInvalidBundle, i, excerpt.Kind)
		}
		if excerpt.Content == "" || !utf8.ValidString(excerpt.Content) || strings.IndexByte(excerpt.Content, 0) >= 0 || len(excerpt.Content) > maxExcerptBytes {
			return fmt.Errorf("%w: excerpt %d content is empty, invalid, or oversized", ErrInvalidBundle, i)
		}
		if excerpt.ContentSHA256 != hashString(excerpt.Content) {
			return fmt.Errorf("%w: excerpt %d content hash mismatch", ErrInvalidBundle, i)
		}
		total += len(excerpt.Content)
	}
	if total > maxExcerptTotalBytes {
		return fmt.Errorf("%w: excerpt contents exceed %d bytes", ErrInvalidBundle, maxExcerptTotalBytes)
	}
	if !validSHA256(bundle.Hash) {
		return fmt.Errorf("%w: bundle hash is invalid", ErrInvalidBundle)
	}
	digest, err := bundleDigest(bundle)
	if err != nil || digest != bundle.Hash {
		return fmt.Errorf("%w: bundle hash mismatch", ErrInvalidBundle)
	}
	data, err := json.Marshal(bundle)
	if err != nil || len(data) > maxBundleBytes {
		return fmt.Errorf("%w: encoded bundle exceeds %d bytes", ErrInvalidBundle, maxBundleBytes)
	}
	return nil
}

// FailureRequestHash fingerprints the canonical failure input without published AI output.
func FailureRequestHash(request ai.FailureAnalysisRequest) string {
	data, err := json.Marshal(canonicalRequest(request))
	if err != nil {
		return ""
	}
	return hashString(string(data))
}

func canonicalRequest(request ai.FailureAnalysisRequest) ai.FailureAnalysisRequest {
	request = analysisruntime.CanonicalFailureAnalysisRequest(request)
	if request.ConsecutiveFailures < 0 {
		request.ConsecutiveFailures = 0
	}
	return request
}

func requestStringsValid(request ai.FailureAnalysisRequest) bool {
	values := []string{
		request.JobID, request.BuildPrefix, request.Build.BuildID, request.Build.JobName,
		request.TestCase.Name, request.TestCase.FailureMessage, request.TestCase.FailureBody,
	}
	for _, value := range values {
		if !utf8.ValidString(value) || len(value) > maxFailureStringBytes {
			return false
		}
	}
	return true
}

func requestsEqual(a, b ai.FailureAnalysisRequest) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && string(left) == string(right)
}

func clonePlan(plan []skills.PlannedSkill) []skills.PlannedSkill {
	if len(plan) == 0 {
		return nil
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return nil
	}
	var cloned []skills.PlannedSkill
	if json.Unmarshal(data, &cloned) != nil {
		return nil
	}
	return cloned
}

func excerptID(excerpt EvidenceExcerpt) string {
	data := excerpt.Path + "\x00" + excerpt.Kind + "\x00" + excerpt.Content
	sum := sha256.Sum256([]byte(data))
	return "evidence-" + hex.EncodeToString(sum[:8])
}

func bundleDigest(bundle EvidenceBundle) (string, error) {
	bundle.Hash = ""
	data, err := json.Marshal(bundle)
	if err != nil {
		return "", fmt.Errorf("%w: encode bundle: %v", ErrInvalidBundle, err)
	}
	if len(data) > maxBundleBytes {
		return "", fmt.Errorf("%w: encoded bundle exceeds %d bytes", ErrInvalidBundle, maxBundleBytes)
	}
	return hashString(string(data)), nil
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
