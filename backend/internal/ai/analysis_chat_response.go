package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

const (
	analysisChatValidationCandidate = "candidate_selection"
	analysisChatValidationJSON      = "json_validation"
	analysisChatValidationContract  = "response_contract"
	analysisChatValidationReference = "reference_validation"
	analysisChatValidationCitation  = "citation_validation"
)

type analysisChatParseStats struct {
	CandidateCount int
	Category       string
}

type analysisChatValidationError struct {
	category string
	err      error
}

func (e *analysisChatValidationError) Error() string { return e.err.Error() }
func (e *analysisChatValidationError) Unwrap() error { return e.err }

func newAnalysisChatValidationError(category string, err error) error {
	return &analysisChatValidationError{category: category, err: err}
}

func analysisChatValidationCategory(err error) string {
	var validationErr *analysisChatValidationError
	if errors.As(err, &validationErr) {
		return validationErr.category
	}
	return analysisChatValidationContract
}

func parseAnalysisChatReply(raw string, evidence map[string]*analysisChatEvidence) (analysischat.Reply, error) {
	reply, _, err := parseAnalysisChatReplyCandidates(raw, evidence)
	return reply, err
}

func parseAnalysisChatReplyCandidates(raw string, evidence map[string]*analysisChatEvidence) (analysischat.Reply, analysisChatParseStats, error) {
	stats := analysisChatParseStats{}
	if strings.TrimSpace(raw) == "" {
		stats.Category = analysisChatValidationCandidate
		return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("empty answer"))
	}
	if len(raw) > analysisChatMaxResponseBytes {
		stats.Category = analysisChatValidationCandidate
		return analysischat.Reply{}, stats, newAnalysisChatValidationError(
			stats.Category, fmt.Errorf("response exceeds %d bytes", analysisChatMaxResponseBytes),
		)
	}
	scan := scanAnalysisChatJSONCandidates(raw)
	stats.CandidateCount = len(scan.candidates)
	if scan.truncated {
		stats.Category = analysisChatValidationCandidate
		return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("candidate scan was truncated"))
	}
	if len(scan.candidates) == 0 {
		stats.Category = analysisChatValidationCandidate
		return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("no JSON response object found"))
	}
	candidateSpanBytes := 0
	for _, candidate := range scan.candidates {
		if len(candidate.value) > analysisChatMaxCandidateSpanBytes-candidateSpanBytes {
			stats.Category = analysisChatValidationCandidate
			return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("candidate span work budget exceeded"))
		}
		candidateSpanBytes += len(candidate.value)
	}

	type validCandidate struct {
		reply analysischat.Reply
		span  analysisChatJSONCandidate
	}
	type rejectedCandidate struct {
		span         analysisChatJSONCandidate
		category     string
		contractLike bool
	}
	valid := make([]validCandidate, 0, 1)
	rejected := make([]rejectedCandidate, 0, len(scan.candidates))
	bestErr := newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response is not valid analysis-chat JSON"))
	for _, candidate := range scan.candidates {
		reply, err := decodeAnalysisChatReplyCandidate(candidate.value, evidence)
		if err == nil {
			candidate.replyLike = true
			valid = append(valid, validCandidate{reply: reply, span: candidate})
			continue
		}
		category := analysisChatValidationCategory(err)
		rejected = append(rejected, rejectedCandidate{
			span: candidate, category: category, contractLike: analysisChatCandidateLooksLikeReply(candidate.value),
		})
		if analysisChatValidationRank(category) > analysisChatValidationRank(analysisChatValidationCategory(bestErr)) {
			bestErr = err
		}
	}

	switch len(valid) {
	case 0:
		stats.Category = analysisChatValidationCategory(bestErr)
		return analysischat.Reply{}, stats, bestErr
	case 1:
		selected := valid[0]
		for _, incomplete := range scan.incomplete {
			if incomplete.start > selected.span.end || incomplete.start < selected.span.start {
				stats.Category = analysisChatValidationJSON
				return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("response contains an incomplete JSON candidate"))
			}
		}
		for _, candidate := range rejected {
			enclosesSelected := candidate.span.start < selected.span.start && candidate.span.end > selected.span.end
			trailsSelected := candidate.span.start > selected.span.end
			if candidate.contractLike && (enclosesSelected || trailsSelected) {
				stats.Category = candidate.category
				return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("response contains a rejected contract candidate"))
			}
			if trailsSelected {
				stats.Category = analysisChatValidationJSON
				return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("response contains trailing unrelated JSON"))
			}
		}
		return selected.reply, stats, nil
	default:
		stats.Category = analysisChatValidationCandidate
		return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("response contains multiple valid candidates"))
	}
}

