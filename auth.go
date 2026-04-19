package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/securecookie"
	"github.com/labstack/echo/v4"
	"golang.org/x/oauth2"
)

const (
	sessionCookieName   = "syncer_session"
	stateCookieName     = "syncer_oidc_state"
	nonceCookieName     = "syncer_oidc_nonce"
	sessionContextKey   = "auth.user"
	defaultSessionTTL   = 8 * time.Hour
	defaultCookieMaxAge = int((8 * time.Hour) / time.Second)
)

type User struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`
}

type sessionData struct {
	User      User
	ExpiresAt time.Time
}

type oidcClaims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Nonce   string `json:"nonce"`
}

type OIDCAuth struct {
	logger        *slog.Logger
	config        Config
	provider      *oidc.Provider
	verifier      *oidc.IDTokenVerifier
	oauth2        oauth2.Config
	cookies       *securecookie.SecureCookie
	secure        bool
	sessionTTL    time.Duration
	allowedEmails map[string]struct{}
}

func oidcEnabled(config Config) bool {
	return strings.TrimSpace(config.OIDCIssuerURL) != "" ||
		strings.TrimSpace(config.OIDCClientID) != "" ||
		strings.TrimSpace(config.OIDCRedirectURL) != "" ||
		strings.TrimSpace(config.SessionSecret) != ""
}

func NewOIDCAuth(ctx context.Context, logger *slog.Logger, config Config) (*OIDCAuth, error) {
	provider, err := oidc.NewProvider(ctx, strings.TrimSpace(config.OIDCIssuerURL))
	if err != nil {
		return nil, fmt.Errorf("oidc provider discovery: %w", err)
	}

	parsedRedirect, err := url.Parse(config.OIDCRedirectURL)
	if err != nil {
		return nil, fmt.Errorf("parse oidc redirect url: %w", err)
	}

	hashKey := sha256.Sum256([]byte(config.SessionSecret))
	blockKey := sha256.Sum256([]byte("block:" + config.SessionSecret))
	cookies := securecookie.New(hashKey[:], blockKey[:])
	cookies.SetSerializer(securecookie.JSONEncoder{})
	cookies.MaxAge(defaultCookieMaxAge)

	scopes := []string{oidc.ScopeOpenID, "profile", "email"}
	if rawScopes := strings.TrimSpace(config.OIDCScopes); rawScopes != "" {
		scopes = splitScopes(rawScopes)
	}

	auth := &OIDCAuth{
		logger:   logger,
		config:   config,
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: config.OIDCClientID}),
		oauth2: oauth2.Config{
			ClientID:     config.OIDCClientID,
			ClientSecret: config.OIDCClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  config.OIDCRedirectURL,
			Scopes:       scopes,
		},
		cookies:       cookies,
		secure:        strings.EqualFold(parsedRedirect.Scheme, "https"),
		sessionTTL:    defaultSessionTTL,
		allowedEmails: makeAllowedEmails(config.OIDCAllowedEmails),
	}

	return auth, nil
}

func splitScopes(raw string) []string {
	result := splitValues(raw, false)
	if len(result) == 0 {
		return []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return result
}

func makeAllowedEmails(raw string) map[string]struct{} {
	values := splitValues(raw, true)
	if len(values) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(values))
	for _, value := range values {
		allowed[value] = struct{}{}
	}
	return allowed
}

func splitValues(raw string, lowercase bool) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	result := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if lowercase {
			part = strings.ToLower(part)
		}
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		result = append(result, part)
	}
	return result
}

func (a *OIDCAuth) RegisterRoutes(e *echo.Echo) {
	e.GET("/auth/login", a.handleLogin)
	e.GET("/auth/callback", a.handleCallback)
	e.GET("/auth/logout", a.handleLogout)
}

func (a *OIDCAuth) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			path := c.Request().URL.Path
			if !strings.HasPrefix(path, "/api/") {
				return next(c)
			}

			session, err := a.readSession(c)
			if err == nil {
				c.Set(sessionContextKey, session.User)
				return next(c)
			}

			a.logger.Warn("unauthorized request", slog.String("path", path), slog.String("error", err.Error()))
			return c.JSON(http.StatusUnauthorized, Result[string]{Error: "authentication required"})
		}
	}
}

func (a *OIDCAuth) handleLogin(c echo.Context) error {
	state, err := randomToken(32)
	if err != nil {
		return fmt.Errorf("generate state token: %w", err)
	}
	nonce, err := randomToken(32)
	if err != nil {
		return fmt.Errorf("generate nonce token: %w", err)
	}

	returnTo := sanitizeReturnTo(c.QueryParam("return_to"))
	if returnTo == "" {
		returnTo = "/"
	}

	a.setCookie(c, stateCookieName, state, 10*time.Minute, true)
	a.setCookie(c, nonceCookieName, nonce, 10*time.Minute, true)
	a.setCookie(c, "syncer_return_to", returnTo, 10*time.Minute, true)

	return c.Redirect(http.StatusFound, a.oauth2.AuthCodeURL(state, oidc.Nonce(nonce)))
}

func (a *OIDCAuth) handleCallback(c echo.Context) error {
	if got, want := c.QueryParam("state"), a.readCookieValue(c, stateCookieName); got == "" || want == "" || got != want {
		return a.renderCallbackError(c, http.StatusBadRequest, "Invalid authentication state.")
	}

	code := strings.TrimSpace(c.QueryParam("code"))
	if code == "" {
		return a.renderCallbackError(c, http.StatusBadRequest, "Missing authentication code.")
	}

	token, err := a.oauth2.Exchange(c.Request().Context(), code)
	if err != nil {
		a.logger.Error("exchange auth code", slog.String("error", err.Error()))
		return a.renderCallbackError(c, http.StatusUnauthorized, "Authentication exchange failed.")
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return a.renderCallbackError(c, http.StatusBadRequest, "Missing ID token.")
	}

	idToken, err := a.verifier.Verify(c.Request().Context(), rawIDToken)
	if err != nil {
		a.logger.Error("verify id token", slog.String("error", err.Error()))
		return a.renderCallbackError(c, http.StatusUnauthorized, "Invalid ID token.")
	}

	var claims oidcClaims
	if err := idToken.Claims(&claims); err != nil {
		a.logger.Error("decode id token claims", slog.String("error", err.Error()))
		return a.renderCallbackError(c, http.StatusUnauthorized, "Failed to read user claims.")
	}

	if got, want := claims.Nonce, a.readCookieValue(c, nonceCookieName); got == "" || want == "" || got != want {
		return a.renderCallbackError(c, http.StatusBadRequest, "Invalid authentication nonce.")
	}
	if !a.isAllowedEmail(claims.Email) {
		a.logger.Warn("oidc login rejected", slog.String("email", claims.Email))
		return a.renderCallbackError(c, http.StatusForbidden, "Your email is not allowed to access this application.")
	}

	expiresAt := time.Now().Add(a.sessionTTL)
	if !idToken.Expiry.IsZero() {
		expiresAt = idToken.Expiry
	}

	session := sessionData{
		User: User{
			Subject: claims.Subject,
			Email:   claims.Email,
			Name:    claims.Name,
		},
		ExpiresAt: expiresAt,
	}

	if err := a.writeSession(c, session); err != nil {
		a.logger.Error("write session", slog.String("error", err.Error()))
		return a.renderCallbackError(c, http.StatusInternalServerError, "Failed to create session.")
	}

	a.clearCookie(c, stateCookieName, true)
	a.clearCookie(c, nonceCookieName, true)
	returnTo := sanitizeReturnTo(a.readCookieValue(c, "syncer_return_to"))
	a.clearCookie(c, "syncer_return_to", true)
	if returnTo == "" {
		returnTo = "/"
	}

	return c.Redirect(http.StatusFound, returnTo)
}

func (a *OIDCAuth) renderCallbackError(c echo.Context, status int, message string) error {
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)

	page := `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width,initial-scale=1">
    <title>Authentication Error</title>
    <style>
      body {
        font-family: system-ui, sans-serif;
        background: #0f172a;
        color: #e2e8f0;
        margin: 0;
        padding: 2rem;
      }

      main {
        max-width: 40rem;
        margin: 10vh auto;
        background: #111827;
        border: 1px solid #334155;
        border-radius: 12px;
        padding: 2rem;
      }

      h1 {
        margin-top: 0;
        font-size: 1.5rem;
      }

      p {
        line-height: 1.5;
        color: #cbd5e1;
      }

      a {
        color: #93c5fd;
      }
    </style>
  </head>
  <body>
    <main>
      <h1>Authentication failed</h1>
      <p>` + html.EscapeString(message) + `</p>
      <p><a href="/auth/login">Try again</a></p>
    </main>
  </body>
