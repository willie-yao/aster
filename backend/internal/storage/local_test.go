package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func newTestLocal(t *testing.T) (Backend, string) {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, root, "logs/job/1/build-log.txt", "line1\nline2\nline3\n")
	writeFixture(t, root, "logs/job/1/artifacts/kubelet.log", strings.Repeat("k\n", 100))
	writeFixture(t, root, "logs/job/1/finished.json", `{"result":"FAILURE"}`)
	b, err := NewLocalBackend(root, "")
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	return b, root
}

func TestLocalBackend_OpenAndRange(t *testing.T) {
	b, _ := newTestLocal(t)
	ctx := context.Background()

	data, err := ReadAll(ctx, b, "logs/job/1/build-log.txt")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "line1\nline2\nline3\n" {
		t.Errorf("ReadAll = %q", data)
	}

	chunk, size, err := b.ReadRange(ctx, "logs/job/1/build-log.txt", 6, 5)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	if string(chunk) != "line2" || size != 18 {
		t.Errorf("ReadRange = %q size=%d, want \"line2\" size=18", chunk, size)
	}

	// Offset past EOF returns empty, not an error.
	chunk, _, err = b.ReadRange(ctx, "logs/job/1/build-log.txt", 999, 10)
	if err != nil || len(chunk) != 0 {
		t.Errorf("ReadRange past EOF = %q, %v", chunk, err)
	}
}

func TestLocalBackend_ReadTail(t *testing.T) {
	b, _ := newTestLocal(t)
	tail, size, err := b.ReadTail(context.Background(), "logs/job/1/build-log.txt", 6)
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	if string(tail) != "line3\n" || size != 18 {
		t.Errorf("ReadTail = %q size=%d, want \"line3\\n\" size=18", tail, size)
	}
}

func TestLocalBackend_List(t *testing.T) {
	b, _ := newTestLocal(t)
	got, err := b.List(context.Background(), "logs/job/1/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got.Dirs) != 1 || got.Dirs[0] != "artifacts/" {
		t.Errorf("Dirs = %v, want [artifacts/]", got.Dirs)
	}
	names := []string{}
	for _, f := range got.Files {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "build-log.txt,finished.json" {
		t.Errorf("Files = %v", names)
	}
	// A non-existent prefix lists empty, not an error.
	empty, err := b.List(context.Background(), "logs/job/nope/")
	if err != nil || len(empty.Files) != 0 || len(empty.Dirs) != 0 {
		t.Errorf("List(missing) = %+v, %v", empty, err)
	}
}

func TestLocalBackend_ListTree(t *testing.T) {
	b, _ := newTestLocal(t)
	paths, truncated, err := b.ListTree(context.Background(), "logs/job/1/", 100)
	if err != nil {
		t.Fatalf("ListTree: %v", err)
	}
	if truncated {
		t.Error("unexpected truncation")
	}
	want := "artifacts/kubelet.log,build-log.txt,finished.json"
	if strings.Join(paths, ",") != want {
		t.Errorf("ListTree = %v, want %s", paths, want)
	}
	// Cap truncates and reports it.
	capped, truncated, err := b.ListTree(context.Background(), "logs/job/1/", 2)
	if err != nil {
		t.Fatalf("ListTree cap: %v", err)
	}
	if !truncated || len(capped) != 2 {
		t.Errorf("capped ListTree = %v truncated=%v, want 2 truncated", capped, truncated)
	}
	// max <= 0 returns nothing, matching the gcs/gcsweb backends.
	none, truncated, err := b.ListTree(context.Background(), "logs/job/1/", 0)
	if err != nil || truncated || len(none) != 0 {
		t.Errorf("ListTree(max=0) = %v truncated=%v err=%v, want empty", none, truncated, err)
	}
}

func TestLocalBackend_RejectsTraversal(t *testing.T) {
	b, root := newTestLocal(t)
	// Even if an out-of-root target exists, ".." must be rejected, not aliased.
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "secret.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret.txt", "../../etc/passwd", "/etc/passwd"} {
		if _, _, err := b.Open(context.Background(), p); err == nil {
			t.Errorf("expected rejection for %q", p)
		}
	}
}

func TestLocalBackend_RejectsSymlinkEscape(t *testing.T) {
	b, root := newTestLocal(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "logs", "job", "1", "leak.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, _, err := b.Open(context.Background(), "logs/job/1/leak.txt"); err == nil {
		t.Error("expected rejection of a symlink resolving outside root")
	}
}

func TestLocalBackend_WebURL(t *testing.T) {
	// Without web_base, links are root-relative and never leak the on-disk root.
	b, _ := newTestLocal(t)
	if got := b.WebURL("logs/job/1/build-log.txt"); got != "/logs/job/1/build-log.txt" {
		t.Errorf("WebURL = %q, want a root-relative path with no filesystem leak", got)
	}
	// With web_base, links point at the public root.
	wb, err := NewLocalBackend(t.TempDir(), "https://example.com/bucket")
	if err != nil {
		t.Fatal(err)
	}
	if got := wb.WebURL("logs/x"); got != "https://example.com/bucket/logs/x" {
		t.Errorf("WebURL with web_base = %q", got)
	}
}

func TestNew_LocalProvider(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "logs/job/1/finished.json", `{"result":"FAILURE"}`)
	b, err := New(Config{Provider: ProviderLocal, Base: root}, nil)
	if err != nil {
		t.Fatalf("New(local): %v", err)
	}
	data, err := ReadAll(context.Background(), b, "logs/job/1/finished.json")
	if err != nil || string(data) != `{"result":"FAILURE"}` {
		t.Errorf("read via New(local) = %q, %v", data, err)
	}
	// Local requires base (the root), not bucket.
	if _, err := New(Config{Provider: ProviderLocal}, nil); err == nil {
		t.Error("expected error for local provider without base")
	}
}

func TestNewLocalBackend_Errors(t *testing.T) {
	if _, err := NewLocalBackend(filepath.Join(t.TempDir(), "missing"), ""); err == nil {
		t.Error("expected error for missing root")
	}
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalBackend(f, ""); err == nil {
		t.Error("expected error when root is a file")
	}
}
