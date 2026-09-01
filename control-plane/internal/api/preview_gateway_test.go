package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

type previewRuntimeStub struct {
	target runtimebackend.PreviewTarget
	calls  int
	refs   []runtimebackend.SandboxRef
}

func (r *previewRuntimeStub) EnsurePreview(_ context.Context, ref runtimebackend.SandboxRef) (runtimebackend.PreviewTarget, error) {
	r.calls++
	r.refs = append(r.refs, ref)
	return r.target, nil
}

type capturePreviewTransport struct {
	request *http.Request
}

func (t *capturePreviewTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.request = r.Clone(r.Context())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Set-Cookie":                 {"sandbox_preview=attacker; Path=/", "auth_token=attacker; Path=/", "application_cookie=ok; Path=/"},
			"X-Sandbox-External-User-Id": {"attacker"},
			"X-Access-Token":             {"attacker"},
			"X-Powered-By":               {"sandbox"},
			"Content-Type":               {"text/plain"},
		},
		Body:    io.NopCloser(strings.NewReader("preview")),
		Request: r,
	}, nil
}

func TestProductionPreviewTicketHostBindingSingleUseAndCookie(t *testing.T) {
	s, owner, _ := previewTestServer(t)
	ticketURL := issuePreviewTicket(t, s, owner, "01OWNER")

	wrong := previewRequest(http.MethodGet, ticketURL, "s-01owner-3001.preview.example.test", "")
	wrongResult := httptest.NewRecorder()
	s.ProductionPreviewHandler().ServeHTTP(wrongResult, wrong)
	if wrongResult.Code != http.StatusNotFound {
		t.Fatalf("wrong host status = %d; want 404", wrongResult.Code)
	}

	first := previewRequest(http.MethodGet, ticketURL, "s-01owner-3000.preview.example.test", "")
	firstResult := httptest.NewRecorder()
	s.ProductionPreviewHandler().ServeHTTP(firstResult, first)
	if firstResult.Code != http.StatusFound || firstResult.Header().Get("Location") != "/" {
		t.Fatalf("ticket exchange = %d location=%q", firstResult.Code, firstResult.Header().Get("Location"))
	}
	cookie := cookieFromName(t, firstResult, previewCookieName)
	if cookie.Domain != "" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe preview cookie: %#v", cookie)
	}
	if strings.Contains(firstResult.Header().Get("Location"), "preview_ticket") {
		t.Fatalf("ticket was retained in redirect: %q", firstResult.Header().Get("Location"))
	}

	replay := previewRequest(http.MethodGet, ticketURL, "s-01owner-3000.preview.example.test", "")
	replayResult := httptest.NewRecorder()
	s.ProductionPreviewHandler().ServeHTTP(replayResult, replay)
	if replayResult.Code != http.StatusNotFound {
		t.Fatalf("replayed ticket status = %d; want 404", replayResult.Code)
	}
}

