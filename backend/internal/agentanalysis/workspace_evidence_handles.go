package agentanalysis

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	WorkspaceEvidenceArtifact = "artifact"
	WorkspaceEvidenceSource   = "source"

	WorkspaceEvidenceHandlesAccepted             = "accepted"
	WorkspaceEvidenceHandlesAcceptedWithWarnings = "accepted_with_warnings"
	WorkspaceEvidenceHandlesRejected             = "rejected"

	WorkspaceEvidenceRangeOverflow          = "evidence_range_overflow"
	WorkspaceEvidenceRangeRootInvalid       = "evidence_range_root_invalid"
	WorkspaceEvidenceRangePathInvalid       = "evidence_range_path_invalid"
	WorkspaceEvidenceRangeUnreadable        = "evidence_range_unreadable"
	WorkspaceEvidenceRangeLineInvalid       = "evidence_range_line_invalid"
	WorkspaceEvidenceHandleNoncanonical     = "evidence_handle_noncanonical"
	WorkspaceEvidenceHandleDuplicate        = "evidence_handle_duplicate"
	WorkspaceEvidenceHandleTruncated        = "evidence_handle_truncated"
	WorkspaceEvidenceArtifactHandlesMissing = "evidence_artifact_handles_missing"

	maxWorkspaceEvidenceRanges     = 512
	maxWorkspaceEvidencePerRoot    = 64
	maxWorkspaceFinalizationPrompt = 64 << 10
)

// WorkspaceEvidenceHandleDiagnostics contains bounded content-free build facts.
type WorkspaceEvidenceHandleDiagnostics struct {
	Status                      string   `json:"status,omitempty"`
	ObservedRangeCount          int      `json:"observed_range_count,omitempty"`
	AcceptedArtifactHandleCount int      `json:"accepted_artifact_handle_count,omitempty"`
	AcceptedSourceHandleCount   int      `json:"accepted_source_handle_count,omitempty"`
	DroppedRangeCount           int      `json:"dropped_range_count,omitempty"`
	Truncated                   bool     `json:"truncated,omitempty"`
	Codes                       []string `json:"codes,omitempty"`
}

// WorkspaceEvidenceRange is one exact file range observed by a successful tool.
type WorkspaceEvidenceRange struct {
	Root      string
	Path      string
	LineStart int
	LineEnd   int
}

// WorkspaceEvidenceHandle is one content-free citation selection.
type WorkspaceEvidenceHandle struct {
	ID        string
	Root      string
	Path      string
	LineStart int
	LineEnd   int
}

