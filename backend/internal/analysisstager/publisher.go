package analysisstager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
	"golang.org/x/sys/unix"
)

const leaseFileName = ".analysis-input-lease"

var githubName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// PublicationResult is the sanitized terminal publisher record.
type PublicationResult struct {
	Version          int                                     `json:"version"`
	Status           string                                  `json:"status"`
	ManifestHash     string                                  `json:"manifest_hash"`
	SourceModePolicy agentanalysis.WorkspaceSourceModePolicy `json:"source_mode_policy"`
}

// CleanupResult is the sanitized terminal cleanup record.
type CleanupResult struct {
	Version      int    `json:"version"`
	Status       string `json:"status"`
	ManifestHash string `json:"manifest_hash"`
}

// PublishOptions configure one namespace-local snapshot publisher.
type PublishOptions struct {
	InputRoot      string
	Client         *http.Client
	PrepareSource  func(context.Context, string, string, string, string) error
	ValidateSource func(context.Context, *http.Client, sourceinvestigation.Repository) error
}

// Publish fetches, verifies, and atomically publishes one content-addressed snapshot.
func Publish(ctx context.Context, request agentanalysis.WorkspacePublishRequest, opts PublishOptions) (PublicationResult, error) {
	var result PublicationResult
	if err := agentanalysis.ValidateWorkspacePublishRequest(request); err != nil {
		return result, err
	}
	root := strings.TrimSpace(opts.InputRoot)
	if root == "" {
		root = defaultInputRoot
	}
	root = filepath.Clean(root)
	if err := requireSafeInputRoot(root); err != nil {
		return result, err
	}
	lock, err := lockSnapshot(root, request.Stage.ManifestHash)
	if err != nil {
		return result, err
	}
	defer unlockSnapshot(lock)
	final := filepath.Join(root, request.Stage.ManifestHash)
	if info, err := os.Lstat(final); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return result, fmt.Errorf("published snapshot target is invalid")
		}
		if err := verifyLease(final, request.LeaseID); err != nil {
			return result, err
		}
		if err := agentanalysis.ValidateWorkspaceSourceSnapshot(ctx, filepath.Join(final, agentanalysis.WorkspaceSourceDir)); err != nil {
			return result, err
		}
		policy, err := agentanalysis.ConfigurePreparedSourceModePolicy(ctx, filepath.Join(final, agentanalysis.WorkspaceSourceDir), request.Stage.Source.Revision)
		if err != nil {
			return result, err
		}
		if _, err := agentanalysis.ReadWorkspaceArtifactManifest(final, request.Stage); err != nil {
			return result, err
		}
		if err := agentanalysis.VerifyArtifactFiles(filepath.Join(final, agentanalysis.WorkspaceArtifactsDir), request.Artifacts); err != nil {
			return result, err
		}
		return PublicationResult{Version: 1, Status: "published", ManifestHash: request.Stage.ManifestHash, SourceModePolicy: policy}, nil
	} else if !os.IsNotExist(err) {
		return result, err
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	validateSource := opts.ValidateSource
	if validateSource == nil {
		validateSource = func(ctx context.Context, client *http.Client, source sourceinvestigation.Repository) error {
			return agentanalysis.ValidatePublicGitHubSourceTree(ctx, client, "https://api.github.com", source)
		}
	}
	source := sourceinvestigation.Repository{Owner: request.Stage.Source.Owner, Name: request.Stage.Source.Name, Revision: request.Stage.Source.Revision}
	if err := validateSource(ctx, client, source); err != nil {
		return result, fmt.Errorf("validate publisher source bounds: %w", err)
	}
	pending, err := os.MkdirTemp(root, ".publish-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(pending)
	sourceRoot := filepath.Join(pending, agentanalysis.WorkspaceSourceDir)
	prepareSource := opts.PrepareSource
	if prepareSource == nil {
		prepareSource = clonePublicSource
	}
	if err := prepareSource(ctx, sourceRoot, request.Stage.Source.Owner, request.Stage.Source.Name, request.Stage.Source.Revision); err != nil {
		return result, err
	}
	if err := agentanalysis.ValidateWorkspaceSourceSnapshot(ctx, sourceRoot); err != nil {
		return result, fmt.Errorf("validate publisher source snapshot: %w", err)
	}
	policy, err := agentanalysis.ConfigurePreparedSourceModePolicy(ctx, sourceRoot, request.Stage.Source.Revision)
	if err != nil {
		return result, err
	}
	artifactRoot := filepath.Join(pending, agentanalysis.WorkspaceArtifactsDir)
	if err := fetchArtifacts(ctx, client, request.Stage.ArtifactBaseURL, artifactRoot, request.Artifacts); err != nil {
		return result, err
	}
	if err := agentanalysis.WriteWorkspaceArtifactManifest(pending, request.Artifacts); err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(pending, leaseFileName), []byte(request.LeaseID+"\n"), 0o600); err != nil {
		return result, err
	}
	if err := os.Rename(pending, final); err != nil {
		return result, fmt.Errorf("publish analyzer snapshot: %w", err)
	}
	return PublicationResult{Version: 1, Status: "published", ManifestHash: request.Stage.ManifestHash, SourceModePolicy: policy}, nil
}

