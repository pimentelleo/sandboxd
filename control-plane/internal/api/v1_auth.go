// v1_auth.go — the console's login authority. First-run password setup,
// password login → HttpOnly session cookie, logout, and change-password. The
// password is bcrypt-hashed; sessions are opaque random tokens stored as their
// sha256. All resolve to the single shared tenant (store.DefaultTenant).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

const sessionTTL = 30 * 24 * time.Hour

// minPasswordLen is the floor for a console password (defence-in-depth; the
// primary gate is that the API is unreachable without a credential at all).
const minPasswordLen = 8

// passwordSet reports whether a console password has been configured.
func (s *Server) passwordSet(r *http.Request) bool {
	if s.Store == nil {
		return false
	}
	_, err := s.Store.GetPasswordHash(r.Context())
	return err == nil
}

func (s *Server) localAccountsExist(r *http.Request) (bool, error) {
	if s.Store == nil {
		return false, store.ErrNotFound
	}
	return s.Store.LocalAccountsExist(r.Context())
}

// GET /v1/auth/status — pre-login probe (exempt from the middleware).
func (s *Server) v1AuthStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.authConfig()
	enabled := cfg.Profile != auth.ProfileLocal || !cfg.Disabled
	actor := auth.ActorFrom(r.Context())
	authed := actor.Kind == "user" || actor.Kind == "service"
	out := map[string]any{
		"enabled":       enabled,
		"authenticated": authed,
		"profile":       cfg.Profile,
	}
	if cfg.Profile != auth.ProfileLocal {
		out["password_set"] = false
		out["login_available"] = cfg.Profile == auth.ProfileEntra && s.Auth != nil && s.Auth.ProductionReady()
		if cfg.Profile == auth.ProfileEntra && actor.Principal != nil {
			out["principal"] = actor.Principal.Safe()
			out["capabilities"] = map[string]bool{
				"console_access": true,
				"administrator":  actor.Principal.HasRole(auth.RoleAdmin),
			}
		}
	} else if cfg.LocalAuthMode == auth.LocalAuthModeAccounts {
		exists, err := s.localAccountsExist(r)
		if err != nil {
			writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
			return
		}
		out["password_set"] = exists
		out["local_auth_mode"] = cfg.LocalAuthMode
		if actor.Kind == "user" && actor.PrincipalID != "" {
			out["principal"] = map[string]any{
				"id": actor.PrincipalID, "email": actor.Name, "roles": actorRoleStrings(actor),
			}
			out["capabilities"] = map[string]bool{
				"console_access": true,
				"administrator":  actor.HasRole(auth.RoleAdmin),
			}
		}
	} else {
		out["password_set"] = s.passwordSet(r)
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /v1/auth/setup {password} — first-run create-password. 409 once set.
func (s *Server) v1AuthSetup(w http.ResponseWriter, r *http.Request) {
	if s.authConfig().Profile == auth.ProfileLocal && s.authConfig().LocalAuthMode == auth.LocalAuthModeAccounts {
		s.v1AuthSetupLocalAccount(w, r)
		return
	}
	if !s.localPasswordProfile(w) {
		return
	}
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	if s.passwordSet(r) {
		writeV1Err(w, http.StatusConflict, "conflict", "a password is already set; use login or change-password")
		return
	}
	pw, ok := decodePassword(w, r, "password")
	if !ok {
		return
	}
	hash, err := console.HashPassword(pw)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not hash password")
		return
	}
	if err := s.Store.SetPasswordHash(r.Context(), hash); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not save password")
		return
	}
	if !s.issueSession(w, r) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) v1AuthSetupLocalAccount(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	email, password, ok := decodeLocalAccountCredentials(w, r)
	if !ok {
		return
	}
	hash, err := console.HashPassword(password)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not hash password")
		return
	}
	principal, err := s.Store.CreateInitialLocalAccount(r.Context(), email, hash)
	if errors.Is(err, store.ErrConflict) {
		writeV1Err(w, http.StatusConflict, "conflict", "an administrator account is already configured; use login")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not create administrator account")
		return
	}
	if !s.issueStoredPrincipalSession(w, r, principal.ID) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/auth/login {password} — verify + set a session cookie.