// BuildWorkspaceEvidenceHandles derives bounded line handles from observed ranges.
func BuildWorkspaceEvidenceHandles(workspaceRoot string, ranges []WorkspaceEvidenceRange) ([]WorkspaceEvidenceHandle, WorkspaceEvidenceHandleDiagnostics, error) {
	diagnostics := WorkspaceEvidenceHandleDiagnostics{ObservedRangeCount: min(len(ranges), maxWorkspaceEvidenceRanges+1)}
	if len(ranges) > maxWorkspaceEvidenceRanges {
		diagnostics.Status = WorkspaceEvidenceHandlesRejected
		diagnostics.DroppedRangeCount = diagnostics.ObservedRangeCount
		diagnostics.Truncated = true
		diagnostics.Codes = []string{WorkspaceEvidenceRangeOverflow}
		return nil, diagnostics, fmt.Errorf("workspace evidence ranges exceed the bound")
	}
	ranges = slices.Clone(ranges)
	for index := range ranges {
		ranges[index].Path = strings.TrimSpace(ranges[index].Path)
		switch {
		case !validWorkspaceEvidenceRoot(ranges[index].Root):
			diagnostics.Status = WorkspaceEvidenceHandlesRejected
			diagnostics.Codes = []string{WorkspaceEvidenceRangeRootInvalid}
			return nil, diagnostics, fmt.Errorf("workspace evidence range root is invalid")
		case !safeWorkspaceSourcePath(ranges[index].Path):
			diagnostics.Status = WorkspaceEvidenceHandlesRejected
			diagnostics.Codes = []string{WorkspaceEvidenceRangePathInvalid}
			return nil, diagnostics, fmt.Errorf("workspace evidence range path is invalid")
		case ranges[index].LineStart < 1 || ranges[index].LineEnd < ranges[index].LineStart:
			diagnostics.Status = WorkspaceEvidenceHandlesRejected
			diagnostics.Codes = []string{WorkspaceEvidenceRangeLineInvalid}
			return nil, diagnostics, fmt.Errorf("workspace evidence range line is invalid")
		}
	}
	sort.Slice(ranges, func(i, j int) bool {
		leftSpan := ranges[i].LineEnd - ranges[i].LineStart
		rightSpan := ranges[j].LineEnd - ranges[j].LineStart
		if leftSpan != rightSpan {
			return leftSpan < rightSpan
		}
		if ranges[i].Root != ranges[j].Root {
			return ranges[i].Root < ranges[j].Root
		}
		if ranges[i].Path != ranges[j].Path {
			return ranges[i].Path < ranges[j].Path
		}
		if ranges[i].LineStart != ranges[j].LineStart {
			return ranges[i].LineStart < ranges[j].LineStart
		}
		return ranges[i].LineEnd < ranges[j].LineEnd
	})
	seen := map[string]bool{}
	counts := map[string]int{}
	var handles []WorkspaceEvidenceHandle
	for _, observed := range ranges {
		root := filepath.Join(workspaceRoot, observed.Root)
		content, err := readWorkspaceText(root, observed.Path, maxWorkspaceFileBytes)
		if err != nil {
			diagnostics.Status = WorkspaceEvidenceHandlesRejected
			diagnostics.Codes = []string{WorkspaceEvidenceRangeUnreadable}
			return nil, diagnostics, fmt.Errorf("workspace evidence range path is unreadable")
		}
		lines := strings.Split(content, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if observed.LineEnd > len(lines) {
			diagnostics.Status = WorkspaceEvidenceHandlesRejected
			diagnostics.Codes = []string{WorkspaceEvidenceRangeLineInvalid}
			return nil, diagnostics, fmt.Errorf("workspace evidence range line is invalid")
		}
		for line := observed.LineStart; line <= observed.LineEnd; line++ {
			if counts[observed.Root] == maxWorkspaceEvidencePerRoot {
				break
			}
			key := observed.Root + "\x00" + observed.Path + "\x00" + strconv.Itoa(line)
			if seen[key] {
				continue
			}
			if _, err := canonicalWorkspaceQuote(content, line, line); err != nil {
				continue
			}
			seen[key] = true
			counts[observed.Root]++
			handles = append(handles, WorkspaceEvidenceHandle{Root: observed.Root, Path: observed.Path, LineStart: line, LineEnd: line})
		}
	}
	sort.Slice(handles, func(i, j int) bool {
		if handles[i].Root != handles[j].Root {
			return handles[i].Root < handles[j].Root
		}
		if handles[i].Path != handles[j].Path {
			return handles[i].Path < handles[j].Path
		}
		return handles[i].LineStart < handles[j].LineStart
	})
	counters := map[string]int{}
	for index := range handles {
		prefix := WorkspaceEvidenceArtifact
		if handles[index].Root == WorkspaceSourceDir {
			prefix = WorkspaceEvidenceSource
		}
		counters[prefix]++
		handles[index].ID = fmt.Sprintf("%s-%03d", prefix, counters[prefix])
	}
	if err := validateWorkspaceEvidenceHandles(handles); err != nil {
		diagnostics.Status = WorkspaceEvidenceHandlesRejected
		diagnostics.Codes = []string{WorkspaceEvidenceHandleNoncanonical}
		return nil, diagnostics, err
	}
	diagnostics.Status = WorkspaceEvidenceHandlesAccepted
	diagnostics.AcceptedArtifactHandleCount = counters[WorkspaceEvidenceArtifact]
	diagnostics.AcceptedSourceHandleCount = counters[WorkspaceEvidenceSource]
	return handles, diagnostics, nil
}

// WorkspaceFinalizationInstruction binds citation IDs to evidence already seen.
func WorkspaceFinalizationInstruction(handles []WorkspaceEvidenceHandle) (string, error) {
	if err := validateWorkspaceEvidenceHandles(handles); err != nil {
		return "", err
	}
	artifactCount := 0
	var lines []string
	for _, handle := range handles {
		mount := WorkspaceArtifactsDir
		if handle.Root == WorkspaceSourceDir {
			mount = WorkspaceSourceDir
		} else {
			artifactCount++
		}
		lines = append(lines, fmt.Sprintf("- %s: %s/%s line %d", handle.ID, mount, handle.Path, handle.LineStart))
	}
	if artifactCount == 0 {
		return "", fmt.Errorf("workspace artifact evidence handles are unavailable")
	}
	instruction := `Finalize the analysis now from evidence already inspected in this session. Use StructuredOutput exactly once. Select only the engine-issued evidence IDs below. Use artifact IDs in artifact_evidence_ids. Use source IDs in source_evidence_ids and relevant_file_ids. The executor reconstructs every path, line range, and quote. Omit source citations and relevant files when no source handle supports the claim.

Available evidence IDs:
` + strings.Join(lines, "\n")
	if len(instruction) > maxWorkspaceFinalizationPrompt {
		return "", fmt.Errorf("workspace finalization instruction exceeds the bound")
	}
	return instruction, nil
}

func validateWorkspaceEvidenceHandles(handles []WorkspaceEvidenceHandle) error {
	if len(handles) > 2*maxWorkspaceEvidencePerRoot {
		return fmt.Errorf("workspace evidence handles exceed the bound")
	}
	seenIDs := map[string]bool{}
	seenRanges := map[string]bool{}
	lastID := ""
	for _, handle := range handles {
		if !validWorkspaceEvidenceRoot(handle.Root) || !validWorkspaceEvidenceID(handle.ID, handle.Root) || !safeWorkspaceSourcePath(handle.Path) || handle.LineStart < 1 || handle.LineEnd != handle.LineStart || seenIDs[handle.ID] {
			return fmt.Errorf("workspace evidence handle is invalid")
		}
		if lastID != "" && handle.ID <= lastID {
			return fmt.Errorf("workspace evidence handles are not canonical")
		}
		key := handle.Root + "\x00" + handle.Path + "\x00" + strconv.Itoa(handle.LineStart)
		if seenRanges[key] {
			return fmt.Errorf("workspace evidence handle range is duplicated")
		}
		seenIDs[handle.ID] = true
		seenRanges[key] = true
		lastID = handle.ID
	}
	return nil
}

func validWorkspaceEvidenceRoot(value string) bool {
	return value == WorkspaceArtifactsDir || value == WorkspaceSourceDir
}

func validWorkspaceEvidenceID(value, root string) bool {
	prefix := WorkspaceEvidenceArtifact
	if root == WorkspaceSourceDir {
		prefix = WorkspaceEvidenceSource
	}
	if !strings.HasPrefix(value, prefix+"-") || len(value) != len(prefix)+4 {
		return false
	}
	for _, r := range value[len(prefix)+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value[len(prefix)+1:] != "000"
}

func workspaceEvidenceHandlesByID(handles []WorkspaceEvidenceHandle) (map[string]WorkspaceEvidenceHandle, error) {
	if err := validateWorkspaceEvidenceHandles(handles); err != nil {
		return nil, err
	}
	result := make(map[string]WorkspaceEvidenceHandle, len(handles))
	for _, handle := range handles {
		result[handle.ID] = handle
	}
	return result, nil
}
