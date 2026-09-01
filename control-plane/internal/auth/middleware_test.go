package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubResolver answers from fixed maps.
type stubResolver struct {
	sessions      map[string]string       // cookie value -> owner
	entraSessions map[string]EntraSession // cookie value -> durable session
	keys          map[string]string       // presented key -> owner
}

func (s stubResolver) ResolveSession(_ context.Context, v string) (string, bool) {
	o, ok := s.sessions[v]
	return o, ok
}
func (s stubResolver) ResolveAPIKey(_ context.Context, v string) (string, bool) {
	o, ok := s.keys[v]
	return o, ok
}
func (s stubResolver) ResolveEntraSession(_ context.Context, v string) (*EntraSession, bool) {
	session, ok := s.entraSessions[v]
	if !ok {
		return nil, false
	}
	return &session, true
}

func newMW(disabled bool, res CredentialResolver) *Middleware {
	return NewMiddleware(&Config{Disabled: disabled}, res, nil, nil)
}

// echo handler records the actor it saw.
func echoActor(seen *Actor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = ActorFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestMiddlewareUniformEnforcement(t *testing.T) {
	res := stubResolver{
		sessions: map[string]string{"good-cookie": "default"},
		keys:     map[string]string{"good-key": "default"},
	}
	mw := newMW(false, res)

	cases := []struct {
		name       string
		path       string
		cookie     string
		bearer     string
		remoteAddr string
		wantCode   int
		wantKind   string
	}{
		{"no credential non-exempt", "/v1/apps", "", "", "203.0.113.9:5000", 401, ""},
		{"loopback no credential still 401", "/v1/apps", "", "", "127.0.0.1:5000", 401, ""},
		{"valid session cookie", "/v1/apps", "good-cookie", "", "203.0.113.9:5000", 200, "user"},
		{"valid bearer key", "/v1/apps", "", "good-key", "203.0.113.9:5000", 200, "service"},
		{"bad session + bad key", "/v1/apps", "nope", "nope", "203.0.113.9:5000", 401, ""},
		{"exempt path no credential", "/v1/auth/status", "", "", "203.0.113.9:5000", 200, "unknown"},
		{"exempt path with session", "/v1/auth/status", "good-cookie", "", "203.0.113.9:5000", 200, "user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen Actor
			h := mw.Wrap(echoActor(&seen))
			r := httptest.NewRequest("GET", tc.path, nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: SessionCookie, Value: tc.cookie})
			}
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", w.Code, tc.wantCode)
			}
			if tc.wantCode == 200 && tc.wantKind != "" && seen.Kind != tc.wantKind {
				t.Fatalf("actor kind = %q, want %q", seen.Kind, tc.wantKind)
			}
		})
	}
}

func TestMiddlewareDisabledRollback(t *testing.T) {
	mw := newMW(true, nil)
	var seen Actor
	h := mw.Wrap(echoActor(&seen))
	r := httptest.NewRequest("GET", "/v1/apps", nil)
	r.RemoteAddr = "203.0.113.9:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("disabled: code = %d, want 200", w.Code)
	}
	if seen.Name != "default" {
		t.Fatalf("disabled actor name = %q, want default", seen.Name)
	}
}

func TestMiddlewareAttachesTypedEntraPrincipal(t *testing.T) {
	principal := Principal{
		OID: "oid-123", TenantID: "tenant-123", DisplayName: "Ada",
		Roles: []Role{RoleUser, RoleAdmin},
	}
	mw := NewMiddleware(&Config{
		Profile: ProfileEntra, Entra: testEntraConfig(),
		APITokens: []NamedToken{{Name: "environment", Token: "environment-key"}},
	}, stubResolver{
		sessions: map[string]string{"legacy-cookie": "default"},
		entraSessions: map[string]EntraSession{"entra-cookie": {
			PrincipalID: "principal-123", BrowserSessionTokenHash: "session-hash", Principal: principal,
		}},
		keys: map[string]string{"database-key": "database"},
	}, nil, nil, WithLoginTransactionStore(&memoryLoginTransactions{}))

	var seen Actor
	h := mw.Wrap(echoActor(&seen))
	r := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
	r.RemoteAddr = "203.0.113.9:5000"
	r.AddCookie(&http.Cookie{Name: EntraSessionCookie, Value: "entra-cookie"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || seen.Principal == nil || seen.PrincipalID != "principal-123" ||
		seen.BrowserSessionTokenHash != "session-hash" ||
		seen.Name != principal.Subject() || !seen.Principal.HasRole(RoleAdmin) {
		t.Fatalf("typed Entra actor = %+v, status=%d", seen, w.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
	r.RemoteAddr = "203.0.113.9:5000"
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "legacy-cookie"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("legacy session in Entra profile = %d, want 401", w.Code)
	}

	for _, token := range []string{"database-key", "environment-key"} {
		r = httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
		r.RemoteAddr = "203.0.113.9:5000"
		r.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("Entra profile accepted bearer %q: %d", token, w.Code)
		}

		seen = Actor{}
		r = httptest.NewRequest(http.MethodGet, "/v1/auth/status", nil)
		r.RemoteAddr = "203.0.113.9:5000"
		r.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK || seen.Kind != "unknown" {
			t.Fatalf("Entra status resolved bearer %q as %#v (code %d)", token, seen, w.Code)
		}
	}
}

func TestMiddlewareFailsClosedForIncompleteEntraConfig(t *testing.T) {
	mw := NewMiddleware(&Config{Profile: ProfileEntra, Entra: EntraConfig{TenantID: "tenant"}}, nil, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
	h := mw.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("incomplete production auth reached handler")
	}))
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestMiddlewareStartupRequiresDurableEntraTransactions(t *testing.T) {
	cfg := &Config{Profile: ProfileEntra, Entra: testEntraConfig()}
	if err := NewMiddleware(cfg, nil, nil, nil).StartupError(); err == nil {
		t.Fatal("Entra middleware without durable transactions passed startup validation")
	}
	if err := NewMiddleware(cfg, nil, nil, nil,
		WithLoginTransactionStore(&memoryLoginTransactions{})).StartupError(); err != nil {
		t.Fatalf("Entra middleware with durable transaction adapter failed startup validation: %v", err)
	}
}
