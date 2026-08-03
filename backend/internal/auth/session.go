package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// sessionCookieName is the cookie holding the encrypted admin session.
const sessionCookieName = "pad_session"

// session is the authenticated state sealed into the cookie. Token is the
// admin's OAuth token, used to perform GitHub writes as them.
type session struct {
	Login  string `json:"login"`
	Token  string `json:"token"`
	Policy string `json:"policy,omitempty"`
	Exp    int64  `json:"exp"`
}

// sessionCodec seals and opens sessions with authenticated encryption so the
// cookie is tamper-proof and its contents (the token) stay confidential.
type sessionCodec struct {
	aead   cipher.AEAD
	secure bool
	ttl    time.Duration
	policy string
}

// deriveKey turns any secret string into a 32-byte AES key, so operators can
// set an arbitrary SESSION_KEY without worrying about exact length.
func deriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// newSessionCodec builds a codec. secure sets the cookie Secure flag (disable
// only for local http testing). ttl is the session lifetime.
func newSessionCodec(secret string, secure bool, ttl time.Duration, policies ...string) (*sessionCodec, error) {
	if secret == "" {
		return nil, errors.New("auth: session key is required")
	}
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return nil, fmt.Errorf("auth: session cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: session aead: %w", err)
	}
	policy := ""
	if len(policies) > 0 {
		policy = policies[0]
	}
	return &sessionCodec{aead: aead, secure: secure, ttl: ttl, policy: policy}, nil
}

// seal encrypts a session into a URL-safe cookie value.
func (c *sessionCodec) seal(s session) (string, error) {
	plain, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := c.aead.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(ct), nil
}

// open decrypts and validates a cookie value, rejecting tampered or expired
// sessions.
func (c *sessionCodec) open(v string) (*session, error) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, err
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("auth: short session")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plain, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("auth: invalid session")
	}
	var s session
	if err := json.Unmarshal(plain, &s); err != nil {
		return nil, err
	}
	if time.Now().Unix() > s.Exp {
		return nil, errors.New("auth: session expired")
	}
	if c.policy != "" && s.Policy != c.policy {
		return nil, errors.New("auth: session policy changed")
	}
	return &s, nil
}

// write sets the session cookie with the sealed value.
func (c *sessionCodec) write(w http.ResponseWriter, login, token string) error {
	exp := time.Now().Add(c.ttl)
	value, err := c.seal(session{Login: login, Token: token, Policy: c.policy, Exp: exp.Unix()})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   c.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// clear expires the session cookie.
func (c *sessionCodec) clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   c.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// read returns the session from the request cookie, or an error when absent,
// tampered, or expired.
func (c *sessionCodec) read(r *http.Request) (*session, error) {
	ck, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil, ErrNoToken
	}
	return c.open(ck.Value)
}
