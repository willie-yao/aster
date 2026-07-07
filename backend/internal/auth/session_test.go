package auth

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionCodec_RoundTrip(t *testing.T) {
	c, err := newSessionCodec("secret", true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.seal(session{Login: "alice", Token: "tok", Exp: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got.Login != "alice" || got.Token != "tok" {
		t.Errorf("round trip = %+v", got)
	}
}

func TestSessionCodec_RejectsExpired(t *testing.T) {
	c, _ := newSessionCodec("secret", true, time.Hour)
	sealed, _ := c.seal(session{Login: "alice", Token: "tok", Exp: time.Now().Add(-time.Minute).Unix()})
	if _, err := c.open(sealed); err == nil {
		t.Fatal("expected expired session to be rejected")
	}
}

func TestSessionCodec_RejectsTamper(t *testing.T) {
	c, _ := newSessionCodec("secret", true, time.Hour)
	sealed, _ := c.seal(session{Login: "alice", Token: "tok", Exp: time.Now().Add(time.Hour).Unix()})
	// Flip a character; GCM auth must reject it.
	tampered := sealed[:len(sealed)-1] + string(rune(sealed[len(sealed)-1]^1))
	if _, err := c.open(tampered); err == nil {
		t.Fatal("expected tampered session to be rejected")
	}
}

func TestSessionCodec_WrongKeyFails(t *testing.T) {
	c1, _ := newSessionCodec("secret-a", true, time.Hour)
	c2, _ := newSessionCodec("secret-b", true, time.Hour)
	sealed, _ := c1.seal(session{Login: "alice", Token: "tok", Exp: time.Now().Add(time.Hour).Unix()})
	if _, err := c2.open(sealed); err == nil {
		t.Fatal("session sealed with a different key must not open")
	}
}

func TestSessionCodec_WriteReadCookie(t *testing.T) {
	c, _ := newSessionCodec("secret", false, time.Hour)
	rec := httptest.NewRecorder()
	if err := c.write(rec, "alice", "tok"); err != nil {
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
	if s.Login != "alice" || s.Token != "tok" {
		t.Errorf("cookie session = %+v", s)
	}
	// The cookie must be httpOnly.
	if !rec.Result().Cookies()[0].HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
}
