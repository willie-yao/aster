package modelprovider

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	// MaxCABundleBytes bounds the public CA material read from a ConfigMap.
	MaxCABundleBytes = 256 << 10
	// CABundleContractVersion identifies the fixed Node extra-CA mount contract.
	CABundleContractVersion = "node-extra-ca-v1"
	// CABundleVolumeName is the fixed executor Pod volume name.
	CABundleVolumeName = "model-provider-ca"
	// CABundleMountDir is the fixed read-only executor mount directory.
	CABundleMountDir = "/etc/prow-ai-dashboard/model-provider-ca"
	// CABundleMountPath is the fixed public CA file path.
	CABundleMountPath = CABundleMountDir + "/ca-bundle.pem"
	// CABundleHashEnv carries the expected public bundle hash into the executor.
	CABundleHashEnv = "PROW_AI_MODEL_PROVIDER_CA_SHA256"
)

var caBundleSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CABundleConfig identifies one exact public CA bundle ConfigMap entry.
type CABundleConfig struct {
	ExistingConfigMap string `json:"existing_config_map,omitempty"`
	Key               string `json:"key,omitempty"`
	SHA256            string `json:"sha256,omitempty"`
}

// Enabled reports whether the complete optional bundle coordinate is configured.
func (c CABundleConfig) Enabled() bool {
	return c.ExistingConfigMap != "" && c.Key != "" && c.SHA256 != ""
}

// ValidateCABundleConfig enforces all-or-none configuration and a lowercase hash.
func ValidateCABundleConfig(c CABundleConfig) error {
	configured := 0
	for _, value := range []string{c.ExistingConfigMap, c.Key, c.SHA256} {
		if value != "" {
			configured++
		}
	}
	if configured == 0 {
		return nil
	}
	if configured != 3 {
		return fmt.Errorf("model provider CA bundle requires ConfigMap name, key, and SHA-256")
	}
	if !caBundleSHA256Pattern.MatchString(c.SHA256) {
		return fmt.Errorf("model provider CA bundle SHA-256 must be 64 lowercase hexadecimal characters")
	}
	return nil
}

// ValidateCABundle verifies exact bytes without exposing certificate metadata.
func ValidateCABundle(data []byte, expectedSHA256 string) error {
	if len(data) == 0 {
		return fmt.Errorf("model provider CA bundle is empty")
	}
	if len(data) > MaxCABundleBytes {
		return fmt.Errorf("model provider CA bundle exceeds %d bytes", MaxCABundleBytes)
	}
	if !caBundleSHA256Pattern.MatchString(expectedSHA256) {
		return fmt.Errorf("model provider CA bundle SHA-256 is invalid")
	}

	remaining := bytes.TrimSpace(data)
	certificates := 0
	for len(remaining) > 0 {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN ")) {
			return fmt.Errorf("model provider CA bundle contains non-PEM data")
		}
		block, rest := pem.Decode(remaining)
		if block == nil {
			return fmt.Errorf("model provider CA bundle contains invalid PEM")
		}
		if strings.Contains(block.Type, "PRIVATE KEY") {
			return fmt.Errorf("model provider CA bundle must not contain private keys")
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return fmt.Errorf("model provider CA bundle contains unsupported PEM material")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("model provider CA bundle contains an invalid certificate")
		}
		certificates++
		remaining = bytes.TrimSpace(rest)
	}
	if certificates == 0 {
		return fmt.Errorf("model provider CA bundle contains no certificates")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expectedSHA256 {
		return fmt.Errorf("model provider CA bundle SHA-256 does not match configuration")
	}
	return nil
}

// ValidateCABundleFile revalidates the exact bytes mounted into an executor.
func ValidateCABundleFile(path, expectedSHA256 string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read model provider CA bundle: %w", err)
	}
	if err := ValidateCABundle(data, expectedSHA256); err != nil {
		return err
	}
	return nil
}

// ValidateProcessCABundleEnvironment verifies the fixed executor environment and mounted bytes.
func ValidateProcessCABundleEnvironment() error {
	return ValidateCABundleEnvironment(os.LookupEnv, ValidateCABundleFile)
}

// ValidateCABundleEnvironment verifies the all-or-none fixed executor environment.
func ValidateCABundleEnvironment(lookup func(string) (string, bool), validateFile func(string, string) error) error {
	expected, hasExpected := lookup(CABundleHashEnv)
	path, hasPath := lookup("NODE_EXTRA_CA_CERTS")
	if !hasExpected && !hasPath {
		return nil
	}
	if !hasExpected || expected == "" || !hasPath || path != CABundleMountPath {
		return fmt.Errorf("model provider CA environment contract is invalid")
	}
	return validateFile(path, expected)
}
