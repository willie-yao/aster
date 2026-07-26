package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"": false, "br": false, "gzip": true, "GZip; q=1": true,
		"gzip;q=0, *;q=1": false, "br;q=1, *;q=0.5": true, "gzip;q=bad": false,
	}
	for value, want := range cases {
		if got := acceptsGzip(value); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestCompressionGzipsEligibleResponse(t *testing.T) {
	body := strings.Repeat(`{"value":"compress me"}`, 200)
	handler := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Length", "999999")
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodGet, "/data.json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if got := recorder.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want empty", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if !headerContains(recorder.Header(), "Vary", "Accept-Encoding") {
		t.Fatalf("Vary = %q", recorder.Header().Values("Vary"))
	}
	if got := gunzipBody(t, recorder.Body.Bytes()); got != body {
		t.Fatalf("decompressed body length = %d, want %d", len(got), len(body))
	}
}

func TestCompressionPreservesPlainAndRangeResponses(t *testing.T) {
	body := []byte(strings.Repeat("0123456789", 200))
	handler := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "data.json", time.Time{}, bytes.NewReader(body))
	}))

	plain := httptest.NewRecorder()
	handler.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/data.json", nil))
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != string(body) {
		t.Fatalf("plain response headers=%v bytes=%d", plain.Header(), plain.Body.Len())
	}
	if !headerContains(plain.Header(), "Vary", "Accept-Encoding") {
		t.Fatalf("plain Vary = %q", plain.Header().Values("Vary"))
	}

	rangeReq := httptest.NewRequest(http.MethodGet, "/data.json", nil)
	rangeReq.Header.Set("Accept-Encoding", "gzip")
	rangeReq.Header.Set("Range", "bytes=0-9")
	ranged := httptest.NewRecorder()
	handler.ServeHTTP(ranged, rangeReq)
	if ranged.Code != http.StatusPartialContent || ranged.Header().Get("Content-Encoding") != "" || ranged.Body.String() != "0123456789" {
		t.Fatalf("range status=%d headers=%v body=%q", ranged.Code, ranged.Header(), ranged.Body.String())
	}
	if ranged.Header().Get("Content-Range") == "" {
		t.Fatal("range response lost Content-Range")
	}
}

func TestCompressionPreservesHeadAndSSE(t *testing.T) {
	body := []byte(strings.Repeat("console.log('x');", 100))
	fileHandler := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "app.js", time.Time{}, bytes.NewReader(body))
	}))
	headReq := httptest.NewRequest(http.MethodHead, "/app.js", nil)
	headReq.Header.Set("Accept-Encoding", "gzip")
	head := httptest.NewRecorder()
	fileHandler.ServeHTTP(head, headReq)
	if head.Code != http.StatusOK || head.Header().Get("Content-Encoding") != "" || head.Body.Len() != 0 {
		t.Fatalf("HEAD status=%d headers=%v bytes=%d", head.Code, head.Header(), head.Body.Len())
	}

	sseHandler := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("compression wrapper hid Flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: progress\ndata: {}\n\n")
		flusher.Flush()
	}))
	sseReq := httptest.NewRequest(http.MethodGet, "/stream", nil)
	sseReq.Header.Set("Accept-Encoding", "gzip")
	sse := httptest.NewRecorder()
	sseHandler.ServeHTTP(sse, sseReq)
	if sse.Header().Get("Content-Encoding") != "" || sse.Body.String() != "event: progress\ndata: {}\n\n" {
		t.Fatalf("SSE headers=%v body=%q", sse.Header(), sse.Body.String())
	}
}

func TestCompressionSkipsEncodedNoTransformAndNoBodyResponses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		setup  func(http.Header)
	}{
		{name: "encoded", status: http.StatusOK, setup: func(h http.Header) { h.Set("Content-Encoding", "br") }},
		{name: "no transform", status: http.StatusOK, setup: func(h http.Header) { h.Set("Cache-Control", "public, no-transform") }},
		{name: "no content", status: http.StatusNoContent, setup: func(http.Header) {}},
		{name: "not modified", status: http.StatusNotModified, setup: func(http.Header) {}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				testCase.setup(w.Header())
				w.WriteHeader(testCase.status)
				if responseHasBody(testCase.status) {
					_, _ = io.WriteString(w, strings.Repeat("text", 100))
				}
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Header().Get("Content-Encoding") == "gzip" {
				t.Fatalf("response was compressed: %v", recorder.Header())
			}
		})
	}
}

func TestHandlerCompressionPreservesRouteCaching(t *testing.T) {
	dataDir := t.TempDir()
	staticDir := t.TempDir()
	dataBody := `{"entries":"` + strings.Repeat("data", 1000) + `"}`
	assetBody := strings.Repeat("console.log('asset');", 1000)
	writeFile(t, dataDir, "search-index.json", dataBody)
	writeFile(t, staticDir, "index.html", "<!doctype html>")
	writeFile(t, staticDir, "assets/app.js", assetBody)
	handler, err := Handler(Options{DataDir: dataDir, StaticDir: staticDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatal(err)
	}

	dataReq := httptest.NewRequest(http.MethodGet, "/data/search-index.json", nil)
	dataReq.Header.Set("Accept-Encoding", "gzip")
	dataRecorder := httptest.NewRecorder()
	handler.ServeHTTP(dataRecorder, dataReq)
	if dataRecorder.Header().Get("Content-Encoding") != "gzip" || dataRecorder.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("data headers = %v", dataRecorder.Header())
	}
	if got := gunzipBody(t, dataRecorder.Body.Bytes()); got != dataBody {
		t.Fatalf("data body length = %d", len(got))
	}

	assetReq := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	assetReq.Header.Set("Accept-Encoding", "gzip")
	assetRecorder := httptest.NewRecorder()
	handler.ServeHTTP(assetRecorder, assetReq)
	if assetRecorder.Header().Get("Content-Encoding") != "gzip" ||
		assetRecorder.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("asset headers = %v", assetRecorder.Header())
	}
	if got := gunzipBody(t, assetRecorder.Body.Bytes()); got != assetBody {
		t.Fatalf("asset body length = %d", len(got))
	}
}

func gunzipBody(t *testing.T, data []byte) string {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(decoded)
}

func headerContains(header http.Header, name, value string) bool {
	for _, current := range header.Values(name) {
		for _, item := range strings.Split(current, ",") {
			if strings.EqualFold(strings.TrimSpace(item), value) {
				return true
			}
		}
	}
	return false
}
