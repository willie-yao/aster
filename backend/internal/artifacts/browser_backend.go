package artifacts

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

// Per-file cache cap. The browser caches whole files at or below this size so
// repeated Read/Tail/Grep on the same small artifact reuse one fetch. Larger
// files are never browser-cached; the gcsweb backend caches those itself to
// emulate ranges.
const smallFileCacheCap = 1 * 1024 * 1024 // 1 MB

// perCallCap bounds a single Grep stream's bytes.
const perCallCap = 64 * 1024 * 1024 // 64 MB

// BackendFactory creates per-build Browsers over a single storage.Backend.
// Browsers are memoized by buildPrefix so analyses of the same build share the
// small-file cache.
type BackendFactory struct {
	backend     storage.Backend
	bucketLabel string

	mu       sync.Mutex
	browsers map[string]*backendBrowser
}

// NewBackendFactory returns a Factory over backend. bucketLabel is used only in
// BuildRoot for logging and the agentic system prompt.
func NewBackendFactory(backend storage.Backend, bucketLabel string) *BackendFactory {
	return &BackendFactory{
		backend:     backend,
		bucketLabel: bucketLabel,
		browsers:    map[string]*backendBrowser{},
	}
}

// ForBuild returns a Browser bound to one Prow build. buildPrefix is the
// bucket-relative, trailing-slashed directory of the build.
func (f *BackendFactory) ForBuild(buildPrefix, displayName string) Browser {
	if !strings.HasSuffix(buildPrefix, "/") {
		buildPrefix += "/"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.browsers[buildPrefix]; ok {
		return b
	}
	b := &backendBrowser{
		backend: f.backend,
		prefix:  buildPrefix,
		root:    f.bucketLabel + "/" + displayName,
		cache:   map[string][]byte{},
	}
	f.browsers[buildPrefix] = b
	return b
}

// backendBrowser implements Browser over a storage.Backend for one build.
type backendBrowser struct {
	backend storage.Backend
	prefix  string // bucket-relative build directory, trailing-slashed
	root    string // human label for BuildRoot

	cacheMu sync.Mutex
	cache   map[string][]byte
}

func (b *backendBrowser) cacheGet(key string) ([]byte, bool) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	data, ok := b.cache[key]
	return data, ok
}

func (b *backendBrowser) cachePut(key string, body []byte) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	b.cache[key] = append([]byte(nil), body...)
}

func (b *backendBrowser) BuildRoot() string { return b.root }

func (b *backendBrowser) List(ctx context.Context, dir string) (*Listing, error) {
	clean, err := SafePath(dir)
	if err != nil {
		return nil, err
	}
	if clean != "" && !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	listing, err := b.backend.List(ctx, b.prefix+clean)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", clean, err)
	}
	out := &Listing{Dir: clean, Dirs: listing.Dirs, Truncated: listing.Truncated}
	for _, f := range listing.Files {
		out.Files = append(out.Files, FileInfo{Name: f.Name, Size: f.Size})
	}
	return out, nil
}

func (b *backendBrowser) ListTree(ctx context.Context, maxPaths int) ([]string, bool, error) {
	if maxPaths <= 0 {
		return nil, false, nil
	}
	return b.backend.ListTree(ctx, b.prefix, maxPaths)
}

func (b *backendBrowser) Read(ctx context.Context, file string, offset, length int) ([]byte, int64, error) {
	clean, err := SafePath(file)
	if err != nil {
		return nil, 0, err
	}
	if clean == "" {
		return nil, 0, fmt.Errorf("read: file path is required")
	}
	if offset < 0 {
		return nil, 0, fmt.Errorf("read: offset must be >= 0")
	}
	if length <= 0 {
		return nil, 0, fmt.Errorf("read: length must be > 0")
	}
	if data, ok := b.cacheGet(clean); ok {
		return sliceCached(data, offset, length), int64(len(data)), nil
	}
	body, total, err := b.backend.ReadRange(ctx, b.prefix+clean, int64(offset), int64(length))
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", clean, err)
	}
	// Cache only when the whole small file was returned.
	if offset == 0 && total >= 0 && int64(len(body)) == total && total <= smallFileCacheCap {
		b.cachePut(clean, body)
	}
	return body, total, nil
}

func sliceCached(data []byte, offset, length int) []byte {
	if offset >= len(data) {
		return nil
	}
	end := offset + length
	if end > len(data) {
		end = len(data)
	}
	return data[offset:end]
}

func (b *backendBrowser) Tail(ctx context.Context, file string, lines, maxBytes int) (*TailResult, error) {
	clean, err := SafePath(file)
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return nil, fmt.Errorf("tail: file path is required")
	}
	if lines <= 0 {
		return nil, fmt.Errorf("tail: lines must be > 0")
	}
	if maxBytes <= 0 || maxBytes > perCallCap {
		maxBytes = perCallCap
	}
	if data, ok := b.cacheGet(clean); ok {
		return tailFromBytes(data, int64(len(data)), lines, maxBytes), nil
	}
	body, total, err := b.backend.ReadTail(ctx, b.prefix+clean, int64(maxBytes))
	if err != nil {
		return nil, fmt.Errorf("tail %s: %w", clean, err)
	}
	if total >= 0 && int64(len(body)) >= total && total <= smallFileCacheCap {
		b.cachePut(clean, body)
	}
	return tailFromBytes(body, total, lines, maxBytes), nil
}