func (s *Server) v1AuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.authConfig().Profile == auth.ProfileLocal && s.authConfig().LocalAuthMode == auth.LocalAuthModeAccounts {
		s.v1AuthLoginLocalAccount(w, r)
		return
	}
	if !s.localPasswordProfile(w) {
		return
	}
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	hash, err := s.Store.GetPasswordHash(r.Context())
	if err != nil || !console.CheckPassword(hash, body.Password) {
		writeV1Err(w, http.StatusUnauthorized, "unauthorized", "invalid password")
		return
	}
	if !s.issueSession(w, r) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) v1AuthLoginLocalAccount(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	email, password, ok := decodeLocalAccountCredentials(w, r)
	if !ok {
		return
	}
	account, err := s.Store.GetLocalAccount(r.Context(), email)
	if err != nil || !console.CheckPassword(account.PasswordHash, password) {
		writeV1Err(w, http.StatusUnauthorized, "unauthorized", "invalid email or password")
		return
	}
	if !s.issueStoredPrincipalSession(w, r, account.Principal.ID) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/auth/logout — revoke the current session + clear the cookie.
// With ?all=true it revokes EVERY session (sign out everywhere).
func (s *Server) v1AuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.Store != nil {
		cfg := s.authConfig()
		if cfg.Profile == auth.ProfileEntra || cfg.LocalAuthMode == auth.LocalAuthModeAccounts {
			actor := auth.ActorFrom(r.Context())
			if actor.Kind == "user" && actor.PrincipalID != "" {
				// Preview authority uses separate host-only cookies and tickets.
				// Revoke both before the console session so a failed revocation
				// remains retryable.
				if err := s.Store.RevokePreviewAuthorityForPrincipal(r.Context(), actor.PrincipalID); err != nil {
					writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "could not revoke preview authority")
					return
				}
			}
			if r.URL.Query().Get("all") == "true" {
				if actor.Kind != "user" || actor.PrincipalID == "" {
					writeV1Err(w, http.StatusForbidden, "forbidden", "sign out everywhere requires a console login")
					return
				}
				if err := s.Store.RevokeBrowserSessionsForPrincipal(r.Context(), actor.PrincipalID); err != nil {
					writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "could not revoke browser sessions")
					return
				}
			} else if ck, err := r.Cookie(auth.SessionCookieForProfile(cfg.Profile)); err == nil && ck.Value != "" {
				if err := s.Store.RevokeBrowserSession(r.Context(), console.HashToken(ck.Value)); err != nil && !errors.Is(err, store.ErrNotFound) {
					writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "could not revoke browser session")
					return
				}
			}
		} else if r.URL.Query().Get("all") == "true" {
			if auth.ActorFrom(r.Context()).Kind != "user" {
				writeV1Err(w, http.StatusForbidden, "forbidden", "sign out everywhere requires a console login")
				return
			}
			_ = s.Store.DeleteAllSessions(r.Context())
		} else if ck, err := r.Cookie(auth.SessionCookieForProfile(s.authConfig().Profile)); err == nil && ck.Value != "" {
			_ = s.Store.DeleteSession(r.Context(), console.HashToken(ck.Value))
		}
	}
	http.SetCookie(w, s.clearSessionCookie(r))
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/auth/password {current_password, new_password} — change password
// (requires a logged-in user). Revokes all other sessions, then re-issues one.
func (s *Server) v1AuthPassword(w http.ResponseWriter, r *http.Request) {
	if s.authConfig().Profile == auth.ProfileLocal && s.authConfig().LocalAuthMode == auth.LocalAuthModeAccounts {
		s.v1AuthPasswordLocalAccount(w, r)
		return
	}
	if !s.localPasswordProfile(w) {
		return
	}
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	if auth.ActorFrom(r.Context()).Kind != "user" {
		writeV1Err(w, http.StatusForbidden, "forbidden", "change password requires a console login")
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	hash, err := s.Store.GetPasswordHash(r.Context())
	if err != nil || !console.CheckPassword(hash, body.CurrentPassword) {
		writeV1Err(w, http.StatusUnauthorized, "unauthorized", "current password is incorrect")
		return
	}
	if len(body.NewPassword) < minPasswordLen {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "new password must be at least 8 characters")
		return
	}
	newHash, err := console.HashPassword(body.NewPassword)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not hash password")
		return
	}
	if err := s.Store.SetPasswordHash(r.Context(), newHash); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not save password")
		return
	}
	_ = s.Store.DeleteAllSessions(r.Context()) // invalidate everyone, including us
	if !s.issueSession(w, r) {                 // re-issue for this browser
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) v1AuthPasswordLocalAccount(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	actor := auth.ActorFrom(r.Context())
	if actor.Kind != "user" || actor.PrincipalID == "" {
		writeV1Err(w, http.StatusForbidden, "forbidden", "change password requires a console login")
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.NewPassword) < minPasswordLen {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "new password must be at least 8 characters")
		return
	}
	account, err := s.Store.GetLocalAccountByPrincipal(r.Context(), actor.PrincipalID)
	if err != nil || !console.CheckPassword(account.PasswordHash, body.CurrentPassword) {
		writeV1Err(w, http.StatusUnauthorized, "unauthorized", "current password is incorrect")
		return
	}
	newHash, err := console.HashPassword(body.NewPassword)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not hash password")
		return
	}
	if err := s.Store.UpdateLocalAccountPassword(r.Context(), actor.PrincipalID, account.PasswordHash, newHash); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeV1Err(w, http.StatusUnauthorized, "unauthorized", "current password is incorrect")
			return
		}
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not save password")
		return
	}
	if !s.issueStoredPrincipalSession(w, r, actor.PrincipalID) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/auth/accounts is administrator-only at the authorization boundary.
