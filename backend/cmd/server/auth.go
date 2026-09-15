package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/server"
)

func configureAuthenticator(opts *server.Options, actionsEnabled bool) error {
	admins := splitList(os.Getenv("ADMIN_LOGINS"))
	switch mode := os.Getenv("AUTH_MODE"); mode {
	case "oauth":
		if strings.TrimSpace(os.Getenv("OAUTH_SCOPE")) != "" {
			return fmt.Errorf("OAUTH_SCOPE is no longer supported; OAuth login uses read:user and BOT_TOKEN performs writes")
		}
		if strings.TrimSpace(os.Getenv("OAUTH_PRIVATE_REPOSITORIES")) != "" {
			return fmt.Errorf("OAUTH_PRIVATE_REPOSITORIES is no longer supported; grant repository access to BOT_TOKEN instead")
		}
		botToken := os.Getenv("BOT_TOKEN")
		if actionsEnabled && botToken == "" {
			return fmt.Errorf("oauth auth mode requires BOT_TOKEN when actions are enabled")
		}
		o, err := auth.NewOAuth(auth.OAuthConfig{
			ClientID:      os.Getenv("OAUTH_CLIENT_ID"),
			ClientSecret:  os.Getenv("OAUTH_CLIENT_SECRET"),
			RedirectURL:   os.Getenv("OAUTH_REDIRECT_URL"),
			Scope:         "read:user",
			WriteToken:    botToken,
			Admins:        admins,
			SessionKey:    os.Getenv("SESSION_KEY"),
			SecureCookies: os.Getenv("COOKIE_INSECURE") != "1",
		})
		if err != nil {
			return err
		}
		opts.Auth = o
		opts.AuthMode = "oauth"
		opts.LoginURL = "/api/auth/login"
	case "proxy":
		botToken := os.Getenv("BOT_TOKEN")
		if actionsEnabled && botToken == "" {
			return fmt.Errorf("proxy auth mode requires BOT_TOKEN when actions are enabled")
		}
		header := os.Getenv("AUTH_PROXY_HEADER")
		if header == "" {
			return fmt.Errorf("proxy auth mode requires AUTH_PROXY_HEADER (the trusted identity header)")
		}
		if len(admins) == 0 {
			return fmt.Errorf("proxy auth mode requires ADMIN_LOGINS (the allowlist of identities that may act)")
		}
		opts.Auth = auth.NewBotAuthenticator(header, botToken, admins, os.Getenv("AUTH_PROXY_SECRET"))
		opts.AuthMode = "proxy"
	case "dev":
		botToken := os.Getenv("BOT_TOKEN")
		if actionsEnabled && botToken == "" {
			return fmt.Errorf("dev auth mode requires BOT_TOKEN when actions are enabled")
		}
		login := os.Getenv("DEV_LOGIN")
		if login == "" {
			login = "dev-admin"
		}
		log.Printf("⚠️  AUTH_MODE=dev: authenticating every request as admin %q; local use only, never expose this server", login)
		opts.Auth = auth.NewDevAuthenticator(login, botToken)
		opts.AuthMode = "dev"
	default:
		return fmt.Errorf("unknown AUTH_MODE %q (want oauth, proxy, or dev)", mode)
	}
	return nil
}

// trustedOrigins collects the public origins the CSRF guard should accept: the
// host of the OAuth redirect URL when set, plus a comma/space separated
// TRUSTED_ORIGINS list.
func trustedOrigins(redirectURL, extra string) []string {
	var out []string
	if redirectURL != "" {
		if u, err := url.Parse(redirectURL); err == nil && u.Host != "" {
			out = append(out, u.Host)
		}
	}
	return append(out, splitList(extra)...)
}

// splitList parses a comma or whitespace separated list, dropping blanks.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	var out []string
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
