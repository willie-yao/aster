package analysisexecutor

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
)

const (
	maxOpenCodeTelemetryBytes        = 4 << 20
	maxOpenCodeTelemetryEvents       = 4096
	maxOpenCodeToolNames             = 64
	maxOpenCodeFieldBytes            = 512
	maxOpenCodeStreamBytes           = 64 << 10
	maxTelemetryParseTime            = 2 * time.Second
	maxOpenCodeEvidenceRangesPerRoot = 512
)

type boundedCapture struct {
	mu        sync.Mutex
	remaining int
	truncated bool
}

func newBoundedCapture(limit int) *boundedCapture { return &boundedCapture{remaining: limit} }

func (b *boundedCapture) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(data) > b.remaining {
		b.truncated = true
		b.remaining = 0
	} else {
		b.remaining -= len(data)
	}
	return len(data), nil
}

func (b *boundedCapture) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

type openCodeMessage struct {
	Info struct {
		Role  string                 `json:"role"`
		Error *openCodeErrorEnvelope `json:"error"`
	} `json:"info"`
	Parts []openCodePart `json:"parts"`
}

type openCodeReadDisplay struct {
	Type      string `json:"type"`
	Path      string `json:"path"`
	LineStart int    `json:"lineStart"`
	LineEnd   int    `json:"lineEnd"`
}

type openCodePart struct {
	Type     string   `json:"type"`
	Tool     string   `json:"tool"`
	Cost     *float64 `json:"cost"`
	Auto     *bool    `json:"auto"`
	Overflow *bool    `json:"overflow"`
	Tokens   *struct {
		Input  *int `json:"input"`
		Output *int `json:"output"`
		Cache  *struct {
			Read *int `json:"read"`
		} `json:"cache"`
	} `json:"tokens"`
	State struct {
		Status   string          `json:"status"`
		Error    string          `json:"error"`
		Input    json.RawMessage `json:"input"`
		Output   string          `json:"output"`
		Metadata struct {
			Matches *int                 `json:"matches"`
			Display *openCodeReadDisplay `json:"display"`
		} `json:"metadata"`
	} `json:"state"`
}

type toolAggregate struct {
	count    int
	failures int
	denied   int
}

type openCodeEvidenceFacts struct {
	ArtifactToolCalls      int
	SourceToolCalls        int
	ArtifactToolAttempts   int
	SourceToolAttempts     int
	SourceToolFailures     int
	UnrootedToolAttempts   int
	OtherToolCalls         int
	NonStructuredToolCalls int
	StructuredOutputCalls  int
	EvidenceRanges         []agentanalysis.WorkspaceEvidenceRange
	EvidenceHandles        []agentanalysis.WorkspaceEvidenceHandle
	EvidenceDiagnostics    agentanalysis.WorkspaceEvidenceHandleDiagnostics
	EvidenceExtraObserved  int
	EvidenceExtraDropped   int
	EvidenceExtraTruncated bool
	EvidenceIssueCodes     map[string]bool
	EvidenceRejected       bool
	EvidenceRetainedByRoot map[string]int
	EvidencePathSafe       map[string]bool
	ParseDeadline          time.Time
}

func parseOpenCodeTelemetry(raw []byte) (agentanalysis.WorkspaceUsage, agentanalysis.WorkspaceOpenCodeTelemetry, error) {
	usage, telemetry, _, err := parseOpenCodeTelemetryForWorkspace(raw, "")
	return usage, telemetry, err
}

