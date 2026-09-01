package auth

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// Actor identifies the authenticated caller of a request. The auth
// middleware attaches it to the request context; handlers read it via
// ActorFrom to populate the audit log.
type Actor struct {
	Kind                    string // service | user | operator | system | unknown
	Name                    string // stable subject, token name, or "loopback"
	IP                      string
	PrincipalID             string     // set for a persistent principal-backed user session
	BrowserSessionTokenHash string     // set for a persistent principal-backed user session
	Principal               *Principal // set for a typed Entra user session
	Roles                   []Role     // local account roles; Entra roles remain on Principal
}

type actorCtxKey struct{}

// WithActor stores an Actor in ctx.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

// ActorFrom returns the Actor stored in ctx, or {Kind:"unknown"}.
func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorCtxKey{}).(Actor); ok {
		return a
	}
	return Actor{Kind: "unknown"}
}

// HasRole resolves authorization roles for either Entra or local accounts.
func (a Actor) HasRole(role Role) bool {
	if a.Principal != nil && a.Principal.HasRole(role) {
		return true
	}
	for _, got := range a.Roles {
		if got == role {
			return true
		}
	}
	return false
}

// AuditWriter is the slice of the audit logger the middleware needs.
// Declared here so internal/auth does not import internal/audit
// (internal/audit imports internal/store; keeping the dependency one-
// directional avoids a cycle).
type AuditWriter interface {
	TokenInvalid(ctx context.Context, ip string)
}

// EntraSession is the durable, server-side result of resolving an opaque
// browser cookie. It carries no plaintext token, verifier, or session
// authority; the token hash preserves the browser-session binding internally.
type EntraSession struct {
	PrincipalID             string
	BrowserSessionTokenHash string
	Principal               Principal
}

// EntraSessionResolver is the optional production-session extension to
// CredentialResolver. It is deliberately separate so local resolvers retain
// their existing interface and behavior.
type EntraSessionResolver interface {
	ResolveEntraSession(ctx context.Context, cookieValue string) (*EntraSession, bool)
}

// LoginTransactionStoreProvider lets a credential resolver supply the durable
// OIDC transaction storage that shares its deployment database.
type LoginTransactionStoreProvider interface {
	LoginTransactionStore() LoginTransactionStore
}

// Middleware is the uniform auth gate. Every request (regardless of origin —
// on-host, console-proxied, or Traefik-routed) must carry a valid credential: a
// console session cookie, or a bearer API key (DB-stored or env-configured).
// There is no locality bypass — "if auth is required, it is required."
type Middleware struct {
	cfg          atomic.Pointer[Config]
	resolver     CredentialResolver
	audit        AuditWriter
	log          *slog.Logger
	oidc         atomic.Pointer[OIDCFlow]
	customOIDC   bool
	transactions LoginTransactionStore
}

// MiddlewareOption supplies optional integrations without changing the local
// constructor path. WithOIDC is primarily useful for injecting a verifier in
// tests or an enterprise verifier implementation.
type MiddlewareOption func(*Middleware)

// WithOIDC installs an OIDC flow. The flow owns the token exchanger and
// injectable JWKS verifier.
func WithOIDC(flow *OIDCFlow) MiddlewareOption {
	return func(m *Middleware) {
		m.customOIDC = true
		if flow != nil {
			m.oidc.Store(flow)
		}
	}
}

// WithLoginTransactionStore enables durable multi-replica OIDC challenges.
func WithLoginTransactionStore(transactions LoginTransactionStore) MiddlewareOption {
	return func(m *Middleware) {
		m.transactions = transactions
	}
}

// NewMiddleware constructs the middleware around an initial config. resolver may
// be nil (env-token-only mode).
func NewMiddleware(initial *Config, resolver CredentialResolver, audit AuditWriter, log *slog.Logger, options ...MiddlewareOption) *Middleware {
	if initial == nil {
		initial = &Config{Profile: ProfileInvalid, Problem: "authentication is not configured"}
	}
	if initial.Profile == "" {
		initial.Profile = ProfileLocal
	}
	m := &Middleware{resolver: resolver, audit: audit, log: log}
	for _, option := range options {
		option(m)
	}
	if m.transactions == nil {
		if provider, ok := resolver.(LoginTransactionStoreProvider); ok {
			m.transactions = provider.LoginTransactionStore()
		}
	}
	m.cfg.Store(initial)
	m.configureOIDC(initial)
	return m
}

