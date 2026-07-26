package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

const (
	analysisChatValidationCandidate = "candidate_extraction"
	analysisChatValidationJSON      = "json_validation"
	analysisChatValidationContract  = "response_contract"
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
	candidates := scan.candidates
	stats.CandidateCount = len(candidates)
	if len(candidates) == 0 {
		stats.Category = analysisChatValidationCandidate
		return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("no JSON response object found"))
	}
	var bestErr error
	for index := len(candidates) - 1; index >= 0; index-- {
		reply, err := decodeAnalysisChatReplyCandidate(candidates[index].value, evidence)
		if err == nil {
			if hasTrailingUnrelatedAnalysisChatCandidate(candidates[index], candidates[index+1:], scan.incomplete) {
				stats.Category = analysisChatValidationJSON
				return analysischat.Reply{}, stats, newAnalysisChatValidationError(
					stats.Category, errors.New("response contains trailing unrelated JSON"),
				)
			}
			return reply, stats, nil
		}
		if bestErr == nil || analysisChatValidationCategory(bestErr) == analysisChatValidationJSON &&
			analysisChatValidationCategory(err) != analysisChatValidationJSON {
			bestErr = err
		}
	}
	stats.Category = analysisChatValidationCategory(bestErr)
	return analysischat.Reply{}, stats, bestErr
}

func hasTrailingUnrelatedAnalysisChatCandidate(
	selected analysisChatJSONCandidate,
	trailing []analysisChatJSONCandidate,
	incomplete []analysisChatCandidateState,
) bool {
	for index, candidate := range trailing {
		if strings.Contains(candidate.value, selected.value) || analysisChatCandidateLooksLikeReply(candidate.value) {
			continue
		}
		related := false
		for _, container := range trailing[index+1:] {
			if strings.Contains(container.value, candidate.value) &&
				(analysisChatCandidateLooksLikeReply(container.value) || strings.Contains(container.value, selected.value)) {
				related = true
				break
			}
		}
		if !related {
			for _, container := range incomplete {
				if container.start < candidate.start &&
					(container.start < selected.start || container.start > selected.end) {
					related = true
					break
				}
			}
		}
		if !related {
			return true
		}
	}
	return false
}

func analysisChatCandidateLooksLikeReply(candidate string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(candidate), &fields) != nil {
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
	var reply analysischat.Reply
	decoder := json.NewDecoder(strings.NewReader(candidate))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationJSON, fmt.Errorf("response is not valid analysis-chat JSON: %w", err),
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
	case "explains", "supports", "challenges", "inconclusive":
	default:
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationContract, errors.New("assessment must be explains, supports, challenges, or inconclusive"),
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
				analysisChatValidationCitation, fmt.Errorf("citation %d has an unsafe path", i+1),
			)
		}
		artifactEvidence := evidence[safe]
		if artifactEvidence == nil {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationCitation, fmt.Errorf("citation %d names an artifact not read during this turn", i+1),
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
	if reply.Assessment == "challenges" {
		if reply.ProposedRevision == nil {
			return analysischat.Reply{}, newAnalysisChatValidationError(
				analysisChatValidationContract, errors.New("a challenges response requires a complete proposed_revision"),
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
	} else if reply.ProposedRevision != nil {
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationContract, errors.New("proposed_revision is allowed only for a challenges response"),
		)
	}
	return reply, nil
}

type analysisChatCandidateState struct {
	start int
}

type analysisChatJSONCandidate struct {
	value string
	start int
	end   int
}

type analysisChatCandidateScan struct {
	candidates []analysisChatJSONCandidate
	incomplete []analysisChatCandidateState
}

func analysisChatJSONCandidates(raw string) []string {
	scan := scanAnalysisChatJSONCandidates(raw)
	out := make([]string, len(scan.candidates))
	for index, candidate := range scan.candidates {
		out[index] = candidate.value
	}
	return out
}

func scanAnalysisChatJSONCandidates(raw string) analysisChatCandidateScan {
	stack := make([]int, 0, 16)
	candidates := make([]analysisChatJSONCandidate, 0, 16)
	inString := false
	escaped := false
	overflowDepth := 0
	for index := 0; index < len(raw); index++ {
		ch := raw[index]
		if len(stack) == 0 {
			if ch == '{' {
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
			}
			if len(stack) == 0 {
				inString = false
				escaped = false
			}
		}
	}
	incomplete := make([]analysisChatCandidateState, len(stack))
	for index, start := range stack {
		incomplete[index] = analysisChatCandidateState{start: start}
	}
	return analysisChatCandidateScan{candidates: candidates, incomplete: incomplete}
}
