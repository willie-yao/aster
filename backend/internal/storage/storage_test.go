package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// ---------- gcsweb backend ----------

// gcswebServer serves raw file bodies and gcsweb-shaped HTML directory listings.
// It deliberately ignores Range headers, like the real gcsweb.
func gcswebServer(t *testing.T, bucket string, objects map[string]string) *httptest.Server {
	t.Helper()
	routePrefix := "/s3/" + bucket + "/"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, routePrefix)
		if !strings.HasSuffix(r.URL.Path, "/") {
			body, ok := objects[rel]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
			// Ignore Range, like gcsweb: always return the whole body.
			_, _ = w.Write([]byte(body))
			return
		}
		// Directory listing: collect immediate children of rel.
		dirs := map[string]bool{}
		files := map[string]bool{}
		for name := range objects {
			if !strings.HasPrefix(name, rel) {
				continue
			}
			sub := strings.TrimPrefix(name, rel)
			if sub == "" {
				continue
			}
			if i := strings.Index(sub, "/"); i >= 0 {
				dirs[sub[:i+1]] = true
			} else {
				files[sub] = true
			}
		}
		var b strings.Builder
		b.WriteString(`<html><body><ul class="resource-grid">`)
		b.WriteString(`<a href="/styles/style.css">css</a>`) // must be ignored
		// ".." parent link (shorter path) must be ignored.
		b.WriteString(`<a href="` + routePrefix + `"><img src="/icons/back.png"> ..</a>`)
		for d := range dirs {
			href := routePrefix + rel + d
			b.WriteString(`<a href="` + href + `"><img src="/icons/dir.png"> ` + d + `</a>`)
		}
		for f := range files {
			href := routePrefix + rel + f
			b.WriteString(`<a href="` + href + `">` + f + `</a>`)
		}
		b.WriteString(`</ul></body></html>`)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(b.String()))
	}))
}

