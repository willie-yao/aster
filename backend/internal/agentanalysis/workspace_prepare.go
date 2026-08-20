package agentanalysis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/evidenceplan"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
	"golang.org/x/sys/unix"
)

var githubRepositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// WorkspacePreparedSource identifies one prepared immutable checkout.
type WorkspacePreparedSource struct {
	ID         string
	Root       string
	ModePolicy WorkspaceSourceModePolicy
}

// WorkspacePreparedInput is one private content-addressed analyzer snapshot.
type WorkspacePreparedInput struct {
	Root             string
	SourcesRoot      string
	Sources          []WorkspacePreparedSource
	ArtifactRoot     string
	Manifest         WorkspaceManifest
	SourceRoot       string
	SourceModePolicy WorkspaceSourceModePolicy
}

// WorkspacePreparationOptions configure one private input snapshot.
type WorkspacePreparationOptions struct {
	PublicOutputDir string
	InputRoot       string
	ConsumerPrompt  string
	SkillSet        *skills.Set
	Browser         artifacts.Browser
	PrepareSource   func(context.Context, string, sourceinvestigation.Repository) (WorkspaceSourceModePolicy, error)
	ValidateSource  func(context.Context, sourceinvestigation.Repository) error
}

// NewWorkspaceManifestWithSkills seals the current skill-set identity and matched plan.
func NewWorkspaceManifestWithSkills(request ai.FailureAnalysisRequest, source sourceinvestigation.Repository, consumerPrompt string, skillSet *skills.Set, files []WorkspaceFile) (WorkspaceManifest, error) {
	return NewWorkspaceManifestWithSourcesAndSkills(request, []WorkspaceSourceRef{{ID: "primary", Repository: source}}, consumerPrompt, skillSet, files)
}