// Cleanup removes one exact leased snapshot.
func Cleanup(_ context.Context, request agentanalysis.WorkspaceCleanupRequest, inputRoot string) (CleanupResult, error) {
	var result CleanupResult
	if err := agentanalysis.ValidateWorkspaceCleanupRequest(request); err != nil {
		return result, err
	}
	root := strings.TrimSpace(inputRoot)
	if root == "" {
		root = defaultInputRoot
	}
	root = filepath.Clean(root)
	if err := requireSafeInputRoot(root); err != nil {
		return result, err
	}
	lock, err := lockSnapshot(root, request.ManifestHash)
	if err != nil {
		return result, err
	}
	defer unlockSnapshot(lock)
	target := filepath.Join(root, request.ManifestHash)
	if info, err := os.Lstat(target); os.IsNotExist(err) {
		return CleanupResult{Version: 1, Status: "absent", ManifestHash: request.ManifestHash}, nil
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return result, fmt.Errorf("cleanup snapshot target is invalid")
	}
	if err := verifyLease(target, request.LeaseID); err != nil {
		return result, err
	}
	if err := os.RemoveAll(target); err != nil {
		return result, fmt.Errorf("remove analyzer snapshot: %w", err)
	}
	return CleanupResult{Version: 1, Status: "deleted", ManifestHash: request.ManifestHash}, nil
}

func requireSafeInputRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("publisher input root must be absolute")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("publisher input root is not a safe directory")
	}
	return nil
}

func clonePublicSource(ctx context.Context, destination, owner, name, revision string) error {
	if !githubName.MatchString(owner) || !githubName.MatchString(name) {
		return fmt.Errorf("publisher source repository is invalid")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	remote := fmt.Sprintf("https://github.com/%s/%s.git", owner, name)
	for _, args := range [][]string{
		{"-C", destination, "init", "--quiet"},
		{"-C", destination, "fetch", "--quiet", "--depth=1", "--no-tags", remote, revision},
		{"-C", destination, "checkout", "--detach", "--quiet", "FETCH_HEAD"},
	} {
		if output, err := runGit(ctx, args...); err != nil {
			return fmt.Errorf("publish source: %v: %s", err, boundedOutput(output))
		}
	}
	if err := os.Remove(filepath.Join(destination, ".git", "FETCH_HEAD")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func fetchArtifacts(ctx context.Context, client *http.Client, baseURL, root string, files []agentanalysis.WorkspaceFile) error {
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	for _, file := range files {
		artifactURL, err := artifactURL(baseURL, file.Path)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("fetch artifact %s: %w", file.Path, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("fetch artifact %s: HTTP %d", file.Path, resp.StatusCode)
		}
		hash := sha256.New()
		target := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			resp.Body.Close()
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			resp.Body.Close()
			return err
		}
		written, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(resp.Body, file.Size+1))
		closeErr := output.Close()
		bodyErr := resp.Body.Close()
		if copyErr != nil || closeErr != nil || bodyErr != nil {
			return fmt.Errorf("copy artifact %s failed", file.Path)
		}
		if written != file.Size || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
			return fmt.Errorf("artifact %s identity changed", file.Path)
		}
	}
	return agentanalysis.VerifyArtifactFiles(root, files)
}

