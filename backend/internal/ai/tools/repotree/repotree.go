// Package repotree implements read-only agent tools over a source repository's
// file tree. They mirror the shape of the filesystem tools (which read a
// build's GCS artifact tree) but read one selected immutable source from
// tools.Env.Sources, so the agent can locate the file a fix should touch by
// grepping and reading real source instead of guessing from a path list.
//
// Tools:
//
//	list_repo_tree(source_id, path)              - immediate children of a directory
//	read_repo_file(source_id, path, offset, len) - byte-range read of one file
//	grep_repo(source_id, pattern, path_glob?)    - RE2 search over a bounded file set
//
// Reads go through GitHub's REST API (one call per file), which is rate-limited
// and higher-latency than GCS, so grep_repo is bounded: it fetches at most
// maxGrepFiles files matching path_glob per call and reports truncation. The
// full tree listing and each file body are memoized in tools.Cache so repeated
// navigation over one repo/ref costs no extra calls.
package repotree

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

// Group is the alias used to enable all repo tools at once.
const Group = "repotree"

const (
	readMaxBytes         = 16384 // per read_repo_file call
	grepMaxBytes         = 16384 // per matched file scanned by grep_repo
	maxGrepFiles         = 40    // files fetched per grep_repo call
	grepMaxCtx           = 5
	grepMaxHits          = 100
	grepEvidenceMaxHits  = 64
	grepEvidenceMaxBytes = 2048
	treeCachePfx         = "repotree/tree/"
	fileCachePfx         = "repotree/file/"
)

// Register adds every tool in this package to the given registry.
func Register(r *tools.Registry) {
	r.Register(&listTool{})
	r.Register(&readTool{})
	r.Register(&grepTool{})
}

// source resolves a required source selector without touching a reader.
func source(env *tools.Env, sourceID string) (tools.RepoSource, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return tools.RepoSource{}, fmt.Errorf("source_id is required")
	}
	if env == nil {
		return tools.RepoSource{}, fmt.Errorf("source catalog is unavailable")
	}
	if env.Sources == nil {
		if sourceID == tools.PrimarySourceID && env.Repo != nil {
			return tools.RepoSource{ID: tools.PrimarySourceID, Reader: env.Repo}, nil
		}
		return tools.RepoSource{}, fmt.Errorf("source catalog is unavailable")
	}
	selected, ok := env.Sources.Source(sourceID)
	if !ok {
		return tools.RepoSource{}, fmt.Errorf("unknown source_id %q", sourceID)
	}
	return selected, nil
}

// tree returns one source's blob paths, memoized in the Cache.
func tree(ctx context.Context, env *tools.Env, selected tools.RepoSource) ([]string, error) {
	key := treeCachePfx + selected.ID
	if env.Cache != nil {
		if v, ok := env.Cache.Get(key); ok {
			return strings.Split(v, "\n"), nil
		}
	}
	paths, err := selected.Reader.ListTree(ctx)
	if err != nil {
		return nil, err
	}
	if env.Cache != nil && len(paths) > 0 {
		env.Cache.Set(key, strings.Join(paths, "\n"))
	}
	return paths, nil
}

// readFile returns one selected source file, memoized in the Cache.
func readFile(ctx context.Context, env *tools.Env, selected tools.RepoSource, path string) (string, bool, error) {
	key := fileCachePfx + selected.ID + "/" + path
	if env.Cache != nil {
		if v, ok := env.Cache.Get(key); ok {
			return v, true, nil
		}
	}
	content, found, err := selected.Reader.ReadFile(ctx, path)
	if err != nil || !found {
		return "", found, err
	}
	if env.Cache != nil {
		env.Cache.Set(key, content)
	}
	return content, true, nil
}

type listTool struct{}

func (*listTool) Name() string  { return "list_repo_tree" }
func (*listTool) Group() string { return Group }
func (*listTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "list_repo_tree",
			Description: "List the immediate children of a directory in the source repository. Pass an empty string for the repo root. Returns subdirectories and files under that directory.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"source_id": map[string]interface{}{"type": "string", "description": "Stable source ID from the system prompt. Use primary for a single project source."},
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Directory path relative to the repo root, e.g. \"\" for root, \"config/\", \"pkg/cloud/\".",
					},
				},
				"required": []string{"source_id", "path"},
			},
		},
	}
}

