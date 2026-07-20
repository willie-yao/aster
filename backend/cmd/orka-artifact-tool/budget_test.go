package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

type budgetTestBrowser struct {
	artifacts.Browser
	content       []byte
	lastGrepLimit int
}

func (b *budgetTestBrowser) Read(_ context.Context, _ string, offset, length int) ([]byte, int64, error) {
	end := min(offset+length, len(b.content))
	return b.content[offset:end], int64(len(b.content)), nil
}

func (b *budgetTestBrowser) Grep(_ context.Context, _ string, _ *regexp.Regexp, _, _, _, maxBytes int) (*artifacts.GrepResult, error) {
	b.lastGrepLimit = maxBytes
	return &artifacts.GrepResult{BytesScanned: int64(maxBytes)}, nil
}

func TestRequestBudgetRejectsIncompleteReadsAndCountsResponses(t *testing.T) {
	budget := newScopeBudget(10, 3)
	browser := &budgetBrowser{Browser: &budgetTestBrowser{content: []byte("hello")}, budget: budget}
	if _, _, err := browser.Read(context.Background(), "file", 0, 5); err == nil {
		t.Fatal("read larger than remaining budget succeeded")
	}
	data, _, err := browser.Read(context.Background(), "file", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hel" {
		t.Fatalf("data = %q, want hel", data)
	}
	if _, _, err := browser.Read(context.Background(), "file", 3, 1); err == nil {
		t.Fatal("read after budget exhaustion succeeded")
	}
	recorder := httptest.NewRecorder()
	writeBudgetedResponse(recorder, budget, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte("1234"))
	})
	model, gcs := budget.remaining()
	if model != 6 || gcs != 0 {
		t.Fatalf("remaining model=%d gcs=%d, want 6/0", model, gcs)
	}
}

func TestRequestBudgetPassesRemainingLimitIntoGrep(t *testing.T) {
	budget := newScopeBudget(10, 7)
	inner := &budgetTestBrowser{}
	browser := &budgetBrowser{Browser: inner, budget: budget}
	if _, err := browser.Grep(context.Background(), "file", regexp.MustCompile("x"), 0, 1, 1000, 100); err != nil {
		t.Fatal(err)
	}
	if inner.lastGrepLimit != 7 {
		t.Fatalf("grep limit = %d, want remaining budget 7", inner.lastGrepLimit)
	}
	_, remaining := budget.remaining()
	if remaining != 0 {
		t.Fatalf("remaining GCS bytes = %d, want 0", remaining)
	}
}

func TestBudgetedResponseRejectsOversizedPayloadBeforeWrite(t *testing.T) {
	budget := newScopeBudget(3, 10)
	recorder := httptest.NewRecorder()
	writeBudgetedResponse(recorder, budget, func(w http.ResponseWriter) {
		w.Header().Set("X-Tool", "value")
		_, _ = w.Write([]byte("four"))
	})
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if recorder.Header().Get("X-Tool") != "" {
		t.Fatal("oversized response committed tool headers")
	}
}

func TestBudgetBackendBoundsReadAll(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner, err := storage.NewLocalBackend(root, "")
	if err != nil {
		t.Fatal(err)
	}
	backend := &budgetBackend{Backend: inner, budget: newScopeBudget(100, 3)}
	if _, err := storage.ReadAll(context.Background(), backend, "file.txt"); err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("ReadAll error = %v", err)
	}
	_, remaining := backend.budget.remaining()
	if remaining != 3 {
		t.Fatalf("remaining = %d, want unchanged budget 3", remaining)
	}
}

func TestBudgetBackendAllowsObjectExactlyAtLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("hey"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner, err := storage.NewLocalBackend(root, "")
	if err != nil {
		t.Fatal(err)
	}
	backend := &budgetBackend{Backend: inner, budget: newScopeBudget(100, 3)}
	data, err := storage.ReadAll(context.Background(), backend, "file.txt")
	if err != nil || string(data) != "hey" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}