// NewWorkspaceManifestWithSourcesAndSkills seals a multi-source skill-set input.
func NewWorkspaceManifestWithSourcesAndSkills(request ai.FailureAnalysisRequest, sources []WorkspaceSourceRef, consumerPrompt string, skillSet *skills.Set, files []WorkspaceFile) (WorkspaceManifest, error) {
	if skillSet == nil || strings.TrimSpace(skillSet.Hash()) == "" {
		return WorkspaceManifest{}, fmt.Errorf("%w: workspace skill set is required", ErrInvalidBundle)
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	plan := skillSet.Plan(evidenceplan.FailureSignal(request.TestCase), paths, evidenceplan.CandidatePathLimit)
	return newWorkspaceManifest(request, sources, consumerPrompt, skillSet.Hash(), plan, files)
}

// PrepareWorkspaceInput freezes one exact source and artifact input outside public output.
func PrepareWorkspaceInput(ctx context.Context, request ai.FailureAnalysisRequest, source sourceinvestigation.Repository, opts WorkspacePreparationOptions) (WorkspacePreparedInput, error) {
	return PrepareWorkspaceInputs(ctx, request, []WorkspaceSourceRef{{ID: "primary", Repository: source}}, opts)
}

// PrepareWorkspaceInputs freezes exact source and artifact inputs outside public output.
func PrepareWorkspaceInputs(ctx context.Context, request ai.FailureAnalysisRequest, sources []WorkspaceSourceRef, opts WorkspacePreparationOptions) (WorkspacePreparedInput, error) {
	root, err := resolvePrivateInputRoot(opts.PublicOutputDir, opts.InputRoot, true)
	if err != nil {
		return WorkspacePreparedInput{}, err
	}
	if opts.Browser == nil {
		return WorkspacePreparedInput{}, fmt.Errorf("workspace artifact browser is required")
	}
	if opts.SkillSet == nil || strings.TrimSpace(opts.SkillSet.Hash()) == "" {
		return WorkspacePreparedInput{}, fmt.Errorf("workspace skill set is required")
	}
	canonicalSources, err := canonicalWorkspaceSources(sources)
	if err != nil {
		return WorkspacePreparedInput{}, err
	}
	pending, err := os.MkdirTemp(root, ".pending-")
	if err != nil {
		return WorkspacePreparedInput{}, err
	}
	cleanupPending := true
	defer func() {
		if cleanupPending {
			_ = os.RemoveAll(pending)
		}
	}()
	if err := os.Chmod(pending, 0o700); err != nil && !unsupportedWorkspacePermission(err) {
		return WorkspacePreparedInput{}, err
	}
	sourcesRoot := filepath.Join(pending, WorkspaceSourcesDir)
	if err := os.Mkdir(sourcesRoot, 0o700); err != nil {
		return WorkspacePreparedInput{}, err
	}
	artifactRoot := filepath.Join(pending, WorkspaceArtifactsDir)
	prepareSource := opts.PrepareSource
	validateSource := opts.ValidateSource
	if prepareSource == nil {
		if validateSource == nil {
			validateSource = func(ctx context.Context, source sourceinvestigation.Repository) error {
				return ValidatePublicGitHubSourceTree(ctx, nil, "https://api.github.com", source)
			}
		}
		prepareSource = preparePublicGitHubSource
	}
	preparedSources := make([]WorkspacePreparedSource, 0, len(canonicalSources))
	for _, source := range canonicalSources {
		if validateSource != nil {
			if err := validateSource(ctx, source.Repository); err != nil {
				return WorkspacePreparedInput{}, fmt.Errorf("validate workspace source %s bounds: %w", source.ID, err)
			}
		}
		sourceRoot := filepath.Join(sourcesRoot, source.ID)
		modePolicy, err := prepareSource(ctx, sourceRoot, source.Repository)
		if err != nil {
			return WorkspacePreparedInput{}, fmt.Errorf("prepare workspace source %s: %w", source.ID, err)
		}
		if err := ValidateWorkspaceSourceSnapshot(ctx, sourceRoot); err != nil {
			return WorkspacePreparedInput{}, fmt.Errorf("validate workspace source %s snapshot: %w", source.ID, err)
		}
		preparedSources = append(preparedSources, WorkspacePreparedSource{ID: source.ID, Root: sourceRoot, ModePolicy: modePolicy})
	}
	if err := ValidateAggregateWorkspaceSources(ctx, sourcesRoot); err != nil {
		return WorkspacePreparedInput{}, err
	}
	if err := materializeWorkspaceArtifacts(ctx, opts.Browser, artifactRoot); err != nil {
		return WorkspacePreparedInput{}, err
	}
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		return WorkspacePreparedInput{}, err
	}
	manifest, err := NewWorkspaceManifestWithSourcesAndSkills(request, canonicalSources, opts.ConsumerPrompt, opts.SkillSet, files)
	if err != nil {
		return WorkspacePreparedInput{}, err
	}
	modes := preparedSourceModes(preparedSources)
	lock, err := lockWorkspaceInput(root, manifest.Hash)
	if err != nil {
		return WorkspacePreparedInput{}, err
	}
	defer unlockWorkspaceInput(lock)
	finalRoot := filepath.Join(root, manifest.Hash)
	if err := publishWorkspaceSnapshot(ctx, pending, finalRoot, manifest, modes); err != nil {
		return WorkspacePreparedInput{}, err
	}
	cleanupPending = false
	result := WorkspacePreparedInput{
		Root: finalRoot, SourcesRoot: filepath.Join(finalRoot, WorkspaceSourcesDir), ArtifactRoot: filepath.Join(finalRoot, WorkspaceArtifactsDir),
		Sources: preparedSourcesAtRoot(preparedSources, finalRoot), Manifest: manifest,
	}
	result.SourceRoot = result.Sources[0].Root
	result.SourceModePolicy = result.Sources[0].ModePolicy
	return result, nil
}

func preparedSourceModes(sources []WorkspacePreparedSource) []WorkspaceSourceMode {
	modes := make([]WorkspaceSourceMode, 0, len(sources))
	for _, source := range sources {
		modes = append(modes, WorkspaceSourceMode{SourceID: source.ID, Policy: source.ModePolicy})
	}
	return modes
}