func (*listTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		SourceID string `json:"source_id"`
		Path     string `json:"path"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	selected, err := source(env, args.SourceID)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	paths, err := tree(ctx, env, selected)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	prefix := normalizeDir(args.Path)
	dirSet := map[string]struct{}{}
	var files []string
	for _, p := range paths {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			dirSet[rest[:i]] = struct{}{}
		} else {
			files = append(files, rest)
		}
	}
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	sort.Strings(files)
	return tools.Result{Payload: map[string]interface{}{
		"source_id": selected.ID,
		"dir":       prefix,
		"dirs":      dirs,
		"files":     files,
	}}
}

// ReadObservation identifies complete source lines returned by read_repo_file.
type ReadObservation struct {
	SourceID           string
	Path               string
	LineStart, LineEnd int
}

func completeReadLineRange(content string, offset, end int) (int, int, bool) {
	if offset < 0 || end <= offset || end > len(content) {
		return 0, 0, false
	}
	start := offset
	if start > 0 && content[start-1] != '\n' {
		if next := strings.IndexByte(content[start:end], '\n'); next >= 0 {
			start += next + 1
		} else {
			return 0, 0, false
		}
	}
	finish := end
	if finish < len(content) && finish > 0 && content[finish-1] != '\n' {
		if prior := strings.LastIndexByte(content[start:finish], '\n'); prior >= 0 {
			finish = start + prior + 1
		} else {
			return 0, 0, false
		}
	}
	if finish <= start || strings.TrimSpace(content[start:finish]) == "" {
		return 0, 0, false
	}
	lineStart := 1 + strings.Count(content[:start], "\n")
	lineEnd := lineStart + strings.Count(content[start:finish], "\n")
	if strings.HasSuffix(content[start:finish], "\n") {
		lineEnd--
	}
	if lineEnd < lineStart {
		lineEnd = lineStart
	}
	return lineStart, lineEnd, true
}

type readTool struct{}

func (*readTool) Name() string  { return "read_repo_file" }
func (*readTool) Group() string { return Group }
func (*readTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "read_repo_file",
			Description: "Read a byte range of a source file. Read a file before choosing it as an edit target. Returns up to 16384 bytes per call.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"source_id": map[string]interface{}{"type": "string", "description": "Stable source ID from the system prompt. Use primary for a single project source."},
					"path":      map[string]interface{}{"type": "string", "description": "File path relative to the repo root."},
					"offset":    map[string]interface{}{"type": "integer", "description": "Byte offset to start from (default 0).", "default": 0},
					"length":    map[string]interface{}{"type": "integer", "description": "Bytes to read (default 8192, max 16384).", "default": 8192},
				},
				"required": []string{"source_id", "path"},
			},
		},
	}
}

func (*readTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		SourceID string        `json:"source_id"`
		Path     string        `json:"path"`
		Offset   tools.FlexInt `json:"offset"`
		Length   tools.FlexInt `json:"length"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	selected, err := source(env, args.SourceID)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	content, found, err := readFile(ctx, env, selected, args.Path)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	if !found {
		return tools.ErrPayload("file not found: " + args.Path)
	}
	offset, length := args.Offset.Int(), args.Length.Int()
	if length <= 0 {
		length = 8192
	}
	if length > readMaxBytes {
		length = readMaxBytes
	}
	if offset < 0 {
		offset = 0
	}
	size := len(content)
	if offset > size {
		offset = size
	}
	end := offset + length
	if end > size {
		end = size
	}
	slice := content[offset:end]
	result := tools.Result{
		BytesFetched: len(slice), ContentBytes: len(slice),
		Payload: map[string]interface{}{
			"source_id": selected.ID,
			"path":      args.Path,
			"file_size": size,
			"offset":    offset,
			"length":    len(slice),
			"content":   slice,
		},
	}
	if lineStart, lineEnd, ok := completeReadLineRange(content, offset, end); ok {
		result.Observation = ReadObservation{SourceID: selected.ID, Path: args.Path, LineStart: lineStart, LineEnd: lineEnd}
	}
	return result
}

// GrepMatchObservation identifies one canonical source range returned by grep_repo.
type GrepMatchObservation struct {
	SourceID  string
	Path      string
	LineStart int
	LineEnd   int
}