func parseOpenCodeTelemetryForWorkspace(raw []byte, workDir string) (agentanalysis.WorkspaceUsage, agentanalysis.WorkspaceOpenCodeTelemetry, openCodeEvidenceFacts, error) {
	started := time.Now()
	unavailable := agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryMalformed}
	telemetry := agentanalysis.WorkspaceOpenCodeTelemetry{
		Status: agentanalysis.WorkspaceTelemetryMalformed, StructuredOutputRetriesKnown: true,
	}
	facts := openCodeEvidenceFacts{ParseDeadline: started.Add(maxTelemetryParseTime)}
	if len(raw) == 0 || len(raw) > maxOpenCodeTelemetryBytes {
		return unavailable, telemetry, facts, fmt.Errorf("telemetry payload is empty or oversized")
	}
	var messages []openCodeMessage
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&messages); err != nil {
		return unavailable, telemetry, facts, fmt.Errorf("decode telemetry")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return unavailable, telemetry, facts, fmt.Errorf("telemetry contains trailing data")
	}
	if len(messages) > maxOpenCodeTelemetryEvents {
		return unavailable, telemetry, facts, fmt.Errorf("telemetry event count exceeds the bound")
	}
	tools := map[string]*toolAggregate{}
	telemetry.ProviderRequestsKnown = true
	usage := agentanalysis.WorkspaceUsage{Available: true, Status: agentanalysis.WorkspaceTelemetryAvailable}
	costKnown := true
	cost := 0.0
	positiveCost := false
	incompleteUsage := false
	events := 0
	toolParts := 0
	for messageIndex, message := range messages {
		events++
		if events > maxOpenCodeTelemetryEvents || time.Since(started) > maxTelemetryParseTime {
			return unavailable, telemetry, facts, fmt.Errorf("telemetry parsing exceeded the bound")
		}
		if message.Info.Role != "assistant" && message.Info.Role != "user" {
			return unavailable, telemetry, facts, fmt.Errorf("telemetry message role is invalid")
		}
		if message.Info.Role != "assistant" && message.Info.Error != nil {
			return unavailable, telemetry, facts, fmt.Errorf("telemetry error is attached to a non-assistant message")
		}
		messageFailure := false
		if message.Info.Role == "assistant" && message.Info.Error != nil {
			sanitized, err := sanitizeOpenCodeError(message.Info.Error)
			if err != nil {
				return unavailable, telemetry, facts, fmt.Errorf("telemetry error field is invalid")
			}
			telemetry.Error = sanitized
			messageFailure = true
			switch message.Info.Error.Name {
			case "StructuredOutputError":
				telemetry.StructuredOutputErrors++
			case "ContextOverflowError":
				telemetry.ContextLimit = true
			}
		}
		messageStarts, messageFinishes, messageToolParts, messageActiveTools := 0, 0, 0, 0
		for _, part := range message.Parts {
			events++
			if events > maxOpenCodeTelemetryEvents || time.Since(started) > maxTelemetryParseTime {
				return unavailable, telemetry, facts, fmt.Errorf("telemetry parsing exceeded the bound")
			}
			if (part.Type == "step-start" || part.Type == "step-finish" || part.Type == "tool") && message.Info.Role != "assistant" {
				return unavailable, telemetry, facts, fmt.Errorf("assistant telemetry part is attached to a non-assistant message")
			}
			if part.Type == "compaction" && message.Info.Role != "user" {
				return unavailable, telemetry, facts, fmt.Errorf("compaction telemetry part is attached to a non-user message")
			}
			switch part.Type {
			case "step-start":
				telemetry.StepsUsed++
				telemetry.ProviderRequests++
				messageStarts++
			case "step-finish":
				messageFinishes++
				if part.Tokens == nil || part.Tokens.Input == nil || part.Tokens.Output == nil || part.Tokens.Cache == nil || part.Tokens.Cache.Read == nil || part.Cost == nil {
					return unavailable, telemetry, facts, fmt.Errorf("telemetry step usage is incomplete")
				}
				usage.ModelRequests++
				if !addTelemetryCount(&usage.InputTokens, *part.Tokens.Input) || !addTelemetryCount(&usage.CachedInputTokens, *part.Tokens.Cache.Read) || !addTelemetryCount(&usage.OutputTokens, *part.Tokens.Output) {
					return unavailable, telemetry, facts, fmt.Errorf("telemetry token count is invalid")
				}
				if *part.Cost < 0 {
					costKnown = false
				} else {
					cost += *part.Cost
					positiveCost = positiveCost || *part.Cost > 0
				}
			case "tool":
				toolParts++
				messageToolParts++
				if !validTelemetryToolName(part.Tool) {
					return unavailable, telemetry, facts, fmt.Errorf("telemetry tool name is invalid")
				}
				aggregate := tools[part.Tool]
				if aggregate == nil {
					if len(tools) >= maxOpenCodeToolNames {
						return unavailable, telemetry, facts, fmt.Errorf("telemetry tool count exceeds the bound")
					}
					aggregate = &toolAggregate{}
					tools[part.Tool] = aggregate
				}
				aggregate.count++
				if part.Tool == "StructuredOutput" {
					facts.StructuredOutputCalls++
				} else {
					facts.NonStructuredToolCalls++
				}
				root := openCodeEvidenceToolRoot(part.Tool, part.State.Input, workDir)
				if part.Tool == "read" || part.Tool == "grep" {
					switch root {
					case agentanalysis.WorkspaceArtifactsDir:
						facts.ArtifactToolAttempts++
					case agentanalysis.WorkspaceSourceDir:
						facts.SourceToolAttempts++
						if part.State.Status != "completed" {
							facts.SourceToolFailures++
						}
					default:
						facts.UnrootedToolAttempts++
					}
				} else if part.Tool != "StructuredOutput" {
					facts.OtherToolCalls++
				}
				switch part.State.Status {
				case "completed":
					if err := recordOpenCodeEvidenceTool(&facts, part.Tool, part.State.Input, part.State.Output, part.State.Metadata.Matches, part.State.Metadata.Display, workDir); err != nil {
						return unavailable, telemetry, facts, err
					}
				case "error":
					if len(part.State.Error) > maxOpenCodeFieldBytes {
						return unavailable, telemetry, facts, fmt.Errorf("telemetry tool error exceeds the bound for %s (%d bytes)", part.Tool, len(part.State.Error))
					}
					aggregate.failures++
					telemetry.ToolFailureCount++
					if deniedToolError(part.State.Error) {
						aggregate.denied++
						telemetry.DeniedToolCount++
					}
				case "pending", "running":
					messageActiveTools++
					aggregate.failures++
					telemetry.ToolFailureCount++
				default:
					return unavailable, telemetry, facts, fmt.Errorf("telemetry tool state is invalid")
				}
			}
		}
		if messageFailure {
			streamProven := unknownErrorProvesResponseStream(telemetry.Error.Classification)
			if messageStarts == 0 {
				incompleteUsage = true
				if telemetry.Error.Name != "UnknownError" || streamProven {
					telemetry.ProviderRequests++
				} else {
					telemetry.ProviderRequestsKnown = false
				}
			}
			applyOpenCodeErrorLifecycle(&telemetry.Error, telemetry.ProviderRequests, toolParts, messageStarts, messageToolParts, messageActiveTools)
		}
		if message.Info.Role == "assistant" {
			if messageFinishes > messageStarts {
				return unavailable, telemetry, facts, fmt.Errorf("telemetry step usage is inconsistent")
			}
			if messageStarts > messageFinishes {
				autoCompaction := messageStarts-messageFinishes == 1 && nextMessageIsOverflowCompaction(messages, messageIndex)
				if messageStarts-messageFinishes != 1 || (!messageFailure && !autoCompaction) {
					return unavailable, telemetry, facts, fmt.Errorf("telemetry step usage is inconsistent")
				}
				incompleteUsage = true
				if autoCompaction {
					telemetry.ContextLimit = true
				}
			}
		}
	}
	if usage.ModelRequests > telemetry.StepsUsed || (telemetry.StepsUsed == 0 && !telemetry.Error.Available) {
		return unavailable, telemetry, facts, fmt.Errorf("telemetry step usage is inconsistent")
	}
	if incompleteUsage || telemetry.StepsUsed == 0 || usage.InputTokens == 0 && usage.OutputTokens == 0 {
		usage = agentanalysis.WorkspaceUsage{Status: agentanalysis.WorkspaceTelemetryUnavailable}
	} else {
		usage.CostAvailable = costKnown && positiveCost
		if usage.CostAvailable {
			usage.CostUSD = strconv.FormatFloat(cost, 'f', 8, 64)
		}
	}
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := tools[name]
		telemetry.Tools = append(telemetry.Tools, agentanalysis.WorkspaceToolTelemetry{Name: name, Count: value.count, Failures: value.failures, Denied: value.denied})
	}
	if workDir != "" {
		handles, diagnostics, err := agentanalysis.BuildWorkspaceEvidenceHandlesWithDeadline(workDir, facts.EvidenceRanges, facts.ParseDeadline)
		diagnostics = mergeOpenCodeEvidenceDiagnostics(diagnostics, facts)
		telemetry.EvidenceHandles = diagnostics
		facts.EvidenceDiagnostics = diagnostics
		if facts.EvidenceRejected {
			handles = nil
			err = fmt.Errorf("workspace evidence ranges were rejected")
		}
		if err != nil && diagnostics.Status != agentanalysis.WorkspaceEvidenceHandlesRejected {
			return unavailable, telemetry, facts, fmt.Errorf("telemetry evidence handles are invalid")
		}
		facts.EvidenceHandles = handles
	}
	telemetry.Available = true
	telemetry.Status = agentanalysis.WorkspaceTelemetryAvailable
	telemetry.EventCount = events
	return usage, telemetry, facts, nil
}

