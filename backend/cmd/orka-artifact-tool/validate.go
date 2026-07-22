package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
)

const evidenceTreeMaxPaths = 5000

func init() {
	registerQTool("/tool/validate_analysis", validateAnalysis)
	registerQTool("/tool/submit_analysis", submitAnalysis)
}

type analysisRequest struct {
	Summary       string   `json:"summary"`
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"`
	IsTransient   *bool    `json:"is_transient"`
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files"`
}

type evidenceRequirementID struct {
	skillID string
	groupID string
}

func (id evidenceRequirementID) String() string {
	return id.skillID + ":" + id.groupID
}

type evidenceRequirement struct {
	id             evidenceRequirementID
	signal         string
	group          skills.EvidenceGroup
	candidatePaths []string
}

type validationRequest struct {
	Analysis       analysisRequest `json:"analysis"`
	EvidenceTokens []string        `json:"evidence_tokens"`
}

type submissionRequest struct {
	analysisRequest
	EvidenceTokens []string `json:"evidence_tokens"`
}

func (a analysisRequest) validation() (orka.AnalysisValidation, error) {
	if strings.TrimSpace(a.Summary) == "" {
		return orka.AnalysisValidation{}, fmt.Errorf("summary is required")
	}
	if strings.TrimSpace(a.RootCause) == "" {
		return orka.AnalysisValidation{}, fmt.Errorf("root_cause is required")
	}
	if a.IsTransient == nil {
		return orka.AnalysisValidation{}, fmt.Errorf("is_transient is required")
	}
	switch strings.ToLower(strings.TrimSpace(a.Severity)) {
	case "critical", "high", "medium", "low":
	default:
		return orka.AnalysisValidation{}, fmt.Errorf("severity %q is invalid", a.Severity)
	}
	if strings.TrimSpace(a.SuggestedFix) == "" {
		return orka.AnalysisValidation{}, fmt.Errorf("suggested_fix is required")
	}
	if a.RelevantFiles == nil {
		return orka.AnalysisValidation{}, fmt.Errorf("relevant_files array is required")
	}
	return orka.AnalysisValidation{
		Summary: a.Summary, RootCause: a.RootCause, Severity: a.Severity,
		IsTransient: *a.IsTransient, SuggestedFix: a.SuggestedFix,
		RelevantFiles: append([]string(nil), a.RelevantFiles...),
	}, nil
}

func validateAnalysis(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args validationRequest
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	analysis, err := args.Analysis.validation()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	validateSubmission(env, w, r, analysis, args.EvidenceTokens, "validate_analysis")
}

func submitAnalysis(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args submissionRequest
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	analysis, err := args.analysisRequest.validation()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	validateSubmission(env, w, r, analysis, args.EvidenceTokens, "submit_analysis")
}

