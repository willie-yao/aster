package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/artifacts"
)

// analysisChatMaxQuoteBytes bounds one citation quote. The engine attributes the
// quote from its own recorded tool output, so this is a clamp on that text, not
// a judgement on the model's. It exists to keep the downstream budgets
// satisfiable and is set as high as those budgets allow: fix generation rejects
// a context over 64 KiB built from up to 16 citations, and the chat store
// rejects state over 64 MiB.
// TestAnalysisChatQuoteCapFitsDownstreamBudgets pins that arithmetic.
const analysisChatMaxQuoteBytes = 2000

// analysisChatMaxQuoteLines bounds how many lines one citation may cover,
// whether the model named a line range or the engine resolved a locator.
const analysisChatMaxQuoteLines = 50

// analysisChatMaxAnswerBytes bounds the answer text, both when the model
// returns a contract-shaped reply and when the engine salvages one.
const analysisChatMaxAnswerBytes = 32 << 10

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
	// EvidenceGate is set when the selected reply was degraded to unverified.
	EvidenceGate string
	// EvidenceDetail is engine-generated repair text for the corrective round.
	EvidenceDetail string
	// ValidationDetail names the specific rule a failed turn tripped. It is
	// engine-generated and never carries model or provider output.
	ValidationDetail string
}

// analysisChatEvidenceFailure is a soft gate failure. It degrades a reply to
// unverified instead of rejecting the turn. Detail is engine-generated and
// never carries model or provider output.
type analysisChatEvidenceFailure struct {
	Gate   string
	Detail string
}

type analysisChatValidationError struct {
	category string
	err      error
}

func (e *analysisChatValidationError) Error() string                    { return e.err.Error() }
func (e *analysisChatValidationError) Unwrap() error                    { return e.err }
func (e *analysisChatValidationError) StructuredValidationCode() string { return e.category }

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

