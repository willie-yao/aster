package artifacts

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/storage"
)

func TestNewUncachedBackendBrowserIsNotMemoized(t *testing.T) {
	factory := NewBackendFactory(nil, "bucket")
	cached := factory.ForBuild("logs/job/1", "job/1")
	if got := factory.ForBuild("logs/job/1/", "other"); got != cached {
		t.Fatal("ForBuild did not reuse the memoized browser")
	}

	first := NewUncachedBackendBrowser(nil, "bucket", "logs/job/1", "job/1")
	second := NewUncachedBackendBrowser(nil, "bucket", "logs/job/1/", "job/1")
	if first == second || first == cached || second == cached {
		t.Fatal("NewUncachedBackendBrowser reused a memoized browser")
	}
}

func TestNewUncachedBackendBrowserDoesNotRetainFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "logs", "job", "1", "file.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	backend, err := storage.NewLocalBackend(root, "")
	if err != nil {
		t.Fatal(err)
	}
	browser := NewUncachedBackendBrowser(backend, "bucket", "logs/job/1/", "job/1")
	if _, _, err := browser.Read(context.Background(), "file.txt", 0, 16); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, _, err := browser.Read(context.Background(), "file.txt", 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("second read = %q, want uncached content", data)
	}
}

func TestGrepStreamCountsBytes(t *testing.T) {
	data := []byte("first\nmatch here\nlast\n")
	got, err := grepStream(bytes.NewReader(data), int64(len(data)), int64(len(data)), regexp.MustCompile("match"), 1, 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got.BytesScanned != int64(len(data)) {
		t.Fatalf("BytesScanned = %d, want %d", got.BytesScanned, len(data))
	}
	if got.ScanTruncated || got.MatchesTruncated {
		t.Fatalf("complete scan marked truncated: scan=%v matches=%v", got.ScanTruncated, got.MatchesTruncated)
	}
	if len(got.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(got.Matches))
	}
}

func TestGrepStreamReportsLongLine(t *testing.T) {
	line := strings.Repeat("x", 1024*1024+1)
	if _, err := grepStream(strings.NewReader(line), int64(len(line)), int64(len(line)), regexp.MustCompile("x"), 0, 1, 1000); err == nil {
		t.Fatal("expected scanner error for oversized line")
	}
}

func TestGrepStreamMarksByteLimit(t *testing.T) {
	data := []byte("one\ntwo\nthree\n")
	limit := int64(8)
	got, err := grepStream(io.LimitReader(bytes.NewReader(data), limit), int64(len(data)), limit, regexp.MustCompile("two"), 0, 10, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ScanTruncated {
		t.Fatal("limited scan was not marked ScanTruncated")
	}
	if got.BytesScanned != limit {
		t.Fatalf("BytesScanned = %d, want %d", got.BytesScanned, limit)
	}
}

// The display cap and the coverage gap are independent: a capped match list
// still proves the pattern is present, while a short scan cannot prove absence.
func TestGrepStreamSeparatesMatchAndScanTruncation(t *testing.T) {
	const (
		hits  = "hit 1\nhit 2\nhit 3\n"
		quiet = "quiet\nquiet\n"
	)
	re := regexp.MustCompile("hit")

	tests := []struct {
		name             string
		scanned          string
		unscanned        string
		maxMatches       int
		wantMatches      bool
		wantScan         bool
		wantTotalMatches int
	}{
		{name: "display cap only", scanned: hits, maxMatches: 1, wantMatches: true, wantTotalMatches: 3},
		{name: "coverage gap only", scanned: quiet, unscanned: hits, maxMatches: 10, wantScan: true},
		{name: "both", scanned: hits, unscanned: quiet, maxMatches: 1, wantMatches: true, wantScan: true, wantTotalMatches: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limit := int64(len(tc.scanned))
			fileSize := limit + int64(len(tc.unscanned))
			got, err := grepStream(strings.NewReader(tc.scanned), fileSize, limit, re, 0, tc.maxMatches, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if got.MatchesTruncated != tc.wantMatches {
				t.Errorf("MatchesTruncated = %v, want %v", got.MatchesTruncated, tc.wantMatches)
			}
			if got.ScanTruncated != tc.wantScan {
				t.Errorf("ScanTruncated = %v, want %v", got.ScanTruncated, tc.wantScan)
			}
			if got.TotalMatches != tc.wantTotalMatches {
				t.Errorf("TotalMatches = %d, want %d", got.TotalMatches, tc.wantTotalMatches)
			}
		})
	}
}