func validateSubmission(
	env *toolEnv,
	w http.ResponseWriter,
	r *http.Request,
	analysis orka.AnalysisValidation,
	evidenceTokens []string,
	toolName string,
) {
	set, err := skills.ParseHeader(r.Header.Get(skills.ContractHeader))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	initialEvidence, err := skills.ParseInitialEvidenceHeader(r.Header.Get(skills.InitialEvidenceHeader))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if env.evidence == nil {
		writeToolError(w, http.StatusInternalServerError, "artifact evidence validation is unavailable")
		return
	}
	validationKey := strings.TrimSpace(r.Header.Get(orka.ValidationKeyHeader))
	if validationKey == "" {
		writeToolError(w, http.StatusInternalServerError, "analysis validation key is unavailable")
		return
	}
	taskName := strings.TrimSpace(r.Header.Get(orka.ValidationTaskHeader))
	if taskName == "" {
		writeToolError(w, http.StatusInternalServerError, "analysis Task identity is unavailable")
		return
	}
	minGCSBytes, err := strconv.Atoi(strings.TrimSpace(r.Header.Get(orka.MinGCSBytesHeader)))
	if err != nil || minGCSBytes < 0 {
		http.Error(w, "invalid minimum GCS byte floor", http.StatusBadRequest)
		return
	}

	readPaths := map[string]bool{}
	seenTokens := map[string]bool{}
	gcsBytes := 0
	invalidTokens := 0
	for _, token := range evidenceTokens {
		token = strings.TrimSpace(token)
		if seenTokens[token] {
			continue
		}
		seenTokens[token] = true
		path, bytesFetched, ok := env.evidence.verifyBytes(requestScope(r), token)
		if !ok {
			invalidTokens++
			continue
		}
		readPaths[path] = true
		gcsBytes += bytesFetched
	}

	readBases := map[string]bool{}
	for readPath := range readPaths {
		readBases[path.Base(readPath)] = true
	}
	fields := []string{analysis.RootCause, analysis.Summary, analysis.SuggestedFix}
	fields = append(fields, analysis.RelevantFiles...)
	citations := ai.ArtifactCitations(strings.Join(fields, "\n"))
	present, missing := []string{}, []string{}
	for _, citation := range citations {
		read := readPaths[citation]
		if !strings.Contains(citation, "/") {
			read = readBases[path.Base(citation)]
		}
		if read {
			present = append(present, citation)
		} else {
			missing = append(missing, citation)
		}
	}
	missingEvidence := []string{}
	missingEvidenceCandidates := map[string][]string{}
	evidenceText := analysis.EvidenceText()
	matchedSkills := set.Match(evidenceText)
	requirements := make([]evidenceRequirement, 0)
	seenRequirements := map[evidenceRequirementID]bool{}
	initialEvidenceKeys := make([]string, 0, len(initialEvidence.Requirements))
	for _, initial := range initialEvidence.Requirements {
		id := evidenceRequirementID{skillID: initial.SkillID, groupID: initial.Group.ID}
		if seenRequirements[id] {
			continue
		}
		seenRequirements[id] = true
		initialEvidenceKeys = append(initialEvidenceKeys, id.String())
		requirements = append(requirements, evidenceRequirement{
			id: id, signal: evidenceText, group: initial.Group, candidatePaths: initial.CandidatePaths,
		})
	}
	for _, skill := range matchedSkills {
		for _, group := range skill.RequiredEvidence {
			if !group.Applies(evidenceText) {
				continue
			}
			id := evidenceRequirementID{skillID: skill.ID, groupID: group.ID}
			if seenRequirements[id] {
				continue
			}
			seenRequirements[id] = true
			requirements = append(requirements, evidenceRequirement{id: id, signal: evidenceText, group: group})
		}
	}
	var tree artifactTreeEvidence
	treeChecked := false
	for _, requirement := range requirements {
		if requirement.group.Satisfied(readPaths) {
			continue
		}
		if !treeChecked {
			tree = loadArtifactTreeEvidence(r.Context(), env.browser)
			treeChecked = true
		}
		if tree.completePaths != nil && !requirement.group.Satisfied(tree.completePaths) {
			continue
		}
		key := requirement.id.String()
		missingEvidence = append(missingEvidence, key)
		candidates := requirement.candidatePaths
		if len(candidates) == 0 {
			candidates = requirement.group.CandidatePaths(requirement.signal, tree.paths, evidenceCandidatePathLimit)
		}
		if len(candidates) > 0 {
			missingEvidenceCandidates[key] = candidates
		}
	}
	valid := invalidTokens == 0 && len(missing) == 0 && len(missingEvidence) == 0 && gcsBytes >= minGCSBytes
	result := map[string]any{
		"checked":                 len(citations),
		"present":                 present,
		"missing":                 missing,
		"read_paths":              sortedEvidencePaths(readPaths),
		"invalid_evidence_tokens": invalidTokens,
		"gcs_bytes":               gcsBytes,
		"min_gcs_bytes":           minGCSBytes,
		"matched_skills":          skillIDs(matchedSkills),
		"initial_evidence":        initialEvidenceKeys,
		"missing_evidence":        missingEvidence,
		"all_present":             valid,
	}
	if len(missingEvidenceCandidates) > 0 {
		result["missing_evidence_candidates"] = missingEvidenceCandidates
	}
	if treeChecked && tree.truncated {
		result["artifact_tree_truncated"] = true
	}
	if !valid {
		log.Printf("⚠ %s paths=%d read=%d missing=%d invalid_tokens=%d evidence_missing=%d", toolName, len(analysis.RelevantFiles), len(readPaths), len(missing), invalidTokens, len(missingEvidence))
		writeJSONStatus(w, http.StatusUnprocessableEntity, result)
		return
	}
	result["validation_token"] = orka.AnalysisValidationToken(validationKey, taskName, analysis, gcsBytes)
	log.Printf("✔ %s paths=%d read=%d matched_skills=%d", toolName, len(analysis.RelevantFiles), len(readPaths), len(matchedSkills))
	writeJSON(w, result)
}

type artifactTreeEvidence struct {
	paths         []string
	completePaths map[string]bool
	truncated     bool
}

func loadArtifactTreeEvidence(ctx context.Context, browser artifacts.Browser) artifactTreeEvidence {
	if browser == nil {
		return artifactTreeEvidence{}
	}
	paths, truncated, err := browser.ListTree(ctx, evidenceTreeMaxPaths)
	if err != nil {
		return artifactTreeEvidence{}
	}
	tree := artifactTreeEvidence{paths: paths, truncated: truncated}
	if truncated {
		return tree
	}
	tree.completePaths = make(map[string]bool, len(paths))
	for _, artifactPath := range paths {
		if normalized := normalizeEvidencePath(artifactPath); normalized != "" {
			tree.completePaths[normalized] = true
		}
	}
	return tree
}

func normalizeEvidencePath(p string) string {
	p = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	return strings.TrimPrefix(p, "/")
}

func sortedEvidencePaths(paths map[string]bool) []string {
	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

func skillIDs(matched []skills.Skill) []string {
	ids := make([]string, 0, len(matched))
	for _, skill := range matched {
		ids = append(ids, skill.ID)
	}
	return ids
}
