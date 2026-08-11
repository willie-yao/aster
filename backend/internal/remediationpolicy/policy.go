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
	preventConversionReviewFailRe = regexp.MustCompile(`(?is)\bprevent(?:s|ed|ing)?\b[^.!?\n,;]{0,100}\bconversion\s*review(?:\s+(?:objects?|requests?))?\b[^.!?\n,;]{0,50}(?:\bfrom\s+fail(?:ing|ure)?\s+to\s+(?:reach|send|post|deliver|forward|submit|receive)\b|\bdelivery\s+failures?\b)`)
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
		if destructiveConversionObjectRe.MatchString(clause) || destructiveConversionFieldRe.MatchString(clause) || conversionStrategyNoneRe.MatchString(clause) || conversionBypassRe.MatchString(clause) {
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
	if preventCallFailureRe.MatchString(text) || callFailureAvoidanceRe.MatchString(text) || conversionReviewFailureRe.MatchString(text) || preventConversionReviewFailRe.MatchString(text) {
		return false
	}
	if strings.Contains(text, "skip") {
		return true
	}
	delivery := conversionReviewDelivery(text)
	deliveryLoss := delivery && conversionReviewDeliveryLoss(text)
	if strings.Contains(text, "prevent") {
		return containsAny(text, "call", "invok") || delivery
	}
	loss := containsAny(text, "stop", "cease", "no longer", "will not", "does not", "cannot", "can no longer", "fail to", "not available", "unavailable")
	return loss && containsAny(text, "call", "invok", "available", "reach", "request") || deliveryLoss
}

func conversionReviewDelivery(value string) bool {
	return containsAny(value, "conversionreview", "conversion review") && containsAny(value, "send", "sent", "post", "deliver", "forward", "submit", "receive", "reach")
}

func conversionReviewDeliveryLoss(value string) bool {
	return containsAny(value,
		"no conversionreview", "no conversion review", "no longer", "cannot", "will not", "does not", "unable", "never",
		"not sent", "not posted", "not delivered", "not forwarded", "not submitted", "not received",
	)
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
		isConversion, qualified := conversionObjectSetting(target.Name)
		if !isConversion || qualified {
			return false
		}
		name := normalizeConversionToken(target.Name)
		for _, action := range []string{"disable", "delete", "remove", "drop", "clear", "unset", "bypass", "turnoff", "skip", "stop", "omit"} {
			if strings.Contains(name, action) {
				return conversionTrueValue(target.Value)
			}
		}
		return conversionFalseValue(target.Value)
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
	actionStart, actionEnd, actionCount := destructiveActionSpan(words)
	if conversionCount == 0 {
		packageName := normalizeConversionToken(requiredCallPackage(value))
		if !strings.Contains(packageName, "conversion") {
			return false
		}
		if strings.Contains(packageName, "strategy") && containsWord(words, "none") {
			return true
		}
		if actionCount == 0 {
			return false
		}
		if actionCount != 1 {
			return true
		}
		if actionStart == 0 {
			return !safeQualifierWords(words[actionEnd:])
		}
		return actionEnd != len(words) || !safePrefixedQualifierWords(words[:actionStart])
	}
	if containsWord(words, "strategy") && containsWord(words, "none") {
		return true
	}
	if actionCount == 0 {
		return false
	}
	if actionCount != 1 || conversionCount != 1 {
		return true
	}
	return !safeQualifiedConversionAction(words, actionStart, actionEnd, conversionStart, conversionEnd)
}

func requiredCallPackage(value string) string {
	separator := strings.LastIndex(value, ".")
	if separator < 0 {
		return ""
	}
	packagePath := value[:separator]
	if slash := strings.LastIndex(packagePath, "/"); slash >= 0 {
		return packagePath[slash+1:]
	}
	return packagePath
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

func safeConversionIdentifierQualifier(value string) bool {
	for _, qualifier := range []string{
		"timeout", "certificate", "certificatedependency", "certificaterotation", "shutdowndependency",
		"shutdown", "shutdowncoordination", "retry", "retrybehavior", "retrypolicy", "backoff", "override",
	} {
		if strings.Contains(value, qualifier) {
			return true
		}
	}
	return false
}

func conversionObjectSetting(value string) (bool, bool) {
	return conversionObjectSettingWithPrefix(value, true)
}

func conversionObjectSettingWithPrefix(value string, allowPrefix bool) (bool, bool) {
	name := normalizeConversionToken(value)
	objects := []string{"conversionwebhook", "webhookconversion", "conversionstrategy", "strategyconversion", "specconversion", "crdconversion"}
	for _, object := range objects {
		index := strings.Index(name, object)
		if index < 0 {
			continue
		}
		suffix := name[index+len(object):]
		if object == "specconversion" && (strings.HasPrefix(suffix, "webhook") || strings.HasPrefix(suffix, "strategy")) {
			continue
		}
		qualified := safeConversionQualifier(suffix)
		if allowPrefix && (safeQualifierBeforeConversionObject(name, index) || safeQualifierAfterConversionAction(name, index+len(object))) {
			qualified = true
		}
		return true, qualified
	}
	return strings.Contains(name, "conversion"), safeConversionIdentifierQualifier(name)
}

func safeQualifierBeforeConversionObject(value string, conversion int) bool {
	action := firstDestructiveAction(value)
	for _, qualifier := range []string{"timeout", "certificate", "dependency", "retry", "shutdown", "backoff", "override"} {
		index := strings.Index(value, qualifier)
		if index < 0 || index >= conversion {
			continue
		}
		if action < 0 || action < index {
			return true
		}
	}
	return false
}

func safeQualifierAfterConversionAction(value string, conversionEnd int) bool {
	action := firstDestructiveAction(value)
	if action < conversionEnd {
		return false
	}
	for _, qualifier := range []string{"timeout", "certificate", "dependency", "retry", "shutdown", "backoff", "override"} {
		if index := strings.Index(value[action+1:], qualifier); index >= 0 {
			return true
		}
	}
	return false
}

func firstDestructiveAction(value string) int {
	action := -1
	for _, candidate := range []string{"delete", "remove", "disable", "drop", "clear", "unset", "bypass", "turnoff", "stop"} {
		if index := strings.Index(value, candidate); index >= 0 && (action < 0 || index < action) {
			action = index
		}
	}
	return action
}

func safeConversionQualifier(suffix string) bool {
	for _, qualifier := range []string{
		"timeout", "certificate", "certificatedependency", "certificaterotation", "dependency",
		"shutdown", "shutdowncoordination", "shutdowndependency", "retry", "retrybehavior", "retrypolicy", "backoff", "override",
	} {
		if strings.HasPrefix(suffix, qualifier) {
			return true
		}
	}
	return false
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
