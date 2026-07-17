package artifacts

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"
)

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
