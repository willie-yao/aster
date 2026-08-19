package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKey generates a small RSA key. 2048 bits keeps the test fast while still
// exercising the real signing path.
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return key
}

func pkcs1PEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func pkcs8PEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling pkcs8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestParsePrivateKeyAcceptsBothPEMForms(t *testing.T) {
	key := testKey(t)
	cases := []struct {
		name string
		pem  string
	}{
		{name: "pkcs1", pem: pkcs1PEM(t, key)},
		{name: "pkcs8", pem: pkcs8PEM(t, key)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePrivateKey(tc.pem)
			if err != nil {
				t.Fatalf("parsePrivateKey: %v", err)
			}
			if !got.Equal(key) {
				t.Fatal("parsed key does not match the original")
			}
		})
	}
}

func TestParsePrivateKeyRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		pem  string
	}{
		{name: "empty", pem: "   "},
		{name: "not pem", pem: "definitely-not-a-pem"},
		{name: "pem with junk body", pem: "-----BEGIN RSA PRIVATE KEY-----\nZm9v\n-----END RSA PRIVATE KEY-----"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parsePrivateKey(tc.pem); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNewRequiresAppID(t *testing.T) {
	if _, err := New(nil, Credentials{PrivateKey: pkcs1PEM(t, testKey(t))}); err == nil {
		t.Fatal("expected an error for a missing app id")
	}
}

// TestMintJWTIsVerifiable proves the assertion is a well-formed RS256 JWT that
// verifies against the App's public key and carries the claims GitHub requires.
func TestMintJWTIsVerifiable(t *testing.T) {
	key := testKey(t)
	client, err := New(nil, Credentials{AppID: "12345", PrivateKey: pkcs1PEM(t, key)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }

	assertion, err := client.mintJWT()
	if err != nil {
		t.Fatalf("mintJWT: %v", err)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d segments, want 3", len(parts))
	}

	enc := base64.RawURLEncoding
	var header map[string]string
	headerJSON, err := enc.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decoding header: %v", err)
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("unmarshaling header: %v", err)
	}
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatalf("header = %v, want RS256/JWT", header)
	}

	var claims struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}
	claimsJSON, err := enc.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshaling claims: %v", err)
	}
	if claims.ISS != "12345" {
		t.Errorf("iss = %q, want %q", claims.ISS, "12345")
	}
	if want := now.Add(-jwtBackdate).Unix(); claims.IAT != want {
		t.Errorf("iat = %d, want %d (backdated for clock drift)", claims.IAT, want)
	}
	// GitHub rejects an assertion whose lifetime exceeds ten minutes.
	if lifetime := time.Duration(claims.EXP-claims.IAT) * time.Second; lifetime > 10*time.Minute {
		t.Errorf("assertion lifetime %s exceeds the GitHub maximum of 10m", lifetime)
	}

	signature, err := enc.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decoding signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

// appServer stands in for the GitHub App API, counting calls so caching is
// observable.
type appServer struct {
	server     *httptest.Server
	appCalls   int
	instCalls  int
	tokenCalls int
	slug       string
	expiresAt  string
	instStatus int
}

func newAppServer(t *testing.T) *appServer {
	t.Helper()
	s := &appServer{slug: "aster-example", expiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		s.appCalls++
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"slug":%q}`, s.slug)
	})
	mux.HandleFunc("/repos/o/r/installation", func(w http.ResponseWriter, _ *http.Request) {
		s.instCalls++
		if s.instStatus != 0 {
			w.WriteHeader(s.instStatus)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, `{"id":42}`)
	})
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		s.tokenCalls++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_installation_%d","expires_at":%q}`, s.tokenCalls, s.expiresAt)
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *appServer) client(t *testing.T) *Client {
	t.Helper()
	client, err := New(s.server.Client(), Credentials{AppID: "1", PrivateKey: pkcs1PEM(t, testKey(t))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.base = s.server.URL
	return client
}

func TestLoginReturnsBotAccountAndCaches(t *testing.T) {
	server := newAppServer(t)
	client := server.client(t)

	for range 3 {
		login, err := client.Login(context.Background())
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		if want := "aster-example[bot]"; login != want {
			t.Fatalf("Login = %q, want %q", login, want)
		}
	}
	if server.appCalls != 1 {
		t.Fatalf("app identity fetched %d times, want 1 (cached)", server.appCalls)
	}
}

func TestInstallationTokenCachesUntilNearExpiry(t *testing.T) {
	server := newAppServer(t)
	client := server.client(t)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }
	server.expiresAt = now.Add(time.Hour).Format(time.RFC3339)

	first, err := client.InstallationToken(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	second, err := client.InstallationToken(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if first != second {
		t.Fatalf("token changed while still valid: %q then %q", first, second)
	}
	if server.tokenCalls != 1 {
		t.Fatalf("minted %d tokens, want 1 (cached)", server.tokenCalls)
	}

	// Move to inside the refresh skew: the cached token must be discarded
	// before it can lapse mid-request.
	now = now.Add(time.Hour).Add(-tokenRefreshSkew).Add(time.Second)
	server.expiresAt = now.Add(time.Hour).Format(time.RFC3339)
	third, err := client.InstallationToken(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if third == first {
		t.Fatal("token was reused inside the refresh skew")
	}
	if server.tokenCalls != 2 {
		t.Fatalf("minted %d tokens, want 2 after refresh", server.tokenCalls)
	}
}

// TestInstallationTokenUnparsableExpiryDoesNotCache proves an unreadable expiry
// is never treated as long-lived.
func TestInstallationTokenUnparsableExpiryDoesNotCache(t *testing.T) {
	server := newAppServer(t)
	server.expiresAt = "not-a-timestamp"
	client := server.client(t)

	for range 2 {
		if _, err := client.InstallationToken(context.Background(), "o", "r"); err != nil {
			t.Fatalf("InstallationToken: %v", err)
		}
	}
	if server.tokenCalls != 2 {
		t.Fatalf("minted %d tokens, want 2 (an unreadable expiry must not cache)", server.tokenCalls)
	}
}

func TestInstallationTokenReportsMissingInstallation(t *testing.T) {
	server := newAppServer(t)
	server.instStatus = http.StatusNotFound
	client := server.client(t)

	_, err := client.InstallationToken(context.Background(), "o", "r")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not installed on o/r") {
		t.Fatalf("error = %v, want it to name the missing installation", err)
	}
}

func TestCredentialsFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		key     string
		wantOK  bool
		wantID  string
		wantKey string
	}{
		{name: "unset", wantOK: false},
		{name: "both set", id: " 55 ", key: " pem ", wantOK: true, wantID: "55", wantKey: "pem"},
		{name: "partial still reported so the error names what is missing", id: "55", wantOK: true, wantID: "55"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvAppID, tc.id)
			t.Setenv(EnvPrivateKey, tc.key)

			creds, ok := CredentialsFromEnv()
			if ok != tc.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tc.wantOK)
			}
			if creds.AppID != tc.wantID || creds.PrivateKey != tc.wantKey {
				t.Fatalf("creds = %+v, want id %q key %q", creds, tc.wantID, tc.wantKey)
			}
		})
	}
}
