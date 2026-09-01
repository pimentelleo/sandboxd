package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// authTestServer builds a Server whose Handler is wrapped by a real auth
// middleware + store resolver, so cookie/bearer resolution is exercised end to
// end.
func authTestServer(t *testing.T) (http.Handler, *Server) {
	t.Helper()
	st, err := store.Open(context.Background(), "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st}
	s.Auth = auth.NewMiddleware(&auth.Config{}, NewStoreResolver(st), nil, nil)
	return s.Auth.Wrap(s.Handler()), s
}

func doAuth(t *testing.T, h http.Handler, method, path, body, cookie, bearer string) *httptest.ResponseRecorder {
	return doAuthWithCookie(t, h, method, path, body, auth.SessionCookie, cookie, bearer)
}

func doAuthWithCookie(t *testing.T, h http.Handler, method, path, body, cookieName, cookie, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, rdr)
	r.RemoteAddr = "203.0.113.5:4000"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// sessionFrom extracts the sbx_session cookie value from a response.
func sessionFrom(t *testing.T, w *httptest.ResponseRecorder) string {
	return sessionFromName(t, w, auth.SessionCookie)
}

func sessionFromName(t *testing.T, w *httptest.ResponseRecorder, name string) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("no %s cookie in response (code %d)", name, w.Code)
	return ""
}

func cookieFromName(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in response (code %d)", name, w.Code)
	return nil
}

func TestAuthSetupLoginFlow(t *testing.T) {
	h, _ := authTestServer(t)

	// status before setup: password not set, not authed
	w := doAuth(t, h, "GET", "/v1/auth/status", "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"password_set":false`) {
		t.Fatalf("pre-setup status: %d %s", w.Code, w.Body.String())
	}

	// setup with a too-short password → 400
	if w := doAuth(t, h, "POST", "/v1/auth/setup", `{"password":"short"}`, "", ""); w.Code != 400 {
		t.Fatalf("short setup: want 400 got %d", w.Code)
	}

	// setup → 204 + session cookie
	w = doAuth(t, h, "POST", "/v1/auth/setup", `{"password":"correcthorse"}`, "", "")
	if w.Code != 204 {
		t.Fatalf("setup: want 204 got %d %s", w.Code, w.Body.String())
	}
	cookie := sessionFrom(t, w)
	localCookie := cookieFromName(t, w, auth.SessionCookie)
	if localCookie.Secure {
		t.Fatal("local plain-HTTP session cookie unexpectedly became Secure")
	}

	// second setup → 409
	if w := doAuth(t, h, "POST", "/v1/auth/setup", `{"password":"anotherone"}`, "", ""); w.Code != 409 {
		t.Fatalf("second setup: want 409 got %d", w.Code)
	}

	// the session authorizes a protected route
	if w := doAuth(t, h, "GET", "/v1/api-keys", "", cookie, ""); w.Code != 200 {
		t.Fatalf("session on protected route: want 200 got %d", w.Code)
	}

	// status with the cookie: authenticated + password_set
	w = doAuth(t, h, "GET", "/v1/auth/status", "", cookie, "")
	if !strings.Contains(w.Body.String(), `"authenticated":true`) || !strings.Contains(w.Body.String(), `"password_set":true`) {
		t.Fatalf("authed status: %s", w.Body.String())
	}

	// login wrong password → 401
	if w := doAuth(t, h, "POST", "/v1/auth/login", `{"password":"nope"}`, "", ""); w.Code != 401 {
		t.Fatalf("bad login: want 401 got %d", w.Code)
	}
	// login right password → 204 + cookie
	if w := doAuth(t, h, "POST", "/v1/auth/login", `{"password":"correcthorse"}`, "", ""); w.Code != 204 {
		t.Fatalf("good login: want 204 got %d", w.Code)
	}

	// change password with bad current → 401
	if w := doAuth(t, h, "POST", "/v1/auth/password", `{"current_password":"x","new_password":"brandnewpass"}`, cookie, ""); w.Code != 401 {
		t.Fatalf("change-pw bad current: want 401 got %d", w.Code)
	}
	// change password ok → 204
	if w := doAuth(t, h, "POST", "/v1/auth/password", `{"current_password":"correcthorse","new_password":"brandnewpass"}`, cookie, ""); w.Code != 204 {
		t.Fatalf("change-pw: want 204 got %d %s", w.Code, w.Body.String())
	}
	// the OLD session was invalidated by the password change
	if w := doAuth(t, h, "GET", "/v1/api-keys", "", cookie, ""); w.Code != 401 {
		t.Fatalf("old session after pw change: want 401 got %d", w.Code)
	}
}

