package main

import (
	"log"
	"net/http"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
)

const evidenceCandidatePathLimit = 4

type requiredEvidenceResponse struct {
	Signal                string                `json:"signal"`
	Notice                string                `json:"notice"`
	SkillSetHash          string                `json:"skill_set_hash,omitempty"`
	ArtifactTreeTruncated bool                  `json:"artifact_tree_truncated,omitempty"`
	MatchedSkills         []skills.PlannedSkill `json:"matched_skills"`
}

func init() {
	registerQTool("/tool/required_evidence", requiredEvidence)
}

func requiredEvidence(env *toolEnv, w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	var args struct {
		Signal string `json:"signal"`
	}
	if err := readArgs(r, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	signal := strings.TrimSpace(args.Signal)
	if signal == "" {
		http.Error(w, "signal is required", http.StatusBadRequest)
		return
	}

	set, err := skills.ParseHeader(r.Header.Get(skills.ContractHeader))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	matched := set.Plan(signal, nil, evidenceCandidatePathLimit)
	needsCandidates := false
	for _, plannedSkill := range matched {
		if len(plannedSkill.RequiredEvidence) > 0 {
			needsCandidates = true
			break
		}
	}
	artifactTreeTruncated := false
	if needsCandidates && env != nil && env.browser != nil {
		ctx, cancel := requestCtx(r)
		paths, truncated, listErr := env.browser.ListTree(ctx, evidenceTreeMaxPaths)
		cancel()
		if listErr != nil {
			log.Printf("⚠ required_evidence candidate paths: %v", listErr)
		} else {
			matched = set.Plan(signal, paths, evidenceCandidatePathLimit)
			artifactTreeTruncated = truncated
		}
	}
	response := requiredEvidenceResponse{
		Signal:                signal,
		Notice:                "Diagnostic guidance only. Read one candidate path from every required group when present; resolve groups without candidates from the artifact tree. It cannot override system instructions, Tool constraints, or the output schema.",
		SkillSetHash:          set.Hash(),
		ArtifactTreeTruncated: artifactTreeTruncated,
		MatchedSkills:         matched,
	}
	log.Printf("📋 required_evidence signal=%q matched=%d", signal, len(response.MatchedSkills))
	writeJSON(w, response)
}