func artifactURL(baseURL, path string) (string, error) {
	base, err := url.ParseRequestURI(baseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", fmt.Errorf("artifact base URL is invalid")
	}
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.Join(parts, "/")
	return base.String(), nil
}

func verifyLease(root, leaseID string) error {
	data, err := os.ReadFile(filepath.Join(root, leaseFileName))
	if err != nil || strings.TrimSpace(string(data)) != leaseID {
		return fmt.Errorf("snapshot lease identity changed")
	}
	return nil
}

func lockSnapshot(root, manifestHash string) (*os.File, error) {
	path := filepath.Join(root, "."+manifestHash+".lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func unlockSnapshot(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}

// PublishPreparedSnapshot copies one already verified local snapshot into private input storage.
func PublishPreparedSnapshot(ctx context.Context, inputRoot string, manifest agentanalysis.WorkspaceManifest, sourceRoot, artifactRoot string, sourceMode agentanalysis.WorkspaceSourceModePolicy) (agentanalysis.WorkspaceSourceModePolicy, error) {
	if err := agentanalysis.ValidateWorkspaceManifest(manifest); err != nil {
		return "", err
	}
	inputRoot = filepath.Clean(inputRoot)
	if err := requireSafeInputRoot(inputRoot); err != nil {
		return "", err
	}
	if err := agentanalysis.VerifyPreparedSourceWorkspace(ctx, sourceRoot, manifest.Source.Revision, sourceMode); err != nil {
		return "", err
	}
	if err := agentanalysis.ValidateWorkspaceSourceSnapshot(ctx, sourceRoot); err != nil {
		return "", err
	}
	if err := agentanalysis.VerifyArtifactFiles(artifactRoot, manifest.Artifacts); err != nil {
		return "", err
	}
	lock, err := lockSnapshot(inputRoot, manifest.Hash)
	if err != nil {
		return "", err
	}
	defer unlockSnapshot(lock)
	final := filepath.Join(inputRoot, manifest.Hash)
	if info, err := os.Lstat(final); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("prepared snapshot target is invalid")
		}
		stage, err := agentanalysis.NewWorkspaceStageRequestWithSourceModePolicies(manifest, sourceMode, agentanalysis.WorkspaceSourceModePreserve)
		if err != nil {
			return "", err
		}
		if _, err := agentanalysis.ReadWorkspaceArtifactManifest(final, stage); err != nil {
			return "", err
		}
		if err := agentanalysis.ValidateWorkspaceSourceSnapshot(ctx, filepath.Join(final, agentanalysis.WorkspaceSourceDir)); err != nil {
			return "", err
		}
		return agentanalysis.ConfigurePreparedSourceModePolicy(ctx, filepath.Join(final, agentanalysis.WorkspaceSourceDir), manifest.Source.Revision)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	pending, err := os.MkdirTemp(inputRoot, ".prepared-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(pending)
	destinationSource := filepath.Join(pending, agentanalysis.WorkspaceSourceDir)
	if err := cloneSource(ctx, sourceRoot, destinationSource, manifest.Source.Revision); err != nil {
		return "", err
	}
	if err := agentanalysis.ValidateWorkspaceSourceSnapshot(ctx, destinationSource); err != nil {
		return "", err
	}
	publishedMode, err := agentanalysis.ConfigurePreparedSourceModePolicy(ctx, destinationSource, manifest.Source.Revision)
	if err != nil {
		return "", err
	}
	if _, _, err := copyTree(ctx, artifactRoot, filepath.Join(pending, agentanalysis.WorkspaceArtifactsDir), len(manifest.Artifacts), 64<<20, 512<<20); err != nil {
		return "", err
	}
	if err := agentanalysis.WriteWorkspaceArtifactManifest(pending, manifest.Artifacts); err != nil {
		return "", err
	}
	if err := os.Rename(pending, final); err != nil {
		return "", err
	}
	return publishedMode, nil
}
