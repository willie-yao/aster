// Package repotree implements read-only agent tools over a source repository's
// file tree. They mirror the shape of the filesystem tools (which read a
// build's GCS artifact tree) but read a GitHub repo at a fixed ref via
// tools.Env.Repo, so the agent can locate the file a fix should touch by
// grepping and reading real source instead of guessing from a path list.
//
// Tools:
//
//	list_repo_tree(path)              - immediate children of a directory
//	read_repo_file(path, offset, len) - byte-range read of one file
//	grep_repo(pattern, path_glob?)    - RE2 search over a bounded file set
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
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

// Group is the alias used to enable all repo tools at once.
const Group = "repotree"

// Bounds shared across tools.
const (
	readMaxBytes = 16384 // per read_repo_file call
	grepMaxBytes = 16384 // per matched file scanned by grep_repo
	maxGrepFiles = 40    // files fetched per grep_repo call
	grepMaxCtx   = 5
	grepMaxHits  = 100
	treeCacheKey = "repotree/tree"
	fileCachePfx = "repotree/file/"
)

// Register adds every tool in this package to the given registry.
func Register(r *tools.Registry) {
	r.Register(&listTool{})
	r.Register(&readTool{})
	r.Register(&grepTool{})
}

// tree returns the repo's blob paths at the bound ref, memoized in the Cache.
func tree(ctx context.Context, env *tools.Env) ([]string, error) {
	if env.Cache != nil {
		if v, ok := env.Cache.Get(treeCacheKey); ok {
			return strings.Split(v, "\n"), nil
		}
	}
	paths, err := env.Repo.ListTree(ctx)
	if err != nil {
		return nil, err
	}
	if env.Cache != nil && len(paths) > 0 {
		env.Cache.Set(treeCacheKey, strings.Join(paths, "\n"))
	}
	return paths, nil
}

// readFile returns a file's full content at the bound ref, memoized in the
// Cache. The bool is false (no error) when the file does not exist.
func readFile(ctx context.Context, env *tools.Env, path string) (string, bool, error) {
	key := fileCachePfx + path
	if env.Cache != nil {
		if v, ok := env.Cache.Get(key); ok {
			return v, true, nil
		}
	}
	content, found, err := env.Repo.ReadFile(ctx, path)
	if err != nil || !found {
		return "", found, err
	}
	if env.Cache != nil {
		env.Cache.Set(key, content)
	}
	return content, true, nil
}

// ---------- list_repo_tree ----------

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
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Directory path relative to the repo root, e.g. \"\" for root, \"config/\", \"pkg/cloud/\".",
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
	paths, err := tree(ctx, env)
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
		"dir":   prefix,
		"dirs":  dirs,
		"files": files,
	}}
}

// ---------- read_repo_file ----------

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
					"path":   map[string]interface{}{"type": "string", "description": "File path relative to the repo root."},
					"offset": map[string]interface{}{"type": "integer", "description": "Byte offset to start from (default 0).", "default": 0},
					"length": map[string]interface{}{"type": "integer", "description": "Bytes to read (default 8192, max 16384).", "default": 8192},
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
	content, found, err := readFile(ctx, env, args.Path)
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
	return tools.Result{
		BytesFetched: len(slice),
		Payload: map[string]interface{}{
			"path":      args.Path,
			"file_size": size,
			"offset":    offset,
			"length":    len(slice),
			"content":   slice,
		},
	}
}

// ---------- grep_repo ----------

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
					"pattern":       map[string]interface{}{"type": "string", "description": "RE2 regex (Go syntax). Use (?i) prefix for case-insensitive."},
					"path_glob":     map[string]interface{}{"type": "string", "description": "Restrict to files whose path matches this substring or *-glob. Strongly recommended; a broad search is capped at 40 files.", "default": ""},
					"context_lines": map[string]interface{}{"type": "integer", "description": "Lines of context before/after each match (default 2, max 5).", "default": 2},
					"max_matches":   map[string]interface{}{"type": "integer", "description": "Max matches to return (default 30, max 100).", "default": 30},
				},
				"required": []string{"pattern"},
			},
		},
	}
}

func (*grepTool) Dispatch(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var args struct {
		Pattern      string        `json:"pattern"`
		PathGlob     string        `json:"path_glob"`
		ContextLines tools.FlexInt `json:"context_lines"`
		MaxMatches   tools.FlexInt `json:"max_matches"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.ErrPayload("invalid arguments: " + err.Error())
	}
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return tools.ErrPayload("invalid regex: " + err.Error())
	}
	ctxLines := args.ContextLines.Int()
	if ctxLines < 0 {
		ctxLines = 2
	}
	if ctxLines > grepMaxCtx {
		ctxLines = grepMaxCtx
	}
	maxMatches := args.MaxMatches.Int()
	if maxMatches <= 0 {
		maxMatches = 30
	}
	if maxMatches > grepMaxHits {
		maxMatches = grepMaxHits
	}

	paths, err := tree(ctx, env)
	if err != nil {
		return tools.ErrPayload(err.Error())
	}
	globRE, err := globToRegexp(args.PathGlob)
	if err != nil {
		return tools.ErrPayload("invalid path_glob: " + err.Error())
	}

	type hit struct {
		Path    string   `json:"path"`
		Line    int      `json:"line"`
		Context []string `json:"context"`
	}
	var hits []hit
	scanned, fetched, bytes := 0, 0, 0
	truncatedFiles := false

	for _, p := range paths {
		if globRE != nil && !globRE.MatchString(p) {
			continue
		}
		if fetched >= maxGrepFiles {
			truncatedFiles = true
			break
		}
		content, found, ferr := readFile(ctx, env, p)
		if ferr != nil || !found {
			continue
		}
		fetched++
		body := content
		if len(body) > grepMaxBytes {
			body = body[:grepMaxBytes]
		}
		bytes += len(body)
		lines := strings.Split(body, "\n")
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
			hits = append(hits, hit{Path: p, Line: i + 1, Context: lines[lo:hi]})
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
		"pattern":       args.Pattern,
		"path_glob":     args.PathGlob,
		"files_scanned": scanned,
		"total_matches": len(hits),
		"matches":       hits,
	}
	if truncatedFiles {
		payload["truncated"] = true
		payload["truncated_reason"] = "max_files"
	}
	return tools.Result{BytesFetched: bytes, Payload: payload}
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
