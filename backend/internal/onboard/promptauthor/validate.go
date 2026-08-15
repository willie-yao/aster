package promptauthor

import (
	"fmt"
	"strings"
)

var requiredHeadings = []string{
	"## Architecture",
	"## Diagnostic lifecycle",
	"## Test and job flavors",
	"## Artifact layout",
	"## Common failure patterns",
	"## Transient classification",
	"## Triage order",
	"## Relevant source repositories",
	"## Unresolved details",
}

// Validate enforces the deterministic prompt-author output contract.
func Validate(prompt string) error {
	if len(prompt) > maxBytes {
		return fmt.Errorf("prompt author: generated prompt exceeds %d bytes", maxBytes)
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("prompt author: generated prompt is empty")
	}
	lines := strings.Split(prompt, "\n")
	var headings []string
	var headingLines []int
	inFence := false
	var fence byte
	fenceLength := 0
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			marker := trimmed[0]
			run := 0
			for run < len(trimmed) && trimmed[run] == marker {
				run++
			}
			if run >= 3 {
				if !inFence {
					inFence, fence, fenceLength = true, marker, run
				} else if marker == fence && run >= fenceLength && strings.TrimSpace(trimmed[run:]) == "" {
					inFence = false
				}
				continue
			}
		}
		if inFence || indent > 3 {
			continue
		}
		line = strings.TrimRight(trimmed, " \t")
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, line)
			headingLines = append(headingLines, i)
		}
	}
	if inFence {
		return fmt.Errorf("prompt author: generated prompt has an unclosed code fence")
	}
	if len(headings) != len(requiredHeadings) {
		return fmt.Errorf("prompt author: generated prompt has %d level-two sections, want %d", len(headings), len(requiredHeadings))
	}
	for i, heading := range requiredHeadings {
		if headings[i] != heading {
			return fmt.Errorf("prompt author: section %d is %q, want %q", i+1, headings[i], heading)
		}
	}
	for index, heading := range requiredHeadings {
		if sectionLinesEmpty(lines, headingLines, index) {
			return fmt.Errorf("prompt author: %s is empty or placeholder-only", heading)
		}
	}
	return nil
}

func sectionLinesEmpty(lines []string, headingLines []int, index int) bool {
	start := headingLines[index] + 1
	end := len(lines)
	if index+1 < len(headingLines) {
		end = headingLines[index+1]
	}
	body := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
	return body == "" || strings.Contains(strings.ToLower(body), "todo:") && len(strings.Fields(body)) < 12
}