func TestPreviewGatewayDeniesForeignOwnerAllowsAuditedAdminAndStripsCredentials(t *testing.T) {
	s, _, admin := previewTestServer(t)
	foreign := testEntraPrincipal("foreign", auth.RoleUser)
	seedAuthorizationPrincipal(t, s, foreign)
	if w := serveAuthorizationRequest(s, http.MethodPost, "/v1/sandboxes/01OWNER/preview-ticket", "", &foreign); w.Code != http.StatusNotFound {
		t.Fatalf("foreign ticket status = %d; want 404", w.Code)
	}

	transport := &capturePreviewTransport{}
	s.PreviewTransport = transport
	ticketURL := issuePreviewTicket(t, s, admin, "01OWNER")
	exchange := httptest.NewRecorder()
	s.ProductionPreviewHandler().ServeHTTP(exchange, previewRequest(http.MethodGet, ticketURL, "s-01owner-3000.preview.example.test", ""))
	if exchange.Code != http.StatusFound {
		t.Fatalf("admin exchange = %d: %s", exchange.Code, exchange.Body.String())
	}
	cookie := cookieFromName(t, exchange, previewCookieName)

	proxyRequest := previewRequest(http.MethodGet, "https://s-01owner-3000.preview.example.test/assets/app.js", "s-01owner-3000.preview.example.test", cookie.Value)
	proxyRequest.Header.Set("Authorization", "Bearer stolen")
	proxyRequest.Header.Set("X-Forwarded-For", "203.0.113.1")
	proxyRequest.Header.Set("X-Sandbox-External-User-Id", "forged")
	proxyRequest.Header.Set("X-Access-Token", "forged")
	proxyRequest.Header.Set("Referer", ticketURL)
	proxyRequest.Header.Set("Cookie", previewCookieName+"="+cookie.Value+"; sbx_session=console; auth_token=api; application_cookie=ok")
	proxy := httptest.NewRecorder()
	s.ProductionPreviewHandler().ServeHTTP(proxy, proxyRequest)
	if proxy.Code != http.StatusOK || proxy.Body.String() != "preview" {
		t.Fatalf("proxy = %d %q", proxy.Code, proxy.Body.String())
	}
	if s.PreviewRuntime.(*previewRuntimeStub).calls != 1 {
		t.Fatalf("preview runtime calls = %d; want 1", s.PreviewRuntime.(*previewRuntimeStub).calls)
	}
	if transport.request == nil {
		t.Fatal("preview request was not proxied")
	}
	for _, name := range []string{"Authorization", "X-Forwarded-For", "X-Sandbox-External-User-Id", "X-Access-Token", "Referer"} {
		if got := transport.request.Header.Get(name); got != "" {
			t.Errorf("sandbox received %s=%q", name, got)
		}
	}
	if got := transport.request.Header.Get("Cookie"); got != "application_cookie=ok" {
		t.Errorf("sandbox cookie = %q; want application cookie only", got)
	}
	for _, name := range []string{"X-Sandbox-External-User-Id", "X-Access-Token"} {
		if got := proxy.Header().Get(name); got != "" {
			t.Errorf("reserved response header %s leaked: %q", name, got)
		}
	}
	if got := proxy.Header().Get("X-Powered-By"); got != "" {
		t.Errorf("powered-by response header leaked: %q", got)
	}
	for _, got := range proxy.Result().Cookies() {
		if got.Name == previewCookieName || got.Name == "auth_token" {
			t.Fatalf("sandbox set reserved cookie %q", got.Name)
		}
	}
	if proxy.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing response security headers")
	}
}

func TestPreviewTicketLocalProfileDoesNotChangeLegacyPreview(t *testing.T) {
	s := &Server{Auth: auth.NewMiddleware(&auth.Config{Profile: auth.ProfileLocal}, nil, nil, nil)}
	if s.ProductionPreviewHandler() != nil {
		t.Fatal("local profile unexpectedly enabled production preview gateway")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/01OWNER/preview-ticket", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("local ticket endpoint = %d; want 404", w.Code)
	}
}

func previewTestServer(t *testing.T) (*Server, auth.Principal, auth.Principal) {
	t.Helper()
	s := productionAuthorizationServer(t)
	s.PreviewDomain = "example.test"
	s.PreviewURLScheme = "https"
	runtime := &previewRuntimeStub{target: runtimebackend.PreviewTarget{
		URL: "http://preview.sandboxd-01owner.svc.cluster.local:3000",
	}}
	s.PreviewRuntime = runtime
	owner := testEntraPrincipal("owner", auth.RoleUser)
	admin := testEntraPrincipal("admin", auth.RoleAdmin)
	ownerID := seedAuthorizationPrincipal(t, s, owner)
	seedAuthorizationPrincipal(t, s, admin)
	if err := s.Store.Create(context.Background(), &store.Sandbox{
		ID: "01OWNER", Status: "stopped", Image: "image", WorkspaceImg: "image", WorkspaceMnt: "mount",
		OwnerPrincipalID: nullStr(ownerID),
	}); err != nil {
		t.Fatal(err)
	}
	return s, owner, admin
}

