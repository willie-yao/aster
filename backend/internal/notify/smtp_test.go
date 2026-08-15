package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

type fakeSMTPSession struct {
	startTLSAvailable bool
	startTLSCalled    bool
	authCalled        bool
	mailFrom          string
	recipients        []string
	data              bytes.Buffer
	quitCalled        bool
	authErr           error
	quitErr           error
}

func (f *fakeSMTPSession) Extension(name string) (bool, string) {
	return name == "STARTTLS" && f.startTLSAvailable, ""
}
func (f *fakeSMTPSession) StartTLS(*tls.Config) error { f.startTLSCalled = true; return nil }
func (f *fakeSMTPSession) Auth(smtp.Auth) error       { f.authCalled = true; return f.authErr }
func (f *fakeSMTPSession) Mail(from string) error     { f.mailFrom = from; return nil }
func (f *fakeSMTPSession) Rcpt(to string) error       { f.recipients = append(f.recipients, to); return nil }
func (f *fakeSMTPSession) Data() (io.WriteCloser, error) {
	return nopWriteCloser{Writer: &f.data}, nil
}
func (f *fakeSMTPSession) Quit() error  { f.quitCalled = true; return f.quitErr }
func (f *fakeSMTPSession) Close() error { return nil }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func testMessage() Message {
	return Message{
		From:     mail.Address{Name: "Dashboard", Address: "from@example.com"},
		To:       []mail.Address{{Name: "One", Address: "one@example.com"}, {Address: "two@example.com"}},
		Subject:  "Persistent failure: TestOne",
		TextBody: "plain body",
		HTMLBody: "<p>html body</p>",
	}
}

func newTestSMTPSender(t *testing.T, config SMTPConfig, session *fakeSMTPSession) (*SMTPSender, *bool, **tls.Config, *time.Time) {
	t.Helper()
	sender, err := NewSMTPSender(config)
	if err != nil {
		t.Fatal(err)
	}
	implicit := false
	var tlsConfig *tls.Config
	var deadline time.Time
	sender.dial = func(_ context.Context, _, _ string, gotImplicit bool, gotTLS *tls.Config, gotDeadline time.Time) (smtpSession, error) {
		implicit = gotImplicit
		tlsConfig = gotTLS
		deadline = gotDeadline
		return session, nil
	}
	sender.now = func() time.Time { return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC) }
	sender.messageID = func(string, time.Time) string { return "<test@example.com>" }
	return sender, &implicit, &tlsConfig, &deadline
}

func TestSMTPSenderStartTLSAndMIME(t *testing.T) {
	session := &fakeSMTPSession{startTLSAvailable: true}
	sender, implicit, tlsConfig, _ := newTestSMTPSender(t, SMTPConfig{Host: "smtp.example.com", Port: 587, TLSMode: "starttls"}, session)
	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if *implicit || !session.startTLSCalled || session.authCalled {
		t.Fatalf("implicit=%v starttls=%v auth=%v", *implicit, session.startTLSCalled, session.authCalled)
	}
	if (*tlsConfig).ServerName != "smtp.example.com" || (*tlsConfig).MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS config = %+v", *tlsConfig)
	}
	if session.mailFrom != "from@example.com" || len(session.recipients) != 2 || !session.quitCalled {
		t.Fatalf("from=%q recipients=%v quit=%v", session.mailFrom, session.recipients, session.quitCalled)
	}

	message, err := mail.ReadMessage(bytes.NewReader(session.data.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if message.Header.Get("Message-ID") != "<test@example.com>" || !strings.Contains(message.Header.Get("To"), "two@example.com") {
		t.Fatalf("headers = %+v", message.Header)
	}
	mediaType, params, err := mimeParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("content type = %q params=%v err=%v", mediaType, params, err)
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	parts := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(part); err != nil {
			t.Fatal(err)
		}
		parts++
	}
	if parts != 2 {
		t.Fatalf("MIME parts = %d", parts)
	}
}

