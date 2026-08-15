package modelprovider

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func testCABundle(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func bundleHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestValidateCABundleConfig(t *testing.T) {
	valid := CABundleConfig{ExistingConfigMap: "provider-ca", Key: "ca-bundle.pem", SHA256: strings.Repeat("a", 64)}
	for _, testCase := range []struct {
		name string
		cfg  CABundleConfig
		want string
	}{
		{name: "disabled"},
		{name: "valid", cfg: valid},
		{name: "partial", cfg: CABundleConfig{ExistingConfigMap: "provider-ca"}, want: "requires ConfigMap name, key, and SHA-256"},
		{name: "uppercase", cfg: CABundleConfig{ExistingConfigMap: "provider-ca", Key: "ca.pem", SHA256: strings.Repeat("A", 64)}, want: "64 lowercase"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateCABundleConfig(testCase.cfg)
			if testCase.want == "" && err != nil {
				t.Fatal(err)
			}
			if testCase.want != "" && (err == nil || !strings.Contains(err.Error(), testCase.want)) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestValidateCABundle(t *testing.T) {
	bundle := testCABundle(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	for _, testCase := range []struct {
		name string
		data []byte
		hash string
		want string
	}{
		{name: "valid", data: bundle, hash: bundleHash(bundle)},
		{name: "wrong hash", data: bundle, hash: strings.Repeat("0", 64), want: "does not match"},
		{name: "empty", hash: strings.Repeat("0", 64), want: "empty"},
		{name: "too large", data: make([]byte, MaxCABundleBytes+1), hash: strings.Repeat("0", 64), want: "exceeds"},
		{name: "private key", data: append(append([]byte(nil), bundle...), privateKey...), hash: bundleHash(append(append([]byte(nil), bundle...), privateKey...)), want: "private keys"},
		{name: "unknown block", data: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")}), hash: bundleHash(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")})), want: "unsupported"},
		{name: "garbage", data: append(append([]byte(nil), bundle...), []byte("garbage")...), hash: bundleHash(append(append([]byte(nil), bundle...), []byte("garbage")...)), want: "non-PEM"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateCABundle(testCase.data, testCase.hash)
			if testCase.want == "" && err != nil {
				t.Fatal(err)
			}
			if testCase.want != "" && (err == nil || !strings.Contains(err.Error(), testCase.want)) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestValidateCABundleEnvironment(t *testing.T) {
	validHash := strings.Repeat("a", 64)
	for _, testCase := range []struct {
		name string
		env  map[string]string
		fail error
		want string
	}{
		{name: "disabled", env: map[string]string{}},
		{name: "valid", env: map[string]string{"NODE_EXTRA_CA_CERTS": CABundleMountPath, CABundleHashEnv: validHash}},
		{name: "missing hash", env: map[string]string{"NODE_EXTRA_CA_CERTS": CABundleMountPath}, want: "environment contract"},
		{name: "alternate path", env: map[string]string{"NODE_EXTRA_CA_CERTS": "/tmp/ca.pem", CABundleHashEnv: validHash}, want: "environment contract"},
		{name: "file failure", env: map[string]string{"NODE_EXTRA_CA_CERTS": CABundleMountPath, CABundleHashEnv: validHash}, fail: errors.New("invalid bundle"), want: "invalid bundle"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) {
				value, ok := testCase.env[name]
				return value, ok
			}
			validated := false
			validateFile := func(path, expected string) error {
				validated = true
				if path != CABundleMountPath || expected != validHash {
					t.Fatalf("validateFile(%q, %q)", path, expected)
				}
				return testCase.fail
			}
			err := ValidateCABundleEnvironment(lookup, validateFile)
			if testCase.want == "" && err != nil {
				t.Fatal(err)
			}
			if testCase.want != "" && (err == nil || !strings.Contains(err.Error(), testCase.want)) {
				t.Fatalf("error = %v", err)
			}
			if testCase.name == "valid" && !validated {
				t.Fatal("bundle was not validated")
			}
		})
	}
}
