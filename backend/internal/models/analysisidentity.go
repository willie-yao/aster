package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

// TestAnalysisContentHash fingerprints one published test analysis and its source evidence.
func TestAnalysisContentHash(testCase TestCase) string {
	analysis := testCase.AIAnalysis
	if analysis == nil {
		return ""
	}
	fileLinks := make([]string, 0, len(analysis.FileLinks))
	for file, link := range analysis.FileLinks {
		fileLinks = append(fileLinks, file+"\x00"+link)
	}
	slices.Sort(fileLinks)
	payload, _ := json.Marshal(struct {
		Name, Source, SuiteName, ClassName, JUnitFile, Status string
		FailureMessage, FailureBody, FailureLocation          string
		GeneratedAt, RootCause, Severity, SuggestedFix, Mode  string
		ModelHash, PromptHash, CacheGeneration                string
		RelevantFiles, FileLinks                              []string
		Citations                                             []EvidenceCitation
		CauseLocation                                         *AnalysisCauseLocation
		CritiquePassed                                        bool
		CritiqueVersion                                       int
	}{
		testCase.Name, testCase.Source, testCase.SuiteName, testCase.ClassName, testCase.JUnitFile, testCase.Status,
		testCase.FailureMessage, testCase.FailureBody, testCase.FailureLocation,
		analysis.GeneratedAt, analysis.RootCause, analysis.Severity, analysis.SuggestedFix, analysis.Mode,
		analysis.ModelHash, analysis.PromptHash, analysis.CacheGeneration,
		slices.Clone(analysis.RelevantFiles), fileLinks, slices.Clone(analysis.EvidenceCitations),
		analysis.CauseLocation,
		analysis.CritiquePassed, analysis.CritiqueVersion,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