func TestSMTPSenderRequiresSTARTTLS(t *testing.T) {
	session := &fakeSMTPSession{}
	sender, _, _, _ := newTestSMTPSender(t, SMTPConfig{Host: "smtp.example.com", Port: 587, TLSMode: "starttls"}, session)
	err := sender.Send(context.Background(), testMessage())
	if err == nil || !strings.Contains(err.Error(), "does not advertise STARTTLS") {
		t.Fatalf("err = %v", err)
	}
}

func TestSMTPSenderImplicitTLSAndAuth(t *testing.T) {
	session := &fakeSMTPSession{}
	sender, implicit, _, _ := newTestSMTPSender(t, SMTPConfig{Host: "smtp.example.com", Port: 465, TLSMode: "tls", Username: "user", Password: "secret"}, session)
	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if !*implicit || !session.authCalled || session.startTLSCalled {
		t.Fatalf("implicit=%v auth=%v starttls=%v", *implicit, session.authCalled, session.startTLSCalled)
	}
}

func TestSMTPSenderPlainRelay(t *testing.T) {
	session := &fakeSMTPSession{}
	sender, implicit, _, _ := newTestSMTPSender(t, SMTPConfig{Host: "relay.internal", Port: 25, TLSMode: "none"}, session)
	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	if *implicit || session.startTLSCalled || session.authCalled {
		t.Fatalf("implicit=%v starttls=%v auth=%v", *implicit, session.startTLSCalled, session.authCalled)
	}
}

func TestNewSMTPSenderValidation(t *testing.T) {
	tests := []struct {
		name   string
		config SMTPConfig
		want   string
	}{
		{name: "missing host", config: SMTPConfig{Port: 25, TLSMode: "none"}, want: "host"},
		{name: "bad port", config: SMTPConfig{Host: "smtp", Port: 0, TLSMode: "none"}, want: "port"},
		{name: "bad TLS", config: SMTPConfig{Host: "smtp", Port: 25, TLSMode: "bad"}, want: "TLS mode"},
		{name: "plaintext auth", config: SMTPConfig{Host: "smtp", Port: 25, TLSMode: "none", Username: "u", Password: "p"}, want: "requires TLS"},
		{name: "missing password", config: SMTPConfig{Host: "smtp", Port: 587, TLSMode: "starttls", Username: "u"}, want: "password"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSMTPSender(tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSMTPSenderUsesEarlierContextDeadline(t *testing.T) {
	session := &fakeSMTPSession{}
	sender, _, _, deadline := newTestSMTPSender(t, SMTPConfig{Host: "relay.internal", Port: 25, TLSMode: "none"}, session)
	base := time.Now()
	sender.now = func() time.Time { return base }
	ctxDeadline := base.Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), ctxDeadline)
	defer cancel()
	if err := sender.Send(ctx, testMessage()); err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(ctxDeadline) {
		t.Fatalf("deadline = %s, want %s", deadline, ctxDeadline)
	}
}

func TestSMTPSenderErrorDoesNotExposePassword(t *testing.T) {
	session := &fakeSMTPSession{authErr: errors.New("authentication rejected")}
	sender, _, _, _ := newTestSMTPSender(t, SMTPConfig{Host: "smtp.example.com", Port: 465, TLSMode: "tls", Username: "user", Password: "super-secret"}, session)
	err := sender.Send(context.Background(), testMessage())
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestEncodeMessageRejectsHeaderInjection(t *testing.T) {
	message := testMessage()
	message.Subject = "subject\r\nBcc: victim@example.com"
	if _, err := encodeMessage(message, time.Now(), "<id@example.com>"); err == nil {
		t.Fatal("expected header injection error")
	}
}

var mimeParseMediaType = mime.ParseMediaType

func TestSMTPSenderIgnoresQuitFailureAfterAcceptance(t *testing.T) {
	session := &fakeSMTPSession{quitErr: errors.New("connection closed")}
	sender, _, _, _ := newTestSMTPSender(t, SMTPConfig{Host: "relay.internal", Port: 25, TLSMode: "none"}, session)
	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("accepted message should not fail on QUIT: %v", err)
	}
	if !session.quitCalled {
		t.Fatal("QUIT was not attempted")
	}
}
