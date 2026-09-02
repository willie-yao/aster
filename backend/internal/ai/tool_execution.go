package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/aster/backend/internal/artifacts"
)

// agenticToolBudget caps bytes returned to the model by any single tool
// call. Keeps one runaway response from eating the whole ModelByteBudget.
// 32 KB leaves room for a useful log excerpt plus the JSON envelope.
const agenticToolBudget = 32 * 1024

// dispatchAgenticTool routes one tool call and returns its model-bound envelope.
func dispatchAgenticTool(ctx context.Context, s *agentState, tc modelToolCall) string {
	envelope, _ := dispatchAgenticToolWithPayload(ctx, s, tc)
	return envelope
}

// dispatchAgenticToolWithPayload also returns the uncapped structured payload.
func dispatchAgenticToolWithPayload(ctx context.Context, s *agentState, tc modelToolCall) (string, map[string]interface{}) {
	s.calls++
	if !agenticToolEnabled(s.enabledTools, tc.Function.Name) {
		message := fmt.Sprintf("tool %q is not enabled for this analysis", tc.Function.Name)
		payload := map[string]interface{}{"error": message}
		recordTrace(ctx, TraceEvent{Kind: "tool_call", Tool: tc.Function.Name, Outcome: "disabled", Grep: undispatchedGrepObservation(tc)})
		return toolErrJSON(message), payload
	}
	if s.modelRemaining() <= 0 {
		s.budgetExhausted = true
		recordTrace(ctx, TraceEvent{Kind: "tool_call", Tool: tc.Function.Name, Outcome: "model_budget_exhausted", Grep: undispatchedGrepObservation(tc)})
		message := "model byte budget exhausted; produce final JSON now"
		payload := map[string]interface{}{"error": message}
		return toolErrJSON(message), payload
	}
	if !isRepoTool(tc.Function.Name) && s.gcsRemaining() <= 0 {
		s.budgetExhausted = true
		recordTrace(ctx, TraceEvent{Kind: "tool_call", Tool: tc.Function.Name, Outcome: "gcs_budget_exhausted", Grep: undispatchedGrepObservation(tc)})
		message := "GCS byte budget exhausted; produce final JSON now"
		payload := map[string]interface{}{"error": message}
		return toolErrJSON(message), payload
	}

	env := &tools.Env{
		Browser:    s.browser,
		Sources:    s.sources,
		Cache:      s.cache,
		WebURLBase: s.webURLBase,
	}
	toolCtx := withEvidenceReadSource(ctx, EvidenceReadSourceModelTool)
	result := dispatchToolCall(toolCtx, s.registry, env, tc, s.modelRemaining(), s.gcsRemaining())
	if !isRepoTool(tc.Function.Name) {
		s.gcsBytes += result.BytesFetched
	}
	if result.BudgetExhausted {
		s.budgetExhausted = true
	}
	_, toolFailed := result.Payload["error"]
	toolOutcome := "success"
	if toolFailed {
		toolOutcome = "error"
	}
	grep := grepCallObservation(result.Observation)
	if grep == nil {
		grep = undispatchedGrepObservation(tc)
	}
	if grep != nil && toolFailed {
		grep.Outcome = tools.GrepOutcomeError
	}
	recordTrace(ctx, TraceEvent{Kind: "tool_call", Tool: tc.Function.Name, Outcome: toolOutcome, Bytes: result.BytesFetched, Grep: grep})
	envelope := toolEnvelopeJSON(s, result.Payload)
	visiblePayload := modelVisibleToolPayload(envelope)

	// Record successful artifact reads so critiqueDraft can flag prose
	// citations of files the agent never opened. Only content-fetching
	// tools count; list/find tools don't justify content claims. The
	// "error" key check prevents a failed read from silently satisfying
	// the hallucination gate.
	if isContentFetchingTool(tc.Function.Name) {
		if !toolFailed {
			if p := extractToolPathArg(tc.Function.Arguments); p != "" && visiblePayload != nil {
				s.recordSuccessfulRead(p)
				beforeLines := analysisEvidenceLineSnapshot(s.analysisEvidence, p)
				if _, roomLeft := recordAnalysisChatEvidence(s.analysisEvidence, tc, visiblePayload, s.analysisEvidenceBudget); !roomLeft {
					s.analysisEvidenceFull = true
				}
				visibleSnippets := toolResultSnippets(tc.Function.Name, visiblePayload)
				newPath := false
				if len(visibleSnippets) > 0 {
					newPath = s.recordEvidenceRead(p)
				}
				contentAdded := false
				for _, snippet := range visibleSnippets {
					contentAdded = s.recordEvidenceContent(p, snippet) || contentAdded
				}
				if contentAdded && !newPath {
					s.evidenceRevision++
				}
				s.recordAnalysisEvidenceRevisions(p, beforeLines)
			}
		}
	}
	if !toolFailed && s.sources != nil {
		emitSourceEvidenceObservations(s.sourceObserver, tc.Function.Name, result.Observation)
		if extractToolSourceID(tc.Function.Arguments) == s.sources.PrimaryID() {
			for _, repoPath := range visibleRepoReadPaths(tc, visiblePayload) {
				s.recordSourceRead(repoPath)
			}
			s.recordSourceContent(tc, visiblePayload, result.Observation)
		}
	}

	// Keep the ranked evidence plan alive across turns. Computed after the read
	// is recorded so a group the model just satisfied is not reported unread,
	// and dropped when it would push the envelope past the per-call budget.
	if visiblePayload != nil {
		if unread := s.unreadEvidenceGroupIDs(); len(unread) > 0 {
			result.Payload["unread_evidence_groups"] = unread
			if rebuilt := toolEnvelopeJSON(s, result.Payload); len(rebuilt) <= agenticToolBudget {
				envelope = rebuilt
			} else {
				delete(result.Payload, "unread_evidence_groups")
			}
		}
	}

	return envelope, result.Payload
}