// GrepObservation is private structured metadata for one grep_repo call.
type GrepObservation struct {
	Matches []GrepMatchObservation
	Call    tools.GrepCallObservation
}

type grepTool struct{}

func (*grepTool) Name() string  { return "grep_repo" }
func (*grepTool) Group() string { return Group }
func (*grepTool) Schema() tools.Schema {
	return tools.Schema{
		Type: "function",
		Function: tools.FunctionDecl{
			Name:        "grep_repo",
			Description: "Regex-search source files for matching lines. Narrow the search with path_glob (a path substring, or a *-glob like \"config/*.yaml\") so it stays cheap; each matched file is fetched over the API. Scans at most 40 files per call and reports truncation. Returns matches with file, line number, and context.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"source_id":     map[string]interface{}{"type": "string", "description": "Stable source ID from the system prompt. Use primary for a single project source."},
					"pattern":       map[string]interface{}{"type": "string", "description": "RE2 regex (Go syntax). Use (?i) prefix for case-insensitive."},
					"path_glob":     map[string]interface{}{"type": "string", "description": "Restrict to files whose path matches this substring or *-glob. Strongly recommended; a broad search is capped at 40 files.", "default": ""},
					"context_lines": map[string]interface{}{"type": "integer", "description": "Lines of context before/after each match (default 2, max 5).", "default": 2},
					"max_matches":   map[string]interface{}{"type": "integer", "description": "Max matches to return (default 30, max 100).", "default": 30},
				},
				"required": []string{"source_id", "pattern"},
			},
		},
	}
}

func (*grepTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		SourceID     string        `json:"source_id"`
		Pattern      string        `json:"pattern"`
		PathGlob     string        `json:"path_glob"`
		ContextLines tools.FlexInt `json:"context_lines"`
		MaxMatches   tools.FlexInt `json:"max_matches"`
	}
	args.ContextLines = -1
	ctxLines, maxMatches := tools.EffectiveGrepLimits(args.ContextLines, args.MaxMatches)
	observation := repoGrepObservation(args.SourceID, args.PathGlob, ctxLines, maxMatches)
	if err := json.Unmarshal(raw, &args); err != nil {
		return repoGrepError(observation, "invalid arguments: "+err.Error())
	}
	ctxLines, maxMatches = tools.EffectiveGrepLimits(args.ContextLines, args.MaxMatches)
	observation = repoGrepObservation(args.SourceID, args.PathGlob, ctxLines, maxMatches)
	selected, err := source(env, args.SourceID)
	if err != nil {
		return repoGrepError(observation, err.Error())
	}
	observation.Call.SelectorID = selected.ID
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return repoGrepError(observation, "invalid regex: "+err.Error())
	}

	paths, err := tree(ctx, env, selected)
	if err != nil {
		return repoGrepError(observation, err.Error())
	}
	globRE, err := globToRegexp(args.PathGlob)
	if err != nil {
		return repoGrepError(observation, "invalid path_glob: "+err.Error())
	}

	type hit struct {
		Path    string   `json:"path"`
		Line    int      `json:"line"`
		Context []string `json:"context"`
	}
	var hits []hit
	var observations []GrepMatchObservation
	var telemetryRanges []tools.GrepRangeObservation
	scanned, attempted, readErrors, bytes, contentBytes := 0, 0, 0, 0, 0
	truncatedFiles := false

	for _, p := range paths {
		if globRE != nil && !globRE.MatchString(p) {
			continue
		}
		if attempted >= maxGrepFiles {
			truncatedFiles = true
			break
		}
		attempted++
		content, found, ferr := readFile(ctx, env, selected, p)
		if ferr != nil || !found {
			readErrors++
			continue
		}
		canonicalContent := strings.ReplaceAll(content, "\r\n", "\n")
		fullLines := strings.Split(canonicalContent, "\n")
		body := content
		bodyTruncated := len(body) > grepMaxBytes
		if bodyTruncated {
			body = body[:grepMaxBytes]
		}
		bytes += len(body)
		body = strings.ReplaceAll(body, "\r\n", "\n")
		lines := strings.Split(body, "\n")
		if bodyTruncated && !strings.HasSuffix(body, "\n") {
			lines = lines[:len(lines)-1]
		} else if strings.HasSuffix(body, "\n") {
			lines = lines[:len(lines)-1]
		}
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			lo := i - ctxLines
			if lo < 0 {
				lo = 0
			}
			hi := i + ctxLines + 1
			if hi > len(lines) {
				hi = len(lines)
			}
			context := lines[lo:hi]
			hits = append(hits, hit{Path: p, Line: i + 1, Context: context})
			telemetryRanges = append(telemetryRanges, tools.GrepRangeObservation{
				SelectorID: selected.ID, Path: p, LineStart: lo + 1, LineEnd: hi,
			})
			if hi <= len(fullLines) {
				fullMatch := strings.Join(fullLines[lo:hi], "\n")
				if strings.TrimSpace(fullMatch) != "" && len(fullMatch) <= grepEvidenceMaxBytes && len(observations) < grepEvidenceMaxHits {
					observations = append(observations, GrepMatchObservation{SourceID: selected.ID, Path: p, LineStart: lo + 1, LineEnd: hi})
				}
			}
			contentBytes += len(strings.Join(context, "\n"))
			if len(hits) >= maxMatches {
				break
			}
		}
		scanned++
		if len(hits) >= maxMatches {
			break
		}
	}

	payload := map[string]interface{}{
		"source_id":        selected.ID,
		"pattern":          args.Pattern,
		"path_glob":        args.PathGlob,
		"files_attempted":  attempted,
		"files_scanned":    scanned,
		"file_read_errors": readErrors,
		"total_matches":    len(hits),
		"matches":          hits,
	}
	if truncatedFiles {
		payload["truncated"] = true
		payload["truncated_reason"] = "max_files"
	}
	observation.Call.MatchCount = len(hits)
	observation.Call.FilesAttempted = attempted
	observation.Call.FilesScanned = scanned
	observation.Call.FileReadErrors = readErrors
	observation.Call.FileScanTruncated = truncatedFiles
	observation.Call.ResultTruncated = truncatedFiles || len(hits) >= maxMatches
	observation.Call.Outcome = tools.GrepOutcomeZeroMatches
	if len(hits) > 0 {
		observation.Call.Outcome = tools.GrepOutcomeMatched
	}
	observation.Matches = observations
	observation.Call.ReturnedRanges = telemetryRanges
	if attempted > 0 && readErrors == attempted {
		return repoGrepError(observation, "repository files could not be read")
	}
	return tools.Result{
		BytesFetched: bytes, ContentBytes: contentBytes, Payload: payload,
		Observation: observation,
	}
}