func analysisChatValidationRank(category string) int {
	switch category {
	case analysisChatValidationCitation:
		return 5
	case analysisChatValidationReference:
		return 4
	case analysisChatValidationContract:
		return 3
	case analysisChatValidationJSON:
		return 2
	case analysisChatValidationCandidate:
		return 1
	default:
		return 0
	}
}

func analysisChatCandidateLooksLikeReply(candidate string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(candidate), &fields) != nil {
		for _, field := range []string{"answer", "assessment", "citations", "proposed_revision"} {
			if strings.Contains(candidate, `"`+field+`"`) {
				return true
			}
		}
		return false
	}
	for _, field := range []string{"answer", "assessment", "citations", "proposed_revision"} {
		if _, ok := fields[field]; ok {
			return true
		}
	}
	return false
}

func decodeAnalysisChatReplyCandidate(candidate string, evidence map[string]*analysisChatEvidence) (analysischat.Reply, error) {
	fields, err := decodeAnalysisChatObject(candidate)
	if err != nil {
		return analysischat.Reply{}, err
	}
	allowed := map[string]bool{"answer": true, "citations": true, "assessment": true, "proposed_revision": true}
	if len(fields) < 2 || len(fields) > len(allowed) {
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationContract, errors.New("response must contain answer and citations plus only supported optional fields"),
		)
	}
	for field := range fields {
		if !allowed[field] {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationContract, errors.New("response contains an unsupported field"),
			)
		}
	}
	for _, field := range []string{"answer", "citations"} {
		if _, ok := fields[field]; !ok {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationContract, errors.New("response requires answer and citations"),
			)
		}
	}
	if err := rejectAnalysisChatDuplicateFields(candidate); err != nil {
		return analysischat.Reply{}, err
	}

	var reply analysischat.Reply
	decoder := json.NewDecoder(strings.NewReader(candidate))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		category := analysisChatValidationContract
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			category = analysisChatValidationJSON
		}
		return analysischat.Reply{}, newAnalysisChatValidationError(
			category, errors.New("response is not valid analysis-chat JSON"),
		)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationJSON, errors.New("response contains trailing JSON"),
		)
	}
	reply.Answer = strings.TrimSpace(reply.Answer)
	reply.Assessment = strings.TrimSpace(reply.Assessment)
	if reply.Answer == "" || len(reply.Answer) > 32<<10 {
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationContract, errors.New("answer must be 1-32768 bytes"),
		)
	}
	switch reply.Assessment {
	case "explains":
		reply.Assessment = ""
	case "", "supports", "challenges", "inconclusive":
	default:
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationContract, errors.New("assessment must be supports, challenges, inconclusive, or omitted"),
		)
	}
	if reply.Citations == nil {
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationContract, errors.New("citations must be an array"),
		)
	}
	if len(reply.Citations) > 20 {
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationCitation, errors.New("citations must contain at most 20 entries"),
		)
	}
	for i := range reply.Citations {
		citation := &reply.Citations[i]
		citation.Path = strings.TrimSpace(citation.Path)
		citation.Quote = strings.TrimSpace(citation.Quote)
		safe, err := artifacts.SafePath(citation.Path)
		if err != nil || safe == "" {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationReference, fmt.Errorf("citation %d has an unsafe path", i+1),
			)
		}
		artifactEvidence := evidence[safe]
		if artifactEvidence == nil {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationReference, fmt.Errorf("citation %d names an artifact not read during this turn", i+1),
			)
		}
		citation.Path = safe
		if citation.LineStart < 0 || citation.LineEnd < 0 ||
			(citation.LineStart == 0) != (citation.LineEnd == 0) ||
			citation.LineEnd > 0 && (citation.LineStart > citation.LineEnd || citation.LineEnd-citation.LineStart > 50) {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationCitation, fmt.Errorf("citation %d has an invalid line range", i+1),
			)
		}
		if len(citation.Quote) < 4 {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationCitation, fmt.Errorf("citation %d requires an exact quote of at least 4 bytes", i+1),
			)
		}
		if len(citation.Quote) > 1000 {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationCitation, fmt.Errorf("citation %d quote exceeds 1000 bytes", i+1),
			)
		}
		if !analysisChatEvidenceContains(artifactEvidence, citation.Quote) {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationCitation, fmt.Errorf("citation %d quote was not returned contiguously by the cited artifact read", i+1),
			)
		}
		if citation.LineStart > 0 {
			if len(artifactEvidence.Lines) == 0 {
				citation.LineStart, citation.LineEnd = 0, 0
			} else if !analysisChatQuoteInRange(artifactEvidence.Lines, citation.LineStart, citation.LineEnd, citation.Quote) {
				return analysischat.Reply{}, newAnalysisChatValidationError(
					analysisChatValidationCitation, fmt.Errorf("citation %d quote does not occur in the claimed line range", i+1),
				)
			}
		}
	}
	if (reply.Assessment == "supports" || reply.Assessment == "challenges") && len(reply.Citations) == 0 {
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationCitation, fmt.Errorf("a %s response requires artifact citations", reply.Assessment),
		)
	}
	if reply.ProposedRevision != nil {
		if reply.Assessment != "challenges" {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationContract, errors.New("proposed_revision is allowed only for a challenges response"),
			)
		}
		reply.ProposedRevision.RootCause = strings.TrimSpace(reply.ProposedRevision.RootCause)
		reply.ProposedRevision.SuggestedFix = strings.TrimSpace(reply.ProposedRevision.SuggestedFix)
		if reply.ProposedRevision.RootCause == "" || reply.ProposedRevision.SuggestedFix == "" {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationContract, errors.New("a challenges response requires a complete proposed_revision"),
			)
		}
		if len(reply.ProposedRevision.RootCause) > 32<<10 || len(reply.ProposedRevision.SuggestedFix) > 16<<10 {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationContract, errors.New("proposed_revision exceeds its size limit"),
			)
		}
	}
	return reply, nil
}