func grepCallObservation(observation any) *tools.GrepCallObservation {
	var value tools.GrepCallObservation
	switch observation := observation.(type) {
	case tools.GrepCallObservation:
		value = observation
	case repotree.GrepObservation:
		value = observation.Call
	default:
		return nil
	}
	value.ReturnedRanges = append([]tools.GrepRangeObservation(nil), value.ReturnedRanges...)
	return &value
}

func undispatchedGrepObservation(tc modelToolCall) *tools.GrepCallObservation {
	switch tc.Function.Name {
	case "grep_artifact":
		args := struct {
			Path         string        `json:"path"`
			ContextLines tools.FlexInt `json:"context_lines"`
			MaxMatches   tools.FlexInt `json:"max_matches"`
		}{ContextLines: -1}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		contextLines, maxMatches := tools.EffectiveGrepLimits(args.ContextLines, args.MaxMatches)
		path, err := artifacts.SafePath(args.Path)
		if err != nil {
			path = args.Path
		}
		filter, supplied, length, redacted := tools.ContentFreePathFilter(path)
		return &tools.GrepCallObservation{
			SelectorID: "artifact-workspace", PathFilter: filter, PathFilterSupplied: supplied,
			PathFilterLength: length, PathFilterRedacted: redacted,
			ContextLines: contextLines, MaxMatches: maxMatches, Outcome: tools.GrepOutcomeError,
			ReturnedRanges: []tools.GrepRangeObservation{},
		}
	case "grep_repo":
		args := struct {
			SourceID     string        `json:"source_id"`
			PathGlob     string        `json:"path_glob"`
			ContextLines tools.FlexInt `json:"context_lines"`
			MaxMatches   tools.FlexInt `json:"max_matches"`
		}{ContextLines: -1}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		contextLines, maxMatches := tools.EffectiveGrepLimits(args.ContextLines, args.MaxMatches)
		filter, supplied, length, redacted := tools.ContentFreePathFilter(args.PathGlob)
		return &tools.GrepCallObservation{
			SelectorID: tools.ContentFreeSelectorID(args.SourceID), PathFilter: filter, PathFilterSupplied: supplied,
			PathFilterLength: length, PathFilterRedacted: redacted,
			ContextLines: contextLines, MaxMatches: maxMatches, Outcome: tools.GrepOutcomeError,
			ReturnedRanges: []tools.GrepRangeObservation{},
		}
	default:
		return nil
	}
}

// unreadEvidenceGroupIDs names the initial-plan groups that have a candidate
// path in this build but no substantive read yet. Bounded so the reminder
// cannot meaningfully grow a tool result.
func (s *agentState) unreadEvidenceGroupIDs() []string {
	var out []string
	for _, group := range s.planCoverage().UnmetGroups {
		if group.GroupID == "" || len(group.CandidatePaths) == 0 {
			continue
		}
		out = append(out, group.GroupID)
		if len(out) == evidenceNudgeMaxGroups {
			break
		}
	}
	return out
}

