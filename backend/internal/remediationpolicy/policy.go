// Package remediationpolicy enforces deterministic safety rules for fixes.
package remediationpolicy

import (
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// UnsafeConversionReason is the public-safe reason for a blocked remediation.
const UnsafeConversionReason = "remediation requires investigation before CRD conversion safety can be established"

var (
	destructiveConversionObjectRe = regexp.MustCompile(`(?i)(?:\b(?:delete|remove|disable|drop|clear|unset|bypass)\b\s+(?:the\s+)?(?:kubernetes\s+)?(?:crd\s+)?conversion(?:\s+webhook)?\s+strategy\b|\b(?:delete|remove|disable|drop|clear|unset|bypass)\b\s+(?:the\s+)?(?:kubernetes\s+)?(?:crd\s+)?conversion\s+webhooks?\b(?:\s*(?:[.,;:]|$)|\s+(?:from|in|on)\b)|\bturn\s+off\b\s+(?:the\s+)?(?:crd\s+)?conversion(?:\s+webhooks?)?\b|\bstop\b\s+(?:the\s+)?(?:crd\s+)?conversion\b(?:\s*(?:[.,;:]|$)|\s+(?:calls?|requests?)\b))`)
	destructiveConversionFieldRe  = regexp.MustCompile(`(?i)\b(?:delete|remove|disable|drop|clear|unset|bypass)\b\s+(?:the\s+)?(?:spec\.)?conversion\.(?:strategy|webhook(?:\.clientconfig)?)\b`)
	conversionStrategyNoneRe      = regexp.MustCompile(`(?i)(?:\b(?:set|change|switch|replace|clear|unset)\b[^.!?\n]{0,80}\b(?:crd\s+)?conversion(?:\s+webhook)?\s+strategy\b[^.!?\n]{0,80}\bnone\b|\bwebhook\s+conversion\b[^.!?\n]{0,80}\bnone\s+strategy\b|\b(?:conversion\.)?strategy\s*(?:to|:|=)\s*none\b)`)
	admissionCleanupRe            = regexp.MustCompile(`(?is)\b(?:delete|remove|clear|unset|disable|drop)\b[^.!?\n]{0,140}\b(?:(?:mutating(?:\s+and\s+validating)?|validating)(?:\s+admission)?\s+webhook(?:\s+configurations?)?|admission\s+webhook(?:\s+configurations?)?|mutatingwebhookconfigurations?|validatingwebhookconfigurations?)\b`)
	admissionReferencedCleanupRe  = regexp.MustCompile(`(?is)\b(?:(?:mutating(?:\s+and\s+validating)?|validating)(?:\s+admission)?\s+webhooks?|admission\s+webhooks?)\b[^.!?\n]{0,100}\b(?:delete|remove|clear|unset|drop)\s+(?:their|those)\s+(?:webhook\s+)?configurations?\b`)
	conversionBypassRe            = regexp.MustCompile(`(?is)(?:\b(?:disable|remove|bypass|eliminate|clear|unset)\b\s+(?:the\s+)?(?:crd\s+)?conversion\b(?:\s*(?:[.,;:]|$)|\s+(?:entirely|calls?|requests?)\b)|\b(?:crd\s+)?conversion\b[^.!?\n]{0,80}\b(?:disabled|removed|bypassed|eliminated|cleared|unset)\b|\bturn\s+off\b\s+(?:the\s+)?(?:crd\s+)?conversion\b|\bstop\b\s+(?:the\s+)?(?:crd\s+)?conversion\b(?:\s*(?:[.,;:]|$)|\s+(?:calls?|requests?)\b))`)
	negatedConversionActionRe     = regexp.MustCompile(`(?is)\b(?:do|must|should)\s+not\s+(?:disable|remove|bypass|eliminate|clear|unset|delete|drop|turn\s+off|stop|prevent|skip)\s+(?:the\s+)?(?:kubernetes\s+)?(?:crd\s+)?conversion(?:\s+(?:webhook|strategy))?\b|\bnever\s+(?:disable|remove|bypass|eliminate|clear|unset|delete|drop|stop|prevent|skip)\s+(?:the\s+)?(?:kubernetes\s+)?(?:crd\s+)?conversion(?:\s+(?:webhook|strategy))?\b`)
	negatedConversionStateRe      = regexp.MustCompile(`(?is)\b(?:crd\s+)?conversion(?:\s+webhook)?\s+(?:is|remains|stays|will\s+be|must\s+be|should\s+be)\s+not\s+(?:disabled|removed|bypassed|eliminated|cleared|unset|stopped)\b`)
	preventCallFailureRe          = regexp.MustCompile(`(?is)\bprevent(?:s|ed|ing)?\b[^.!?\n,;]{0,100}\b(?:api\s*server\s+)?conversion(?:\s+webhook)?\s+(?:calls?|invocations?)\b[^.!?\n,;]{0,40}\bfrom\s+fail(?:ing|ure)?\b`)
	callFailureAvoidanceRe        = regexp.MustCompile(`(?is)\b(?:api\s*server\s+)?conversion(?:\s+webhook)?\s+(?:calls?|invocations?|requests?)\b[^.!?\n,;]{0,40}\b(?:will\s+not|cannot|can\s+not|do\s+not|does\s+not|never)\s+fail\b`)
	conversionReviewFailureRe     = regexp.MustCompile(`(?is)\bconversion\s*review(?:\s+(?:objects?|requests?))?\b[^.!?\n,;]{0,50}\b(?:will\s+not|cannot|can\s+not|do\s+not|does\s+not|never)\s+fail\s+to\s+(?:reach|send|post|deliver|forward|submit|receive)\b`)
	preventConversionReviewFailRe = regexp.MustCompile(`(?is)\bprevent(?:s|ed|ing)?\b(?:[^.!?\n,;]{0,100}\bconversion\s*review(?:\s+(?:objects?|requests?))?\b[^.!?\n,;]{0,50}(?:\bfrom\s+fail(?:ing|ure)?\s+to\s+(?:reach|send|post|deliver|forward|submit|receive)\b|\bdelivery\s+failures?\b)|[^.!?\n,;]{0,100}\bfail(?:ing|ures?)?\b[^.!?\n,;]{0,50}\b(?:to|when|while)?\s*(?:reach(?:ing)?|send(?:ing)?|post(?:ing)?|deliver(?:ing)?|forward(?:ing)?|submit(?:ting)?|receiv(?:e|ing))\b[^.!?\n,;]{0,40}\bconversion\s*review\b)`)
	directConversionOperationRe   = regexp.MustCompile(`(?i)\b([a-z][a-z-]*)\s+(?:the\s+)?(?:kubernetes\s+)?(?:(?:crd\s+)?conversion(?:\s+webhook)?|webhook\s+conversion)(?:\s+(?:configuration|strategy))?\b`)
	directConversionStateRe       = regexp.MustCompile(`(?i)\b(?:(?:crd\s+)?conversion(?:\s+webhook)?|webhook\s+conversion)(?:\s+(?:configuration|strategy))?\s+(?:(?:is|are|will\s+be|must\s+be|should\s+be|gets?|becomes?)\s+)?([a-z][a-z-]*)\b`)
)

// Reason returns a privacy-safe reason when an actionable remediation is unsafe.
func Reason(recommendation string, targets []models.RemediationTarget) string {
	actionable := false
	for _, target := range targets {
		if target.Intent == models.RemediationIntentInvestigate {
			continue
		}
		actionable = true
		if unsafeConversionTarget(target) {
			return UnsafeConversionReason
		}
	}
	if !actionable {
		return ""
	}
	text := strings.TrimSpace(recommendation)
	admissionCleanup := admissionCleanupRe.MatchString(text) || admissionReferencedCleanupRe.MatchString(text)
	for _, clause := range policyClauses(text) {
		clause = strings.TrimSpace(stripNegatedConversionSafety(clause))
		if clause == "" {
			continue
		}
		if destructiveConversionObjectRe.MatchString(clause) || destructiveConversionFieldRe.MatchString(clause) || conversionStrategyNoneRe.MatchString(clause) || conversionBypassRe.MatchString(clause) || unsafeDirectConversionOperation(clause) || unsafeDirectConversionState(clause) {
			return UnsafeConversionReason
		}
		if admissionCleanup && conversionInvocationLoss(clause) {
			return UnsafeConversionReason
		}
	}
	return ""
}

func policyClauses(text string) []string {
	clauses := make([]string, 0, 4)
	start := 0
	for i := 0; i < len(text); i++ {
		separator := text[i] == '\n' || text[i] == ';' || text[i] == '!' || text[i] == '?'
		if text[i] == '.' && (i+1 == len(text) || isPolicySpace(text[i+1])) {
			separator = true
		}
		if !separator {
			continue
		}
		clauses = append(clauses, text[start:i])
		start = i + 1
	}
	clauses = append(clauses, text[start:])
	return clauses
}

func isPolicySpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func stripNegatedConversionSafety(clause string) string {
	clause = negatedConversionActionRe.ReplaceAllString(clause, "")
	return negatedConversionStateRe.ReplaceAllString(clause, "")
}

func conversionInvocationLoss(clause string) bool {
	text := strings.ToLower(strings.Join(strings.Fields(clause), " "))
	text = strings.NewReplacer("won't", "will not", "won’t", "will not", "can't", "cannot", "can’t", "cannot", "doesn't", "does not", "doesn’t", "does not").Replace(text)
	if !strings.Contains(text, "conversion") {
		return false
	}
	if preventCallFailureRe.MatchString(text) || callFailureAvoidanceRe.MatchString(text) {
		return false
	}
	if conversionReviewDelivery(text) {
		if hardConversionReviewLoss(text) {
			return true
		}
		return !conversionReviewDeliveryPreserved(text)
	}
	if strings.Contains(text, "skip") {
		return true
	}
	if strings.Contains(text, "prevent") {
		return containsAny(text, "call", "invok")
	}
	loss := containsAny(text, "stop", "cease", "no longer", "will not", "does not", "cannot", "can no longer", "fail to", "not available", "unavailable", "never")
	return loss && containsAny(text, "call", "invok", "available", "reach", "request")
}

func conversionReviewDelivery(value string) bool {
	return containsAny(value, "conversionreview", "conversion review") && containsAny(value, "send", "sent", "post", "deliver", "forward", "submit", "receive", "reach")
}

func hardConversionReviewLoss(value string) bool {
	if conversionReviewFailurePreserved(value) {
		return false
	}
	return containsAny(value,
		"stop", "cease", "no longer", "zero", "none", "not a single", "eliminat", "abolish", "suppress",
		"block", "bypass", "remove", "disable", "unable", "cannot", "will not", "does not", "never", "not sent",
		"not posted", "not delivered", "not forwarded", "not submitted", "not received",
	)
}

func conversionReviewDeliveryPreserved(value string) bool {
	if conversionReviewFailurePreserved(value) {
		return true
	}
	return containsAny(value,
		"keep", "preserv", "maintain", "remain available", "remains available", "stay available", "stays available",
		"continue", "succeed", "successful", "working", "reachable",
	)
}

func conversionReviewFailurePreserved(value string) bool {
	if conversionReviewFailureRe.MatchString(value) || preventConversionReviewFailRe.MatchString(value) {
		return true
	}
	words := identifierWords(value)
	failureIndexes := wordIndexesWithPrefix(words, "fail")
	deliveryIndexes := deliveryWordIndexes(words)
	if len(failureIndexes) == 0 || len(deliveryIndexes) == 0 || !nearAny(failureIndexes, deliveryIndexes, 7) {
		return false
	}
	for _, failure := range failureIndexes {
		start := failure - 4
		if start < 0 {
			start = 0
		}
		if containsWord(words[start:failure], "prevent", "prevents", "prevented", "preventing", "ensure", "ensures", "ensuring", "cannot", "never", "not") {
			return true
		}
	}
	return false
}

func wordIndexesWithPrefix(words []string, prefix string) []int {
	indexes := make([]int, 0, 2)
	for i, word := range words {
		if strings.HasPrefix(word, prefix) {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func deliveryWordIndexes(words []string) []int {
	indexes := make([]int, 0, 2)
	for i, word := range words {
		if containsAny(word, "send", "sent", "post", "deliver", "forward", "submit", "receiv", "reach") {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func nearAny(left, right []int, distance int) bool {
	for _, a := range left {
		for _, b := range right {
			delta := a - b
			if delta < 0 {
				delta = -delta
			}
			if delta <= distance {
				return true
			}
		}
	}
	return false
}

func unsafeDirectConversionOperation(clause string) bool {
	for _, match := range directConversionOperationRe.FindAllStringSubmatchIndex(clause, -1) {
		operation := strings.ToLower(clause[match[2]:match[3]])
		if ignoredDirectConversionWord(operation) {
			continue
		}
		suffix := identifierWords(clause[match[1]:])
		if preservingOperationWord(operation) && !unsafePreservationSuffix(suffix) {
			continue
		}
		if operation == "prevent" && directConversionFailurePrevention(suffix) {
			continue
		}
		if destructiveActionWord(operation) && leadingSafeQualifierPhrase(suffix) {
			continue
		}
		if containsWord([]string{operation}, "set", "change", "switch", "replace") && startsWithWords(suffix, "to", "webhook") {
			continue
		}
		return true
	}
	return false
}

func unsafePreservationSuffix(words []string) bool {
	if containsWord(words, "disabled", "removed", "bypassed", "unavailable") {
		return true
	}
	for i, word := range words {
		if word != "none" {
			continue
		}
		if i == 0 || !containsWord([]string{words[i-1]}, "not", "never") {
			return true
		}
	}
	return false
}

func unsafeDirectConversionState(clause string) bool {
	for _, match := range directConversionStateRe.FindAllStringSubmatchIndex(clause, -1) {
		state := strings.ToLower(clause[match[2]:match[3]])
		if containsWord([]string{state},
			"available", "availability", "calls", "continues", "failures", "outage", "outages", "preserved",
			"independently", "maintained", "ready", "reachable", "remains", "requests", "retry", "serving", "stays", "strategy", "webhook",
		) || safeQualifierWord(state) || ignoredDirectConversionWord(state) {
			continue
		}
		return true
	}
	return false
}

func ignoredDirectConversionWord(word string) bool {
	return containsWord([]string{word}, "a", "an", "and", "api", "as", "before", "after", "because", "crd", "for", "if", "of", "on", "server", "so", "that", "the", "to", "until", "webhook", "when", "while", "with")
}

func directConversionFailurePrevention(words []string) bool {
	hasFailure := containsWord(words, "fail", "fails", "failed", "failing", "failure", "failures", "outage", "outages")
	hasSubject := containsWord(words, "call", "calls", "invocation", "invocations", "request", "requests", "outage", "outages")
	return hasFailure && hasSubject
}

func preservingOperationWord(word string) bool {
	return containsWord([]string{word}, "keep", "keeping", "maintain", "maintaining", "preserve", "preserving", "ensure", "ensuring", "serve", "serving")
}

func leadingSafeQualifierPhrase(words []string) bool {
	qualified := false
	for i, word := range words {
		if containsWord([]string{word}, "while", "until", "so", "to", "before", "after", "by") {
			return qualified
		}
		if word == "and" {
			if i+1 >= len(words) || !safeQualifierWord(words[i+1]) {
				return false
			}
			continue
		}
		if !safeQualifierWord(word) {
			return false
		}
		qualified = true
	}
	return qualified
}

func startsWithWords(words []string, expected ...string) bool {
	if len(words) < len(expected) {
		return false
	}
	for i, word := range expected {
		if words[i] != word {
			return false
		}
	}
	return true
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func unsafeConversionTarget(target models.RemediationTarget) bool {
	switch target.Intent {
	case models.RemediationIntentAddSymbol, models.RemediationIntentModifySymbol:
		return destructiveConversionIdentifier(target.Symbol) || destructiveConversionIdentifier(target.RequiredCall)
	case models.RemediationIntentRemoveConfiguration:
		key, _, _ := strings.Cut(target.Value, "=")
		isConversion, qualified := conversionObjectSetting(key)
		return isConversion && !qualified
	case models.RemediationIntentSetConfiguration:
		key, value, _ := strings.Cut(target.Value, "=")
		isConversion, qualified := conversionObjectSetting(key)
		return isConversion && !qualified && conversionFalseValue(value)
	case models.RemediationIntentSetJobEnvironment:
		if !conversionIdentifierPresent(target.Name) {
			return false
		}
		if conversionAvailabilityToggle(target.Name) {
			return conversionFalseValue(target.Value)
		}
		if !destructiveConversionIdentifier(target.Name) {
			return false
		}
		return conversionFalseValue(target.Value) || conversionTrueValue(target.Value)
	default:
		return false
	}
}

func destructiveConversionIdentifier(value string) bool {
	tail := value
	if index := strings.LastIndex(value, "."); index >= 0 {
		tail = value[index+1:]
	}
	words := identifierWords(tail)
	conversionStart, conversionEnd, conversionCount := conversionIdentifierSpan(words)
	packageName := normalizeConversionToken(requiredCallPackageIdentity(value))
	if conversionCount == 0 && !strings.Contains(packageName, "conversion") {
		return false
	}
	if containsWord(words, "none") && (containsWord(words, "strategy") || strings.Contains(packageName, "strategy")) {
		return true
	}
	if conversionCount == 0 {
		return !safePackageConversionOperation(words)
	}
	if conversionCount != 1 {
		return true
	}
	return !safeInlineConversionOperation(words, conversionStart, conversionEnd)
}

func requiredCallPackageIdentity(value string) string {
	separator := strings.LastIndex(value, ".")
	if separator < 0 {
		return ""
	}
	segments := strings.Split(value[:separator], "/")
	for len(segments) > 0 && versionPackageSegment(segments[len(segments)-1]) {
		segments = segments[:len(segments)-1]
	}
	if len(segments) == 0 {
		return ""
	}
	identity := normalizeConversionToken(segments[len(segments)-1])
	if len(segments) > 1 && containsAny(identity, "webhook", "strategy") {
		previous := normalizeConversionToken(segments[len(segments)-2])
		if previous == "conversion" || previous == "crdconversion" {
			identity = previous + identity
		}
	}
	return identity
}

func versionPackageSegment(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 2 || value[0] != 'v' || value[1] < '0' || value[1] > '9' {
		return false
	}
	for _, r := range value[2:] {
		if r < 'a' || r > 'z' {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func safePackageConversionOperation(words []string) bool {
	actionStart, actionEnd, actionCount := destructiveActionSpan(words)
	if actionCount == 1 {
		if actionStart == 0 && safeQualifierWords(words[actionEnd:]) {
			return true
		}
		if actionEnd == len(words) && safePrefixedQualifierWords(words[:actionStart]) {
			return true
		}
	}
	return safeExplicitConversionOperation(words)
}

func safeInlineConversionOperation(words []string, conversionStart, conversionEnd int) bool {
	actionStart, actionEnd, actionCount := destructiveActionSpan(words)
	if actionCount == 1 && safeQualifiedConversionAction(words, actionStart, actionEnd, conversionStart, conversionEnd) {
		return true
	}
	remaining := make([]string, 0, len(words)-(conversionEnd-conversionStart))
	remaining = append(remaining, words[:conversionStart]...)
	remaining = append(remaining, words[conversionEnd:]...)
	return safeExplicitConversionOperation(remaining)
}

func safeExplicitConversionOperation(words []string) bool {
	if len(words) == 0 {
		return false
	}
	qualified := false
	preserving := false
	for _, word := range words {
		if safeQualifierWord(word) || safeAvailabilityWord(word) {
			qualified = true
			continue
		}
		if preservingOperationWord(word) {
			preserving = true
			continue
		}
		if !safeConversionOperationWord(word) {
			return false
		}
	}
	return qualified || preserving
}

func safeConversionOperationWord(word string) bool {
	switch word {
	case "add", "configure", "coordinate", "ensure", "is", "keep", "maintain", "patch", "preserve",
		"renew", "replace", "rotate", "set", "update", "verify", "wait", "check",
		"for", "from", "of", "on", "to", "until", "while", "and":
		return true
	default:
		return false
	}
}

func safeAvailabilityWord(word string) bool {
	switch word {
	case "available", "availability", "ready", "readiness", "reachable", "serving":
		return true
	default:
		return false
	}
}

func identifierWords(value string) []string {
	words := make([]string, 0, 8)
	start := -1
	runes := []rune(value)
	for i, current := range runes {
		if !isIdentifierRune(current) {
			if start >= 0 {
				words = append(words, strings.ToLower(string(runes[start:i])))
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
			continue
		}
		previous := runes[i-1]
		nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
		if current >= 'A' && current <= 'Z' && (previous >= 'a' && previous <= 'z' || previous >= '0' && previous <= '9' || previous >= 'A' && previous <= 'Z' && nextLower) {
			words = append(words, strings.ToLower(string(runes[start:i])))
			start = i
		}
	}
	if start >= 0 {
		words = append(words, strings.ToLower(string(runes[start:])))
	}
	return words
}

func isIdentifierRune(value rune) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func conversionIdentifierSpan(words []string) (int, int, int) {
	start, end, count := -1, -1, 0
	for i, word := range words {
		if word != "conversion" {
			continue
		}
		count++
		if start >= 0 {
			continue
		}
		start, end = i, i+1
		if i > 0 && containsWord([]string{words[i-1]}, "crd", "webhook", "strategy") {
			start--
		}
		if i+1 < len(words) && containsWord([]string{words[i+1]}, "webhook", "strategy") {
			end++
		}
	}
	return start, end, count
}

func destructiveActionSpan(words []string) (int, int, int) {
	start, end, count := -1, -1, 0
	for i := 0; i < len(words); i++ {
		matchedEnd := i + 1
		matched := destructiveActionWord(words[i])
		if words[i] == "turn" && i+1 < len(words) && words[i+1] == "off" {
			matched = true
			matchedEnd = i + 2
		}
		if !matched {
			continue
		}
		if start < 0 {
			start, end = i, matchedEnd
		}
		count++
		i = matchedEnd - 1
	}
	return start, end, count
}

func destructiveActionWord(word string) bool {
	switch word {
	case "delete", "deleted", "deleting", "remove", "removed", "removing", "disable", "disabled", "disabling",
		"drop", "dropped", "dropping", "clear", "cleared", "clearing", "unset", "unsetting",
		"bypass", "bypassed", "bypassing", "turnoff", "stop", "stopped", "stopping",
		"teardown", "destroy", "destroyed", "destroying":
		return true
	default:
		return false
	}
}

func safeQualifiedConversionAction(words []string, actionStart, actionEnd, conversionStart, conversionEnd int) bool {
	if actionStart < conversionStart {
		between := words[actionEnd:conversionStart]
		after := words[conversionEnd:]
		if len(between) == 0 {
			return safeQualifierWords(after)
		}
		if len(after) != 0 || !containsWord([]string{between[len(between)-1]}, "for", "from", "of", "on") {
			return false
		}
		return safeQualifierWords(between[:len(between)-1])
	}
	if actionStart == conversionEnd {
		return safeQualifierWords(words[actionEnd:])
	}
	return false
}

func safeQualifierWords(words []string) bool {
	if len(words) == 0 {
		return false
	}
	for _, word := range words {
		if !safeQualifierWord(word) {
			return false
		}
	}
	return true
}

func safePrefixedQualifierWords(words []string) bool {
	if len(words) == 0 {
		return false
	}
	if words[0] == "ensure" {
		words = words[1:]
	}
	return safeQualifierWords(words)
}

func safeQualifierWord(word string) bool {
	switch word {
	case "timeout", "timeouts", "certificate", "certificates", "cert", "certs", "dependency", "dependencies",
		"rotation", "rotations", "shutdown", "coordination", "retry", "retries", "behavior", "behaviors",
		"policy", "policies", "backoff", "backoffs", "override", "overrides", "and":
		return true
	default:
		return false
	}
}

func containsWord(words []string, candidates ...string) bool {
	for _, word := range words {
		for _, candidate := range candidates {
			if word == candidate {
				return true
			}
		}
	}
	return false
}

func conversionIdentifierPresent(value string) bool {
	words := expandedIdentifierWords(value)
	_, _, count := conversionIdentifierSpan(words)
	return count > 0 || strings.Contains(normalizeConversionToken(value), "conversion")
}

func conversionAvailabilityToggle(value string) bool {
	words := expandedIdentifierWords(value)
	start, end, count := conversionIdentifierSpan(words)
	if count != 1 {
		return false
	}
	remaining := make([]string, 0, len(words)-(end-start))
	remaining = append(remaining, words[:start]...)
	remaining = append(remaining, words[end:]...)
	if len(remaining) == 0 {
		return false
	}
	for _, word := range remaining {
		if !containsWord([]string{word}, "enable", "enabled", "available", "availability") {
			return false
		}
	}
	return true
}

func conversionObjectSetting(value string) (bool, bool) {
	words := expandedIdentifierWords(value)
	start, end, count := conversionIdentifierSpan(words)
	if count == 0 {
		return strings.Contains(normalizeConversionToken(value), "conversion"), false
	}
	if count != 1 {
		return true, false
	}
	remaining := make([]string, 0, len(words)-(end-start))
	remaining = append(remaining, words[:start]...)
	remaining = append(remaining, words[end:]...)
	if containsWord(remaining, "and") {
		return true, false
	}
	qualified := false
	for _, word := range remaining {
		if safeQualifierWord(word) {
			qualified = true
			continue
		}
		if !containsWord([]string{word}, "spec", "enable", "enabled", "disable", "disabled", "value", "name") {
			return true, false
		}
	}
	return true, qualified
}

func expandedIdentifierWords(value string) []string {
	words := identifierWords(value)
	expanded := make([]string, 0, len(words)+2)
	for _, word := range words {
		switch word {
		case "conversionwebhook":
			expanded = append(expanded, "conversion", "webhook")
		case "webhookconversion":
			expanded = append(expanded, "webhook", "conversion")
		case "conversionstrategy":
			expanded = append(expanded, "conversion", "strategy")
		case "strategyconversion":
			expanded = append(expanded, "strategy", "conversion")
		case "crdconversion":
			expanded = append(expanded, "crd", "conversion")
		default:
			expanded = append(expanded, word)
		}
	}
	return expanded
}

func normalizeConversionToken(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func conversionFalseValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "n", "no", "off", "disable", "disabled", "none", "null", "nil":
		return true
	default:
		return false
	}
}

func conversionTrueValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "y", "yes", "on", "enable", "enabled":
		return true
	default:
		return false
	}
}
