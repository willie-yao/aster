package agentanalysis

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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
	WorkspaceEvidenceHandleTimeout          = "evidence_handle_timeout"
	WorkspaceEvidenceArtifactHandlesMissing = "evidence_artifact_handles_missing"

	maxWorkspaceEvidenceRanges     = 512
	maxWorkspaceEvidencePerRoot    = 64
	maxWorkspaceEvidenceCacheBytes = 64 << 20
	workspaceEvidenceIndexStride   = 1024
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
	SourceID  string
	Path      string
	LineStart int
	LineEnd   int
}

// WorkspaceEvidenceHandle is one content-free citation selection.
type WorkspaceEvidenceHandle struct {
	ID        string
	Root      string
	SourceID  string
	Path      string
	LineStart int
	LineEnd   int
}

// BuildWorkspaceEvidenceHandles derives bounded line handles from observed ranges.
func BuildWorkspaceEvidenceHandles(workspaceRoot string, ranges []WorkspaceEvidenceRange) ([]WorkspaceEvidenceHandle, WorkspaceEvidenceHandleDiagnostics, error) {
	return buildWorkspaceEvidenceHandles(workspaceRoot, ranges, time.Time{}, readWorkspaceText, time.Now)
}

// BuildWorkspaceEvidenceHandlesWithDeadline bounds evidence canonicalization.
func BuildWorkspaceEvidenceHandlesWithDeadline(workspaceRoot string, ranges []WorkspaceEvidenceRange, deadline time.Time) ([]WorkspaceEvidenceHandle, WorkspaceEvidenceHandleDiagnostics, error) {
	return buildWorkspaceEvidenceHandles(workspaceRoot, ranges, deadline, readWorkspaceText, time.Now)
}

type workspaceEvidenceTextReader func(string, string, int64) (string, error)

type workspaceEvidenceFileIndex struct {
	content     string
	lineOffsets []int
	lineCount   int
	cacheBytes  int64
}