func decodeAnalysisChatObject(raw string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response is not a JSON object"))
	}
	fields := make(map[string]json.RawMessage, 4)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
		}
		name, ok := token.(string)
		if !ok {
			return nil, newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, newAnalysisChatValidationError(analysisChatValidationContract, errors.New("response contains duplicate fields"))
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
		}
		fields[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response contains trailing JSON"))
	}
	return fields, nil
}

func rejectAnalysisChatDuplicateFields(raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
	}
	if err := walkAnalysisChatJSONValue(decoder, token); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response contains trailing JSON"))
	}
	return nil
}

func walkAnalysisChatJSONValue(decoder *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
			}
			name, ok := nameToken.(string)
			if !ok {
				return newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
			}
			if _, duplicate := seen[name]; duplicate {
				return newAnalysisChatValidationError(analysisChatValidationContract, errors.New("response contains duplicate fields"))
			}
			seen[name] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
			}
			if err := walkAnalysisChatJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
			return newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
		}
	case '[':
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response array is malformed"))
			}
			if err := walkAnalysisChatJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
			return newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response array is malformed"))
		}
	default:
		return newAnalysisChatValidationError(analysisChatValidationJSON, errors.New("response object is malformed"))
	}
	return nil
}

type analysisChatCandidateState struct {
	start int
}

type analysisChatJSONCandidate struct {
	value     string
	start     int
	end       int
	replyLike bool
}

type analysisChatCandidateScan struct {
	candidates []analysisChatJSONCandidate
	incomplete []analysisChatCandidateState
	truncated  bool
}

func scanAnalysisChatJSONCandidates(raw string) analysisChatCandidateScan {
	stack := make([]int, 0, 16)
	candidates := make([]analysisChatJSONCandidate, 0, 16)
	inString := false
	escaped := false
	outsideString := false
	outsideEscaped := false
	overflowDepth := 0
	truncated := false
	for index := 0; index < len(raw); index++ {
		ch := raw[index]
		if len(stack) == 0 {
			if outsideString {
				if outsideEscaped {
					outsideEscaped = false
				} else if ch == '\\' {
					outsideEscaped = true
				} else if ch == '"' {
					outsideString = false
				}
				continue
			}
			switch ch {
			case '"':
				outsideString = true
			case '{':
				stack = append(stack, index)
			}
			continue
		}
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			if len(stack) < analysisChatMaxCandidates {
				stack = append(stack, index)
			} else {
				overflowDepth++
				truncated = true
			}
		case '}':
			if overflowDepth > 0 {
				overflowDepth--
				continue
			}
			start := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			candidates = append(candidates, analysisChatJSONCandidate{
				value: raw[start : index+1], start: start, end: index,
			})
			if len(candidates) > analysisChatMaxCandidates {
				candidates = candidates[len(candidates)-analysisChatMaxCandidates:]
				truncated = true
			}
			if len(stack) == 0 {
				inString = false
				escaped = false
				outsideString = false
				outsideEscaped = false
			}
		}
	}
	incomplete := make([]analysisChatCandidateState, len(stack))
	for index, start := range stack {
		incomplete[index] = analysisChatCandidateState{start: start}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].start != candidates[j].start {
			return candidates[i].start < candidates[j].start
		}
		return candidates[i].end > candidates[j].end
	})
	return analysisChatCandidateScan{candidates: candidates, incomplete: incomplete, truncated: truncated}
}