func analysisEvidenceLineSnapshot(evidence map[string]*analysisChatEvidence, rawPath string) map[int]string {
	path, err := artifacts.SafePath(strings.TrimSpace(rawPath))
	if err != nil || path == "" || evidence[path] == nil {
		return nil
	}
	out := make(map[int]string, len(evidence[path].Lines))
	for line, text := range evidence[path].Lines {
		out[line] = text
	}
	return out
}

func (s *agentState) recordAnalysisEvidenceRevisions(rawPath string, before map[int]string) {
	path, err := artifacts.SafePath(strings.TrimSpace(rawPath))
	if err != nil || path == "" || s.analysisEvidence[path] == nil {
		return
	}
	if s.analysisEvidenceRevision == nil {
		s.analysisEvidenceRevision = map[string]map[int]int{}
	}
	if s.analysisEvidenceRevision[path] == nil {
		s.analysisEvidenceRevision[path] = map[int]int{}
	}
	for line, text := range s.analysisEvidence[path].Lines {
		if previous, ok := before[line]; ok && previous == text {
			continue
		}
		s.analysisEvidenceRevision[path][line] = s.evidenceRevision
	}
}

func modelVisibleToolPayload(envelope string) map[string]interface{} {
	var payload map[string]interface{}
	if json.Unmarshal([]byte(envelope), &payload) != nil {
		return nil
	}
	return payload
}

func agenticToolEnabled(enabledTools []string, name string) bool {
	for _, enabled := range enabledTools {
		if enabled == name {
			return true
		}
	}
	return false
}

// isContentFetchingTool reports whether a tool name is one of the three
// filesystem read primitives that actually return file bytes. Listing
// tools are excluded: a directory listing doesn't justify content claims.
func isContentFetchingTool(name string) bool {
	switch name {
	case "read_artifact", "tail_artifact", "grep_artifact":
		return true
	}
	return false
}

func isRepoTool(name string) bool {
	return name == "list_repo_tree" || name == "read_repo_file" || name == "grep_repo"
}

func emitSourceEvidenceObservations(observer SourceEvidenceObserver, tool string, observation any) {
	if observer == nil {
		return
	}
	switch value := observation.(type) {
	case repotree.ReadObservation:
		observer(SourceEvidenceObservation{SourceID: value.SourceID, Tool: tool, Path: value.Path, LineStart: value.LineStart, LineEnd: value.LineEnd})
	case repotree.GrepObservation:
		for _, match := range value.Matches {
			observer(SourceEvidenceObservation{SourceID: match.SourceID, Tool: tool, Path: match.Path, LineStart: match.LineStart, LineEnd: match.LineEnd})
		}
	}
}

func visibleRepoReadPaths(tc modelToolCall, payload map[string]interface{}) []string {
	if payload == nil {
		return nil
	}
	switch tc.Function.Name {
	case "read_repo_file":
		if _, visible := payload["content"]; visible {
			if p := extractToolPathArg(tc.Function.Arguments); p != "" {
				return []string{p}
			}
		}
	case "grep_repo":
		seen := map[string]bool{}
		var out []string
		if matches, ok := payload["matches"].([]interface{}); ok {
			for _, raw := range matches {
				match, _ := raw.(map[string]interface{})
				p, _ := match["path"].(string)
				if p != "" && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
		return out
	}
	return nil
}

// extractToolPathArg pulls the "path" field out of a content-fetching tool's
// args. Returns "" on parse error or missing field. All content-fetching tools
// use the same `{"path": "..."}` arg shape.
func extractToolSourceID(raw string) string {
	if raw == "" {
		return ""
	}
	var args struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.SourceID)
}

func extractToolPathArg(raw string) string {
	if raw == "" {
		return ""
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.Path)
}

// toolResultSnippets extracts bounded positive evidence from filesystem reads.
// Each grep match remains a separate snippet so distant hits cannot fabricate
// regex adjacency.
func toolResultSnippets(name string, payload map[string]interface{}) []string {
	switch name {
	case "read_artifact", "tail_artifact":
		if content := flattenToolContent(payload["content"]); content != "" {
			return []string{content}
		}
	case "grep_artifact":
		var sections []string
		switch matches := payload["matches"].(type) {
		case []map[string]interface{}:
			for _, match := range matches {
				if content := flattenGrepContext(match["context"]); content != "" {
					sections = append(sections, content)
				}
			}
		case []interface{}:
			for _, raw := range matches {
				match, _ := raw.(map[string]interface{})
				if content := flattenGrepContext(match["context"]); content != "" {
					sections = append(sections, content)
				}
			}
		}
		return sections
	}
	return nil
}

var grepContextLineRE = regexp.MustCompile(`^[> ]\s*\d+:\s?(.*)$`)

func flattenGrepContext(value interface{}) string {
	switch context := value.(type) {
	case string:
		if match := grepContextLineRE.FindStringSubmatch(context); len(match) == 2 {
			if strings.TrimSpace(match[1]) == "" {
				return ""
			}
			return match[1]
		}
		if strings.TrimSpace(context) == "" {
			return ""
		}
		return context
	case []string:
		var sections []string
		for _, line := range context {
			if content := flattenGrepContext(line); content != "" {
				sections = append(sections, content)
			}
		}
		return strings.Join(sections, "\n")
	case []interface{}:
		var sections []string
		for _, item := range context {
			if content := flattenGrepContext(item); content != "" {
				sections = append(sections, content)
			}
		}
		return strings.Join(sections, "\n")
	}
	return ""
}

func flattenToolContent(value interface{}) string {
	switch content := value.(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			return ""
		}
		return content
	case []string:
		var sections []string
		for _, line := range content {
			if strings.TrimSpace(line) != "" {
				sections = append(sections, line)
			}
		}
		return strings.Join(sections, "\n")
	case []interface{}:
		var sections []string
		for _, item := range content {
			if section := flattenToolContent(item); section != "" {
				sections = append(sections, section)
			}
		}
		return strings.Join(sections, "\n")
	}
	return ""
}

