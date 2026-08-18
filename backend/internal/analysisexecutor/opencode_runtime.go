package analysisexecutor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultOpenCodeRuntimeManifest = "/usr/local/share/aster/opencode-runtime.json"
	openCodeRuntimeManifestVersion = 1
	openCodeUpstreamVersion        = "1.18.2"
	openCodeUpstreamRevision       = "70b56a0a93d366889cae950379cc9d2537148fa2"
	openCodeSourceArchiveSHA256    = "13d277b405def808734be8ce4c6f68d3b40df866358556aefb48b5be90ea53c1"
	openCodeModelsDevSHA256        = "2f6a5a4ab4d450e3ddabdbf0313e51bd76d51577ec1d7936326c484aded33b51"
	openCodeBuilderImage           = "docker.io/oven/bun:1.3.14-alpine@sha256:5acc90a93e91ff07bf72aa90a7c9f0fa189765aec90b47bdbf2152d2196383c0"
	openCodeBuilderBunSHA256       = "500e6edbf321ddf490adcc55a6a01639993a07924616ab67492e1256c15557e2"
	openCodeBunVersion             = "1.3.14"
	openCodePatchVersion           = "aster-disable-project-instructions-v1"
	openCodePatchSHA256            = "48031f5d9a3c675406c43697682291efba78feb208c9f5dc2a977645aa41e6a3"
	openCodeBuildPatchVersion      = "aster-single-target-build-v1"
	openCodeBuildPatchSHA256       = "1d90634eebd407761327da845aa8cb3a72b18ea2dd33e6cd0f1904215db0b595"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type openCodeRuntimeManifest struct {
	Version             int    `json:"version"`
	UpstreamVersion     string `json:"upstream_version"`
	UpstreamRevision    string `json:"upstream_revision"`
	SourceArchiveSHA256 string `json:"source_archive_sha256"`
	ModelsDevSHA256     string `json:"models_dev_sha256"`
	BuilderImage        string `json:"builder_image"`
	BuilderBunSHA256    string `json:"builder_bun_sha256"`
	BunVersion          string `json:"bun_version"`
	EmbeddedWebUI       bool   `json:"embedded_web_ui"`
	PatchVersion        string `json:"patch_version"`
	PatchSHA256         string `json:"patch_sha256"`
	BuildPatchVersion   string `json:"build_patch_version"`
	BuildPatchSHA256    string `json:"build_patch_sha256"`
	BinarySHA256        string `json:"binary_sha256"`
}

func verifyOpenCodeRuntime(ctx context.Context, bin, manifestPath string) error {
	manifest, err := readOpenCodeRuntimeManifest(manifestPath)
	if err != nil {
		return err
	}
	if err := validateOpenCodeRuntimeManifest(manifest); err != nil {
		return err
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("locate OpenCode binary: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return fmt.Errorf("resolve OpenCode binary: %w", err)
	}
	binary, err := os.Open(filepath.Clean(resolved))
	if err != nil {
		return fmt.Errorf("open OpenCode binary: %w", err)
	}
	info, statErr := binary.Stat()
	if statErr != nil {
		_ = binary.Close()
		return fmt.Errorf("inspect OpenCode binary: %w", statErr)
	}
	if info.Size() <= 0 || info.Size() > 512<<20 {
		_ = binary.Close()
		return fmt.Errorf("OpenCode binary size is outside the bound")
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, binary)
	closeErr := binary.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("hash OpenCode binary: %w", err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != manifest.BinarySHA256 {
		return fmt.Errorf("OpenCode binary digest differs from the pinned runtime")
	}
	command := exec.CommandContext(ctx, resolved, "--version")
	command.Env = append(nonCredentialSubprocessEnvironment(), "HOME=/tmp", "TMPDIR=/tmp", "TMP=/tmp", "TEMP=/tmp")
	var stdout bytes.Buffer
	stderr := newBoundedCapture(1024)
	command.Stdout = &stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("read OpenCode version: %w", err)
	}
	if stdout.Len() > 256 {
		return fmt.Errorf("OpenCode version output exceeds the bound")
	}
	if got := strings.TrimSpace(stdout.String()); got != openCodeUpstreamVersion {
		return fmt.Errorf("OpenCode version %q differs from the pinned runtime", got)
	}
	return nil
}

func readOpenCodeRuntimeManifest(path string) (openCodeRuntimeManifest, error) {
	var manifest openCodeRuntimeManifest
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return manifest, fmt.Errorf("read OpenCode runtime manifest: %w", err)
	}
	if len(data) == 0 || len(data) > 16<<10 {
		return manifest, fmt.Errorf("OpenCode runtime manifest is empty or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode OpenCode runtime manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return manifest, fmt.Errorf("decode OpenCode runtime manifest: trailing data")
	}
	return manifest, nil
}

func validateOpenCodeRuntimeManifest(manifest openCodeRuntimeManifest) error {
	if manifest.Version != openCodeRuntimeManifestVersion ||
		manifest.UpstreamVersion != openCodeUpstreamVersion ||
		manifest.UpstreamRevision != openCodeUpstreamRevision ||
		manifest.SourceArchiveSHA256 != openCodeSourceArchiveSHA256 ||
		manifest.ModelsDevSHA256 != openCodeModelsDevSHA256 ||
		manifest.BuilderImage != openCodeBuilderImage ||
		manifest.BuilderBunSHA256 != openCodeBuilderBunSHA256 ||
		manifest.BunVersion != openCodeBunVersion ||
		manifest.EmbeddedWebUI ||
		manifest.PatchVersion != openCodePatchVersion ||
		manifest.PatchSHA256 != openCodePatchSHA256 ||
		manifest.BuildPatchVersion != openCodeBuildPatchVersion ||
		manifest.BuildPatchSHA256 != openCodeBuildPatchSHA256 ||
		!sha256Pattern.MatchString(manifest.BinarySHA256) {
		return fmt.Errorf("OpenCode runtime identity differs from the pinned analyzer contract")
	}
	return nil
}
