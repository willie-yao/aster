package storage

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// queryEscape percent-encodes a URL path or query component.
// Spaces become %20, and escapePath handles slash-separated segments.
func queryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// escapePath percent-encodes each segment of a slash-separated path while
// preserving the slashes.
func escapePath(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// joinURL joins a base URL with bucket-relative path segments.
// A trailing slash on the final segment is preserved for directory bases.
func joinURL(base string, parts ...string) string {
	out := strings.TrimRight(base, "/")
	trailing := false
	for _, p := range parts {
		if p == "" {
			continue
		}
		trailing = strings.HasSuffix(p, "/")
		out += "/" + strings.Trim(p, "/")
	}
	if trailing {
		out += "/"
	}
	return out
}

// trimTotal extracts the "/<total>" suffix from a Content-Range header value.
// Returns -1 if absent or unparseable.
func trimTotal(contentRange string) int64 {
	if contentRange == "" {
		return -1
	}
	i := strings.LastIndex(contentRange, "/")
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(contentRange[i+1:]), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func validateRange(path string, offset, length int64) (int64, error) {
	if offset < 0 {
		return 0, fmt.Errorf("read %s: offset must be >= 0", path)
	}
	if length <= 0 {
		return 0, fmt.Errorf("read %s: length must be > 0", path)
	}
	if length > perCallCap {
		length = perCallCap
	}
	if offset > math.MaxInt64-length {
		return 0, fmt.Errorf("read %s: range overflows int64", path)
	}
	return length, nil
}

// sliceRange returns body[offset:offset+length] clamped to body.
func sliceRange(body []byte, offset, length int64) []byte {
	if offset >= int64(len(body)) {
		return nil
	}
	end := min(offset+length, int64(len(body)))
	return body[offset:end]
}
