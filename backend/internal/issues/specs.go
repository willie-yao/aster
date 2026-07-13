package issues

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

// BuildInput carries everything the spec builder needs to turn findings into
// issue specs.
type BuildInput struct {
	Report       models.FlakinessReport
	JobDetails   []models.JobDetail
	Triggers     []string // project.IssueTrigger* values to include
	Labels       []string
	DashboardURL string // branding.site_url without a trailing slash
}

// BuildSpecs assembles the issue specs for the enabled triggers. Each spec
// embeds a hidden marker derived from its key for idempotent dedup.
func BuildSpecs(in BuildInput) []IssueSpec {
	site := strings.TrimRight(in.DashboardURL, "/")
	var specs []IssueSpec

	if hasTrigger(in.Triggers, project.IssueTriggerPatterns) {
		for _, pa := range in.Report.RecurringPatterns {
			if !pa.Systemic || pa.JobID == "" {
				continue
			}
			specs = append(specs, patternSpec(pa, site, in.Labels))
		}
	}

	if hasTrigger(in.Triggers, project.IssueTriggerPersistent) {
		ai := buildAILookup(in.JobDetails)
		for _, tf := range in.Report.PersistentFailures {
			if tf.ConsecutiveFailures < 3 {
				continue
			}
			summary, rootCause := ai[aiKey(tf.JobID, tf.TestName)].Summary, ai[aiKey(tf.JobID, tf.TestName)].RootCause
			specs = append(specs, persistentSpec(tf, summary, rootCause, site, in.Labels))
		}
	}

	return specs
}

func patternSpec(pa models.PatternAnalysis, site string, labels []string) IssueSpec {
	key := KeyPrefixPattern + pa.JobID
	jobURL := jobLink(site, pa.JobID)

	var b strings.Builder
	fmt.Fprintf(&b, "A recurring failure pattern was detected across **%d recent builds** of [`%s`](%s) (confidence: **%s**).\n\n",
		pa.BuildsAnalyzed, pa.Subject, jobURL, pa.Confidence)
	if pa.SharedRootCause != "" {
		fmt.Fprintf(&b, "### Shared root cause\n\n%s\n\n", pa.SharedRootCause)
	}
	if pa.SuggestedFix != "" {
		fmt.Fprintf(&b, "### Suggested fix\n\n%s\n\n", pa.SuggestedFix)
	}
	if len(pa.SharedBuilds) > 0 {
		b.WriteString("### Affected builds\n\n")
		for _, bid := range pa.SharedBuilds {
			fmt.Fprintf(&b, "- [%s](%s?run=%s)\n", bid, jobURL, url.QueryEscape(bid))
		}
		b.WriteString("\n")
	}
	if pa.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", pa.Summary)
	}
	b.WriteString(footer(key))

	title := fmt.Sprintf("[%s] Recurring failure pattern", pa.Subject)
	return IssueSpec{Key: key, Title: clampTitle(title), Body: b.String(), Labels: labels}
}

func persistentSpec(tf models.TestFlakiness, summary, rootCause, site string, labels []string) IssueSpec {
	key := KeyPrefixPersistent + tf.JobID + "::" + tf.TestName
	lastBuild := ""
	if tf.LastFailure != nil {
		lastBuild = tf.LastFailure.BuildID
	}
	testURL := testLink(site, tf.JobID, tf.TestName, lastBuild)

	var b strings.Builder
	fmt.Fprintf(&b, "Test [`%s`](%s) has failed in **%d consecutive runs** of job `%s`.\n\n",
		tf.TestName, testURL, tf.ConsecutiveFailures, tf.JobName)
	if rootCause != "" {
		fmt.Fprintf(&b, "### Root cause\n\n%s\n\n", rootCause)
	} else if summary != "" {
		fmt.Fprintf(&b, "### Summary\n\n%s\n\n", summary)
	}
	if tf.LastFailure != nil && tf.LastFailure.FailureMessage != "" {
		fmt.Fprintf(&b, "### Latest failure\n\n```\n%s\n```\n\n", textutil.Truncate(tf.LastFailure.FailureMessage, 800))
	}
	b.WriteString(footer(key))

	title := fmt.Sprintf("[%s] Persistent failure: %s", tf.JobName, tf.TestName)
	return IssueSpec{Key: key, Title: clampTitle(title), Body: b.String(), Labels: labels}
}

// footer appends the standard provenance line and the hidden dedup marker.
func footer(key string) string {
	return fmt.Sprintf("---\n_Filed automatically by [prow-ai-dashboard]. It updates and resolves on its own as builds change._\n\n%s\n", markerFor(key))
}

func jobLink(site, jobID string) string {
	return fmt.Sprintf("%s/job/%s", site, url.PathEscape(jobID))
}

func testLink(site, jobID, testName, buildID string) string {
	u := fmt.Sprintf("%s/job/%s/test/%s", site, url.PathEscape(jobID), url.PathEscape(testName))
	if buildID != "" {
		u += "?run=" + url.QueryEscape(buildID)
	}
	return u
}

func clampTitle(s string) string {
	const max = 240
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func hasTrigger(triggers []string, name string) bool {
	for _, t := range triggers {
		if t == name {
			return true
		}
	}
	return false
}

// AI root-cause lookup mirrors notify.buildAILookup while keeping this package
// self-contained.

type aiEntry struct {
	Summary   string
	RootCause string
}

func aiKey(jobID, testName string) string { return jobID + "::" + testName }

func buildAILookup(jobDetails []models.JobDetail) map[string]aiEntry {
	lookup := make(map[string]aiEntry)
	for _, jd := range jobDetails {
		for _, run := range jd.Runs {
			for _, tc := range run.TestCases {
				if tc.Status != "failed" {
					continue
				}
				key := aiKey(jd.JobID, tc.Name)
				if _, exists := lookup[key]; exists {
					continue // keep the first entry, since runs are newest-first
				}
				var entry aiEntry
				if tc.AISummary != nil {
					entry.Summary = tc.AISummary.Summary
				}
				if tc.AIAnalysis != nil {
					entry.RootCause = tc.AIAnalysis.RootCause
				}
				if entry.Summary != "" || entry.RootCause != "" {
					lookup[key] = entry
				}
			}
		}
	}
	return lookup
}