func tailFromBytes(data []byte, fileSize int64, lines, maxBytes int) *TailResult {
	// Drop trailing newline so split doesn't yield a phantom empty line.
	data = bytes.TrimRight(data, "\n")
	// Split on \n. If we fetched a partial suffix the first line may be
	// truncated; drop it unless we got the whole file.
	all := bytes.Split(data, []byte("\n"))
	if fileSize > 0 && int64(len(data)) < fileSize && len(all) > 0 {
		all = all[1:]
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	out := bytes.Join(all, []byte("\n"))
	if len(out) > maxBytes {
		out = out[len(out)-maxBytes:]
	}
	return &TailResult{
		FileSize:      fileSize,
		LinesReturned: len(all),
		Content:       out,
	}
}

func (b *backendBrowser) Grep(ctx context.Context, file string, re *regexp.Regexp, contextLines, maxMatches, maxLineLen int) (*GrepResult, error) {
	clean, err := SafePath(file)
	if err != nil {
		return nil, err
	}
	if clean == "" {
		return nil, fmt.Errorf("grep: file path is required")
	}
	if re == nil {
		return nil, fmt.Errorf("grep: regex is required")
	}
	if contextLines < 0 {
		contextLines = 0
	}
	if maxMatches <= 0 {
		maxMatches = 30
	}
	if maxLineLen <= 0 {
		maxLineLen = 1000
	}
	if data, ok := b.cacheGet(clean); ok {
		return grepStream(bytes.NewReader(data), int64(len(data)), int64(len(data)), re, contextLines, maxMatches, maxLineLen), nil
	}
	rc, size, err := b.backend.Open(ctx, b.prefix+clean)
	if err != nil {
		return nil, fmt.Errorf("grep %s: %w", clean, err)
	}
	defer rc.Close()
	limited := io.LimitReader(rc, perCallCap)
	return grepStream(limited, size, perCallCap, re, contextLines, maxMatches, maxLineLen), nil
}

// grepStream scans r for matching lines with surrounding context. Long lines
// are truncated, and maxBytes caps consumption from r.
func grepStream(r io.Reader, fileSize, maxBytes int64, re *regexp.Regexp, contextLines, maxMatches, maxLineLen int) *GrepResult {
	out := &GrepResult{FileSize: fileSize}

	// Ring buffer of recent lines for before context.
	before := make([]string, contextLines)
	beforeIdx := 0

	// Pending after-context capture for each recorded match.
	type pendingAfter struct {
		matchIdx int
		count    int
	}
	var pendings []*pendingAfter

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if len(line) > maxLineLen {
			line = line[:maxLineLen] + "...<truncated>"
		}

		// Append after context to pending matches.
		stillPending := pendings[:0]
		for _, p := range pendings {
			out.Matches[p.matchIdx].Context = append(
				out.Matches[p.matchIdx].Context,
				fmt.Sprintf("  %d: %s", lineNo, line),
			)
			p.count--
			if p.count > 0 {
				stillPending = append(stillPending, p)
			}
		}
		pendings = stillPending

		if re.MatchString(line) {
			out.TotalMatches++
			if len(out.Matches) < maxMatches {
				ctx := make([]string, 0, 2*contextLines+1)
				for i := 0; i < contextLines; i++ {
					idx := (beforeIdx + i) % contextLines
					if before[idx] == "" {
						continue
					}
					ctx = append(ctx, before[idx])
				}
				ctx = append(ctx, fmt.Sprintf("> %d: %s", lineNo, line))
				m := GrepMatch{LineNo: lineNo, Context: ctx}
				out.Matches = append(out.Matches, m)
				if contextLines > 0 {
					pendings = append(pendings, &pendingAfter{
						matchIdx: len(out.Matches) - 1,
						count:    contextLines,
					})
				}
			} else {
				out.Truncated = true
			}
		}

		// Slide the before buffer.
		if contextLines > 0 {
			before[beforeIdx] = fmt.Sprintf("  %d: %s", lineNo, line)
			beforeIdx = (beforeIdx + 1) % contextLines
		}
	}
	_ = scanner.Err()
	out.BytesScanned = countBytesScanned(maxBytes, lineNo)
	return out
}

// countBytesScanned estimates bytes consumed for logging. LimitReader already
// enforces the read budget.
func countBytesScanned(maxBytes int64, lines int) int64 {
	if int64(lines)*80 > maxBytes {
		return maxBytes
	}
	return int64(lines) * 80
}
