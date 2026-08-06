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
	planArtifactSchemaVersion = 1
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
	if err := validatePlan(plan); err != nil {
		return "", err
	}
	planCopy := *plan
	planCopy.Files = nil
	if err := bindPlanArtifactDestination(&planCopy); err != nil {
		return "", err
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
	file, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create onboarding plan artifact: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(filepath.Clean(path))
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
	return planArtifactDigest(data), nil
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
	info, err := os.Lstat(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("inspect onboarding plan artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("onboarding plan artifact must be a regular file, not a symlink")
	}
	if info.Size() > maxPlanArtifactBytes {
		return nil, fmt.Errorf("onboarding plan artifact exceeds %d bytes", maxPlanArtifactBytes)
	}
	file, err := os.Open(filepath.Clean(path))
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
	if !artifact.Plan.Destination.OpenPR && !filepath.IsAbs(artifact.Plan.Destination.OutDir) {
		return nil, fmt.Errorf("onboarding plan artifact local destination is not absolute")
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
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") {
		return nil, fmt.Errorf("onboarding plan digest must use sha256:<hex>")
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	if err != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("onboarding plan digest must use sha256:<hex>")
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
	plan.Destination.OutDir = filepath.Clean(abs)
	return nil
}
