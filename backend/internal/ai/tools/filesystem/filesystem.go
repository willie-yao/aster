// Package filesystem implements tier-1 agent tools that give the model raw
// access to a build's GCS artifact tree. These tools are universal across
// every project shape because the Prow artifact convention is universal.
//
// Tools:
//
//	list_artifacts(path)                  - directory listing
//	read_artifact(path, offset, length)   - byte-range read of a small file
//	tail_artifact(path, lines)            - last N lines of a file
//	grep_artifact(path, pattern, ...)     - streaming RE2 search
//	find_artifacts(pattern, root?, ...)   - bounded path search by basename regex
//
// The tools live in their own package so the registry can be tested without
// importing the AI loop and future tool packages can follow the same shape.
package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/artifacts"
)

// Group is the alias used in config to enable all filesystem tools at once.
const Group = "filesystem"

// Register adds every tool in this package to the given registry.
func Register(r *tools.Registry) {
	r.Register(&listTool{})
	r.Register(&readTool{})
	r.Register(&tailTool{})
	r.Register(&grepTool{})
	r.Register(&findTool{})
	r.Register(&timelineTool{})
}

type listTool struct{}

func (*listTool) Name() string  { return "list_artifacts" }
func (*listTool) Group() string { return Group }
func (*listTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "list_artifacts",
			Description: "List the immediate children of a directory in the build's GCS artifact tree. Pass an empty string for the build root. Returns dirs and files (with sizes).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Directory path relative to the build root, e.g. \"\" for root, \"artifacts/\", \"artifacts/clusters/foo/machines/bar/\".",
					},
				},
				"required": []string{"path"},
			},
		},
	}
}

func (*listTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	res, err := env.Browser.List(ctx, args.Path)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	files := make([]map[string]interface{}, 0, len(res.Files))
	for _, f := range res.Files {
		files = append(files, map[string]interface{}{"name": f.Name, "size": f.Size})
	}
	payload := map[string]interface{}{
		"dir":   res.Dir,
		"dirs":  res.Dirs,
		"files": files,
	}
	if res.Truncated {
		payload["truncated"] = true
	}
	return tools.Result{Payload: payload}
}

type readTool struct{}

func (*readTool) Name() string  { return "read_artifact" }
func (*readTool) Group() string { return Group }
func (*readTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "read_artifact",
			Description: "Read a byte range of a file. Use for small/known files. For large logs prefer tail_artifact or grep_artifact. Returns up to 16384 bytes per call.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":   map[string]interface{}{"type": "string", "description": "File path relative to build root."},
					"offset": map[string]interface{}{"type": "integer", "description": "Byte offset to start reading from (default 0).", "default": 0},
					"length": map[string]interface{}{"type": "integer", "description": "Number of bytes to read (default 8192, max 16384).", "default": 8192},
				},
				"required": []string{"path"},
			},
		},
	}
}

func (*readTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Path   string        `json:"path"`
		Offset tools.FlexInt `json:"offset"`
		Length tools.FlexInt `json:"length"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	offset, length := args.Offset.Int(), args.Length.Int()
	if length <= 0 {
		length = 8192
	}
	if length > 16384 {
		length = 16384
	}
	data, size, err := env.Browser.Read(ctx, args.Path, offset, length)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	return tools.Result{
		BytesFetched: len(data), ContentBytes: len(data),
		Payload: map[string]interface{}{
			"path":      args.Path,
			"file_size": size,
			"offset":    offset,
			"length":    len(data),
			"content":   string(data),
		},
	}
}

type tailTool struct{}

func (*tailTool) Name() string  { return "tail_artifact" }
func (*tailTool) Group() string { return Group }
func (*tailTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "tail_artifact",
			Description: "Return the last N lines of a file. Most efficient way to inspect the end of a build log or controller log. Default 500 lines, max 2000.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":  map[string]interface{}{"type": "string", "description": "File path relative to build root."},
					"lines": map[string]interface{}{"type": "integer", "description": "Number of trailing lines (default 500, max 2000).", "default": 500},
				},
				"required": []string{"path"},
			},
		},
	}
}

// tailMaxBytes leaves headroom inside the per-call 32KB tool budget for the
// envelope overhead.
const tailMaxBytes = 32*1024 - 256

func (*tailTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Path  string        `json:"path"`
		Lines tools.FlexInt `json:"lines"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	lines := args.Lines.Int()
	if lines <= 0 {
		lines = 500
	}
	if lines > 2000 {
		lines = 2000
	}
	res, err := env.Browser.Tail(ctx, args.Path, lines, tailMaxBytes)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	return tools.Result{
		BytesFetched: len(res.Content), ContentBytes: len(res.Content),
		Payload: map[string]interface{}{
			"path":           args.Path,
			"file_size":      res.FileSize,
			"lines_returned": res.LinesReturned,
			"content":        string(res.Content),
		},
	}
}