func issuePreviewTicket(t *testing.T, s *Server, principal auth.Principal, id string) string {
	t.Helper()
	stored, err := s.Store.GetPrincipal(context.Background(), "entra", principal.TenantID, principal.OID)
	if err != nil {
		t.Fatal(err)
	}
	browserSessionTokenHash := "browser-session-" + principal.OID
	if err := s.Store.CreateBrowserSession(context.Background(), store.BrowserSession{
		TokenHash: browserSessionTokenHash, PrincipalID: stored.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil && !errors.Is(err, store.ErrConflict) {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+id+"/preview-ticket", nil)
	r = r.WithContext(auth.WithActor(r.Context(), auth.Actor{
		Kind: "user", Name: principal.Subject(), PrincipalID: stored.ID,
		BrowserSessionTokenHash: browserSessionTokenHash, Principal: &principal,
	}))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("ticket response = %d: %s", w.Code, w.Body.String())
	}
	var result previewTicketResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.URL == "" {
		t.Fatal("ticket response omitted URL")
	}
	return result.URL
}

func previewRequest(method, rawURL, host, cookie string) *http.Request {
	request := httptest.NewRequest(method, rawURL, nil)
	request.Host = host
	request.Header.Set("X-Sandboxd-Preview-Gateway", "1")
	if cookie != "" {
		request.AddCookie(&http.Cookie{Name: previewCookieName, Value: cookie})
	}
	return request
}

func TestProductionPreviewRejectsClientForwardedHTTPSHeader(t *testing.T) {
	s, owner, _ := previewTestServer(t)
	ticketURL := issuePreviewTicket(t, s, owner, "01OWNER")
	request := httptest.NewRequest(http.MethodGet, strings.Replace(ticketURL, "https://", "http://", 1), nil)
	request.Host = "s-01owner-3000.preview.example.test"
	request.Header.Set("X-Forwarded-Proto", "https")

	result := httptest.NewRecorder()
	s.ProductionPreviewHandler().ServeHTTP(result, request)
	if result.Code != http.StatusNotFound {
		t.Fatalf("client forwarded-proto status = %d; want 404", result.Code)
	}
}

func TestValidPreviewTargetRejectsNonClusterTargets(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:3000",
		"https://preview.sandboxd-01owner.svc.cluster.local:3000",
		"http://preview.sandboxd-01owner.svc.cluster.local",
		"http://preview.sandboxd-01owner.svc.cluster.local:3000/path",
	} {
		if _, err := validPreviewTarget(runtimebackend.PreviewTarget{URL: raw}); err == nil {
			t.Errorf("unsafe target accepted: %q", raw)
		}
	}
	if _, err := validPreviewTarget(runtimebackend.PreviewTarget{URL: "http://preview.sandboxd-01owner.svc.cluster.local:3000"}); err != nil {
		t.Fatalf("safe target rejected: %v", err)
	}
}

func TestProviderPreviewTargetBindsRuntimeNamespaceAndPort(t *testing.T) {
	s := &Server{PreviewClusterDomain: "cluster.example", ProviderWebPort: 4173}
	ref := runtimebackend.SandboxRef{ID: "01OWNER", RuntimeID: "sandboxd-01owner"}
	valid := runtimebackend.PreviewTarget{URL: "http://preview.sandboxd-01owner.svc.cluster.example:4173"}
	if _, err := s.validProviderPreviewTarget(valid, ref); err != nil {
		t.Fatalf("valid provider target rejected: %v", err)
	}
	for _, raw := range []string{
		"http://other.sandboxd-01owner.svc.cluster.example:4173",
		"http://preview.other.svc.cluster.example:4173",
		"http://preview.sandboxd-01owner.svc.cluster.example:3000",
		"http://preview.sandboxd-01owner.svc.cluster.local:4173",
	} {
		if _, err := s.validProviderPreviewTarget(runtimebackend.PreviewTarget{URL: raw}, ref); err == nil {
			t.Errorf("provider target escaped sandbox binding: %q", raw)
		}
	}
	if _, err := s.validProviderPreviewTarget(valid, runtimebackend.SandboxRef{ID: ref.ID, RuntimeID: "sandbox-ns/01owner"}); err == nil {
		t.Fatal("provider target accepted a non-namespace runtime handle")
	}
}