func (s *Server) v1CreateLocalAccount(w http.ResponseWriter, r *http.Request) {
	if !s.localAccountProfile(w) {
		return
	}
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	email, password, ok := decodeLocalAccountCredentials(w, r)
	if !ok {
		return
	}
	hash, err := console.HashPassword(password)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not hash password")
		return
	}
	principal, err := s.Store.CreateLocalAccount(r.Context(), email, hash)
	if errors.Is(err, store.ErrConflict) {
		writeV1Err(w, http.StatusConflict, "conflict", "an account with that email already exists")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not create account")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": principal.ID, "email": principal.Email.String, "roles": principal.Roles,
	})
}

// GET /v1/auth/entra/login begins the Entra authorization-code flow. It is
// intentionally a browser redirect rather than a JSON URL so the console never
// sees an authorization code, token, verifier, state, or nonce.
func (s *Server) v1AuthEntraLogin(w http.ResponseWriter, r *http.Request) {
	flow, ok := s.entraFlow(w)
	if !ok {
		return
	}
	start, err := flow.BeginWithReturn(r.Context(), r.URL.Query().Get("return"))
	if err != nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "enterprise sign-in is unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, start.AuthorizationURL, http.StatusFound)
}

// GET /v1/auth/entra/callback consumes Entra's authorization response. A valid
// callback always ends at the local console root; provider errors are collapsed
// to a small safe reason and never reflected to the browser.
func (s *Server) v1AuthEntraCallback(w http.ResponseWriter, r *http.Request) {
	flow, ok := s.entraFlow(w)
	if !ok {
		return
	}
	principal, returnLocation, err := flow.CompleteWithReturn(r.Context(), r.URL.Query().Get("state"), r.URL.Query().Get("code"))
	if err != nil {
		if errors.Is(err, auth.ErrOIDCState) {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid or expired sign-in state")
			return
		}
		authRedirect(w, r, "denied")
		return
	}
	if !s.issuePrincipalSession(w, r, principal) {
		return
	}
	authRedirectTo(w, r, returnLocation)
}

// ── helpers ──────────────────────────────────────────────────────────

func (s *Server) authConfig() *auth.Config {
	if s.Auth == nil || s.Auth.Snapshot() == nil {
		return &auth.Config{Profile: auth.ProfileLocal}
	}
	return s.Auth.Snapshot()
}

func (s *Server) localPasswordProfile(w http.ResponseWriter) bool {
	if s.authConfig().Profile == auth.ProfileLocal && s.authConfig().LocalAuthMode != auth.LocalAuthModeAccounts {
		return true
	}
	writeV1Err(w, http.StatusNotFound, "not_found", "local password authentication is not enabled")
	return false
}

func (s *Server) localAccountProfile(w http.ResponseWriter) bool {
	if s.authConfig().Profile == auth.ProfileLocal && s.authConfig().LocalAuthMode == auth.LocalAuthModeAccounts {
		return true
	}
	writeV1Err(w, http.StatusNotFound, "not_found", "local account authentication is not enabled")
	return false
}

func (s *Server) entraFlow(w http.ResponseWriter) (*auth.OIDCFlow, bool) {
	cfg := s.authConfig()
	if cfg.Profile != auth.ProfileEntra || !cfg.ProductionReady() || s.Auth == nil || s.Auth.OIDC() == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "enterprise sign-in is unavailable")
		return nil, false
	}
	return s.Auth.OIDC(), true
}

func authRedirect(w http.ResponseWriter, r *http.Request, result string) {
	target := "/"
	if result != "" {
		target += "?auth_error=" + url.QueryEscape(result)
	}
	authRedirectTo(w, r, target)
}

func authRedirectTo(w http.ResponseWriter, r *http.Request, target string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// decodePassword decodes {field: "..."} and validates length. Writes the error
// response and returns ok=false on failure.
func decodePassword(w http.ResponseWriter, r *http.Request, field string) (string, bool) {
	var raw map[string]string
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	if err := dec.Decode(&raw); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return "", false
	}
	pw := raw[field]
	if len(pw) < minPasswordLen {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "password must be at least 8 characters")
		return "", false
	}
	return pw, true
}