// Reload atomically swaps the config — the SIGHUP token-rotation path.
func (m *Middleware) Reload(c *Config) {
	if c == nil {
		c = &Config{Profile: ProfileInvalid, Problem: "authentication is not configured"}
	}
	if c.Profile == "" {
		c.Profile = ProfileLocal
	}
	m.cfg.Store(c)
	m.configureOIDC(c)
}

// Snapshot returns the current config; callers treat it as read-only.
func (m *Middleware) Snapshot() *Config { return m.cfg.Load() }

// OIDC returns the configured production flow, or nil if the active profile is
// local or lacks required production configuration.
func (m *Middleware) OIDC() *OIDCFlow { return m.oidc.Load() }

// ProductionReady reports whether the active Entra configuration includes both
// validated OIDC settings and a durable transaction store.
func (m *Middleware) ProductionReady() bool {
	if m == nil || !m.Snapshot().ProductionReady() {
		return false
	}
	flow := m.OIDC()
	return flow != nil && flow.Persistent()
}

// LocalAccountsReady reports whether the active authentication configuration
// supports durable local email/password accounts.
func (m *Middleware) LocalAccountsReady() bool {
	return m != nil && m.Snapshot() != nil && m.Snapshot().LocalAccountsReady()
}

// StartupError lets the daemon refuse an Entra deployment before serving when
// the required OIDC configuration or durable transaction storage is absent.
func (m *Middleware) StartupError() error {
	if m == nil || m.Snapshot() == nil {
		return errors.New("authentication is not configured")
	}
	if m.Snapshot().Profile != ProfileEntra {
		if m.Snapshot().Profile == ProfileLocal && !m.Snapshot().LocalAccountsReady() &&
			m.Snapshot().LocalAuthMode == LocalAuthModeAccounts {
			return errors.New("local account authentication is not configured")
		}
		return nil
	}
	if !m.ProductionReady() {
		return errors.New("production authentication is not configured")
	}
	return nil
}

func (m *Middleware) configureOIDC(cfg *Config) {
	if m.customOIDC {
		return
	}
	if cfg.ProductionReady() && m.transactions != nil {
		m.oidc.Store(NewOIDCFlow(cfg.Entra, nil, nil, m.transactions))
		return
	}
	m.oidc.Store(nil)
}

// exemptPaths are reachable on the external path without a bearer
// token. /preview-auth and /forward-auth validate their
// own JWTs; /healthz and /readyz carry nothing sensitive.
var exemptPaths = map[string]bool{
	"/healthz":                true,
	"/readyz":                 true,
	"/preview-auth":           true,
	"/forward-auth":           true,
	"/llm.txt":                true, // public API contract for integrators (no token)
	"/v1/auth/status":         true, // console asks "is auth on / am I logged in / is a password set" pre-login
	"/v1/auth/login":          true, // you cannot be authenticated in order to authenticate
	"/v1/auth/setup":          true, // first-run "create password" (self-guards: 409 once set)
	"/v1/auth/logout":         true, // idempotent sign-out; ?all=true self-guards for a console session
	"/v1/auth/entra/login":    true, // begins a state-protected OIDC redirect
	"/v1/auth/entra/callback": true, // consumes the OIDC state and sets a session
}

// Wrap returns next gated by the uniform credential check.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := m.cfg.Load()
		ip := ClientIP(r)

		// /metrics is loopback-only — never exposed externally. (This is the
		// only remaining use of loopback detection: it does NOT bypass auth.)
		if r.URL.Path == "/metrics" && !isLoopbackReq(r) {
			http.NotFound(w, r)
			return
		}

		// Resolve a credential, in order: console session cookie, then a bearer
		// API key (DB-stored via the resolver, else env-configured tokens).
		actor, authed := m.resolve(r, cfg, ip)

		// Exempt paths serve regardless of whether a credential was present
		// (they carry nothing sensitive, or self-guard). Attach whatever actor
		// resolved so handlers like /v1/auth/status can report authenticated.
		if exemptPaths[r.URL.Path] {
			next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), actor)))
			return
		}

		// An Entra profile must never fall back to disabled/unauthenticated access
		// when its tenant, client, redirect, or endpoints are missing.
		if (cfg.Profile != ProfileLocal && !m.ProductionReady()) ||
			(cfg.Profile == ProfileLocal && cfg.LocalAuthMode == LocalAuthModeAccounts && !cfg.LocalAccountsReady()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}

		// SANDBOXD_API_AUTH_DISABLED rollback — every request runs as the shared
		// tenant, unauthenticated. Explicit opt-out; trips the warning banner.
		if cfg.Disabled {
			next.ServeHTTP(w, r.WithContext(WithActor(r.Context(),
				Actor{Kind: "service", Name: "default", IP: ip})))
			return
		}

		if !authed {
			if m.audit != nil {
				m.audit.TokenInvalid(r.Context(), ip)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), actor)))
	})
}