type grepTool struct{}

const artifactGrepSelector = "artifact-workspace"

func (*grepTool) Name() string  { return "grep_artifact" }
func (*grepTool) Group() string { return Group }
func (*grepTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "grep_artifact",
			Description: "Regex-search a file for matching lines. Returns matches with surrounding context lines and line numbers. Use this for huge build-logs where you want to find specific errors.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":          map[string]interface{}{"type": "string", "description": "File path relative to build root."},
					"pattern":       map[string]interface{}{"type": "string", "description": "RE2 regex (Go syntax). Use (?i) prefix for case-insensitive."},
					"context_lines": map[string]interface{}{"type": "integer", "description": "Lines of context before/after each match (default 2, max 5).", "default": 2},
					"max_matches":   map[string]interface{}{"type": "integer", "description": "Max matches to return (default 30, max 100).", "default": 30},
				},
				"required": []string{"path", "pattern"},
			},
		},
	}
}

func (*grepTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Path         string        `json:"path"`
		Pattern      string        `json:"pattern"`
		ContextLines tools.FlexInt `json:"context_lines"`
		MaxMatches   tools.FlexInt `json:"max_matches"`
	}
	args.ContextLines = -1
	contextLines, maxMatches := tools.EffectiveGrepLimits(args.ContextLines, args.MaxMatches)
	observation := artifactGrepObservation(args.Path, contextLines, maxMatches)
	if err := json.Unmarshal(raw, &args); err != nil {
		return artifactGrepError(observation, "invalid arguments: "+err.Error())
	}
	contextLines, maxMatches = tools.EffectiveGrepLimits(args.ContextLines, args.MaxMatches)
	observation = artifactGrepObservation(args.Path, contextLines, maxMatches)
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return artifactGrepError(observation, "invalid regex: "+err.Error())
	}
	observation.FilesAttempted = 1
	res, err := env.Browser.Grep(ctx, args.Path, re, contextLines, maxMatches, 1000, env.RemainingGCSBytes)
	if err != nil {
		observation.FileReadErrors = 1
		return artifactGrepError(observation, err.Error())
	}
	matches := make([]map[string]interface{}, 0, len(res.Matches))
	for _, m := range res.Matches {
		matches = append(matches, map[string]interface{}{
			"line":    m.LineNo,
			"context": m.Context,
		})
	}
	observation.MatchCount = res.TotalMatches
	observation.FilesScanned = 1
	observation.FileScanTruncated = res.ScanTruncated
	observation.ResultTruncated = res.MatchesTruncated
	observation.Outcome = tools.GrepOutcomeZeroMatches
	if res.TotalMatches > 0 {
		observation.Outcome = tools.GrepOutcomeMatched
	}
	observation.ReturnedRanges = artifactGrepRanges(canonicalArtifactGrepPath(args.Path), res.Matches)
	payload := map[string]interface{}{
		"path":              args.Path,
		"file_size":         res.FileSize,
		"bytes_scanned":     res.BytesScanned,
		"total_matches":     res.TotalMatches,
		"matches":           matches,
		"matches_truncated": res.MatchesTruncated,
		"scan_truncated":    res.ScanTruncated,
	}
	if res.ScanTruncated {
		payload["scan_hint"] = scanTruncationHint(res.BytesScanned, res.FileSize)
	}
	return tools.Result{
		BytesFetched: int(res.BytesScanned),
		Payload:      payload,
		Observation:  observation,
	}
}

// scanTruncationHint states how much of the file grep_artifact actually read
// and how to reach the rest, so an incomplete scan is not read as absence.
func scanTruncationHint(bytesScanned, fileSize int64) string {
	scope := fmt.Sprintf("Scanned only the first %d bytes of this file", bytesScanned)
	if fileSize > 0 {
		scope = fmt.Sprintf("Scanned only the first %d of %d bytes of this file", bytesScanned, fileSize)
	}
	return scope + "; anything after that point was never read, so a zero or low match count does not prove the pattern is absent. Use tail_artifact to inspect the end of the file, or grep a smaller, more specific artifact."
}

func artifactGrepObservation(path string, contextLines, maxMatches int) tools.GrepCallObservation {
	canonical := canonicalArtifactGrepPath(path)
	if canonical == "" {
		canonical = path
	}
	filter, supplied, length, redacted := tools.ContentFreePathFilter(canonical)
	return tools.GrepCallObservation{
		SelectorID: artifactGrepSelector, PathFilter: filter, PathFilterSupplied: supplied,
		PathFilterLength: length, PathFilterRedacted: redacted,
		ContextLines: contextLines, MaxMatches: maxMatches, Outcome: tools.GrepOutcomeError,
		ReturnedRanges: []tools.GrepRangeObservation{},
	}
}

