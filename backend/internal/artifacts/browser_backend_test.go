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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
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
	if got.Truncated {
		t.Fatal("complete scan marked truncated")
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
	if !got.Truncated {
		t.Fatal("limited scan was not marked truncated")
	}
	if got.BytesScanned != limit {
		t.Fatalf("BytesScanned = %d, want %d", got.BytesScanned, limit)
	}
}
