package server

import (
	"compress/gzip"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

func compressResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &compressionResponseWriter{ResponseWriter: w, request: r}
		var responseWriter http.ResponseWriter = writer
		if flusher, ok := w.(http.Flusher); ok {
			responseWriter = &compressionFlusher{compressionResponseWriter: writer, flusher: flusher}
		}
		defer writer.close()
		next.ServeHTTP(responseWriter, r)
	})
}

type compressionResponseWriter struct {
	http.ResponseWriter
	request *http.Request
	gzip    *gzip.Writer
	wrote   bool
}

func (w *compressionResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *compressionResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	header := w.Header()
	compressible := compressionContentType(header.Get("Content-Type")) &&
		header.Get("Content-Encoding") == "" && !hasNoTransform(header.Get("Cache-Control"))
	if compressible {
		addVary(header, "Accept-Encoding")
	}
	if compressible && responseHasBody(status) && status != http.StatusPartialContent && w.request.Method != http.MethodHead &&
		w.request.Header.Get("Range") == "" && header.Get("Content-Range") == "" &&
		acceptsGzip(w.request.Header.Get("Accept-Encoding")) {
		header.Del("Content-Length")
		header.Set("Content-Encoding", "gzip")
		w.gzip = gzip.NewWriter(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *compressionResponseWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(data))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.gzip != nil {
		return w.gzip.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *compressionResponseWriter) close() {
	if w.gzip != nil {
		_ = w.gzip.Close()
	}
}

type compressionFlusher struct {
	*compressionResponseWriter
	flusher http.Flusher
}

func (w *compressionFlusher) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.gzip != nil {
		_ = w.gzip.Flush()
	}
	w.flusher.Flush()
}

func responseHasBody(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusResetContent && status != http.StatusNotModified
}

func compressionContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	if mediaType == "text/event-stream" {
		return false
	}
	if strings.HasPrefix(mediaType, "text/") || strings.HasSuffix(mediaType, "+json") {
		return true
	}
	switch mediaType {
	case "application/json", "application/javascript", "application/x-javascript",
		"application/xml", "application/xhtml+xml", "image/svg+xml":
		return true
	default:
		return false
	}
}

func acceptsGzip(value string) bool {
	gzipSeen := false
	gzipAllowed := false
	wildcardAllowed := false
	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(item, ";")
		encoding := strings.ToLower(strings.TrimSpace(parts[0]))
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(name, "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				quality = 0
			} else {
				quality = parsed
			}
		}
		switch encoding {
		case "gzip":
			gzipSeen = true
			gzipAllowed = quality > 0
		case "*":
			wildcardAllowed = quality > 0
		}
	}
	if gzipSeen {
		return gzipAllowed
	}
	return wildcardAllowed
}

func addVary(header http.Header, value string) {
	for _, current := range header.Values("Vary") {
		for _, item := range strings.Split(current, ",") {
			item = strings.TrimSpace(item)
			if item == "*" || strings.EqualFold(item, value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}

func hasNoTransform(cacheControl string) bool {
	for _, directive := range strings.Split(cacheControl, ",") {
		if strings.EqualFold(strings.TrimSpace(directive), "no-transform") {
			return true
		}
	}
	return false
}
