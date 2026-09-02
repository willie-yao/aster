package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func sealLegacySession(t *testing.T, c *sessionCodec, login, token string, exp int64) string {
	t.Helper()
	plain, err := json.Marshal(struct {
		Login  string `json:"login"`
		Token  string `json:"token"`
		Policy string `json:"policy,omitempty"`
		Exp    int64  `json:"exp"`
	}{Login: login, Token: token, Policy: c.policy, Exp: exp})
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	sealed := c.aead.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed)
}

func TestSessionCodec_RoundTrip(t *testing.T) {
	c, err := newSessionCodec("secret", true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.seal(session{Login: "alice", Exp: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got.Login != "alice" {
		t.Errorf("round trip = %+v", got)
	}
}

func TestSessionCodec_OpensLegacyTokenField(t *testing.T) {
	c, err := newSessionCodec("secret", true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sealed := sealLegacySession(t, c, "alice", "legacy-user-token", time.Now().Add(time.Hour).Unix())
	got, err := c.open(sealed)
	if err != nil {
		t.Fatalf("open legacy session: %v", err)
	}
	if got.Login != "alice" {
		t.Fatalf("legacy session = %+v", got)
	}
}

func TestSessionCodec_RejectsExpired(t *testing.T) {
	c, _ := newSessionCodec("secret", true, time.Hour)
	sealed, _ := c.seal(session{Login: "alice", Exp: time.Now().Add(-time.Minute).Unix()})
	if _, err := c.open(sealed); err == nil {
		t.Fatal("expected expired session to be rejected")
	}
}

func TestSessionCodec_RejectsTamper(t *testing.T) {
	c, _ := newSessionCodec("secret", true, time.Hour)
	sealed, _ := c.seal(session{Login: "alice", Exp: time.Now().Add(time.Hour).Unix()})
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := c.open(tampered); err == nil {
		t.Fatal("expected tampered session to be rejected")
	}
}

func TestSessionCodec_WrongKeyFails(t *testing.T) {
	c1, _ := newSessionCodec("secret-a", true, time.Hour)
	c2, _ := newSessionCodec("secret-b", true, time.Hour)
	sealed, _ := c1.seal(session{Login: "alice", Exp: time.Now().Add(time.Hour).Unix()})
	if _, err := c2.open(sealed); err == nil {
		t.Fatal("session sealed with a different key must not open")
	}
}

func TestSessionCodec_WriteReadCookie(t *testing.T) {
	c, _ := newSessionCodec("secret", false, time.Hour)
	rec := httptest.NewRecorder()
	if err := c.write(rec, "alice"); err != nil {
		t.Fatal(err)
	}
	// Feed the Set-Cookie back into a request.
	req := httptest.NewRequest("GET", "/", nil)
	for _, ck := range rec.Result().Cookies() {
		req.AddCookie(ck)
	}
	s, err := c.read(req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if s.Login != "alice" {
		t.Errorf("cookie session = %+v", s)
	}
	// The cookie must be httpOnly.
	if !rec.Result().Cookies()[0].HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
}

func TestSessionCodec_SecureCookieAttributes(t *testing.T) {
	c, _ := newSessionCodec("secret", true, time.Hour)
	rec := httptest.NewRecorder()
	if err := c.write(rec, "alice"); err != nil {
		t.Fatal(err)
	}
	cookie := rec.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session attributes = Secure:%t HttpOnly:%t SameSite:%v", cookie.Secure, cookie.HttpOnly, cookie.SameSite)
	}

	clearRec := httptest.NewRecorder()
	c.clear(clearRec)
	cleared := clearRec.Result().Cookies()[0]
	if !cleared.Secure || !cleared.HttpOnly || cleared.SameSite != http.SameSiteLaxMode || cleared.MaxAge != -1 {
		t.Errorf("cleared session attributes = Secure:%t HttpOnly:%t SameSite:%v MaxAge:%d", cleared.Secure, cleared.HttpOnly, cleared.SameSite, cleared.MaxAge)
	}
}
