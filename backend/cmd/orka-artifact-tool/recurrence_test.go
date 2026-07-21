package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

func TestRecurrenceJUnitFailedStopsAfterMatchingFailure(t *testing.T) {
	root := t.TempDir()
	object := "logs/job/1/artifacts/junit.xml"
	writeRecurrenceObject(t, root, object,
		`<testsuite><testcase name="target"><failure>boom</failure></testcase>`+
			strings.Repeat(`<testcase name="other"/>`, 300000)+`</testsuite>`)
	backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bytesScanned := int64(0)
	failed, matched, err := recurrenceJUnitFailed(context.Background(), backend, object, "target", &bytesScanned)
	if err != nil {
		t.Fatal(err)
	}
	if !failed || !matched {
		t.Fatalf("failed=%t matched=%t", failed, matched)
	}
	if bytesScanned >= 1<<20 {
		t.Fatalf("bytes scanned = %d, want an early bounded match", bytesScanned)
	}
}

func TestRecurrenceJUnitFailedReportsPassingMatch(t *testing.T) {
	root := t.TempDir()
	object := "logs/job/1/artifacts/junit.xml"
	writeRecurrenceObject(t, root, object, `<testsuite><testcase name="target"/></testsuite>`)
	backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bytesScanned := int64(0)
	failed, matched, err := recurrenceJUnitFailed(context.Background(), backend, object, "target", &bytesScanned)
	if err != nil {
		t.Fatal(err)
	}
	if failed || !matched {
		t.Fatalf("failed=%t matched=%t", failed, matched)
	}
}

func TestRecurrenceJUnitFailedRejectsOversizedObject(t *testing.T) {
	root := t.TempDir()
	object := "logs/job/1/artifacts/junit.xml"
	writeRecurrenceObject(t, root, object, strings.Repeat("x", recurrenceMaxObjectBytes+1))
	backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bytesScanned := int64(0)
	if _, _, err := recurrenceJUnitFailed(context.Background(), backend, object, "target", &bytesScanned); err == nil {
		t.Fatal("oversized JUnit object was accepted")
	}
}

func writeRecurrenceObject(t *testing.T, root, object, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(object))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