func mergeOpenCodeEvidenceDiagnostics(value agentanalysis.WorkspaceEvidenceHandleDiagnostics, facts openCodeEvidenceFacts) agentanalysis.WorkspaceEvidenceHandleDiagnostics {
	value.ObservedRangeCount = min(value.ObservedRangeCount+facts.EvidenceExtraObserved, 513)
	value.DroppedRangeCount = min(value.DroppedRangeCount+facts.EvidenceExtraDropped, 513)
	value.Truncated = value.Truncated || facts.EvidenceExtraTruncated
	codes := map[string]bool{}
	for _, code := range value.Codes {
		codes[code] = true
	}
	for code := range facts.EvidenceIssueCodes {
		codes[code] = true
	}
	value.Codes = value.Codes[:0]
	for code := range codes {
		value.Codes = append(value.Codes, code)
	}
	sort.Strings(value.Codes)
	switch {
	case facts.EvidenceRejected:
		value.Status = agentanalysis.WorkspaceEvidenceHandlesRejected
	case value.Status == agentanalysis.WorkspaceEvidenceHandlesAccepted && len(value.Codes) > 0:
		value.Status = agentanalysis.WorkspaceEvidenceHandlesAcceptedWithWarnings
	}
	return value
}

func recordOpenCodeEvidenceIssue(facts *openCodeEvidenceFacts, code string, observed, dropped int, truncated, rejected bool) {
	if facts.EvidenceIssueCodes == nil {
		facts.EvidenceIssueCodes = map[string]bool{}
	}
	facts.EvidenceIssueCodes[code] = true
	facts.EvidenceExtraObserved = min(facts.EvidenceExtraObserved+max(observed, 0), 513)
	facts.EvidenceExtraDropped = min(facts.EvidenceExtraDropped+max(dropped, 0), 513)
	facts.EvidenceExtraTruncated = facts.EvidenceExtraTruncated || truncated
	facts.EvidenceRejected = facts.EvidenceRejected || rejected
}

