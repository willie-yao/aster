package ai

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/ai/tools/repotree"
)

const toolPayloadLimitError = `{"error":"tool result exceeds the serialized byte limit or is invalid; request a narrower result","truncated":true}`

// boundToolResult trims content before it can enter any evidence index.
// BytesFetched and scan totals still describe the actual investigation work.
func boundToolResult(name string, result *tools.Result) {
	payload := result.Payload
	if toolPayloadFits(payload) {
		return
	}
	payload["truncated"] = true
	switch name {
	case "list_artifacts", "list_repo_tree":
		raw, err := json.Marshal(payload)
		if err != nil {
			break
		}
		var listing struct {
			Dirs  []json.RawMessage `json:"dirs"`
			Files []json.RawMessage `json:"files"`
		}
		if json.Unmarshal(raw, &listing) != nil {
			break
		}
		setEntries := func(n int) {
			payload["dirs"] = listing.Dirs[:min(n, len(listing.Dirs))]
			payload["files"] = listing.Files[:max(0, n-len(listing.Dirs))]
		}
		if fittingToolPrefix(len(listing.Dirs)+len(listing.Files), payload, setEntries) > 0 {
			return
		}
	case "read_artifact", "read_repo_file":
		content, ok := payload["content"].(string)
		if !ok {
			break
		}
		if read, ok := result.Observation.(repotree.ReadObservation); ok && read.LineStart > 0 {
			if trimNumberedToolRead(payload, &read) {
				result.Observation = read
				return
			}
			break
		}
		offset, ok := payload["offset"].(int)
		if !ok {
			break
		}
		setPrefix := func(n int) {
			for n > 0 && n < len(content) && !utf8.RuneStart(content[n]) {
				n--
			}
			payload["content"], payload["length"] = content[:n], n
			payload["next_offset"] = offset + n
		}
		n := fittingToolPrefix(len(content), payload, setPrefix)
		if n > 0 && payload["content"] != "" {
			return
		}
	case "tail_artifact":
		content, ok := payload["content"].(string)
		if !ok || content == "" {
			break
		}
		lines := strings.Split(content, "\n")
		setSuffix := func(n int) {
			payload["content"] = strings.Join(lines[len(lines)-n:], "\n")
			payload["lines_returned"] = n
		}
		if fittingToolPrefix(len(lines), payload, setSuffix) > 0 {
			return
		}
	case "grep_artifact", "grep_repo":
		// Normalize typed tool matches once; only whole matches may survive.
		raw, err := json.Marshal(payload["matches"])
		if err != nil {
			break
		}
		var matches []map[string]any
		if json.Unmarshal(raw, &matches) != nil {
			break
		}
		payload["matches_truncated"] = true
		setMatches := func(n int) { payload["matches"] = matches[:n] }
		n := fittingToolPrefix(len(matches), payload, setMatches)
		if n > 0 {
			result.Observation = trimToolGrepObservation(result.Observation, n)
			return
		}
	}
	result.Payload = modelVisibleToolPayload(toolPayloadLimitError)
	result.Observation = nil
}

func toolPayloadFits(payload map[string]any) bool {
	raw, err := json.Marshal(payload)
	return err == nil && len(raw) <= agenticToolBudget
}

// fittingToolPrefix measures the complete serialized envelope, including flags.
func fittingToolPrefix(limit int, payload map[string]any, set func(int)) int {
	n := sort.Search(limit+1, func(n int) bool {
		set(n)
		return !toolPayloadFits(payload)
	}) - 1
	if n >= 0 {
		set(n)
	}
	return n
}

func trimNumberedToolRead(payload map[string]any, read *repotree.ReadObservation) bool {
	content, _ := payload["content"].(string)
	if _, ok := readRepoFileLines(content, *read); !ok || len(read.LineEndOffsets) != len(read.Lines) {
		return false
	}
	numbered := strings.Split(content, "\n")
	offset, ok := payload["offset"].(int)
	if !ok {
		return false
	}
	setLines := func(n int) {
		next := offset
		if n > 0 {
			next = read.LineEndOffsets[n-1]
		}
		payload["content"] = strings.Join(numbered[:n], "\n")
		payload["line_end"] = read.LineStart + n - 1
		payload["length"], payload["next_offset"] = next-offset, next
	}
	n := fittingToolPrefix(len(numbered), payload, setLines)
	if n <= 0 {
		return false
	}
	read.LineEnd = read.LineStart + n - 1
	read.Lines = read.Lines[:n]
	read.LineEndOffsets = read.LineEndOffsets[:n]
	return true
}

func trimToolGrepObservation(observation any, count int) any {
	switch value := observation.(type) {
	case tools.GrepCallObservation:
		value.ReturnedRanges = value.ReturnedRanges[:min(count, len(value.ReturnedRanges))]
		value.ResultTruncated = true
		return value
	case repotree.GrepObservation:
		value.Call = trimToolGrepObservation(value.Call, count).(tools.GrepCallObservation)
		visible := make(map[repotree.GrepMatchObservation]bool, len(value.Call.ReturnedRanges))
		for _, span := range value.Call.ReturnedRanges {
			visible[repotree.GrepMatchObservation{
				SourceID: span.SelectorID, Path: span.Path, LineStart: span.LineStart, LineEnd: span.LineEnd,
			}] = true
		}
		matches := make([]repotree.GrepMatchObservation, 0, len(value.Matches))
		for _, match := range value.Matches {
			if visible[match] {
				matches = append(matches, match)
			}
		}
		value.Matches = matches
		return value
	}
	return observation
}
