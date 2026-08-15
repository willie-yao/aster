package models

// PatternRefreshFor returns the refresh status for a job.
func PatternRefreshFor(details []JobDetail, jobID string) *PatternRefreshStatus {
	for i := range details {
		if details[i].JobID == jobID {
			return details[i].PatternRefresh
		}
	}
	return nil
}

// PatternIsCurrent reports whether a job's pattern result is fresh this pass.
func PatternIsCurrent(details []JobDetail, jobID string) bool {
	status := PatternRefreshFor(details, jobID)
	return status == nil || status.State == PatternRefreshCurrent
}

// PatternEvidenceAvailable reports whether every shared build is in this job window.
func PatternEvidenceAvailable(detail JobDetail, pattern PatternAnalysis) bool {
	present := make(map[string]bool, len(detail.Runs))
	for _, run := range detail.Runs {
		present[run.BuildID] = true
	}
	for _, buildID := range pattern.SharedBuilds {
		if !present[buildID] {
			return false
		}
	}
	return len(pattern.SharedBuilds) > 0
}