func parseAnalysisChatReplyCandidates(
	raw string,
	evidence map[string]*analysisChatEvidence,
) (analysischat.Reply, analysisChatParseStats, error) {
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
		reply   analysischat.Reply
		failure *analysisChatEvidenceFailure
		span    analysisChatJSONCandidate
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
		reply, failure, err := decodeAnalysisChatReplyCandidate(candidate.value, evidence)
		if err == nil {
			candidate.replyLike = true
			valid = append(valid, validCandidate{reply: reply, failure: failure, span: candidate})
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

	// A verified candidate wins over a degraded one. The degraded candidates join
	// the rejected set so a trailing or enclosing one still invalidates the pick.
	if clean := slices.DeleteFunc(slices.Clone(valid), func(candidate validCandidate) bool {
		return candidate.failure != nil
	}); len(clean) > 0 && len(clean) < len(valid) {
		for _, candidate := range valid {
			if candidate.failure != nil {
				rejected = append(rejected, rejectedCandidate{
					span:         candidate.span,
					category:     analysisChatEvidenceCategory(candidate.failure.Gate),
					contractLike: true,
				})
			}
		}
		valid = clean
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
		if selected.failure != nil {
			stats.Category = analysisChatEvidenceCategory(selected.failure.Gate)
			stats.EvidenceGate = selected.failure.Gate
			stats.EvidenceDetail = selected.failure.Detail
		}
		return selected.reply, stats, nil
	default:
		stats.Category = analysisChatValidationCandidate
		return analysischat.Reply{}, stats, newAnalysisChatValidationError(stats.Category, errors.New("response contains multiple valid candidates"))
	}
}

// analysisChatEvidenceCategory maps a soft evidence gate to its telemetry category.
func analysisChatEvidenceCategory(gate string) string {
	if gate == analysischat.UnverifiedReference {
		return analysisChatValidationReference
	}
	return analysisChatValidationCitation
}

// degradeAnalysisChatReply strips unproven evidence from a reply and labels it
// unverified so the answer still reaches the maintainer.
func degradeAnalysisChatReply(reply *analysischat.Reply, failure *analysisChatEvidenceFailure) {
	reply.Citations = nil
	reply.ProposedRevision = nil
	reply.Assessment = "inconclusive"
	reply.Unverified = true
	reply.UnverifiedReason = failure.Gate
}

// salvageAnalysisChatReply recovers the answer text from a response that failed
// the contract, so a formatting failure degrades the same way an unverifiable
// citation already does instead of discarding a usable answer. The salvaged
// reply carries no evidence, so it cannot start a fix or a correction.
func salvageAnalysisChatReply(raw string) (analysischat.Reply, bool) {
	scan := scanAnalysisChatJSONCandidates(raw)
	answer := ""
	// A response that is mostly JSON was an attempt at the contract, so its
	// answer field is the answer. A response that is mostly prose is the answer,
	// even when it quotes a JSON path, a manifest, or its own earlier draft.
	if analysisChatJSONShaped(raw, scan) {
		for _, candidate := range scan.candidates {
			var fields struct {
				Answer string `json:"answer"`
			}
			if rejectAnalysisChatDuplicateFields(candidate.value) != nil ||
				json.Unmarshal([]byte(candidate.value), &fields) != nil {
				continue
			}
			// Two different answers in one response mean the model wrapped a
			// draft around its conclusion, and picking either is a guess.
			if found := strings.TrimSpace(fields.Answer); found != "" && found != answer {
				if answer != "" {
					return analysischat.Reply{}, false
				}
				answer = found
			}
		}
	} else {
		answer = strings.TrimSpace(raw)
	}
	if answer == "" {
		return analysischat.Reply{}, false
	}
	reply := analysischat.Reply{Answer: clampAnalysisChatText(answer, analysisChatMaxAnswerBytes)}
	degradeAnalysisChatReply(&reply, &analysisChatEvidenceFailure{Gate: analysischat.UnverifiedFormat})
	return reply, true
}

// analysisChatJSONShaped reports whether a response was an attempt at the JSON
// contract rather than prose, by how much of it sits inside JSON spans. An
// unclosed object counts to the end of the response, so a truncated reply is
// shaped even though it produced no candidate.
func analysisChatJSONShaped(raw string, scan analysisChatCandidateScan) bool {
	spans := make([][2]int, 0, len(scan.candidates)+len(scan.incomplete))
	for _, candidate := range scan.candidates {
		spans = append(spans, [2]int{candidate.start, candidate.end})
	}
	for _, fragment := range scan.incomplete {
		spans = append(spans, [2]int{fragment.start, len(raw) - 1})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	covered, end := 0, -1
	for _, span := range spans {
		if span[1] <= end {
			continue
		}
		if span[0] > end {
			covered += span[1] - span[0] + 1
		} else {
			covered += span[1] - end
		}
		end = span[1]
	}
	return covered*2 > len(strings.TrimSpace(raw))
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

// decodeAnalysisChatReplyCandidate validates one candidate. A contract failure
// returns an error; an evidence failure returns the reply degraded to
// unverified together with the gate that rejected it.
func decodeAnalysisChatReplyCandidate(
	candidate string,
	evidence map[string]*analysisChatEvidence,
) (analysischat.Reply, *analysisChatEvidenceFailure, error) {
	reply, err := decodeAnalysisChatReplyContract(candidate)
	if err != nil {
		return analysischat.Reply{}, nil, err
	}
	failure := validateAnalysisChatCitations(&reply, evidence)
	if failure != nil {
		degradeAnalysisChatReply(&reply, failure)
	}
	return reply, failure, nil
}

func decodeAnalysisChatReplyContract(candidate string) (analysischat.Reply, error) {
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
		return analysischat.Reply{}, analysisChatDecodeError(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return analysischat.Reply{}, newAnalysisChatValidationError(
			analysisChatValidationJSON, errors.New("response contains trailing JSON"),
		)
	}
	reply.Answer = strings.TrimSpace(reply.Answer)
	reply.Assessment = strings.TrimSpace(reply.Assessment)
	if reply.Answer == "" || len(reply.Answer) > analysisChatMaxAnswerBytes {
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

// validateAnalysisChatCitations attributes each citation's quote from the
// conversation evidence and verifies it names something the tools returned. It
// returns the first soft gate failure rather than an error, so the caller can
// repair or degrade the reply.
func validateAnalysisChatCitations(
	reply *analysischat.Reply,
	evidence map[string]*analysisChatEvidence,
) *analysisChatEvidenceFailure {
	if len(reply.Citations) > 20 {
		return &analysisChatEvidenceFailure{
			Gate: analysischat.UnverifiedCitation, Detail: "citations must contain at most 20 entries",
		}
	}
	for i := range reply.Citations {
		citation := &reply.Citations[i]
		citation.Path = strings.TrimSpace(citation.Path)
		citation.Quote = strings.TrimSpace(citation.Quote)
		safe, err := artifacts.SafePath(citation.Path)
		if err != nil || safe == "" {
			return &analysisChatEvidenceFailure{
				Gate: analysischat.UnverifiedReference, Detail: fmt.Sprintf("citation %d has an unsafe path", i+1),
			}
		}
		artifactEvidence := evidence[safe]
		if artifactEvidence == nil {
			return &analysisChatEvidenceFailure{
				Gate:   analysischat.UnverifiedReference,
				Detail: fmt.Sprintf("citation %d names an artifact not read during this conversation", i+1),
			}
		}
		citation.Path = safe
		if citation.LineStart < 0 || citation.LineEnd < 0 ||
			(citation.LineStart == 0) != (citation.LineEnd == 0) ||
			citation.LineEnd > 0 && (citation.LineStart > citation.LineEnd || citation.LineEnd-citation.LineStart > analysisChatMaxQuoteLines) {
			return &analysisChatEvidenceFailure{
				Gate: analysischat.UnverifiedCitation, Detail: fmt.Sprintf("citation %d has an invalid line range", i+1),
			}
		}
		locator := normalizeCitationText(citation.Quote)
		// A quote of nothing but colour codes normalizes away and would match
		// any passage.
		if len(citation.Quote) < 4 || locator == "" {
			return &analysisChatEvidenceFailure{
				Gate:   analysischat.UnverifiedCitation,
				Detail: fmt.Sprintf("citation %d requires a quote of at least 4 bytes from the artifact", i+1),
			}
		}
		tooLong := &analysisChatEvidenceFailure{
			Gate: analysischat.UnverifiedCitation,
			Detail: fmt.Sprintf(
				"citation %d quote is too long to record; quote the passage that supports the answer", i+1,
			),
		}
		// A line range pins the passage on its own, so it is the attribution.
		// Without one the quote is a locator the engine resolves, and a locator
		// that could name two different passages is not evidence.
		if citation.LineStart > 0 && len(artifactEvidence.Lines) > 0 {
			quote, ok := analysisChatQuoteForRange(artifactEvidence.Lines, citation.LineStart, citation.LineEnd)
			if !ok || !analysisChatEvidenceContains(artifactEvidence, quote) {
				return &analysisChatEvidenceFailure{
					Gate:   analysischat.UnverifiedCitation,
					Detail: fmt.Sprintf("citation %d line range was not returned by the cited artifact read", i+1),
				}
			}
			// A range too long to record narrows to the lines that fit, so the
			// stored text and the cited lines still describe each other. What
			// survives has to still cover the passage the model pointed at.
			clamped, kept := clampAnalysisChatQuote(quote)
			if clamped != quote && !strings.Contains(normalizeCitationText(clamped), locator) {
				return tooLong
			}
			citation.Quote = clamped
			citation.LineEnd = citation.LineStart + kept - 1
			continue
		}
		quote, matches := attributeAnalysisChatQuote(artifactEvidence, citation.Quote)
		switch {
		case matches == 0:
			return &analysisChatEvidenceFailure{
				Gate: analysischat.UnverifiedCitation,
				Detail: fmt.Sprintf(
					"citation %d quote does not appear in the cited artifact read; quote text the tools returned", i+1,
				),
			}
		case matches > 1:
			return &analysisChatEvidenceFailure{
				Gate: analysischat.UnverifiedCitation,
				Detail: fmt.Sprintf(
					"citation %d quote matches more than one passage in the cited artifact; quote a longer, unique passage", i+1,
				),
			}
		}
		clamped, _ := clampAnalysisChatQuote(quote)
		// Recording only part of the passage would leave the maintainer reading
		// text that no longer covers what the citation claimed.
		if !strings.Contains(normalizeCitationText(clamped), locator) {
			return tooLong
		}
		citation.Quote = clamped
		citation.LineStart, citation.LineEnd = 0, 0
	}
	if (reply.Assessment == "supports" || reply.Assessment == "challenges") && len(reply.Citations) == 0 {
		return &analysisChatEvidenceFailure{
			Gate:   analysischat.UnverifiedMissing,
			Detail: fmt.Sprintf("a %s response requires artifact citations", reply.Assessment),
		}
	}
	return nil
}

// analysisChatDecodeError maps a decoder failure onto actionable repair text.
// Top-level unknown fields are rejected before decoding, so an unknown field
// here is nested in a citation or proposed_revision. The offending key is
// model-chosen, so the message names the allowed keys instead of echoing it.
func analysisChatDecodeError(err error) error {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return newAnalysisChatValidationError(
			analysisChatValidationJSON, errors.New("response is not valid JSON"),
		)
	}
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		return newAnalysisChatValidationError(
			analysisChatValidationContract,
			errors.New("a nested object uses an unsupported key; a citation uses only path, line_start, line_end, and quote, and proposed_revision uses only root_cause and suggested_fix"),
		)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return newAnalysisChatValidationError(
			analysisChatValidationContract,
			errors.New("a response field has the wrong type; answer is a string, citations is an array, citation path and quote are strings, citation line_start and line_end are integers or null, assessment is a string or null, and proposed_revision is null or an object with string root_cause and suggested_fix"),
		)
	}
	return newAnalysisChatValidationError(
		analysisChatValidationContract, errors.New("response is not valid analysis-chat JSON"),
	)
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
