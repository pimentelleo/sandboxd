package auth

import "context"

const (
	// SessionCookie is the local-profile console session cookie. Its legacy
	// name and HTTP-compatible attributes preserve local-install behavior.
	SessionCookie = "sbx_session"
	// EntraSessionCookie is host-only by the __Host- prefix contract.
	EntraSessionCookie = "__Host-sandboxd_session"
)

// SessionCookieForProfile returns the only session cookie accepted by a profile.
func SessionCookieForProfile(profile Profile) string {
	if profile == ProfileEntra {
		return EntraSessionCookie
	}
	return SessionCookie
}

// LocalAccountSession is the durable local-account equivalent of EntraSession.
// It contains no password hash or plaintext browser credential.
type LocalAccountSession struct {
	PrincipalID             string
	BrowserSessionTokenHash string
	Subject                 string
	DisplayName             string
	Email                   string
	Roles                   []Role
}

// LocalAccountSessionResolver is intentionally separate from the legacy local
// resolver: account mode must never fall back to a shared owner or API key.
type LocalAccountSessionResolver interface {
	ResolveLocalAccountSession(ctx context.Context, cookieValue string) (*LocalAccountSession, bool)
}

// CredentialResolver resolves the two DB-backed credential types to an owner
// (tenant). It is implemented in the api package over *store.Store; declaring it
// here keeps internal/auth free of a store dependency (store imports would form
// a cycle). A nil resolver means "only env-configured SANDBOXD_API_TOKENS are
// accepted" — the pre-console behaviour.
type CredentialResolver interface {
	// ResolveSession maps a session cookie value to its owner. ok=false when the
	// cookie is absent, unknown, or expired.
	ResolveSession(ctx context.Context, cookieValue string) (owner string, ok bool)
	// ResolveAPIKey maps a presented bearer key to its owner. ok=false when the
	// key is absent or unknown.
	ResolveAPIKey(ctx context.Context, presented string) (owner string, ok bool)
}
