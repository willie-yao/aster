// Package storage abstracts read access to a Prow build-artifact store.
// A Backend exposes object reads, prefix listings, and human-facing URLs.
//
// Three providers implement Backend:
//
//   - gcs: native Google Cloud Storage with HTTP Range and the GCS JSON list API.
//   - gcsweb: any gcsweb HTTP gateway in front of a bucket. HTTP Range is not
//     assumed, so ranged reads are emulated by fetching and slicing.
//   - local: a directory tree used for offline fetches and tests.
//
// All Backend paths are bucket-relative, with no leading slash or bucket prefix.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const maxWholeObjectBytes = 64 * 1024 * 1024

// ErrNotFound identifies a missing storage object across providers.
var ErrNotFound = errors.New("storage object not found")

// Provider names a storage backend implementation.
type Provider string

const (
	ProviderGCS    Provider = "gcs"
	ProviderGCSWeb Provider = "gcsweb"
	// ProviderLocal reads artifacts from a local directory tree mirroring the
	// bucket layout. Base is the root directory. Suited to offline or air-gapped
	// fetches against a downloaded artifact tree, and to hermetic tests.
	ProviderLocal Provider = "local"
)

// Config selects and configures a Backend. Bucket is required for remote
// providers. Local storage uses Base as its root directory.
type Config struct {
	Provider Provider
	Bucket   string
	// Base is the gcsweb gateway root that serves both raw objects and HTML
	// listings. Required for the gcsweb provider; ignored by gcs.
	Base string
	// WebBase is the root for human-browsable build links. Defaults to
	// the gcs default or Base for gcsweb.
	WebBase string
	// ProwBase is the root for Prow deck deep links.
	// Defaults to WebBase when unset for gcsweb.
	ProwBase string
}

// Listing is the result of Backend.List: the immediate children of a prefix.
type Listing struct {
	// Dirs holds subdirectory names, each trailing-slashed and relative to
	// the listed prefix.
	Dirs []string
	// Files holds the immediate file children relative to the listed prefix.
	Files []Object
	// Truncated is true when the listing was capped before exhaustion.
	Truncated bool
}

// Object is a single file entry in a Listing. Size is 0 when unknown.
type Object struct {
	Name string
	Size int64
}

// Backend is read-only access to one Prow artifact bucket. Implementations are
// safe for concurrent use. Paths are bucket-relative.
type Backend interface {
	// Open returns a reader plus total size, or -1 when unknown.
	// The caller must Close the reader.
	Open(ctx context.Context, path string) (io.ReadCloser, int64, error)

	// ReadRange returns up to length bytes starting at offset, plus the
	// object's total size, or -1 when unknown.
	ReadRange(ctx context.Context, path string, offset, length int64) ([]byte, int64, error)

	// ReadTail returns up to maxBytes from the end of the object, plus the
	// total size, or -1 when unknown.
	ReadTail(ctx context.Context, path string, maxBytes int64) ([]byte, int64, error)

	// List returns the immediate children of prefix, paginating internally.
	// prefix should be trailing-slashed.
	List(ctx context.Context, prefix string) (*Listing, error)

	// ListTree returns up to max object paths under prefix, relative to prefix.
	// truncated is true when more objects exist.
	ListTree(ctx context.Context, prefix string, max int) (paths []string, truncated bool, err error)

	// WebURL returns the human-browsable URL for a bucket-relative path.
	WebURL(path string) string

	// ProwURL returns the Prow deck URL for a bucket-relative path.
	ProwURL(path string) string
}

// New builds a Backend from cfg. A nil client uses http.DefaultClient.
func New(cfg Config, client *http.Client) (Backend, error) {
	if client == nil {
		client = http.DefaultClient
	}
	// The local backend reads from a directory, not a bucket, so it requires
	// Base (the root) rather than Bucket.
	if cfg.Provider == ProviderLocal {
		if cfg.Base == "" {
			return nil, fmt.Errorf("storage: local provider requires base (the root directory)")
		}
		return NewLocalBackend(cfg.Base, cfg.WebBase)
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("storage: bucket is required")
	}
	switch cfg.Provider {
	case ProviderGCS:
		return newGCSBackend(cfg, client), nil
	case ProviderGCSWeb:
		if cfg.Base == "" {
			return nil, fmt.Errorf("storage: gcsweb provider requires a base URL")
		}
		return newGCSWebBackend(cfg, client), nil
	case "":
		return nil, fmt.Errorf("storage: provider is required (gcs, gcsweb, or local)")
	default:
		return nil, fmt.Errorf("storage: unknown provider %q (want gcs, gcsweb, or local)", cfg.Provider)
	}
}

// ReadAll reads the whole object at path via the backend.
func ReadAll(ctx context.Context, b Backend, path string) ([]byte, error) {
	rc, size, err := b.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if size > maxWholeObjectBytes {
		return nil, fmt.Errorf("storage: object %s is %d bytes, exceeds %d-byte whole-read limit", path, size, maxWholeObjectBytes)
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxWholeObjectBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxWholeObjectBytes {
		return nil, fmt.Errorf("storage: object %s exceeds %d-byte whole-read limit", path, maxWholeObjectBytes)
	}
	return data, nil
}
