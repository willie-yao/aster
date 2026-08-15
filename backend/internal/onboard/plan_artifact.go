package onboard

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	planArtifactSchemaVersion = 2
	maxPlanArtifactBytes      = 4 << 20
)

type planArtifact struct {
	SchemaVersion int               `json:"schema_version"`
	Plan          Plan              `json:"plan"`
	Files         map[string]string `json:"files"`
}

// WritePlanArtifact writes a credential-free reviewed plan to a new private file.
func WritePlanArtifact(path string, plan *Plan) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("onboarding plan output path is required")
	}
	if plan == nil {
		return "", fmt.Errorf("onboarding plan is nil")
	}
	if plan.Destination.OpenPR {
		return "", fmt.Errorf("onboarding plan artifacts do not support open-PR destinations")
	}
	if err := validatePlan(plan); err != nil {
		return "", err
	}
	planCopy := *plan
	planCopy.Files = nil
	if err := bindPlanArtifactDestination(&planCopy); err != nil {
		return "", err
	}
	canonicalPath, err := canonicalPlanArtifactPath(path)
	if err != nil {
		return "", err
	}
	inside, err := pathWithinDirectory(canonicalPath, planCopy.Destination.OutDir)
	if err != nil {
		return "", fmt.Errorf("compare onboarding plan artifact and destination paths: %w", err)
	}
	if inside {
		return "", fmt.Errorf("onboarding plan artifact must be outside the dashboard consumer directory")
	}
	artifact := planArtifact{
		SchemaVersion: planArtifactSchemaVersion,
		Plan:          planCopy,
		Files:         copyPlanFiles(plan.Files),
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal onboarding plan artifact: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxPlanArtifactBytes {
		return "", fmt.Errorf("onboarding plan artifact is %d bytes, exceeds %d bytes", len(data), maxPlanArtifactBytes)
	}
	file, err := os.OpenFile(canonicalPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create onboarding plan artifact: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(canonicalPath)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("write onboarding plan artifact: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("sync onboarding plan artifact: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close onboarding plan artifact: %w", err)
	}
	complete = true
	digest := planArtifactDigest(data)
	plan.reviewedDigest = digest
	return digest, nil
}

// ReadPlanArtifact loads and validates an exact reviewed plan artifact.
func ReadPlanArtifact(path, expectedDigest string) (*Plan, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("onboarding plan artifact path is required")
	}
	expected, err := parsePlanArtifactDigest(expectedDigest)
	if err != nil {
		return nil, err
	}
	canonicalPath, err := canonicalPlanArtifactPath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(canonicalPath)
	if err != nil {
		return nil, fmt.Errorf("inspect onboarding plan artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("onboarding plan artifact must be a regular file, not a symlink")
	}
	if info.Size() > maxPlanArtifactBytes {
		return nil, fmt.Errorf("onboarding plan artifact exceeds %d bytes", maxPlanArtifactBytes)
	}
	file, err := os.Open(canonicalPath)
	if err != nil {
		return nil, fmt.Errorf("open onboarding plan artifact: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPlanArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read onboarding plan artifact: %w", err)
	}
	if len(data) > maxPlanArtifactBytes {
		return nil, fmt.Errorf("onboarding plan artifact exceeds %d bytes", maxPlanArtifactBytes)
	}
	actualSum := sha256.Sum256(data)
	if subtle.ConstantTimeCompare(actualSum[:], expected) != 1 {
		return nil, fmt.Errorf("onboarding plan artifact digest does not match the reviewed digest")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var artifact planArtifact
	if err := decoder.Decode(&artifact); err != nil {
		return nil, fmt.Errorf("decode onboarding plan artifact: %w", err)
	}
	if err := ensurePlanArtifactEOF(decoder); err != nil {
		return nil, err
	}
	if artifact.SchemaVersion != planArtifactSchemaVersion {
		return nil, fmt.Errorf("onboarding plan artifact schema %d is unsupported", artifact.SchemaVersion)
	}
	artifact.Plan.Files = copyPlanFiles(artifact.Files)
	artifact.Plan.reviewedDigest = planArtifactDigest(data)
	if artifact.Plan.Destination.OpenPR {
		return nil, fmt.Errorf("onboarding plan artifacts do not support open-PR destinations")
	}
	if !artifact.Plan.Destination.OpenPR && !filepath.IsAbs(artifact.Plan.Destination.OutDir) {
		return nil, fmt.Errorf("onboarding plan artifact local destination is not absolute")
	}
	if !artifact.Plan.Destination.OpenPR {
		canonical, err := canonicalLocalDestination(artifact.Plan.Destination.OutDir)
		if err != nil {
			return nil, err
		}
		if canonical != artifact.Plan.Destination.OutDir {
			return nil, fmt.Errorf("onboarding plan artifact local destination no longer resolves to the reviewed target")
		}
	}
	if err := validatePlan(&artifact.Plan); err != nil {
		return nil, fmt.Errorf("validate onboarding plan artifact: %w", err)
	}
	return &artifact.Plan, nil
}

func ensurePlanArtifactEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode onboarding plan artifact trailing data: %w", err)
	}
	return fmt.Errorf("onboarding plan artifact contains multiple JSON values")
}

func planArtifactDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func parsePlanArtifactDigest(value string) ([]byte, error) {
	return parseSHA256Digest(value, "onboarding plan digest")
}

func parseSHA256Digest(value, label string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") {
		return nil, fmt.Errorf("%s must use sha256:<hex>", label)
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	if err != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("%s must use sha256:<hex>", label)
	}
	return decoded, nil
}

func copyPlanFiles(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for path, content := range files {
		out[path] = content
	}
	return out
}

func bindPlanArtifactDestination(plan *Plan) error {
	if plan == nil || plan.Destination.OpenPR {
		return nil
	}
	abs, err := filepath.Abs(plan.Destination.OutDir)
	if err != nil {
		return fmt.Errorf("resolve reviewed dashboard consumer directory: %w", err)
	}
	canonical, err := canonicalLocalDestination(abs)
	if err != nil {
		return err
	}
	plan.Destination.OutDir = canonical
	return nil
}

func canonicalLocalDestination(path string) (string, error) {
	return canonicalPathWithExistingAncestors(path, "reviewed dashboard consumer directory")
}

func canonicalPlanArtifactPath(path string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", fmt.Errorf("resolve onboarding plan artifact path: %w", err)
	}
	return canonicalPathWithExistingAncestors(abs, "onboarding plan artifact path")
}

func canonicalPathWithExistingAncestors(path, label string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not absolute", label)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s must not be a symlink", label)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", label, err)
		}
		return filepath.Clean(resolved), nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect %s: %w", label, err)
	}

	current := path
	var suffix []string
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resolve %s: no existing ancestor", label)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect %s ancestor: %w", label, err)
		}
		if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("%s ancestor %s is not a directory", label, current)
		}
		resolvedInfo, err := os.Stat(current)
		if err != nil {
			return "", fmt.Errorf("inspect resolved %s ancestor: %w", label, err)
		}
		if !resolvedInfo.IsDir() {
			return "", fmt.Errorf("%s ancestor %s does not resolve to a directory", label, current)
		}
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return "", fmt.Errorf("resolve %s ancestor: %w", label, err)
		}
		for i := len(suffix) - 1; i >= 0; i-- {
			resolved = filepath.Join(resolved, suffix[i])
		}
		return filepath.Clean(resolved), nil
	}
}

func pathWithinDirectory(path, directory string) (bool, error) {
	rel, err := filepath.Rel(directory, path)
	if err != nil {
		return false, err
	}
	return rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)), nil
}
