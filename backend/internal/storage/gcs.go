package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Default human-facing URL roots for the gcs provider.
const (
	defaultGCSObjectBase = "https://storage.googleapis.com"
	defaultGCSWebBase    = "https://gcsweb.k8s.io/gcs"
	defaultGCSProwBase   = "https://prow.k8s.io/view/gs"
)

// perCallCap bounds the bytes any single read pulls from the store, a
// circuit-breaker against a confused or malicious range request.
const perCallCap = 64 * 1024 * 1024 // 64 MB

// gcsBackend reads native Google Cloud Storage objects and listings.
type gcsBackend struct {
	bucket    string
	client    *http.Client
	objectURL string // "https://storage.googleapis.com/<bucket>/"
	listURL   string // "https://storage.googleapis.com/storage/v1/b/<bucket>/o"
	webBase   string
	prowBase  string
}

func newGCSBackend(cfg Config, client *http.Client) *gcsBackend {
	webBase := cfg.WebBase
	if webBase == "" {
		webBase = defaultGCSWebBase
	}
	prowBase := cfg.ProwBase
	if prowBase == "" {
		prowBase = defaultGCSProwBase
	}
	return &gcsBackend{
		bucket:    cfg.Bucket,
		client:    client,
		objectURL: defaultGCSObjectBase + "/" + cfg.Bucket + "/",
		listURL:   defaultGCSObjectBase + "/storage/v1/b/" + cfg.Bucket + "/o",
		webBase:   webBase,
		prowBase:  prowBase,
	}
}

func (b *gcsBackend) objURL(path string) string {
	return b.objectURL + escapePath(strings.TrimLeft(path, "/"))
}

func (b *gcsBackend) Open(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.objURL(path), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, 0, fmt.Errorf("open %s: %w", path, ErrNotFound)
		}
		return nil, 0, fmt.Errorf("open %s: HTTP %d", path, resp.StatusCode)
	}
	size := int64(-1)
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			size = n
		}
	}
	return resp.Body, size, nil
}

func (b *gcsBackend) ReadRange(ctx context.Context, path string, offset, length int64) ([]byte, int64, error) {
	length, err := validateRange(path, offset, length)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.objURL(path), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, 0, fmt.Errorf("read %s: HTTP %d", path, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK {
		// Server ignored Range and returned the whole object from byte 0.
		// Read offset+length and slice so we still return the right window.
		full, err := io.ReadAll(io.LimitReader(resp.Body, offset+length))
		if err != nil {
			return nil, 0, fmt.Errorf("read %s: %w", path, err)
		}
		return sliceRange(full, offset, length), int64(len(full)), nil
	}
	// Partial Content means the body is exactly the requested window.
	body, err := io.ReadAll(io.LimitReader(resp.Body, length))
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", path, err)
	}
	return body, trimTotal(resp.Header.Get("Content-Range")), nil
}

func (b *gcsBackend) ReadTail(ctx context.Context, path string, maxBytes int64) ([]byte, int64, error) {
	if maxBytes <= 0 || maxBytes > perCallCap {
		maxBytes = perCallCap
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.objURL(path), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=-%d", maxBytes))
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("tail %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, 0, fmt.Errorf("tail %s: HTTP %d", path, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK {
		// Server ignored the suffix Range and returned the whole object; keep
		// the last maxBytes so we still return the tail.
		full, err := io.ReadAll(io.LimitReader(resp.Body, perCallCap))
		if err != nil {
			return nil, 0, fmt.Errorf("tail %s: %w", path, err)
		}
		if int64(len(full)) > maxBytes {
			return full[int64(len(full))-maxBytes:], int64(len(full)), nil
		}
		return full, int64(len(full)), nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("tail %s: %w", path, err)
	}
	return body, trimTotal(resp.Header.Get("Content-Range")), nil
}

// gcsListPage is the subset of the GCS JSON list response we consume.
type gcsListPage struct {
	Items []struct {
		Name string `json:"name"`
		Size string `json:"size"`
	} `json:"items"`
	Prefixes      []string `json:"prefixes"`
	NextPageToken string   `json:"nextPageToken"`
}

func (b *gcsBackend) listPage(ctx context.Context, prefix, delimiter, pageToken string) (*gcsListPage, error) {
	u := fmt.Sprintf("%s?prefix=%s&maxResults=1000", b.listURL, queryEscape(prefix))
	if delimiter != "" {
		u += "&delimiter=" + queryEscape(delimiter)
	}
	if pageToken != "" {
		u += "&pageToken=" + queryEscape(pageToken)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %s: HTTP %d", prefix, resp.StatusCode)
	}
	var page gcsListPage
	if err := json.NewDecoder(io.LimitReader(resp.Body, perCallCap)).Decode(&page); err != nil {
		return nil, fmt.Errorf("list %s: decode: %w", prefix, err)
	}
	return &page, nil
}

func (b *gcsBackend) List(ctx context.Context, prefix string) (*Listing, error) {
	out := &Listing{}
	pageToken := ""
	for {
		page, err := b.listPage(ctx, prefix, "/", pageToken)
		if err != nil {
			return nil, err
		}
		for _, p := range page.Prefixes {
			out.Dirs = append(out.Dirs, strings.TrimPrefix(p, prefix))
		}
		for _, it := range page.Items {
			name := strings.TrimPrefix(it.Name, prefix)
			if name == "" {
				continue
			}
			size, _ := strconv.ParseInt(it.Size, 10, 64)
			out.Files = append(out.Files, Object{Name: name, Size: size})
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return out, nil
}

func (b *gcsBackend) ListTree(ctx context.Context, prefix string, max int) ([]string, bool, error) {
	if max <= 0 {
		return nil, false, nil
	}
	var out []string
	pageToken := ""
	for {
		page, err := b.listPage(ctx, prefix, "", pageToken)
		if err != nil {
			return nil, false, err
		}
		for _, it := range page.Items {
			name := strings.TrimPrefix(it.Name, prefix)
			if name == "" || strings.HasSuffix(name, "/") {
				continue
			}
			out = append(out, name)
			if len(out) >= max {
				return out, true, nil
			}
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return out, false, nil
}

func (b *gcsBackend) WebURL(path string) string {
	return joinURL(b.webBase, b.bucket, path)
}

func (b *gcsBackend) ProwURL(path string) string {
	return joinURL(b.prowBase, b.bucket, path)
}