func buildWorkspaceEvidenceHandles(workspaceRoot string, ranges []WorkspaceEvidenceRange, deadline time.Time, readText workspaceEvidenceTextReader, now func() time.Time) ([]WorkspaceEvidenceHandle, WorkspaceEvidenceHandleDiagnostics, error) {
	diagnostics := WorkspaceEvidenceHandleDiagnostics{ObservedRangeCount: min(len(ranges), maxWorkspaceEvidenceRanges+1)}
	codes := map[string]bool{}
	addCode := func(code string) { codes[code] = true }
	dropRange := func() {
		if diagnostics.DroppedRangeCount < maxWorkspaceEvidenceRanges+1 {
			diagnostics.DroppedRangeCount++
		}
	}
	finishCodes := func() {
		diagnostics.Codes = diagnostics.Codes[:0]
		for code := range codes {
			diagnostics.Codes = append(diagnostics.Codes, code)
		}
		sort.Strings(diagnostics.Codes)
	}
	reject := func(code, message string) ([]WorkspaceEvidenceHandle, WorkspaceEvidenceHandleDiagnostics, error) {
		addCode(code)
		finishCodes()
		diagnostics.Status = WorkspaceEvidenceHandlesRejected
		return nil, diagnostics, fmt.Errorf("%s", message)
	}
	deadlineExceeded := func() bool { return !deadline.IsZero() && now().After(deadline) }
	if deadlineExceeded() {
		return reject(WorkspaceEvidenceHandleTimeout, "workspace evidence handle construction timed out")
	}

	selectedByRoot := map[string][]WorkspaceEvidenceRange{
		WorkspaceArtifactsDir: {},
		WorkspaceSourceDir:    {},
	}
	type pathValidation struct {
		valid bool
		hard  bool
		size  int64
	}
	validatedPaths := map[string]pathValidation{}
	for index, observed := range ranges {
		if index%64 == 0 && deadlineExceeded() {
			return reject(WorkspaceEvidenceHandleTimeout, "workspace evidence handle construction timed out")
		}
		observed.Path = strings.TrimSpace(observed.Path)
		switch {
		case !validWorkspaceEvidenceRoot(observed.Root):
			return reject(WorkspaceEvidenceRangeRootInvalid, "workspace evidence range root is invalid")
		case !validWorkspaceEvidenceSourceID(observed.Root, observed.SourceID):
			return reject(WorkspaceEvidenceRangeRootInvalid, "workspace evidence range source id is invalid")
		case !safeWorkspaceSourcePath(observed.Path):
			return reject(WorkspaceEvidenceRangePathInvalid, "workspace evidence range path is invalid")
		case observed.LineStart < 1 || observed.LineEnd < observed.LineStart:
			dropRange()
			addCode(WorkspaceEvidenceRangeLineInvalid)
			continue
		}
		pathKey := observed.Root + "\x00" + observed.SourceID + "\x00" + observed.Path
		validation, ok := validatedPaths[pathKey]
		if !ok {
			root := workspaceEvidenceFilesystemRoot(workspaceRoot, observed.Root, observed.SourceID)
			_, info, exists, err := resolveSourcePathWithinRoot(root, filepath.FromSlash(observed.Path), map[string]bool{}, 0)
			validation.hard = err != nil
			validation.valid = err == nil && exists && info != nil && info.Mode().IsRegular()
			if validation.valid {
				validation.size = info.Size()
			}
			validatedPaths[pathKey] = validation
		}
		if validation.hard {
			return reject(WorkspaceEvidenceRangePathInvalid, "workspace evidence range path is invalid")
		}
		if !validation.valid {
			dropRange()
			addCode(WorkspaceEvidenceRangeUnreadable)
			continue
		}
		selected := selectedByRoot[observed.Root]
		if len(selected) == maxWorkspaceEvidenceRanges {
			dropRange()
			diagnostics.Truncated = true
			addCode(WorkspaceEvidenceRangeOverflow)
			addCode(WorkspaceEvidenceHandleTruncated)
			continue
		}
		selectedByRoot[observed.Root] = append(selected, observed)
	}
	less := func(left, right WorkspaceEvidenceRange) bool {
		leftSpan := left.LineEnd - left.LineStart
		rightSpan := right.LineEnd - right.LineStart
		if leftSpan != rightSpan {
			return leftSpan < rightSpan
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.LineStart != right.LineStart {
			return left.LineStart < right.LineStart
		}
		return left.LineEnd < right.LineEnd
	}
	validRanges := make([]WorkspaceEvidenceRange, 0, len(selectedByRoot[WorkspaceArtifactsDir])+len(selectedByRoot[WorkspaceSourceDir]))
	for _, root := range []string{WorkspaceArtifactsDir, WorkspaceSourceDir} {
		selected := selectedByRoot[root]
		sort.Slice(selected, func(i, j int) bool { return less(selected[i], selected[j]) })
		validRanges = append(validRanges, selected...)
	}
	if deadlineExceeded() {
		return reject(WorkspaceEvidenceHandleTimeout, "workspace evidence handle construction timed out")
	}

	seenRanges := map[string]bool{}
	seenLines := map[string]bool{}
	counts := map[string]int{}
	contentCache := map[string]workspaceEvidenceFileIndex{}
	cacheBytes := map[string]int64{}
	var handles []WorkspaceEvidenceHandle
	for index, observed := range validRanges {
		if index%64 == 0 && deadlineExceeded() {
			return reject(WorkspaceEvidenceHandleTimeout, "workspace evidence handle construction timed out")
		}
		if counts[observed.Root] == maxWorkspaceEvidencePerRoot {
			dropRange()
			diagnostics.Truncated = true
			addCode(WorkspaceEvidenceHandleTruncated)
			continue
		}
		rangeKey := observed.Root + "\x00" + observed.SourceID + "\x00" + observed.Path + "\x00" + strconv.Itoa(observed.LineStart) + "\x00" + strconv.Itoa(observed.LineEnd)
		if seenRanges[rangeKey] {
			dropRange()
			addCode(WorkspaceEvidenceHandleDuplicate)
			continue
		}
		seenRanges[rangeKey] = true

		pathKey := observed.Root + "\x00" + observed.SourceID + "\x00" + observed.Path
		cachedFile, cached := contentCache[pathKey]
		if !cached {
			validation := validatedPaths[pathKey]
			if cacheBytes[observed.Root]+validation.size > maxWorkspaceEvidenceCacheBytes {
				dropRange()
				diagnostics.Truncated = true
				addCode(WorkspaceEvidenceHandleTruncated)
				continue
			}
			root := workspaceEvidenceFilesystemRoot(workspaceRoot, observed.Root, observed.SourceID)
			var err error
			content, err := readText(root, observed.Path, maxWorkspaceFileBytes)
			if err != nil {
				if strings.Contains(err.Error(), "unsafe") || strings.Contains(err.Error(), "escapes the root") {
					return reject(WorkspaceEvidenceRangePathInvalid, "workspace evidence range path is invalid")
				}
				dropRange()
				addCode(WorkspaceEvidenceRangeUnreadable)
				continue
			}
			if deadlineExceeded() {
				return reject(WorkspaceEvidenceHandleTimeout, "workspace evidence handle construction timed out")
			}
			var indexErr error
			cachedFile, indexErr = indexWorkspaceEvidenceFile(content, deadlineExceeded)
			if indexErr != nil {
				return reject(WorkspaceEvidenceHandleTimeout, "workspace evidence handle construction timed out")
			}
			if cacheBytes[observed.Root]+cachedFile.cacheBytes > maxWorkspaceEvidenceCacheBytes {
				dropRange()
				diagnostics.Truncated = true
				addCode(WorkspaceEvidenceHandleTruncated)
				continue
			}
			contentCache[pathKey] = cachedFile
			cacheBytes[observed.Root] += cachedFile.cacheBytes
		}
		if observed.LineEnd > cachedFile.lineCount {
			dropRange()
			addCode(WorkspaceEvidenceRangeLineInvalid)
			continue
		}
		before := len(handles)
		for line := observed.LineStart; line <= observed.LineEnd; line++ {
			if deadlineExceeded() {
				return reject(WorkspaceEvidenceHandleTimeout, "workspace evidence handle construction timed out")
			}
			if counts[observed.Root] == maxWorkspaceEvidencePerRoot {
				diagnostics.Truncated = true
				addCode(WorkspaceEvidenceHandleTruncated)
				break
			}
			key := observed.Root + "\x00" + observed.SourceID + "\x00" + observed.Path + "\x00" + strconv.Itoa(line)
			if seenLines[key] {
				addCode(WorkspaceEvidenceHandleDuplicate)
				continue
			}
			quote := workspaceEvidenceLine(cachedFile, line)
			if strings.TrimSpace(quote) == "" || len(quote) > maxCitationQuoteBytes {
				addCode(WorkspaceEvidenceRangeLineInvalid)
				continue
			}
			seenLines[key] = true
			counts[observed.Root]++
			handles = append(handles, WorkspaceEvidenceHandle{Root: observed.Root, SourceID: observed.SourceID, Path: observed.Path, LineStart: line, LineEnd: line})
		}
		if len(handles) == before {
			dropRange()
		}
	}
	sort.Slice(handles, func(i, j int) bool {
		if handles[i].Root != handles[j].Root {
			return handles[i].Root < handles[j].Root
		}
		if handles[i].SourceID != handles[j].SourceID {
			return handles[i].SourceID < handles[j].SourceID
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
	diagnostics.AcceptedArtifactHandleCount = counters[WorkspaceEvidenceArtifact]
	diagnostics.AcceptedSourceHandleCount = counters[WorkspaceEvidenceSource]
	if err := validateWorkspaceEvidenceHandles(handles); err != nil {
		code := WorkspaceEvidenceHandleNoncanonical
		if strings.Contains(err.Error(), "duplicated") {
			code = WorkspaceEvidenceHandleDuplicate
		}
		return reject(code, err.Error())
	}
	if diagnostics.AcceptedArtifactHandleCount == 0 {
		return reject(WorkspaceEvidenceArtifactHandlesMissing, "workspace artifact evidence handles are unavailable")
	}
	finishCodes()
	if len(diagnostics.Codes) == 0 {
		diagnostics.Status = WorkspaceEvidenceHandlesAccepted
	} else {
		diagnostics.Status = WorkspaceEvidenceHandlesAcceptedWithWarnings
	}
	if deadlineExceeded() {
		return reject(WorkspaceEvidenceHandleTimeout, "workspace evidence handle construction timed out")
	}
	return handles, diagnostics, nil
}

func indexWorkspaceEvidenceFile(content string, deadlineExceeded func() bool) (cached workspaceEvidenceFileIndex, err error) {
	cached.content = content
	if content == "" {
		return cached, nil
	}
	cached.lineOffsets = []int{0}
	cached.lineCount = 1
	for index := 0; index < len(content); index++ {
		if index%(64<<10) == 0 && deadlineExceeded() {
			return cached, fmt.Errorf("workspace evidence line indexing timed out")
		}
		if content[index] != '\n' {
			continue
		}
		next := index + 1
		if next == len(content) {
			continue
		}
		cached.lineCount++
		if (cached.lineCount-1)%workspaceEvidenceIndexStride == 0 {
			cached.lineOffsets = append(cached.lineOffsets, next)
		}
	}
	cached.cacheBytes = int64(len(content) + len(cached.lineOffsets)*8)
	return cached, nil
}

func workspaceEvidenceLine(file workspaceEvidenceFileIndex, line int) string {
	block := (line - 1) / workspaceEvidenceIndexStride
	start := file.lineOffsets[block]
	current := block*workspaceEvidenceIndexStride + 1
	for current < line {
		next := strings.IndexByte(file.content[start:], '\n')
		if next < 0 {
			return ""
		}
		start += next + 1
		current++
	}
	end := strings.IndexByte(file.content[start:], '\n')
	if end < 0 {
		return file.content[start:]
	}
	return file.content[start : start+end]
}

// WorkspaceSourceEvidenceCorrectionInstruction requests one bounded source investigation.
func WorkspaceSourceEvidenceCorrectionInstruction() string {
	return `Required source grounding is not yet usable. Continue the evidence investigation with a focused, content-bearing read or grep under sources/<source_id>/. Use only the failure metadata and artifact findings already inspected to choose relevant source. Do not inspect more artifacts, use StructuredOutput, or provide the final analysis. Respond briefly after the source operation completes.`
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
			mount = filepath.ToSlash(filepath.Join(WorkspaceSourcesDir, handle.SourceID))
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
		if !validWorkspaceEvidenceRoot(handle.Root) || !validWorkspaceEvidenceSourceID(handle.Root, handle.SourceID) || !validWorkspaceEvidenceID(handle.ID, handle.Root) || !safeWorkspaceSourcePath(handle.Path) || handle.LineStart < 1 || handle.LineEnd != handle.LineStart || seenIDs[handle.ID] {
			return fmt.Errorf("workspace evidence handle is invalid")
		}
		if lastID != "" && handle.ID <= lastID {
			return fmt.Errorf("workspace evidence handles are not canonical")
		}
		key := handle.Root + "\x00" + handle.SourceID + "\x00" + handle.Path + "\x00" + strconv.Itoa(handle.LineStart)
		if seenRanges[key] {
			return fmt.Errorf("workspace evidence handle range is duplicated")
		}
		seenIDs[handle.ID] = true
		seenRanges[key] = true
		lastID = handle.ID
	}
	return nil
}

func workspaceEvidenceFilesystemRoot(workspaceRoot, root, sourceID string) string {
	if root == WorkspaceSourceDir {
		return filepath.Join(workspaceRoot, WorkspaceSourcesDir, sourceID)
	}
	return filepath.Join(workspaceRoot, root)
}

// ValidWorkspaceSourceID reports whether an ID can select one staged source.
func ValidWorkspaceSourceID(value string) bool { return workspaceSourceID.MatchString(value) }

func validWorkspaceEvidenceSourceID(root, sourceID string) bool {
	if root == WorkspaceSourceDir {
		return ValidWorkspaceSourceID(sourceID)
	}
	return sourceID == ""
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