func repoGrepObservation(sourceID, pathFilter string, contextLines, maxMatches int) GrepObservation {
	filter, supplied, length, redacted := tools.ContentFreePathFilter(pathFilter)
	return GrepObservation{Call: tools.GrepCallObservation{
		SelectorID: tools.ContentFreeSelectorID(sourceID), PathFilter: filter, PathFilterSupplied: supplied,
		PathFilterLength: length, PathFilterRedacted: redacted,
		ContextLines: contextLines, MaxMatches: maxMatches, Outcome: tools.GrepOutcomeError,
		ReturnedRanges: []tools.GrepRangeObservation{},
	}}
}

func repoGrepError(observation GrepObservation, message string) tools.Result {
	observation.Call.Outcome = tools.GrepOutcomeError
	return tools.Result{Payload: map[string]interface{}{"error": message}, Observation: observation}
}

// normalizeDir turns a user directory arg into a clean prefix ending in "/"
// (or "" for the root), tolerating a missing or extra trailing slash.
func normalizeDir(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// globToRegexp compiles a path filter. An empty glob matches every path. A glob
// with no "*" is a plain substring match. A glob containing "*" is anchored at
// both ends and "*" becomes ".*", so "*.go" matches only paths ending in .go
// and "config/*.yaml" only paths under config/ ending in .yaml. Returns nil for
// the match-everything case.
func globToRegexp(glob string) (*regexp.Regexp, error) {
	glob = strings.TrimSpace(glob)
	if glob == "" {
		return nil, nil
	}
	if !strings.Contains(glob, "*") {
		return regexp.Compile(regexp.QuoteMeta(glob))
	}
	var b strings.Builder
	b.WriteByte('^')
	for i, part := range strings.Split(glob, "*") {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}