// recordSuccessfulRead normalizes a successfully-read path and adds it to
// both the full-path and basename indices. Silent no-op when critique is
// disabled because the maps are nil. Uses the same NormalizeArtifactCitation as
// findUnreadArtifactCitations so writer and reader stay consistent.
func (s *agentState) recordSuccessfulRead(rawPath string) {
	if s.readArtifactsFull == nil && s.readArtifactsBase == nil {
		return
	}
	_, norm := canonicalTrackedArtifactPath(rawPath)
	if norm == "" {
		return
	}
	s.readArtifactsFull[norm] = true
	s.readArtifactsBase[path.Base(norm)] = true
}

func (s *agentState) recordSourceRead(rawPath string) {
	_, norm := canonicalTrackedArtifactPath(rawPath)
	if norm != "" {
		if s.readSourceFull == nil {
			s.readSourceFull = map[string]bool{}
		}
		s.readSourceFull[norm] = true
		if s.sourceOwner != "" && s.sourceName != "" {
			s.readSourceFull[strings.ToLower(s.sourceOwner+"/"+s.sourceName+"/"+norm)] = true
			s.readSourceFull[strings.ToLower("github.com/"+s.sourceOwner+"/"+s.sourceName+"/"+norm)] = true
			s.readSourceFull[strings.ToLower(s.sourceName+"/"+norm)] = true
		}
	}
}

// recordEvidenceRead adds a successful non-empty content read to the set used
// for initial evidence-plan coverage.
func (s *agentState) recordEvidenceRead(rawPath string) bool {
	if _, norm := canonicalTrackedArtifactPath(rawPath); norm != "" {
		if s.evidenceArtifactsFull == nil {
			s.evidenceArtifactsFull = map[string]bool{}
		}
		if !s.evidenceArtifactsFull[norm] {
			s.evidenceArtifactsFull[norm] = true
			s.evidenceRevision++
			return true
		}
	}
	return false
}

func (s *agentState) recordEvidenceSnippets(rawPath string, snippets []string) {
	if len(snippets) == 0 {
		return
	}
	newPath := s.recordEvidenceRead(rawPath)
	contentAdded := false
	for _, snippet := range snippets {
		contentAdded = s.recordEvidenceContent(rawPath, snippet) || contentAdded
	}
	if contentAdded && !newPath {
		s.evidenceRevision++
	}
}

func (s *agentState) recordEvidenceContent(rawPath, content string) bool {
	norm, _ := canonicalTrackedArtifactPath(rawPath)
	if norm == "" || strings.TrimSpace(content) == "" {
		return false
	}
	if s.evidenceContentByPath == nil {
		s.evidenceContentByPath = map[string][]string{}
	}
	for _, existing := range s.evidenceContentByPath[norm] {
		if existing == content {
			return false
		}
	}
	s.evidenceContentByPath[norm] = append(s.evidenceContentByPath[norm], content)
	return true
}