func canonicalArtifactGrepPath(path string) string {
	canonical, err := artifacts.SafePath(path)
	if err != nil {
		return ""
	}
	return canonical
}

func artifactGrepError(observation tools.GrepCallObservation, message string) tools.Result {
	observation.Outcome = tools.GrepOutcomeError
	return tools.Result{Payload: map[string]interface{}{"error": message}, Observation: observation}
}

func artifactGrepRanges(path string, matches []artifacts.GrepMatch) []tools.GrepRangeObservation {
	ranges := make([]tools.GrepRangeObservation, 0, len(matches))
	for _, match := range matches {
		start, end := match.LineNo, match.LineNo
		for _, line := range match.Context {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, ">"), " "))
			prefix, _, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value, err := strconv.Atoi(strings.TrimSpace(prefix))
			if err != nil || value <= 0 {
				continue
			}
			if value < start {
				start = value
			}
			if value > end {
				end = value
			}
		}
		ranges = append(ranges, tools.GrepRangeObservation{SelectorID: artifactGrepSelector, Path: path, LineStart: start, LineEnd: end})
	}
	return ranges
}

type findTool struct{}

func (*findTool) Name() string  { return "find_artifacts" }
func (*findTool) Group() string { return Group }
func (*findTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "find_artifacts",
			Description: "Recursively search the artifact tree for files whose basename matches a regex. Bounded: walks at most max_dirs subdirectories and returns at most max_results matches. Use for locating files when you know the name pattern but not the path (e.g. junit_*.xml, kubelet.log, build-log.txt).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern":     map[string]interface{}{"type": "string", "description": "RE2 regex matched against each file's basename. Use (?i) prefix for case-insensitive."},
					"root":        map[string]interface{}{"type": "string", "description": "Directory to walk under, relative to build root. Default empty (build root).", "default": ""},
					"max_results": map[string]interface{}{"type": "integer", "description": "Max matching files to return (default 50, max 200).", "default": 50},
					"max_dirs":    map[string]interface{}{"type": "integer", "description": "Max directories to scan (default 200, max 1000).", "default": 200},
				},
				"required": []string{"pattern"},
			},
		},
	}
}

// ErrFindTruncated is exported so tests can verify the bounded-walk
// behavior; the loop just sees truncated=true in the payload.
var ErrFindTruncated = errors.New("find_artifacts: walk truncated")

func (*findTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Pattern    string        `json:"pattern"`
		Root       string        `json:"root"`
		MaxResults tools.FlexInt `json:"max_results"`
		MaxDirs    tools.FlexInt `json:"max_dirs"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	maxResults, maxDirs := args.MaxResults.Int(), args.MaxDirs.Int()
	if maxResults <= 0 {
		maxResults = 50
	}
	if maxResults > 200 {
		maxResults = 200
	}
	if maxDirs <= 0 {
		maxDirs = 200
	}
	if maxDirs > 1000 {
		maxDirs = 1000
	}
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return tools.ErrPayload("invalid regex: " + err.Error())
	}

	type match struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	var matches []match
	scanned := 0
	truncatedByResults := false
	truncatedByDirs := false

	type queueItem struct{ dir string }
	queue := []queueItem{{dir: args.Root}}

	for len(queue) > 0 && len(matches) < maxResults && scanned < maxDirs {
		head := queue[0]
		queue = queue[1:]
		scanned++

		listing, err := env.Browser.List(ctx, head.dir)
		if err != nil {
			// Skip unlistable subtrees such as missing or unsafe paths.
			continue
		}
		for _, f := range listing.Files {
			if !re.MatchString(f.Name) {
				continue
			}
			matches = append(matches, match{
				Path: joinPath(listing.Dir, f.Name),
				Size: f.Size,
			})
			if len(matches) >= maxResults {
				truncatedByResults = true
				break
			}
		}
		if len(matches) >= maxResults {
			break
		}
		for _, sub := range listing.Dirs {
			queue = append(queue, queueItem{dir: joinPath(listing.Dir, sub)})
		}
	}
	if scanned >= maxDirs && len(queue) > 0 {
		truncatedByDirs = true
	}

	payload := map[string]interface{}{
		"pattern":      args.Pattern,
		"root":         args.Root,
		"scanned_dirs": scanned,
		"matches":      matches,
	}
	if truncatedByResults || truncatedByDirs {
		payload["truncated"] = true
		if truncatedByResults {
			payload["truncated_reason"] = "max_results"
		} else {
			payload["truncated_reason"] = "max_dirs"
		}
	}
	return tools.Result{Payload: payload}
}

// joinPath joins a directory and child name, preserving Browser.List's trailing
// slash convention. If name already ends in "/", the result also ends in "/".
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + name
}