func appendOpenCodeEvidenceRange(facts *openCodeEvidenceFacts, value agentanalysis.WorkspaceEvidenceRange) {
	if facts.EvidenceRetainedByRoot == nil {
		facts.EvidenceRetainedByRoot = map[string]int{}
	}
	if facts.EvidenceRetainedByRoot[value.Root] == maxOpenCodeEvidenceRangesPerRoot {
		recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangeOverflow, 1, 1, true, false)
		facts.EvidenceIssueCodes[agentanalysis.WorkspaceEvidenceHandleTruncated] = true
		return
	}
	facts.EvidenceRetainedByRoot[value.Root]++
	facts.EvidenceRanges = append(facts.EvidenceRanges, value)
}

func openCodeEvidencePathIsSafe(facts *openCodeEvidenceFacts, workDir, root, relative, absolute string) bool {
	if facts.EvidencePathSafe == nil {
		facts.EvidencePathSafe = map[string]bool{}
	}
	key := root + "\x00" + relative
	if safe, ok := facts.EvidencePathSafe[key]; ok {
		return safe
	}
	base, err := filepath.EvalSymlinks(filepath.Join(workDir, root))
	if err != nil {
		facts.EvidencePathSafe[key] = false
		return false
	}
	if _, err := os.Lstat(absolute); os.IsNotExist(err) {
		facts.EvidencePathSafe[key] = true
		return true
	} else if err != nil {
		facts.EvidencePathSafe[key] = false
		return false
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		facts.EvidencePathSafe[key] = false
		return false
	}
	rel, err := filepath.Rel(base, real)
	safe := err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	facts.EvidencePathSafe[key] = safe
	return safe
}

func applyOpenCodeErrorLifecycle(value *agentanalysis.WorkspaceOpenCodeErrorTelemetry, observedProviderRequests, observedToolParts, messageStarts, messageToolParts, activeTools int) {
	if value == nil || value.Name != "UnknownError" {
		return
	}
	streamObserved := unknownErrorProvesResponseStream(value.Classification)
	providerObserved := observedProviderRequests > 0 || streamObserved
	if providerObserved {
		value.BeforeProviderRequest = openCodeTelemetryBool(false)
	}
	if observedToolParts > 0 {
		value.BeforeFirstTool = openCodeTelemetryBool(false)
	} else if providerObserved {
		value.BeforeFirstTool = openCodeTelemetryBool(true)
	}
	if activeTools > 0 {
		value.DuringToolExecution = openCodeTelemetryBool(true)
	} else if streamObserved || messageStarts > 0 && messageToolParts == 0 {
		value.DuringToolExecution = openCodeTelemetryBool(false)
	}
	if streamObserved {
		value.DuringStreamProcessing = openCodeTelemetryBool(true)
	} else if activeTools > 0 {
		value.DuringStreamProcessing = openCodeTelemetryBool(false)
	}
	if value.DuringSessionPersistence == nil && (streamObserved || activeTools > 0) {
		value.DuringSessionPersistence = openCodeTelemetryBool(false)
	}
}

