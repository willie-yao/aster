// Package actiondraft validates model-generated issue and pull request text
// before it can enter an operator preview.
package actiondraft

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxTitleRunes = 256
	MaxTitleBytes = 1024
	MaxBodyBytes  = 64 << 10
)

var headingPattern = regexp.MustCompile(`^#{1,3}[ \t]+(.+?)[ \t]*#*[ \t]*$`)
var numberedDraftPattern = regexp.MustCompile(`(?im)^[ \t]*(?:draft|version|option)[ \t]+[12][:.][ \t]*`)

var reasoningPhrases = []string{
	"the user wants me",
	"i need to",
	"let me draft",
	"current structure",
	"this looks good",
	"one final check",
	"final answer:",
	"revised issue body:",
}

var promptEchoPhrases = []string{
	"maintainer instruction:",
	"current issue body:",
	"description to fit into the template:",
	"candidate templates:",
	"report content:",
	"pull request template:",
	"system prompt:",
	"developer instruction:",
	"<!-- prow-ai-dashboard-key:",
}

// ValidateBody rejects unsafe or malformed model-generated draft text.
func ValidateBody(body string) error {
	if err := validateText("body", body, MaxBodyBytes); err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("body is empty")
	}
	if err := validatePromptAndReasoning("body", body, 2, 3); err != nil {
		return err
	}
	if err := validateUniqueSections(body); err != nil {
		return err
	}
	return nil
}

// ValidateTitleBody validates a complete issue draft.
func ValidateTitleBody(title, body string) error {
	if err := validateText("title", title, MaxTitleBytes); err != nil {
		return err
	}
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("title is empty")
	}
	if utf8.RuneCountInString(title) > MaxTitleRunes {
		return fmt.Errorf("title exceeds %d characters", MaxTitleRunes)
	}
	if err := validatePromptAndReasoning("title", title, 1, 1); err != nil {
		return err
	}
	return ValidateBody(body)
}

func validatePromptAndReasoning(field, value string, preambleMinSignals, documentMinSignals int) error {
	lower := strings.ToLower(value)
	for _, phrase := range promptEchoPhrases {
		if strings.Contains(lower, phrase) {
			return fmt.Errorf("%s contains prompt text", field)
		}
	}
	preamble := strings.ToLower(reasoningPreamble(value))
	if countReasoningSignals(preamble) >= preambleMinSignals || countReasoningSignals(lower) >= documentMinSignals {
		return fmt.Errorf("%s contains model reasoning", field)
	}
	if len(numberedDraftPattern.FindAllStringIndex(preamble, -1)) > 1 {
		return fmt.Errorf("%s contains multiple draft variants", field)
	}
	return nil
}

func countReasoningSignals(value string) int {
	signals := 0
	for _, phrase := range reasoningPhrases {
		if strings.Contains(value, phrase) {
			signals++
		}
	}
	return signals
}

func reasoningPreamble(value string) string {
	const maxPreambleBytes = 2048
	lines := strings.Split(value, "\n")
	var preamble strings.Builder
	for _, line := range lines {
		if headingPattern.MatchString(line) {
			break
		}
		if preamble.Len()+len(line)+1 > maxPreambleBytes {
			remaining := maxPreambleBytes - preamble.Len()
			if remaining > 0 {
				preamble.WriteString(line[:min(remaining, len(line))])
			}
			break
		}
		preamble.WriteString(line)
		preamble.WriteByte('\n')
	}
	return preamble.String()
}

func validateText(field, value string, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, maxBytes)
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return fmt.Errorf("%s contains control characters", field)
		}
	}
	return nil
}

func validateUniqueSections(body string) error {
	seenSections := map[string]bool{}
	affectedBuilds := false
	seenBuilds := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if match := headingPattern.FindStringSubmatch(line); match != nil {
			name := normalizeHeading(match[1])
			if name != "" && seenSections[name] {
				return fmt.Errorf("body repeats issue section %q", name)
			}
			if name != "" {
				seenSections[name] = true
			}
			affectedBuilds = name == "affected builds"
			continue
		}
		if !affectedBuilds {
			continue
		}
		entry := strings.TrimSpace(line)
		if !strings.HasPrefix(entry, "- ") && !strings.HasPrefix(entry, "* ") {
			continue
		}
		entry = strings.Join(strings.Fields(strings.ToLower(entry[2:])), " ")
		if entry != "" && seenBuilds[entry] {
			return fmt.Errorf("body repeats an affected build entry")
		}
		seenBuilds[entry] = entry != ""
	}
	return nil
}

func normalizeHeading(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "*_`~ ")
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}