func TestGCSWebBackend_ReadAndList(t *testing.T) {
	objects := map[string]string{
		"logs/job/1/started.json":          `{"timestamp":100}`,
		"logs/job/1/finished.json":         `{"passed":false}`,
		"logs/job/1/build-log.txt":         "line1\nline2\nline3\n",
		"logs/job/1/artifacts/junit.xml":   "<testsuites/>",
		"logs/job/1/artifacts/sub/more.go": "package x",
	}
	srv := gcswebServer(t, "b", objects)
	defer srv.Close()

	b, err := New(Config{Provider: ProviderGCSWeb, Bucket: "b", Base: srv.URL + "/s3", ProwBase: "https://prow.example/view/s3"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// ReadAll.
	got, err := ReadAll(ctx, b, "logs/job/1/started.json")
	if err != nil || string(got) != `{"timestamp":100}` {
		t.Fatalf("ReadAll started.json = %q, %v", got, err)
	}

	// ReadRange emulated over the whole file.
	rng, total, err := b.ReadRange(ctx, "logs/job/1/build-log.txt", 0, 5)
	if err != nil || string(rng) != "line1" || total != int64(len(objects["logs/job/1/build-log.txt"])) {
		t.Fatalf("ReadRange = %q total=%d err=%v", rng, total, err)
	}

	// ReadTail emulated.
	tail, _, err := b.ReadTail(ctx, "logs/job/1/build-log.txt", 6)
	if err != nil || string(tail) != "line3\n" {
		t.Fatalf("ReadTail = %q err=%v", tail, err)
	}

	// List immediate children: one dir (artifacts/) + three files.
	listing, err := b.List(ctx, "logs/job/1/")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(listing.Dirs, "artifacts/") {
		t.Errorf("dirs = %v, want artifacts/", listing.Dirs)
	}
	var fileNames []string
	for _, f := range listing.Files {
		fileNames = append(fileNames, f.Name)
	}
	sort.Strings(fileNames)
	want := []string{"build-log.txt", "finished.json", "started.json"}
	if strings.Join(fileNames, ",") != strings.Join(want, ",") {
		t.Errorf("files = %v, want %v", fileNames, want)
	}

	// ListTree recursive, relative to prefix.
	tree, trunc, err := b.ListTree(ctx, "logs/job/1/", 100)
	if err != nil || trunc {
		t.Fatalf("ListTree err=%v trunc=%v", err, trunc)
	}
	if !contains(tree, "artifacts/junit.xml") || !contains(tree, "artifacts/sub/more.go") {
		t.Errorf("tree missing nested files: %v", tree)
	}

	// Directory URLs keep their trailing slash.
	if got := b.ProwURL("logs/job/1/"); got != "https://prow.example/view/s3/b/logs/job/1/" {
		t.Errorf("ProwURL = %q", got)
	}
}

func TestGCSWebBackend_ListTreeCap(t *testing.T) {
	objects := map[string]string{}
	for i := 0; i < 20; i++ {
		objects[fmt.Sprintf("d/f%02d.txt", i)] = "x"
	}
	srv := gcswebServer(t, "b", objects)
	defer srv.Close()
	b, _ := New(Config{Provider: ProviderGCSWeb, Bucket: "b", Base: srv.URL + "/s3"}, srv.Client())
	tree, trunc, err := b.ListTree(context.Background(), "d/", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 5 || !trunc {
		t.Errorf("ListTree cap: got %d paths trunc=%v, want 5 truncated", len(tree), trunc)
	}
}

// ---------- gcs backend ----------

// gcsServer serves the subset of the GCS object + JSON list API the backend
// uses: ranged object GETs and delimiter/recursive listings.
func gcsServer(t *testing.T, objects map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/storage/v1/b/") {
			prefix := r.URL.Query().Get("prefix")
			delim := r.URL.Query().Get("delimiter")
			items := []map[string]string{}
			prefixes := map[string]bool{}
			for name := range objects {
				if !strings.HasPrefix(name, prefix) {
					continue
				}
				rest := strings.TrimPrefix(name, prefix)
				if delim == "/" {
					if i := strings.Index(rest, "/"); i >= 0 {
						prefixes[prefix+rest[:i+1]] = true
						continue
					}
				}
				items = append(items, map[string]string{"name": name, "size": fmt.Sprintf("%d", len(objects[name]))})
			}
			var pfx []string
			for p := range prefixes {
				pfx = append(pfx, p)
			}
			resp := map[string]any{"items": items, "prefixes": pfx}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Object GET. Honor a forward Range request.
		rel := strings.TrimPrefix(r.URL.Path, "/b/")
		body, ok := objects[rel]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
			var start, end int
			spec := strings.TrimPrefix(rng, "bytes=")
			if strings.HasPrefix(spec, "-") {
				fmt.Sscanf(spec, "-%d", &end)
				start = len(body) - end
				if start < 0 {
					start = 0
				}
				end = len(body) - 1
			} else {
				fmt.Sscanf(spec, "%d-%d", &start, &end)
			}
			if end >= len(body) {
				end = len(body) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(body[start : end+1]))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write([]byte(body))
	}))
}