func preparedSourcesAtRoot(sources []WorkspacePreparedSource, root string) []WorkspacePreparedSource {
	result := make([]WorkspacePreparedSource, 0, len(sources))
	for _, source := range sources {
		result = append(result, WorkspacePreparedSource{ID: source.ID, Root: filepath.Join(root, WorkspaceSourcesDir, source.ID), ModePolicy: source.ModePolicy})
	}
	return result
}

// ValidatePrivateInputRoot rejects input storage that resolves inside public output.
func ValidatePrivateInputRoot(publicOutputDir, inputRoot string) error {
	_, err := resolvePrivateInputRoot(publicOutputDir, inputRoot, false)
	return err
}

// CleanupWorkspaceInput removes one exact content-addressed private snapshot.
func CleanupWorkspaceInput(publicOutputDir, inputRoot, manifestHash string) error {
	if !validSHA256(manifestHash) {
		return fmt.Errorf("workspace cleanup manifest hash is invalid")
	}
	root, err := resolvePrivateInputRoot(publicOutputDir, inputRoot, false)
	if err != nil {
		return err
	}
	lock, err := lockWorkspaceInput(root, manifestHash)
	if err != nil {
		return err
	}
	defer unlockWorkspaceInput(lock)
	target := filepath.Join(root, manifestHash)
	if filepath.Dir(target) != root {
		return fmt.Errorf("workspace cleanup target is invalid")
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove workspace input: %w", err)
	}
	return nil
}