func TestProductionProviderPreviewUsesBoundServiceAndRefreshesActivity(t *testing.T) {
	s := productionAuthorizationServer(t)
	s.PreviewDomain = "example.test"
	s.PreviewURLScheme = "https"
	s.PreviewClusterDomain = "cluster.example"
	s.ProviderWebPort = 4173
	provider := newProviderRuntimeFake()
	preview := &previewRuntimeStub{target: runtimebackend.PreviewTarget{
		URL: "http://preview.sandboxd-provider.svc.cluster.example:4173",
	}}
	s.RuntimeLifecycle = provider
	s.PreviewRuntime = preview
	s.PreviewReadiness = provider

	owner := testEntraPrincipal("provider-owner", auth.RoleUser)
	ownerID := seedAuthorizationPrincipal(t, s, owner)
	id := newULID()
	if err := s.Store.Create(context.Background(), &store.Sandbox{
		ID:               id,
		Status:           "running",
		Image:            "sandboxd:test",
		OwnerPrincipalID: nullStr(ownerID),
		Visibility:       "private",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetRuntimeState(context.Background(), id, "running", "sandboxd-provider"); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.sandboxes[id] = runtimebackend.Sandbox{
		Ref:   runtimebackend.SandboxRef{ID: id, RuntimeID: "sandboxd-provider"},
		State: runtimebackend.LifecycleRunning,
	}
	provider.mu.Unlock()

	ticketURL := issuePreviewTicket(t, s, owner, id)
	parsed, err := url.Parse(ticketURL)
	if err != nil {
		t.Fatal(err)
	}
	exchange := httptest.NewRecorder()
	s.ProductionPreviewHandler().ServeHTTP(exchange, previewRequest(http.MethodGet, ticketURL, parsed.Host, ""))
	if exchange.Code != http.StatusFound {
		t.Fatalf("provider ticket exchange = %d: %s", exchange.Code, exchange.Body.String())
	}
	cookie := cookieFromName(t, exchange, previewCookieName)
	transport := &capturePreviewTransport{}
	s.PreviewTransport = transport
	proxyURL := *parsed
	proxyURL.RawQuery = ""
	proxyURL.Path = "/asset.js"
	proxy := httptest.NewRecorder()
	s.ProductionPreviewHandler().ServeHTTP(proxy, previewRequest(http.MethodGet, proxyURL.String(), parsed.Host, cookie.Value))
	if proxy.Code != http.StatusOK {
		t.Fatalf("provider preview proxy = %d: %s", proxy.Code, proxy.Body.String())
	}
	if transport.request == nil || transport.request.URL.Host != "preview.sandboxd-provider.svc.cluster.example:4173" {
		t.Fatalf("provider preview target = %#v", transport.request)
	}
	if len(preview.refs) != 1 || preview.refs[0].RuntimeID != "sandboxd-provider" {
		t.Fatalf("preview runtime reference = %#v", preview.refs)
	}
	provider.mu.Lock()
	inspectRefs := append([]runtimebackend.SandboxRef(nil), provider.inspectRefs...)
	provider.mu.Unlock()
	if len(inspectRefs) == 0 || inspectRefs[0].RuntimeID != "sandboxd-provider" {
		t.Fatalf("provider inspection reference = %#v", inspectRefs)
	}
	provider.mu.Lock()
	previewRefs := append([]runtimebackend.SandboxRef(nil), provider.previewRefs...)
	provider.mu.Unlock()
	if len(previewRefs) != 1 || previewRefs[0].RuntimeID != "sandboxd-provider" {
		t.Fatalf("provider preview readiness reference = %#v", previewRefs)
	}
	fresh, err := s.Store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.LastActiveAt.IsZero() {
		t.Fatal("provider preview did not refresh durable activity")
	}
	if s.Docker != nil || s.Loopback != nil {
		t.Fatal("provider preview test unexpectedly configured a local runtime")
	}
}

func TestPreviewTicketURLHasOnlyOpaqueTicket(t *testing.T) {
	s, owner, _ := previewTestServer(t)
	raw := issuePreviewTicket(t, s, owner, "01OWNER")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "s-01owner-3000.preview.example.test" || len(parsed.Query()["preview_ticket"]) != 1 {
		t.Fatalf("ticket URL = %q", raw)
	}
	if strings.Contains(raw, "owner") && strings.Contains(parsed.RawQuery, "owner") {
		t.Fatalf("ticket query contains identity: %q", parsed.RawQuery)
	}
	if console.HashToken(parsed.Query().Get("preview_ticket")) == parsed.Query().Get("preview_ticket") {
		t.Fatal("ticket appears to be a stored hash")
	}
}
