package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// localBackend serves Prow artifacts from a local directory tree, mirroring the
// bucket layout under root. It backs hermetic end-to-end tests so the agentic
// tools (list/read/tail/grep) operate on committed fixtures with no network.
// Paths are bucket-relative and use forward slashes, like the other backends.
type localBackend struct {
	root    string
	webBase string
}

// NewLocalBackend returns a Backend reading objects from root. webBase, when
// set, is the prefix returned by WebURL/ProwURL for human-browsable links;
// otherwise a file:// URL into root is returned. root must exist.
func NewLocalBackend(root, webBase string) (Backend, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("storage: local root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("storage: local root %q is not a directory", root)
	}
	return &localBackend{root: root, webBase: strings.TrimRight(webBase, "/")}, nil
}

// resolve maps a bucket-relative path to an on-disk path. It rejects absolute
// paths, ".." segments, and any path whose real (symlink-resolved) location
// would fall outside root, so a fixture cannot read outside the tree.
func (b *localBackend) resolve(path string) (string, error) {
	p := strings.TrimLeft(path, "/")
	if filepath.IsAbs(filepath.FromSlash(path)) {
		return "", fmt.Errorf("storage: path %q is not bucket-relative", path)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("storage: path %q must not contain ..", path)
		}
	}
	full := filepath.Join(b.root, filepath.FromSlash(p))
	rel, err := filepath.Rel(b.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("storage: path %q escapes the local root", path)
	}
	// Defense in depth: when the path exists, reject it if symlinks resolve it
	// outside root. Missing paths fail naturally on the later os call.
	if real, err := filepath.EvalSymlinks(full); err == nil {
		if realRoot, err := filepath.EvalSymlinks(b.root); err == nil {
			rr, err := filepath.Rel(realRoot, real)
			if err != nil || rr == ".." || strings.HasPrefix(rr, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("storage: path %q escapes the local root via symlink", path)
			}
		}
	}
	return full, nil
}

func (b *localBackend) Open(_ context.Context, path string) (io.ReadCloser, int64, error) {
	full, err := b.resolve(path)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func (b *localBackend) ReadRange(_ context.Context, path string, offset, length int64) ([]byte, int64, error) {
	length, err := validateRange(path, offset, length)
	if err != nil {
		return nil, 0, err
	}
	full, err := b.resolve(path)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	if offset >= size {
		return nil, size, nil
	}
	if offset+length > size {
		length = size - offset
	}
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, size, err
	}
	return buf[:n], size, nil
}

func (b *localBackend) ReadTail(_ context.Context, path string, maxBytes int64) ([]byte, int64, error) {
	full, err := b.resolve(path)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	offset := int64(0)
	if maxBytes <= 0 {
		maxBytes = perCallCap
	}
	if size > maxBytes {
		offset = size - maxBytes
	}
	buf := make([]byte, size-offset)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, size, err
	}
	return buf[:n], size, nil
}

func (b *localBackend) List(_ context.Context, prefix string) (*Listing, error) {
	full, err := b.resolve(prefix)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return &Listing{}, nil
		}
		return nil, err
	}
	out := &Listing{}
	for _, e := range entries {
		// Only real files and directories; skip symlinks and irregular entries
		// so a fixture tree cannot reach outside root.
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if e.IsDir() {
			out.Dirs = append(out.Dirs, e.Name()+"/")
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		size := int64(0)
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		out.Files = append(out.Files, Object{Name: e.Name(), Size: size})
	}
	sort.Slice(out.Dirs, func(i, j int) bool { return out.Dirs[i] < out.Dirs[j] })
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	return out, nil
}

func (b *localBackend) ListTree(_ context.Context, prefix string, max int) ([]string, bool, error) {
	if max <= 0 {
		return nil, false, nil
	}
	root, err := b.resolve(prefix)
	if err != nil {
		return nil, false, err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, false, nil
	}
	var paths []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		if max > 0 && len(paths) >= max {
			truncated = true
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return nil, false, walkErr
	}
	sort.Strings(paths)
	return paths, truncated, nil
}

// WebURL returns a human-browsable link for a bucket-relative path. With
// web_base set it points at that public root; otherwise it returns a
// root-relative path rather than a file:// URL, so a local-provider run never
// leaks the on-disk root into published output.
func (b *localBackend) WebURL(path string) string {
	p := strings.TrimLeft(path, "/")
	if b.webBase != "" {
		return b.webBase + "/" + p
	}
	return "/" + p
}

func (b *localBackend) ProwURL(path string) string {
	return b.WebURL(path)
}