// resolve returns the authenticated actor (and true) for a request, or a
// zero-value actor (and false) when no valid credential is present.
func (m *Middleware) resolve(r *http.Request, cfg *Config, ip string) (Actor, bool) {
	if cfg.Profile != ProfileLocal && !m.ProductionReady() {
		return Actor{Kind: "unknown", IP: ip}, false
	}
	// 1. The profile-specific console session cookie.
	if cfg.Profile == ProfileEntra {
		resolver, ok := m.resolver.(EntraSessionResolver)
		if !ok {
			return Actor{Kind: "unknown", IP: ip}, false
		}
		ck, err := r.Cookie(SessionCookieForProfile(cfg.Profile))
		if err != nil || ck.Value == "" {
			return Actor{Kind: "unknown", IP: ip}, false
		}
		session, ok := resolver.ResolveEntraSession(r.Context(), ck.Value)
		if !ok || session == nil || session.PrincipalID == "" ||
			session.Principal.TenantID != cfg.Entra.TenantID || !safeIdentifier(session.Principal.OID) ||
			len(recognizedRoles(roleStrings(session.Principal.Roles))) == 0 {
			return Actor{Kind: "unknown", IP: ip}, false
		}
		principal := session.Principal
		return Actor{
			Kind: "user", Name: principal.Subject(), IP: ip, PrincipalID: session.PrincipalID,
			BrowserSessionTokenHash: session.BrowserSessionTokenHash, Principal: &principal,
		}, true
	}
	if cfg.LocalAuthMode == LocalAuthModeAccounts {
		resolver, ok := m.resolver.(LocalAccountSessionResolver)
		if !ok {
			return Actor{Kind: "unknown", IP: ip}, false
		}
		ck, err := r.Cookie(SessionCookie)
		if err != nil || ck.Value == "" {
			return Actor{Kind: "unknown", IP: ip}, false
		}
		session, ok := resolver.ResolveLocalAccountSession(r.Context(), ck.Value)
		if !ok || session == nil || session.PrincipalID == "" || session.Subject == "" ||
			len(recognizedRoles(roleStrings(session.Roles))) == 0 {
			return Actor{Kind: "unknown", IP: ip}, false
		}
		return Actor{
			Kind:                    "user",
			Name:                    session.Subject,
			IP:                      ip,
			PrincipalID:             session.PrincipalID,
			BrowserSessionTokenHash: session.BrowserSessionTokenHash,
			Roles:                   append([]Role(nil), session.Roles...),
		}, true
	}
	if m.resolver != nil {
		if ck, err := r.Cookie(SessionCookieForProfile(cfg.Profile)); err == nil && ck.Value != "" {
			if owner, ok := m.resolver.ResolveSession(r.Context(), ck.Value); ok {
				return Actor{Kind: "user", Name: owner, IP: ip}, true
			}
		}
	}
	// 2. bearer API key — DB-stored, then env-configured. Account mode returned
	// above so global legacy API keys can never gain principal-scoped access.
	if tok := bearerToken(r); tok != "" {
		if m.resolver != nil {
			if owner, ok := m.resolver.ResolveAPIKey(r.Context(), tok); ok {
				return Actor{Kind: "service", Name: owner, IP: ip}, true
			}
		}
		if name, ok := MatchToken(tok, cfg.APITokens); ok {
			return Actor{Kind: "service", Name: name, IP: ip}, true
		}
	}
	return Actor{Kind: "unknown", IP: ip}, false
}

// bearerToken extracts the token from an `Authorization: Bearer <t>`
// header, or "" when absent / malformed.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// ClientIP returns the best-effort caller IP: the first hop of
// X-Forwarded-For when present, else the RemoteAddr host.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLoopbackReq reports whether the request arrived directly over the
// loopback socket with no X-Forwarded-For — i.e. an on-host operator
// call, not a Traefik-forwarded one.
func isLoopbackReq(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