func TestGCSBackend_RangeAndList(t *testing.T) {
	objects := map[string]string{
		"logs/job/1/started.json": `{"timestamp":1}`,
		"logs/job/2/started.json": `{"timestamp":2}`,
		"logs/job/1/build.txt":    "abcdefghij",
	}
	srv := gcsServer(t, objects)
	defer srv.Close()
	// Construct gcsBackend directly so endpoints point at the test server.
	b := &gcsBackend{
		bucket:    "b",
		client:    srv.Client(),
		objectURL: srv.URL + "/b/",
		listURL:   srv.URL + "/storage/v1/b/b/o",
		webBase:   "https://web.example",
		prowBase:  "https://prow.example",
	}
	ctx := context.Background()

	rng, total, err := b.ReadRange(ctx, "logs/job/1/build.txt", 2, 3)
	if err != nil || string(rng) != "cde" || total != 10 {
		t.Fatalf("ReadRange = %q total=%d err=%v", rng, total, err)
	}
	tail, _, err := b.ReadTail(ctx, "logs/job/1/build.txt", 3)
	if err != nil || string(tail) != "hij" {
		t.Fatalf("ReadTail = %q err=%v", tail, err)
	}
	listing, err := b.List(ctx, "logs/")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(listing.Dirs)
	if strings.Join(listing.Dirs, ",") != "job/" {
		t.Errorf("dirs = %v, want [job/]", listing.Dirs)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestURLEscaping(t *testing.T) {
	cases := map[string]string{
		"space here": "space%20here",
		"slash/here": "slash%2Fhere",
		"日本語":       "%E6%97%A5%E6%9C%AC%E8%AA%9E",
		"a?b=c&d+e":  "a%3Fb%3Dc%26d%2Be",
	}
	for input, want := range cases {
		if got := queryEscape(input); got != want {
			t.Errorf("queryEscape(%q) = %q, want %q", input, got, want)
		}
	}
	if got := escapePath("space here/slash"); got != "space%20here/slash" {
		t.Errorf("escapePath = %q", got)
	}
}

func TestURLsPreserveTrailingSlash(t *testing.T) {
	// Directory URLs must keep their trailing slash: callers (the AI artifact
	// link base, the notifier's Prow base) concatenate relative paths directly.
	gw, _ := New(Config{Provider: ProviderGCSWeb, Bucket: "b", Base: "https://gw/s3", ProwBase: "https://prow/view/s3"}, nil)
	if got := gw.WebURL("logs/job/1/"); got != "https://gw/s3/b/logs/job/1/" {
		t.Errorf("gcsweb WebURL(dir) = %q, want trailing slash", got)
	}
	if got := gw.ProwURL("logs/"); got != "https://prow/view/s3/b/logs/" {
		t.Errorf("gcsweb ProwURL(dir) = %q, want trailing slash", got)
	}
	if got := gw.WebURL("logs/job/1/build-log.txt"); strings.HasSuffix(got, "/") {
		t.Errorf("gcsweb WebURL(file) = %q, should not end in slash", got)
	}

	gcs := &gcsBackend{bucket: "b", webBase: "https://web", prowBase: "https://prow"}
	if got := gcs.WebURL("logs/job/1/"); got != "https://web/b/logs/job/1/" {
		t.Errorf("gcs WebURL(dir) = %q, want trailing slash", got)
	}
	if got := gcs.WebURL("logs/job/1/x.txt"); strings.HasSuffix(got, "/") {
		t.Errorf("gcs WebURL(file) = %q, should not end in slash", got)
	}
}

func TestStreamTail(t *testing.T) {
	// streamTail keeps only the last maxBytes regardless of total size, which
	// is how gcsweb tails objects larger than the buffer cap.
	data := strings.Repeat("abcdefghij", 1000) // 10000 bytes
	got, total := streamTail(strings.NewReader(data), 7)
	if string(got) != "defghij" || total != 10000 {
		t.Errorf("streamTail = %q total=%d, want last 7 bytes of 10000", got, total)
	}
	// maxBytes larger than the stream returns the whole thing.
	got, total = streamTail(strings.NewReader("short"), 100)
	if string(got) != "short" || total != 5 {
		t.Errorf("streamTail small = %q total=%d", got, total)
	}
}

func TestSliceRange(t *testing.T) {
	body := []byte("0123456789")
	if got := sliceRange(body, 2, 3); string(got) != "234" {
		t.Errorf("sliceRange(2,3) = %q", got)
	}
	if got := sliceRange(body, 8, 100); string(got) != "89" {
		t.Errorf("sliceRange clamp = %q", got)
	}
	if got := sliceRange(body, 20, 5); got != nil {
		t.Errorf("sliceRange past end = %q, want nil", got)
	}
}
