package patterns

import (
	"fmt"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// MergeLastGood applies fresh outcomes and retains exact prior verdicts on failure.
func MergeLastGood(details []models.JobDetail, prior map[string]models.JobDetail, result AnalyzeResult) (models.PatternRefreshReport, error) {
	report := models.PatternRefreshReport{Jobs: map[string]models.PatternRefreshStatus{}}
	for i := range details {
		detail := &details[i]
		status := models.PatternRefreshStatus{State: models.PatternRefreshNotApplicable}
		if !IsEligible(detail) {
			detail.PatternAnalyses = nil
			report.NotApplicable++
			detail.PatternRefresh = &status
			report.Jobs[detail.JobID] = status
			continue
		}
		outcome, ok := result.Outcomes[detail.JobID]
		if !ok {
			status.State = models.PatternRefreshUnavailable
			report.Unavailable++
			previous := prior[detail.JobID]
			if err := retainPriorPattern(detail, previous, &status); err != nil {
				return report, err
			}
			detail.PatternRefresh = &status
			report.Jobs[detail.JobID] = status
			continue
		}
		status.Attempts = outcome.Attempts
		status.Repairs = outcome.Repairs
		if outcome.Succeeded {
			status.State = models.PatternRefreshCurrent
			for j := range detail.PatternAnalyses {
				models.AssignPatternIdentity(&detail.PatternAnalyses[j])
				status.LastSuccessfulAt = detail.PatternAnalyses[j].GeneratedAt
				status.EvidenceAvailable = models.PatternEvidenceAvailable(*detail, detail.PatternAnalyses[j])
			}
			report.Current++
		} else {
			status.FailureCategory = string(outcome.FailureCategory)
			previous := prior[detail.JobID]
			if len(previous.PatternAnalyses) == 0 {
				status.State = models.PatternRefreshFailed
				report.Failed++
				detail.PatternAnalyses = nil
			} else {
				status.State = models.PatternRefreshRetained
				if err := retainPriorPattern(detail, previous, &status); err != nil {
					return report, err
				}
				report.Retained++
			}
		}
		detail.PatternRefresh = &status
		report.Jobs[detail.JobID] = status
	}
	return report, nil
}

func retainPriorPattern(detail *models.JobDetail, previous models.JobDetail, status *models.PatternRefreshStatus) error {
	if len(previous.PatternAnalyses) == 0 {
		detail.PatternAnalyses = nil
		return nil
	}
	for _, pattern := range previous.PatternAnalyses {
		if pattern.JobID != detail.JobID || pattern.ID == "" || pattern.ID != models.PatternID(pattern) || pattern.ContentHash == "" || pattern.ContentHash != models.PatternHash(pattern) {
			return fmt.Errorf("prior pattern identity is invalid for job %s", detail.JobID)
		}
	}
	status.LastSuccessfulAt = previous.PatternAnalyses[0].GeneratedAt
	detail.PatternAnalyses = append([]models.PatternAnalysis(nil), previous.PatternAnalyses...)
	status.EvidenceAvailable = models.PatternEvidenceAvailable(*detail, detail.PatternAnalyses[0])
	return nil
}

// CurrentRecurring returns only fresh systemic patterns for side effects.
func CurrentRecurring(details []models.JobDetail) []models.PatternAnalysis {
	var out []models.PatternAnalysis
	for _, detail := range details {
		if detail.PatternRefresh == nil || detail.PatternRefresh.State != models.PatternRefreshCurrent {
			continue
		}
		for _, pattern := range detail.PatternAnalyses {
			if pattern.Systemic && models.PatternIsActive(pattern) {
				out = append(out, pattern)
			}
		}
	}
	return out
}
