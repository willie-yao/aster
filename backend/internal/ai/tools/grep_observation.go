package tools

import (
	"regexp"
	"strings"
)

const (
	GrepOutcomeMatched     = "matched"
	GrepOutcomeZeroMatches = "zero_matches"
	GrepOutcomeError       = "error"
)

var contentFreePathFilterRE = regexp.MustCompile(`^[A-Za-z0-9._/*+@-]+$`)

// GrepRangeObservation identifies source or artifact lines returned to the model.
type GrepRangeObservation struct {
	SelectorID string `json:"selector_id"`
	Path       string `json:"path"`
	LineStart  int    `json:"line_start"`
	LineEnd    int    `json:"line_end"`
}

// GrepCallObservation is content-free telemetry for one grep tool call.
type GrepCallObservation struct {
	SelectorID         string `json:"selector_id"`
	PathFilter         string `json:"path_filter,omitempty"`
	PathFilterSupplied bool   `json:"path_filter_supplied"`
	PathFilterLength   int    `json:"path_filter_length,omitempty"`
	PathFilterRedacted bool   `json:"path_filter_redacted,omitempty"`
	ContextLines       int    `json:"context_lines"`
	MaxMatches         int    `json:"max_matches"`
	MatchCount         int    `json:"match_count"`
	FilesAttempted     int    `json:"files_attempted"`
	FilesScanned       int    `json:"files_scanned"`
	FileReadErrors     int    `json:"file_read_errors"`
	// FileScanTruncated reports that scanned content was cut short, so a low
	// match count does not prove the pattern is absent.
	FileScanTruncated bool `json:"file_scan_truncated,omitempty"`
	// ResultTruncated reports that matches beyond the returned set exist.
	ResultTruncated bool `json:"result_truncated,omitempty"`
	// RangesTruncated reports that ReturnedRanges was capped by the trace
	// writer, independently of what the tool returned to the model.
	RangesTruncated bool                   `json:"ranges_truncated,omitempty"`
	Outcome         string                 `json:"outcome"`
	ReturnedRanges  []GrepRangeObservation `json:"returned_ranges"`
}

// EffectiveGrepLimits resolves the shared grep defaults and maximums.
func EffectiveGrepLimits(contextLines, maxMatches FlexInt) (int, int) {
	context := contextLines.Int()
	if context < 0 {
		context = 2
	}
	if context > 5 {
		context = 5
	}
	matches := maxMatches.Int()
	if matches <= 0 {
		matches = 30
	}
	if matches > 100 {
		matches = 100
	}
	return context, matches
}

// ContentFreePathFilter retains path-shaped filters and redacts prose-like values.
func ContentFreePathFilter(raw string) (value string, supplied bool, length int, redacted bool) {
	value = strings.TrimSpace(raw)
	if value == "" {
		return "", false, 0, false
	}
	length = len(value)
	if length > 256 || !contentFreePathFilterRE.MatchString(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "..") || !strings.ContainsAny(value, "/*.") {
		return "", true, length, true
	}
	return value, true, length, false
}

// ContentFreeSelectorID retains only syntactically valid source selectors.
func ContentFreeSelectorID(raw string) string {
	value := strings.TrimSpace(raw)
	if !validSourceID(value) {
		return ""
	}
	return value
}
