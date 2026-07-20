package main

import (
	"context"
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

const skillAbsenceTreeCap = 5000

func init() {
	registerQTool("/tool/validate_analysis", validateAnalysis)
}

func validateAnalysis(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		Analysis       orka.AnalysisValidation `json:"analysis"`
		EvidenceTokens []string                `json:"evidence_tokens"`
	}
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	set, err := skills.ParseHeader(r.Header.Get(skills.ContractHeader))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(args.Analysis.RootCause) == "" {
		http.Error(w, "analysis.root_cause is required", http.StatusBadRequest)
		return
	}
	if args.Analysis.RelevantFiles == nil {
		http.Error(w, "analysis.relevant_files array is required", http.StatusBadRequest)
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
	for _, token := range args.EvidenceTokens {
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
	fields := []string{args.Analysis.RootCause, args.Analysis.Summary, args.Analysis.SuggestedFix}
	fields = append(fields, args.Analysis.RelevantFiles...)
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
	matchedSkills := set.Match(args.Analysis.EvidenceText())
	var treePaths map[string]bool
	treeChecked := false
	for _, skill := range matchedSkills {
		for _, group := range skill.RequiredEvidence {
			if group.Satisfied(readPaths) {
				continue
			}
			if !treeChecked {
				treePaths = artifactTreeEvidenceSet(r.Context(), env.browser)
				treeChecked = true
			}
			if treePaths != nil && !group.Satisfied(treePaths) {
				continue
			}
			missingEvidence = append(missingEvidence, skill.ID+":"+group.ID)
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
		"missing_evidence":        missingEvidence,
		"all_present":             valid,
	}
	if !valid {
		log.Printf("⚠ validate_analysis paths=%d read=%d missing=%d invalid_tokens=%d evidence_missing=%d", len(args.Analysis.RelevantFiles), len(readPaths), len(missing), invalidTokens, len(missingEvidence))
		writeJSONStatus(w, http.StatusUnprocessableEntity, result)
		return
	}
	result["validation_token"] = orka.AnalysisValidationToken(validationKey, taskName, args.Analysis, gcsBytes)
	log.Printf("✔ validate_analysis paths=%d read=%d matched_skills=%d", len(args.Analysis.RelevantFiles), len(readPaths), len(matchedSkills))
	writeJSON(w, result)
}

func artifactTreeEvidenceSet(ctx context.Context, browser artifacts.Browser) map[string]bool {
	if browser == nil {
		return nil
	}
	paths, truncated, err := browser.ListTree(ctx, skillAbsenceTreeCap)
	if err != nil || truncated {
		return nil
	}
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		if normalized := normalizeEvidencePath(path); normalized != "" {
			set[normalized] = true
		}
	}
	return set
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
