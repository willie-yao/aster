package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

var errArtifactBudget = errors.New("artifact byte budget exhausted")

const (
	defaultScopeModelBytes = 8 << 20
	defaultScopeGCSBytes   = 64 << 20
)

type scopeBudget struct {
	mu         sync.Mutex
	modelLimit int
	gcsLimit   int
	modelUsed  int
	gcsUsed    int
}

func newScopeBudget(modelLimit, gcsLimit int) *scopeBudget {
	return &scopeBudget{modelLimit: modelLimit, gcsLimit: gcsLimit}
}

func (b *scopeBudget) remaining() (model, gcs int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, b.modelLimit-b.modelUsed), max(0, b.gcsLimit-b.gcsUsed)
}

func (b *scopeBudget) consumeModel(n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.modelUsed += n
	b.mu.Unlock()
}

func (b *scopeBudget) consumeGCS(n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.gcsUsed += n
	b.mu.Unlock()
}

type bufferedResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{header: make(http.Header)}
}

func (w *bufferedResponseWriter) Header() http.Header { return w.header }

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func writeBudgetedResponse(w http.ResponseWriter, budget *scopeBudget, render func(http.ResponseWriter)) {
	buffer := newBufferedResponseWriter()
	render(buffer)
	remaining, _ := budget.remaining()
	if buffer.body.Len() > remaining {
		http.Error(w, "tool response exceeds model byte budget", http.StatusTooManyRequests)
		return
	}
	for key, values := range buffer.header {
		w.Header()[key] = append([]string(nil), values...)
	}
	status := buffer.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	n, _ := w.Write(buffer.body.Bytes())
	budget.consumeModel(n)
}

type budgetBrowser struct {
	artifacts.Browser
	budget *scopeBudget
}

func (b *budgetBrowser) Read(ctx context.Context, file string, offset, length int) ([]byte, int64, error) {
	_, remaining := b.budget.remaining()
	if remaining == 0 {
		return nil, -1, errArtifactBudget
	}
	if length > remaining {
		return nil, -1, fmt.Errorf("%w: requested %d bytes with %d remaining", errArtifactBudget, length, remaining)
	}
	data, size, err := b.Browser.Read(ctx, file, offset, length)
	b.budget.consumeGCS(len(data))
	return data, size, err
}

func (b *budgetBrowser) Tail(ctx context.Context, file string, lines, maxBytes int) (*artifacts.TailResult, error) {
	_, remaining := b.budget.remaining()
	if remaining == 0 {
		return nil, errArtifactBudget
	}
	if maxBytes > remaining {
		return nil, fmt.Errorf("%w: requested %d bytes with %d remaining", errArtifactBudget, maxBytes, remaining)
	}
	result, err := b.Browser.Tail(ctx, file, lines, maxBytes)
	if result != nil {
		b.budget.consumeGCS(len(result.Content))
	}
	return result, err
}

func (b *budgetBrowser) Grep(ctx context.Context, file string, re *regexp.Regexp, contextLines, maxMatches, maxLineLen, _ int) (*artifacts.GrepResult, error) {
	_, remaining := b.budget.remaining()
	if remaining == 0 {
		return nil, errArtifactBudget
	}
	result, err := b.Browser.Grep(ctx, file, re, contextLines, maxMatches, maxLineLen, remaining)
	if result != nil {
		b.budget.consumeGCS(int(result.BytesScanned))
	}
	return result, err
}

type budgetBackend struct {
	storage.Backend
	budget *scopeBudget
}

func (b *budgetBackend) Open(ctx context.Context, object string) (io.ReadCloser, int64, error) {
	_, remaining := b.budget.remaining()
	if remaining == 0 {
		return nil, -1, errArtifactBudget
	}
	reader, size, err := b.Backend.Open(ctx, object)
	if err != nil {
		return nil, size, err
	}
	if size >= 0 && size > int64(remaining) {
		_ = reader.Close()
		return nil, size, fmt.Errorf("%w: object is %d bytes with %d remaining", errArtifactBudget, size, remaining)
	}
	return &budgetReadCloser{ReadCloser: reader, budget: b.budget, remaining: remaining, objectRemaining: size}, size, nil
}

func (b *budgetBackend) ReadRange(ctx context.Context, object string, offset, length int64) ([]byte, int64, error) {
	_, remaining := b.budget.remaining()
	if length > int64(remaining) {
		return nil, -1, fmt.Errorf("%w: requested %d bytes with %d remaining", errArtifactBudget, length, remaining)
	}
	data, size, err := b.Backend.ReadRange(ctx, object, offset, length)
	b.budget.consumeGCS(len(data))
	return data, size, err
}

func (b *budgetBackend) ReadTail(ctx context.Context, object string, maxBytes int64) ([]byte, int64, error) {
	_, remaining := b.budget.remaining()
	if maxBytes > int64(remaining) {
		return nil, -1, fmt.Errorf("%w: requested %d bytes with %d remaining", errArtifactBudget, maxBytes, remaining)
	}
	data, size, err := b.Backend.ReadTail(ctx, object, maxBytes)
	b.budget.consumeGCS(len(data))
	return data, size, err
}

type budgetReadCloser struct {
	io.ReadCloser
	budget          *scopeBudget
	remaining       int
	objectRemaining int64
}

func (r *budgetReadCloser) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		if r.objectRemaining == 0 {
			return 0, io.EOF
		}
		return 0, errArtifactBudget
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= n
	if r.objectRemaining >= 0 {
		r.objectRemaining -= int64(n)
	}
	r.budget.consumeGCS(n)
	return n, err
}