func unknownErrorProvesResponseStream(classification string) bool {
	return classification == "response_stream"
}

func openCodeTelemetryBool(value bool) *bool {
	return &value
}

func openCodeEvidenceToolRoot(tool string, raw json.RawMessage, workDir string) string {
	if workDir == "" || (tool != "read" && tool != "grep") || len(raw) == 0 || len(raw) > 16<<10 {
		return ""
	}
	var input struct {
		FilePath string `json:"filePath"`
		Path     string `json:"path"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return ""
	}
	candidate := input.FilePath
	if tool == "grep" {
		candidate = input.Path
	}
	root, _, _, ok := openCodeEvidenceLocation(workDir, candidate, true)
	if !ok {
		return ""
	}
	return root
}

func recordOpenCodeEvidenceTool(facts *openCodeEvidenceFacts, tool string, raw json.RawMessage, output string, matches *int, display *openCodeReadDisplay, workDir string) error {
	if tool == "StructuredOutput" {
		return nil
	}
	if workDir == "" || (tool != "read" && tool != "grep") {
		return nil
	}
	if len(raw) == 0 || len(raw) > 16<<10 {
		return fmt.Errorf("telemetry tool input is invalid or oversized")
	}
	var input struct {
		FilePath string `json:"filePath"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return fmt.Errorf("telemetry tool input is invalid")
	}
	candidate := input.FilePath
	if tool == "grep" {
		candidate = input.Path
		if matches != nil && *matches == 0 {
			return nil
		}
	}
	root, relative, absolute, ok := openCodeEvidenceLocation(workDir, candidate, true)
	if !ok {
		recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangePathInvalid, 1, 1, false, true)
		return nil
	}
	if !openCodeEvidencePathIsSafe(facts, workDir, root, relative, absolute) {
		recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangePathInvalid, 1, 1, false, true)
		return nil
	}
	validEvidence := false
	switch tool {
	case "read":
		if display == nil {
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangeLineInvalid, 1, 1, false, false)
			break
		}
		if display.Type != "file" || display.LineStart < 1 || display.LineEnd < display.LineStart {
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangeLineInvalid, 1, 1, false, false)
			break
		}
		displayRoot, displayRelative, displayAbsolute, displayOK := openCodeEvidenceLocation(workDir, display.Path, false)
		if !displayOK || displayRoot != root {
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangePathInvalid, 1, 1, false, true)
			break
		}
		if !openCodeEvidencePathIsSafe(facts, workDir, displayRoot, displayRelative, displayAbsolute) {
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangePathInvalid, 1, 1, false, true)
			break
		}
		info, statErr := os.Stat(absolute)
		if statErr != nil || !info.Mode().IsRegular() {
			appendOpenCodeEvidenceRange(facts, agentanalysis.WorkspaceEvidenceRange{Root: root, Path: relative, LineStart: display.LineStart, LineEnd: display.LineEnd})
			break
		}
		if !sameOpenCodeEvidenceFile(absolute, displayAbsolute) {
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangePathInvalid, 1, 1, false, true)
			break
		}
		appendOpenCodeEvidenceRange(facts, agentanalysis.WorkspaceEvidenceRange{Root: root, Path: displayRelative, LineStart: display.LineStart, LineEnd: display.LineEnd})
		validEvidence = true
	case "grep":
		if matches == nil || *matches < 1 {
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangeLineInvalid, 1, 1, false, false)
			break
		}
		if output == "" {
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangeLineInvalid, *matches, *matches, false, false)
			break
		}
		if len(output) > maxOpenCodeTelemetryBytes {
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangeOverflow, 513, 513, true, false)
			break
		}
		valid, invalid, rejected, parseErr := openCodeGrepEvidenceRanges(output, workDir, root, facts, facts.ParseDeadline, func(value agentanalysis.WorkspaceEvidenceRange) {
			appendOpenCodeEvidenceRange(facts, value)
		})
		if parseErr != nil {
			return parseErr
		}
		validEvidence = valid > 0
		if rejected {
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangePathInvalid, 1, 1, false, true)
		}
		if invalid > 0 || valid+invalid != *matches {
			missing := max(*matches-valid, invalid)
			recordOpenCodeEvidenceIssue(facts, agentanalysis.WorkspaceEvidenceRangeLineInvalid, missing, missing, false, false)
		}
	}
	if !facts.EvidenceRejected && validEvidence {
		switch root {
		case agentanalysis.WorkspaceArtifactsDir:
			facts.ArtifactToolCalls++
		case agentanalysis.WorkspaceSourceDir:
			facts.SourceToolCalls++
		}
	}
	return nil
}