</html>`

	return c.HTML(status, page)
}

func (a *OIDCAuth) handleLogout(c echo.Context) error {
	a.clearCookie(c, sessionCookieName, true)
	a.clearCookie(c, stateCookieName, true)
	a.clearCookie(c, nonceCookieName, true)
	a.clearCookie(c, "syncer_return_to", true)
	return c.Redirect(http.StatusFound, "/")
}

func (a *OIDCAuth) CurrentUser(c echo.Context) (User, bool) {
	value := c.Get(sessionContextKey)
	user, ok := value.(User)
	return user, ok
}

func (a *OIDCAuth) readSession(c echo.Context) (sessionData, error) {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil {
		return sessionData{}, fmt.Errorf("read session cookie: %w", err)
	}

	var session sessionData
	if err := a.cookies.Decode(sessionCookieName, cookie.Value, &session); err != nil {
		return sessionData{}, fmt.Errorf("decode session cookie: %w", err)
	}
	if session.ExpiresAt.IsZero() || time.Now().After(session.ExpiresAt) {
		return sessionData{}, errors.New("session expired")
	}
	if strings.TrimSpace(session.User.Subject) == "" {
		return sessionData{}, errors.New("session subject missing")
	}
	return session, nil
}

func (a *OIDCAuth) writeSession(c echo.Context, session sessionData) error {
	encoded, err := a.cookies.Encode(sessionCookieName, session)
	if err != nil {
		return err
	}

	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    encoded,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  session.ExpiresAt,
	}
	c.SetCookie(cookie)
	return nil
}

func (a *OIDCAuth) setCookie(c echo.Context, name, value string, ttl time.Duration, httpOnly bool) {
	c.SetCookie(&http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
	})
}

func (a *OIDCAuth) clearCookie(c echo.Context, name string, httpOnly bool) {
	c.SetCookie(&http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

func (a *OIDCAuth) readCookieValue(c echo.Context, name string) string {
	cookie, err := c.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func sanitizeReturnTo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return ""
	}
	return raw
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (a *OIDCAuth) isAllowedEmail(email string) bool {
	if len(a.allowedEmails) == 0 {
		return true
	}
	_, ok := a.allowedEmails[strings.ToLower(strings.TrimSpace(email))]
	return ok
}

func UserHandler(auth *OIDCAuth) echo.HandlerFunc {
	return func(c echo.Context) error {
		if auth == nil {
			return c.JSON(http.StatusOK, Result[User]{})
		}
		user, ok := auth.CurrentUser(c)
		if !ok {
			return c.JSON(http.StatusUnauthorized, Result[string]{Error: "authentication required"})
		}
		return c.JSON(http.StatusOK, Result[User]{Results: []User{user}})
	}
}