func TestLocalAccountBootstrapAuthenticationAndAuthorization(t *testing.T) {
	h, s := localAccountAuthTestServer(t)

	status := doAuth(t, h, http.MethodGet, "/v1/auth/status", "", "", "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"local_auth_mode":"accounts"`) ||
		!strings.Contains(status.Body.String(), `"password_set":false`) {
		t.Fatalf("initial local-account status = %d %s", status.Code, status.Body.String())
	}

	setup := doAuth(t, h, http.MethodPost, "/v1/auth/setup",
		`{"email":"Admin@Example.Test","password":"correcthorse"}`, "", "")
	if setup.Code != http.StatusNoContent {
		t.Fatalf("bootstrap administrator = %d %s", setup.Code, setup.Body.String())
	}
	adminCookie := sessionFrom(t, setup)

	status = doAuth(t, h, http.MethodGet, "/v1/auth/status", "", adminCookie, "")
	for _, want := range []string{`"authenticated":true`, `"email":"admin@example.test"`, `"administrator":true`} {
		if !strings.Contains(status.Body.String(), want) {
			t.Fatalf("administrator status missing %s: %s", want, status.Body.String())
		}
	}
	if w := doAuth(t, h, http.MethodPost, "/v1/auth/setup",
		`{"email":"again@example.test","password":"correcthorse"}`, "", ""); w.Code != http.StatusConflict {
		t.Fatalf("repeat bootstrap = %d, want 409", w.Code)
	}
	if w := doAuth(t, h, http.MethodPost, "/v1/auth/login",
		`{"email":"admin@example.test","password":"wrongpassword"}`, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid credentials = %d, want 401", w.Code)
	}

	createUser := doAuth(t, h, http.MethodPost, "/v1/auth/accounts",
		`{"email":"user@example.test","password":"correcthorse"}`, adminCookie, "")
	if createUser.Code != http.StatusCreated {
		t.Fatalf("create user = %d %s", createUser.Code, createUser.Body.String())
	}
	createOther := doAuth(t, h, http.MethodPost, "/v1/auth/accounts",
		`{"email":"other@example.test","password":"correcthorse"}`, adminCookie, "")
	if createOther.Code != http.StatusCreated {
		t.Fatalf("create second user = %d %s", createOther.Code, createOther.Body.String())
	}

	login := doAuth(t, h, http.MethodPost, "/v1/auth/login",
		`{"email":"user@example.test","password":"correcthorse"}`, "", "")
	if login.Code != http.StatusNoContent {
		t.Fatalf("user login = %d %s", login.Code, login.Body.String())
	}
	userCookie := sessionFrom(t, login)
	if w := doAuth(t, h, http.MethodGet, "/v1/settings", "", userCookie, ""); w.Code != http.StatusForbidden {
		t.Fatalf("user settings access = %d, want 403", w.Code)
	}
	if w := doAuth(t, h, http.MethodPost, "/v1/auth/accounts",
		`{"email":"denied@example.test","password":"correcthorse"}`, userCookie, ""); w.Code != http.StatusForbidden {
		t.Fatalf("user provisioning access = %d, want 403", w.Code)
	}
	if w := doAuth(t, h, http.MethodGet, "/v1/api-keys", "", userCookie, ""); w.Code != http.StatusNotFound {
		t.Fatalf("account-mode API keys = %d, want 404", w.Code)
	}
	if w := doAuth(t, h, http.MethodGet, "/v1/apps", "", "", "legacy-api-key"); w.Code != http.StatusUnauthorized {
		t.Fatalf("account-mode bearer credential = %d, want 401", w.Code)
	}

	createApp := doAuth(t, h, http.MethodPost, "/v1/apps", `{"name":"User app"}`, userCookie, "")
	if createApp.Code != http.StatusCreated {
		t.Fatalf("create user app = %d %s", createApp.Code, createApp.Body.String())
	}
	var app v1App
	if err := json.Unmarshal(createApp.Body.Bytes(), &app); err != nil {
		t.Fatal(err)
	}
	otherLogin := doAuth(t, h, http.MethodPost, "/v1/auth/login",
		`{"email":"other@example.test","password":"correcthorse"}`, "", "")
	otherCookie := sessionFrom(t, otherLogin)
	if w := doAuth(t, h, http.MethodGet, "/v1/apps/"+app.ID, "", otherCookie, ""); w.Code != http.StatusNotFound {
		t.Fatalf("other user app access = %d, want 404", w.Code)
	}
	if w := doAuth(t, h, http.MethodGet, "/v1/apps/"+app.ID, "", adminCookie, ""); w.Code != http.StatusOK {
		t.Fatalf("administrator app access = %d, want 200", w.Code)
	}

	change := doAuth(t, h, http.MethodPost, "/v1/auth/password",
		`{"current_password":"correcthorse","new_password":"newcorrecthorse"}`, userCookie, "")
	if change.Code != http.StatusNoContent {
		t.Fatalf("account password change = %d %s", change.Code, change.Body.String())
	}
	rotatedCookie := sessionFrom(t, change)
	if w := doAuth(t, h, http.MethodGet, "/v1/apps", "", userCookie, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("old account session after password change = %d, want 401", w.Code)
	}
	if w := doAuth(t, h, http.MethodPost, "/v1/auth/login",
		`{"email":"user@example.test","password":"correcthorse"}`, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("old account password remains valid = %d, want 401", w.Code)
	}
	if w := doAuth(t, h, http.MethodPost, "/v1/auth/login",
		`{"email":"user@example.test","password":"newcorrecthorse"}`, "", ""); w.Code != http.StatusNoContent {
		t.Fatalf("new account password login = %d, want 204", w.Code)
	}
	logout := doAuth(t, h, http.MethodPost, "/v1/auth/logout", "", rotatedCookie, "")
	if logout.Code != http.StatusNoContent {
		t.Fatalf("account logout = %d %s", logout.Code, logout.Body.String())
	}
	if w := doAuth(t, h, http.MethodGet, "/v1/apps", "", rotatedCookie, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out account session = %d, want 401", w.Code)
	}

	account, err := s.Store.GetLocalAccount(context.Background(), "user@example.test")
	if err != nil || account.Principal.ID == "" {
		t.Fatalf("stored account = %#v, %v", account, err)
	}
}

func localAccountAuthTestServer(t *testing.T) (http.Handler, *Server) {
	t.Helper()
	st, err := store.Open(context.Background(), "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st}
	s.Auth = auth.NewMiddleware(&auth.Config{
		Profile:       auth.ProfileLocal,
		LocalAuthMode: auth.LocalAuthModeAccounts,
	}, NewStoreResolver(st), nil, nil)
	return s.Auth.Wrap(s.Handler()), s
}

func TestNoCredentialIsUnauthorized(t *testing.T) {
	h, _ := authTestServer(t)
	if w := doAuth(t, h, "GET", "/v1/api-keys", "", "", ""); w.Code != 401 {
		t.Fatalf("no credential: want 401 got %d", w.Code)
	}
}

type testEntraExchanger struct{}

func (testEntraExchanger) ExchangeAuthorizationCode(_ context.Context, _ auth.EntraConfig, code, _ string) (string, error) {
	if code == "good-code" {
		return "verified-id-token", nil
	}
	return "", auth.ErrOIDCProvider
}

type testEntraVerifier struct {
	roles []string
	nonce string
}

func (v *testEntraVerifier) VerifyIDToken(_ context.Context, raw string, expected auth.OIDCValidation) (auth.OIDCClaims, error) {
	if raw != "verified-id-token" {
		return auth.OIDCClaims{}, auth.ErrOIDCToken
	}
	return auth.OIDCClaims{
		Issuer: expected.Issuer, Audience: []string{expected.Audience}, TenantID: expected.TenantID,
		Nonce: v.nonce, OID: "oid-123", Name: "Ada", PreferredUsername: "ada@example.test",
		Roles: v.roles, ExpiresAt: expected.Now.Add(time.Hour), NotBefore: expected.Now.Add(-time.Minute),
		IssuedAt: expected.Now.Add(-time.Minute),
	}, nil
}

func newEntraAuthTestServer(t *testing.T, roles []string) (http.Handler, *Server, *testEntraVerifier) {
	t.Helper()
	st, err := store.Open(context.Background(), "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	cfg := auth.EntraConfig{
		TenantID: "tenant-123", ClientID: "client-123", ClientSecret: "not-returned",
		RedirectURL:  "https://console.example.test/v1/auth/entra/callback",
		Issuer:       "https://login.example.test/tenant-123/v2.0",
		AuthorizeURL: "https://login.example.test/tenant-123/oauth2/v2.0/authorize",
		TokenURL:     "https://login.example.test/tenant-123/oauth2/v2.0/token",
		JWKSURL:      "https://login.example.test/tenant-123/discovery/v2.0/keys",
	}
	verifier := &testEntraVerifier{roles: roles}
	flow := auth.NewOIDCFlow(cfg, testEntraExchanger{}, verifier, NewEntraLoginTransactionStore(st))
	s := &Server{Store: st}
	s.Auth = auth.NewMiddleware(
		&auth.Config{
			Profile: auth.ProfileEntra, Entra: cfg,
			APITokens: []auth.NamedToken{{Name: "environment", Token: "environment-key"}},
		},
		NewStoreResolver(st), nil, nil, auth.WithOIDC(flow),
	)
	return s.Auth.Wrap(s.Handler()), s, verifier
}

func completeEntraLogin(t *testing.T, h http.Handler, verifier *testEntraVerifier) string {
	t.Helper()
	start := doAuth(t, h, http.MethodGet, "/v1/auth/entra/login", "", "", "")
	if start.Code != http.StatusFound {
		t.Fatalf("Entra start: want 302 got %d %s", start.Code, start.Body.String())
	}
	u, err := url.Parse(start.Result().Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	verifier.nonce = u.Query().Get("nonce")
	callback := doAuth(t, h, http.MethodGet,
		"/v1/auth/entra/callback?state="+url.QueryEscape(u.Query().Get("state"))+"&code=good-code", "", "", "")
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("Entra callback: want 303 got %d %s", callback.Code, callback.Body.String())
	}
	return sessionFromName(t, callback, auth.EntraSessionCookie)
}

func TestEntraLoginCallbackAndSafeStatus(t *testing.T) {
	h, s, verifier := newEntraAuthTestServer(t, []string{string(auth.RoleUser), string(auth.RoleAdmin)})

	start := doAuth(t, h, http.MethodGet, "/v1/auth/entra/login", "", "", "")
	if start.Code != http.StatusFound {
		t.Fatalf("Entra start: want 302 got %d %s", start.Code, start.Body.String())
	}
	location := start.Result().Header.Get("Location")
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" || q.Get("code_verifier") != "" {
		t.Fatalf("unsafe or incomplete authorize URL: %s", location)
	}
	verifier.nonce = q.Get("nonce")

	callback := doAuth(t, h, http.MethodGet,
		"/v1/auth/entra/callback?state="+url.QueryEscape(q.Get("state"))+"&code=good-code", "", "", "")
	if callback.Code != http.StatusSeeOther || callback.Result().Header.Get("Location") != "/" {
		t.Fatalf("callback: want safe redirect, got %d %q", callback.Code, callback.Result().Header.Get("Location"))
	}
	cookie := sessionFromName(t, callback, auth.EntraSessionCookie)
	entraCookie := cookieFromName(t, callback, auth.EntraSessionCookie)
	if entraCookie.Path != "/" || !entraCookie.Secure || entraCookie.Domain != "" {
		t.Fatalf("Entra session cookie is not host-only and secure: %#v", entraCookie)
	}
	if _, err := s.Store.GetActiveBrowserSession(context.Background(), console.HashToken(cookie)); err != nil {
		t.Fatalf("opaque Entra session was not persisted: %v", err)
	}

	status := doAuthWithCookie(t, h, http.MethodGet, "/v1/auth/status", "", auth.EntraSessionCookie, cookie, "")
	body := status.Body.String()
	for _, want := range []string{
		`"profile":"entra"`, `"authenticated":true`, `"oid":"oid-123"`,
		`"tenant_id":"tenant-123"`, `"administrator":true`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("status missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "not-returned") || strings.Contains(body, "verified-id-token") {
		t.Fatalf("status exposed a credential or token: %s", body)
	}
	replica := auth.NewMiddleware(s.Auth.Snapshot(), NewStoreResolver(s.Store), nil, nil)
	replicaStatus := doAuthWithCookie(t, replica.Wrap(s.Handler()), http.MethodGet, "/v1/auth/status", "", auth.EntraSessionCookie, cookie, "")
	if !strings.Contains(replicaStatus.Body.String(), `"authenticated":true`) {
		t.Fatalf("a second middleware instance did not resolve persisted Entra session: %s", replicaStatus.Body.String())
	}
	if w := doAuthWithCookie(t, h, http.MethodGet, "/v1/api-keys", "", auth.EntraSessionCookie, cookie, ""); w.Code != http.StatusOK {
		t.Fatalf("typed Entra session on protected route: want 200 got %d", w.Code)
	}
	if w := doAuth(t, h, http.MethodGet, "/v1/auth/status", "", "", "environment-key"); strings.Contains(w.Body.String(), `"authenticated":true`) {
		t.Fatalf("Entra status treated a bearer credential as authenticated: %s", w.Body.String())
	}
	browserSession, err := s.Store.GetActiveBrowserSession(context.Background(), console.HashToken(cookie))
	if err != nil {
		t.Fatalf("load Entra browser session: %v", err)
	}
	if err := s.Store.Create(context.Background(), &store.Sandbox{
		ID: "01PREVIEW", Status: "stopped", Image: "image", WorkspaceImg: "workspace", WorkspaceMnt: "mount",
		OwnerPrincipalID: sql.NullString{String: browserSession.PrincipalID, Valid: true},
	}); err != nil {
		t.Fatalf("create preview sandbox: %v", err)
	}
	if err := s.Store.CreatePreviewTicket(context.Background(), store.PreviewTicket{
		TokenHash: "logout-preview-ticket", SandboxID: "01PREVIEW", PrincipalID: browserSession.PrincipalID,
		BrowserSessionTokenHash: browserSession.TokenHash,
		PreviewHost:             "s-01preview-3000.preview.example.test", ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("create preview ticket: %v", err)
	}
	if _, err := s.Store.ConsumePreviewTicket(context.Background(), "logout-preview-ticket", "logout-preview-session",
		"01PREVIEW", "s-01preview-3000.preview.example.test", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("create preview session: %v", err)
	}
	if err := s.Store.CreatePreviewTicket(context.Background(), store.PreviewTicket{
		TokenHash: "logout-unredeemed-preview-ticket", SandboxID: "01PREVIEW", PrincipalID: browserSession.PrincipalID,
		BrowserSessionTokenHash: browserSession.TokenHash,
		PreviewHost:             "s-01preview-3000.preview.example.test", ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("create unredeemed preview ticket: %v", err)
	}
	logout := doAuthWithCookie(t, h, http.MethodPost, "/v1/auth/logout", "", auth.EntraSessionCookie, cookie, "")
	if logout.Code != http.StatusNoContent {
		t.Fatalf("Entra logout: want 204 got %d", logout.Code)
	}
	cleared := cookieFromName(t, logout, auth.EntraSessionCookie)
	if cleared.Path != "/" || !cleared.Secure || cleared.Domain != "" || cleared.MaxAge >= 0 {
		t.Fatalf("Entra logout did not clear matching host-only cookie: %#v", cleared)
	}
	if _, err := s.Store.GetActiveBrowserSession(context.Background(), console.HashToken(cookie)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("logout did not revoke persistent Entra session: %v", err)
	}
	if _, err := s.Store.GetActivePreviewSession(context.Background(), "logout-preview-session"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("logout did not revoke persistent preview session: %v", err)
	}
	if _, err := s.Store.ConsumePreviewTicket(context.Background(), "logout-unredeemed-preview-ticket", "logout-unredeemed-preview-session",
		"01PREVIEW", "s-01preview-3000.preview.example.test", time.Now().Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("logout did not revoke unredeemed preview ticket: %v", err)
	}
}

func TestEntraLoginKeepsOnlySafeReturnLocation(t *testing.T) {
	h, _, verifier := newEntraAuthTestServer(t, []string{string(auth.RoleUser)})
	start := doAuth(t, h, http.MethodGet, "/v1/auth/entra/login?return=%2Fapps%3Ftab%3Dmine", "", "", "")
	if start.Code != http.StatusFound {
		t.Fatalf("start code = %d", start.Code)
	}
	u, err := url.Parse(start.Result().Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	verifier.nonce = u.Query().Get("nonce")
	callback := doAuth(t, h, http.MethodGet,
		"/v1/auth/entra/callback?state="+url.QueryEscape(u.Query().Get("state"))+"&code=good-code", "", "", "")
	if callback.Code != http.StatusSeeOther || callback.Result().Header.Get("Location") != "/apps?tab=mine" {
		t.Fatalf("safe return redirect = %d %q", callback.Code, callback.Result().Header.Get("Location"))
	}
}

func TestEntraCallbackDeniesUnrecognizedRoleAndRejectsStateReplay(t *testing.T) {
	h, _, verifier := newEntraAuthTestServer(t, []string{"not-sandboxd"})
	start := doAuth(t, h, http.MethodGet, "/v1/auth/entra/login", "", "", "")
	u, err := url.Parse(start.Result().Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	verifier.nonce = u.Query().Get("nonce")
	callbackPath := "/v1/auth/entra/callback?state=" + url.QueryEscape(state) + "&code=good-code"
	denied := doAuth(t, h, http.MethodGet, callbackPath, "", "", "")
	if denied.Code != http.StatusSeeOther || denied.Result().Header.Get("Location") != "/?auth_error=denied" {
		t.Fatalf("denied callback = %d %q", denied.Code, denied.Result().Header.Get("Location"))
	}
	replay := doAuth(t, h, http.MethodGet, callbackPath, "", "", "")
	if replay.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d, want 400", replay.Code)
	}
}

func TestLogoutEverywhereRequiresConsoleSession(t *testing.T) {
	h, _ := authTestServer(t)
	if w := doAuth(t, h, http.MethodPost, "/v1/auth/logout?all=true", "", "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated global logout: want 403 got %d", w.Code)
	}
}