func openCodeGrepEvidenceRanges(output, workDir, wantRoot string, facts *openCodeEvidenceFacts, deadline time.Time, retain func(agentanalysis.WorkspaceEvidenceRange)) (int, int, bool, error) {
	currentPath := ""
	valid := 0
	invalid := 0
	lines := 0
	for line := range strings.SplitSeq(output, "\n") {
		lines++
		if lines%256 == 0 && !deadline.IsZero() && time.Now().After(deadline) {
			return valid, invalid, false, fmt.Errorf("telemetry parsing exceeded the bound")
		}
		if strings.HasPrefix(line, "  Line ") {
			if currentPath == "" {
				invalid++
				continue
			}
			rest := strings.TrimPrefix(line, "  Line ")
			separator := strings.Index(rest, ":")
			if separator < 1 {
				invalid++
				continue
			}
			lineNumber, err := strconv.Atoi(rest[:separator])
			if err != nil || lineNumber < 1 {
				invalid++
				continue
			}
			valid++
			retain(agentanalysis.WorkspaceEvidenceRange{Root: wantRoot, Path: currentPath, LineStart: lineNumber, LineEnd: lineNumber})
			continue
		}
		if strings.HasSuffix(line, ":") && !strings.HasPrefix(line, " ") {
			candidate := strings.TrimSuffix(line, ":")
			root, relative, absolute, ok := openCodeEvidenceLocation(workDir, candidate, false)
			if !ok || root != wantRoot || !openCodeEvidencePathIsSafe(facts, workDir, root, relative, absolute) {
				return valid, invalid, true, nil
			}
			currentPath = relative
		}
	}
	return valid, invalid, false, nil
}

func openCodeEvidenceLocation(workDir, candidate string, allowDirectory bool) (string, string, string, bool) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", "", "", false
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(workDir, candidate)
	}
	candidate = filepath.Clean(candidate)
	for _, root := range []string{agentanalysis.WorkspaceArtifactsDir, agentanalysis.WorkspaceSourceDir} {
		base := filepath.Clean(filepath.Join(workDir, root))
		relative, err := filepath.Rel(base, candidate)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == "." && !allowDirectory {
			continue
		}
		return root, filepath.ToSlash(relative), candidate, true
	}
	return "", "", "", false
}

func sameOpenCodeEvidenceFile(left, right string) bool {
	leftReal, leftErr := filepath.EvalSymlinks(left)
	rightReal, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftReal) == filepath.Clean(rightReal)
}

func nextMessageIsOverflowCompaction(messages []openCodeMessage, index int) bool {
	if index+1 >= len(messages) || messages[index+1].Info.Role != "user" {
		return false
	}
	for _, part := range messages[index+1].Parts {
		if part.Type == "compaction" && part.Auto != nil && *part.Auto && part.Overflow != nil && *part.Overflow {
			return true
		}
	}
	return false
}

func recognizedOpenCodeError(value string) bool {
	switch value {
	case "ProviderAuthError", "UnknownError", "MessageOutputLengthError", "MessageAbortedError", "StructuredOutputError", "ContextOverflowError", "ContentFilterError", "APIError":
		return true
	default:
		return false
	}
}

func addTelemetryCount(total *int, value int) bool {
	const maxTelemetryCount = int(1 << 50)
	if value < 0 || *total < 0 || value > maxTelemetryCount-*total {
		return false
	}
	*total += value
	return true
}

func validTelemetryToolName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_.:-", r) {
			continue
		}
		return false
	}
	return true
}

func deniedToolError(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "permission") && (strings.Contains(value, "denied") || strings.Contains(value, "rejected"))
}