func resolvePrivateInputRoot(publicDir, inputRoot string, create bool) (string, error) {
	if !filepath.IsAbs(inputRoot) {
		return "", fmt.Errorf("agent analysis input root must be absolute")
	}
	publicResolved, err := resolvePathWithMissing(publicDir)
	if err != nil {
		return "", err
	}
	resolved, err := resolvePathWithMissing(inputRoot)
	if err != nil {
		return "", err
	}
	if pathWithin(publicResolved, resolved) {
		return "", fmt.Errorf("agent analysis input root resolves inside public output")
	}
	if create {
		if err := os.MkdirAll(resolved, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(resolved, 0o700); err != nil && !unsupportedWorkspacePermission(err) {
			return "", err
		}
		resolved, err = filepath.EvalSymlinks(resolved)
		if err != nil {
			return "", err
		}
		publicResolved, err = resolvePathWithMissing(publicDir)
		if err != nil {
			return "", err
		}
		if pathWithin(publicResolved, resolved) {
			return "", fmt.Errorf("agent analysis input root resolves inside public output")
		}
	}
	info, err := os.Lstat(resolved)
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("agent analysis input root must be a directory")
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func preparePublicGitHubSource(ctx context.Context, destination string, source sourceinvestigation.Repository) (WorkspaceSourceModePolicy, error) {
	if err := sourceinvestigation.ValidateRepository(source); err != nil {
		return "", err
	}
	if !githubRepositoryPart.MatchString(source.Owner) || !githubRepositoryPart.MatchString(source.Name) {
		return "", fmt.Errorf("source repository is not a public GitHub repository")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"-C", destination, "init", "--quiet"},
		{"-C", destination, "remote", "add", "origin", fmt.Sprintf("https://github.com/%s/%s.git", source.Owner, source.Name)},
		{"-C", destination, "fetch", "--quiet", "--depth=1", "--no-tags", "origin", source.Revision},
		{"-C", destination, "checkout", "--quiet", "--detach", "FETCH_HEAD"},
		{"-C", destination, "remote", "remove", "origin"},
	} {
		if err := runWorkspacePreparationGit(ctx, args...); err != nil {
			return "", err
		}
	}
	return ConfigurePreparedSourceModePolicy(ctx, destination, source.Revision)
}

func materializeWorkspaceArtifacts(ctx context.Context, browser artifacts.Browser, destination string) error {
	paths, truncated, err := browser.ListTree(ctx, maxWorkspaceFiles+1)
	if err != nil {
		return fmt.Errorf("list workspace artifacts: %w", err)
	}
	if truncated || len(paths) < 1 || len(paths) > maxWorkspaceFiles {
		return fmt.Errorf("workspace artifact tree must contain between 1 and %d complete paths", maxWorkspaceFiles)
	}
	sort.Strings(paths)
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	var total int64
	previous := ""
	for _, artifactPath := range paths {
		clean, err := artifacts.SafePath(artifactPath)
		if err != nil || clean == "" || clean != artifactPath || clean == previous {
			return fmt.Errorf("workspace artifact path is unsafe or duplicated")
		}
		data, size, err := browser.Read(ctx, clean, 0, int(maxWorkspaceFileBytes+1))
		if err != nil {
			return fmt.Errorf("read workspace artifact %s: %w", clean, err)
		}
		if int64(len(data)) > maxWorkspaceFileBytes || size > maxWorkspaceFileBytes || size >= 0 && size != int64(len(data)) {
			return fmt.Errorf("workspace artifact %s exceeds the bounded snapshot", clean)
		}
		total += int64(len(data))
		if total > maxWorkspaceTotalBytes {
			return fmt.Errorf("workspace artifact bytes exceed %d", maxWorkspaceTotalBytes)
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		previous = clean
	}
	return nil
}

func publishWorkspaceSnapshot(ctx context.Context, pending, final string, manifest WorkspaceManifest, modes []WorkspaceSourceMode) error {
	if err := validateWorkspaceSourceModes(manifest.Sources, modes); err != nil {
		return err
	}
	if info, err := os.Lstat(final); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace input target is invalid")
		}
		for _, source := range manifest.Sources {
			mode, _ := WorkspaceSourceModeFor(modes, source.ID)
			if err := VerifyPreparedSourceWorkspace(ctx, filepath.Join(final, WorkspaceSourcesDir, source.ID), source.Repository.Revision, mode); err != nil {
				return fmt.Errorf("verify workspace source %s: %w", source.ID, err)
			}
		}
		if err := ValidateAggregateWorkspaceSources(ctx, filepath.Join(final, WorkspaceSourcesDir)); err != nil {
			return err
		}
		return VerifyArtifactWorkspace(filepath.Join(final, WorkspaceArtifactsDir), manifest)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(pending, final); err != nil {
		return fmt.Errorf("publish workspace input: %w", err)
	}
	return nil
}

// ValidateAggregateWorkspaceSources enforces one bound across the complete source catalog.
func ValidateAggregateWorkspaceSources(ctx context.Context, root string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace sources root is not a safe directory")
	}
	files := 0
	var total int64
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if current == root || entry.IsDir() {
			return nil
		}
		var size int64
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			size = int64(len(target))
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("workspace sources contain an unsupported file")
			}
			size = info.Size()
		}
		files++
		total += size
		if files > WorkspaceSourceMaxFiles || total > WorkspaceSourceMaxSnapshotBytes {
			return fmt.Errorf("workspace sources exceed aggregate file or byte bounds")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if files == 0 {
		return fmt.Errorf("workspace sources contain no files")
	}
	return nil
}

type boundedCommandOutput struct {
	buf bytes.Buffer
}

func (w *boundedCommandOutput) Write(data []byte) (int, error) {
	remaining := 64<<10 - w.buf.Len()
	if remaining > 0 {
		_, _ = w.buf.Write(data[:min(len(data), remaining)])
	}
	return len(data), nil
}

func runWorkspacePreparationGit(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, "git", args...)
	command.Env = workspacePreparationEnvironment()
	var output boundedCommandOutput
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, boundedGitOutput(output.buf.Bytes()))
	}
	return nil
}

func workspacePreparationEnvironment() []string {
	values := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0"}
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(name); ok {
			values = append(values, name+"="+value)
		}
	}
	return values
}

func unsupportedWorkspacePermission(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS)
}

func lockWorkspaceInput(root, manifestHash string) (*os.File, error) {
	lock, err := openNoFollow(filepath.Join(root, "."+manifestHash+".lock"), unix.O_CREAT|unix.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

func unlockWorkspaceInput(lock *os.File) {
	if lock == nil {
		return
	}
	_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	_ = lock.Close()
}

var _ io.Writer = (*boundedCommandOutput)(nil)