func decodeLocalAccountCredentials(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return "", "", false
	}
	email, ok := normalizeLocalAccountEmail(body.Email)
	if !ok {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "a valid email address is required")
		return "", "", false
	}
	if len(body.Password) < minPasswordLen {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "password must be at least 8 characters")
		return "", "", false
	}
	return email, body.Password, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return false
	}
	return true
}

func normalizeLocalAccountEmail(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 0 || len(value) > 254 {
		return "", false
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value {
		return "", false
	}
	at := strings.LastIndexByte(value, '@')
	if at < 1 || at == len(value)-1 || !strings.Contains(value[at+1:], ".") {
		return "", false
	}
	return value, true
}

func actorRoleStrings(actor auth.Actor) []string {
	if actor.Principal != nil {
		roles := make([]string, 0, len(actor.Principal.Roles))
		for _, role := range actor.Principal.Roles {
			roles = append(roles, string(role))
		}
		return roles
	}
	roles := make([]string, 0, len(actor.Roles))
	for _, role := range actor.Roles {
		roles = append(roles, string(role))
	}
	return roles
}

// issueSession mints a session, stores it, and sets the cookie. Writes an error
// response and returns false on failure.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request) bool {
	return s.issueSessionOwner(w, r, store.DefaultTenant)
}

// issuePrincipalSession writes a durable identity and opaque browser session.
// No Entra token, authorization code, or session authority is persisted in the
// cookie or legacy console-session owner field.
func (s *Server) issuePrincipalSession(w http.ResponseWriter, r *http.Request, principal auth.Principal) bool {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return false
	}
	principalID, err := s.upsertEntraPrincipal(r.Context(), principal)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not create session")
		return false
	}
	return s.issueStoredPrincipalSessionForProfile(w, r, principalID, auth.ProfileEntra)
}

func (s *Server) issueStoredPrincipalSession(w http.ResponseWriter, r *http.Request, principalID string) bool {
	return s.issueStoredPrincipalSessionForProfile(w, r, principalID, auth.ProfileLocal)
}

func (s *Server) issueStoredPrincipalSessionForProfile(w http.ResponseWriter, r *http.Request, principalID string, profile auth.Profile) bool {
	if s.Store == nil || principalID == "" {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return false
	}
	value, hash, err := console.NewSessionValue()
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not create session")
		return false
	}
	now := time.Now()
	exp := now.Add(sessionTTL)
	if err := s.Store.CreateBrowserSession(r.Context(), store.BrowserSession{
		TokenHash: hash, PrincipalID: principalID, CreatedAt: now, LastUsedAt: now, ExpiresAt: exp,
	}); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not persist session")
		return false
	}
	s.setSessionCookie(w, r, value, exp, profile)
	return true
}

func (s *Server) upsertEntraPrincipal(ctx context.Context, principal auth.Principal) (string, error) {
	if s.Store == nil || principal.OID == "" || principal.TenantID == "" || len(principal.Roles) == 0 {
		return "", errors.New("invalid principal")
	}
	roles := make([]string, 0, len(principal.Roles))
	for _, role := range principal.Roles {
		roles = append(roles, string(role))
	}
	stored := &store.Principal{
		ID:          newULID(),
		Provider:    "entra",
		TenantID:    principal.TenantID,
		Subject:     principal.OID,
		DisplayName: nullStr(principal.DisplayName),
		Email:       nullStr(principal.UPN),
		Roles:       roles,
	}
	if err := s.Store.UpsertPrincipal(ctx, stored); err != nil {
		return "", err
	}
	return stored.ID, nil
}

func (s *Server) issueSessionOwner(w http.ResponseWriter, r *http.Request, owner string) bool {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return false
	}
	value, hash, err := console.NewSessionValue()
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not create session")
		return false
	}
	now := time.Now()
	exp := now.Add(sessionTTL)
	if err := s.Store.CreateSession(r.Context(), hash, owner, now.Unix(), now.Unix(), exp.Unix()); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not persist session")
		return false
	}
	profile := s.authConfig().Profile
	s.setSessionCookie(w, r, value, exp, profile)
	return true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, exp time.Time, profile auth.Profile) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieForProfile(profile),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   profile == auth.ProfileEntra || isHTTPS(r),
		Expires:  exp,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

func (s *Server) clearSessionCookie(r *http.Request) *http.Cookie {
	profile := s.authConfig().Profile
	return &http.Cookie{
		Name:     auth.SessionCookieForProfile(profile),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   profile == auth.ProfileEntra || isHTTPS(r),
		MaxAge:   -1,
	}
}

// isHTTPS reports whether the original client request was over TLS, honouring
// the Traefik/nginx X-Forwarded-Proto header (the control plane itself speaks
// plain HTTP behind the proxy).
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}