func canonicalTrackedArtifactPath(rawPath string) (string, string) {
	casePath, err := artifacts.SafePath(strings.TrimSpace(rawPath))
	if err != nil || casePath == "" {
		return "", ""
	}
	return casePath, NormalizeArtifactCitation(casePath)
}

func (s *agentState) recordSourceContent(tc modelToolCall, payload map[string]interface{}, observation any) {
	if payload == nil {
		return
	}
	if s.sourceContentByPath == nil {
		s.sourceContentByPath = map[string][]string{}
	}
	add := func(rawPath, content string) {
		_, norm := canonicalTrackedArtifactPath(rawPath)
		if norm == "" || strings.TrimSpace(content) == "" {
			return
		}
		s.sourceContentByPath[norm] = append(s.sourceContentByPath[norm], content)
	}
	addCitationEvidence := func(rawPath, content string) *analysisChatEvidence {
		if s.sourceEvidenceByPath == nil || strings.TrimSpace(content) == "" {
			return nil
		}
		path, err := artifacts.SafePath(strings.TrimSpace(rawPath))
		if err != nil || path == "" {
			return nil
		}
		entry := s.sourceEvidenceByPath[path]
		if entry == nil {
			entry = &analysisChatEvidence{Lines: map[int]string{}}
			s.sourceEvidenceByPath[path] = entry
		}
		appendAnalysisChatEvidenceCandidate(entry, content)
		return entry
	}
	switch tc.Function.Name {
	case "read_repo_file":
		path := extractToolPathArg(tc.Function.Arguments)
		if content, _ := payload["content"].(string); content != "" {
			add(path, content)
			entry := addCitationEvidence(path, content)
			if entry == nil {
				return
			}
			visibleLengthMatches := false
			switch length := payload["length"].(type) {
			case int:
				visibleLengthMatches = length == len(content)
			case float64:
				visibleLengthMatches = length == float64(len(content))
			}
			// JSON replaces invalid UTF-8, so raw byte offsets are valid only
			// when the visible content kept the same byte length.
			if !visibleLengthMatches {
				return
			}
			var read repotree.ReadObservation
			switch value := observation.(type) {
			case repotree.ReadObservation:
				read = value
			case *repotree.ReadObservation:
				if value != nil {
					read = *value
				}
			}
			safePath, err := artifacts.SafePath(strings.TrimSpace(path))
			observationPath, observationErr := artifacts.SafePath(strings.TrimSpace(read.Path))
			if err != nil || observationErr != nil || safePath == "" || observationPath != safePath ||
				read.SourceID != s.sources.PrimaryID() || read.LineStart <= 0 || read.LineEnd < read.LineStart ||
				read.ByteStart < 0 || read.ByteEnd <= read.ByteStart || read.ByteEnd > len(content) {
				return
			}
			lines := strings.Split(content[read.ByteStart:read.ByteEnd], "\n")
			if strings.HasSuffix(content[read.ByteStart:read.ByteEnd], "\n") {
				lines = lines[:len(lines)-1]
			}
			if len(lines) != read.LineEnd-read.LineStart+1 {
				return
			}
			for i, line := range lines {
				entry.Lines[read.LineStart+i] = line
			}
		}
	case "grep_repo":
		for _, match := range analysisChatEvidenceMatches(payload["matches"]) {
			path, _ := match["path"].(string)
			content := flattenGrepContext(match["context"])
			add(path, content)
			addCitationEvidence(path, content)
		}
	}
}

func toolEnvelopeJSON(s *agentState, payload map[string]interface{}) string {
	payload["remaining_model_bytes"] = s.modelRemaining()
	payload["remaining_gcs_bytes"] = s.gcsRemaining()
	payload["elapsed_seconds"] = int(time.Since(s.startTime).Seconds())
	out, _ := json.Marshal(payload)
	return capJSON(string(out))
}

func toolErrJSON(msg string) string {
	out, _ := json.Marshal(map[string]string{"error": msg})
	return string(out)
}

// capJSON trims a tool result to agenticToolBudget so a single response can't
// blow the per-call budget. Returned as-is when within budget.
func capJSON(s string) string {
	if len(s) <= agenticToolBudget {
		return s
	}
	return s[:agenticToolBudget] + `..."truncated":true}`
}
