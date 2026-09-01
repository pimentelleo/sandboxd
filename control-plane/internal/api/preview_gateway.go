package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

const (
	previewTicketLifetime  = 2 * time.Minute
	previewSessionLifetime = 20 * time.Minute
	previewCookieName      = "sandbox_preview"
)

type previewTicketResponse struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// v1CreatePreviewTicket produces a one-time browser bootstrap URL after the
// route boundary has performed the owner/admin lookup. It is deliberately not
// available only to principal-backed profiles, whose Docker preview routing is
// unchanged.
func (s *Server) v1CreatePreviewTicket(w http.ResponseWriter, r *http.Request) {
	if !s.productionAuthorization() {
		writeV1Err(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if s.Store == nil || s.PreviewRuntime == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "preview gateway unavailable")
		return
	}
	if scheme, _ := s.previewScheme(); scheme != "https" &&
		(!s.AllowInsecurePreview || scheme != "http") {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "preview gateway requires HTTPS")
		return
	}
	id := r.PathValue("id")
	sb, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.writeAuthorizationNotFound(w, r)
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "preview gateway unavailable")
		return
	}
	actor := auth.ActorFrom(r.Context())
	principal := principalID(r)
	if actor.BrowserSessionTokenHash == "" || principal == "" {
		s.writeAuthorizationError(w, r, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !requestIsAdmin(r) && (!sb.OwnerPrincipalID.Valid || sb.OwnerPrincipalID.String != principal) {
		s.writeAuthorizationNotFound(w, r)
		return
	}
	webPort := webPortOf(sb)
	if s.usesRuntimeProvider() {
		// Provider previews have one policy-controlled Service port. Do not
		// mint a browser capability for a stale or caller-selected row port.
		webPort = s.providerWebPort()
	}
	host, ok := s.previewHostFor(sb.ID, webPort)
	if !ok {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "preview gateway unavailable")
		return
	}
	ticket, ticketHash, err := console.NewSessionValue()
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not create preview ticket")
		return
	}
	now := time.Now().UTC()
	expires := now.Add(previewTicketLifetime)
	if err := s.Store.CreatePreviewTicket(r.Context(), store.PreviewTicket{
		TokenHash: ticketHash, SandboxID: sb.ID, PrincipalID: principal,
		BrowserSessionTokenHash: actor.BrowserSessionTokenHash, PreviewHost: host,
		AdminOverride: requestIsAdmin(r), CreatedAt: now, ExpiresAt: expires,
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeAuthorizationError(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "preview gateway unavailable")
		return
	}
	action := "preview.ticket_issued"
	if requestIsAdmin(r) {
		action = "preview.admin_override_issued"
	}
	s.auditAction(r, audit.Entry{Action: action, Target: sb.ID})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, previewTicketResponse{
		URL:       s.previewURL(sb.ID, webPort) + "?preview_ticket=" + url.QueryEscape(ticket),
		ExpiresAt: expires.Format(time.RFC3339),
	})
}

// ProductionPreviewHandler returns the identity-aware gateway. Main dispatches
// preview hosts here before the Docker wake handler, so a request cannot invoke
// a wake until its cookie has been authorized.
func (s *Server) ProductionPreviewHandler() http.Handler {
	if !s.productionAuthorization() {
		return nil
	}
	return http.HandlerFunc(s.handleProductionPreview)
}

// IsPreviewHost identifies the constrained public preview hostname shape. Main
// uses it to dispatch production preview traffic before the authenticated API
// mux; Docker's wake handler remains the matcher for the local profile.
func (s *Server) IsPreviewHost(rawHost string) bool {
	_, _, ok := s.previewHostForRequest(rawHost)
	return ok
}

func (s *Server) handleProductionPreview(w http.ResponseWriter, r *http.Request) {
	id, host, ok := s.previewHostForRequest(r.Host)
	if !ok || !isHTTPSPreviewRequest(r) {
		s.previewNotFound(w, r)
		return
	}
	query := r.URL.Query()
	tickets, hasTicket := query["preview_ticket"]
	if hasTicket {
		if r.Method != http.MethodGet || len(tickets) != 1 || tickets[0] == "" || len(tickets[0]) > 256 {
			s.previewNotFound(w, r)
			return
		}
		s.exchangePreviewTicket(w, r, id, host, query, tickets[0])
		return
	}

	sessionValue, ok := previewCookie(r)
	if !ok || s.Store == nil {
		s.previewNotFound(w, r)
		return
	}
	session, err := s.Store.GetActivePreviewSession(r.Context(), console.HashToken(sessionValue))
	if err != nil || session.SandboxID != id || session.PreviewHost != host {
		s.previewNotFound(w, r)
		return
	}
	sb, err := s.Store.Get(r.Context(), id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.previewUnavailable(w)
			return
		}
		s.previewNotFound(w, r)
		return
	}
	if !session.AdminOverride && (!sb.OwnerPrincipalID.Valid || sb.OwnerPrincipalID.String != session.PrincipalID) {
		s.previewNotFound(w, r)
		return
	}
	if session.AdminOverride {
		s.auditAction(r, audit.Entry{
			ActorKind: "user", Action: "preview.admin_override_access", Target: id,
		})
	}
	if s.PreviewRuntime == nil {
		s.previewUnavailable(w)
		return
	}
	// A preview may be a long-lived response (including upgraded browser
	// connections). Keep it visible to the reaper until proxying completes.
	if s.Inflight != nil {
		s.Inflight.Enter(id)
		defer s.Inflight.Exit(id)
	}
	var targetURL *url.URL
	if s.usesRuntimeProvider() {
		err = s.withRuntimeLease(r.Context(), id, func(ctx context.Context) error {
			current, getErr := s.Store.Get(ctx, id)
			if getErr != nil {
				return getErr
			}
			target, ensureErr := s.PreviewRuntime.EnsurePreview(ctx, s.runtimeRef(current))
			if ensureErr != nil {
				return ensureErr
			}
			targetURL, ensureErr = s.validProviderPreviewTarget(target, s.runtimeRef(current))
			if ensureErr != nil {
				return ensureErr
			}
			live, inspectErr := s.RuntimeLifecycle.Inspect(ctx, s.runtimeRef(current))
			if inspectErr != nil {
				return inspectErr
			}
			if persistErr := s.persistRuntimeState(ctx, id, live); persistErr != nil {
				return persistErr
			}
			// Keep the durable activity timestamp in the same lease as the
			// provider wake. A request which reaches its verified target must
			// not race the idle reaper into stopping that target.
			return s.Store.BumpLastActive(ctx, id, time.Now().UTC())
		})
	} else {
		target, ensureErr := s.PreviewRuntime.EnsurePreview(r.Context(), s.runtimeRef(sb))
		if ensureErr == nil {
			targetURL, ensureErr = s.validPreviewTarget(target)
		}
		err = ensureErr
	}
	if err != nil {
		s.previewUnavailable(w)
		return
	}
	if s.usesRuntimeProvider() {
		running, err := s.waitForProviderRunning(r.Context(), id)
		if err != nil {
			s.previewUnavailable(w)
			return
		}
		if s.PreviewReadiness == nil {
			// A provider preview must never optimistically proxy to a Service
			// whose ready endpoint has not been confirmed.
			s.previewUnavailable(w)
			return
		}
		if err := s.PreviewReadiness.WaitForPreviewReady(r.Context(), s.runtimeRef(running)); err != nil {
			s.previewUnavailable(w)
			return
		}
	}
	if targetURL == nil {
		s.previewUnavailable(w)
		return
	}
	s.newPreviewReverseProxy(targetURL).ServeHTTP(w, r)
}

func (s *Server) exchangePreviewTicket(w http.ResponseWriter, r *http.Request, id, host string, query url.Values, ticket string) {
	if s.Store == nil {
		s.previewUnavailable(w)
		return
	}
	value, hash, err := console.NewSessionValue()
	if err != nil {
		s.previewUnavailable(w)
		return
	}
	now := time.Now().UTC()
	session, err := s.Store.ConsumePreviewTicket(
		r.Context(), console.HashToken(ticket), hash, id, host, now.Add(previewSessionLifetime),
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.previewNotFound(w, r)
			return
		}
		s.previewUnavailable(w)
		return
	}
	maxAge := int(time.Until(session.ExpiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: previewCookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: !s.AllowInsecurePreview, SameSite: http.SameSiteLaxMode,
	})
	query.Del("preview_ticket")
	location := r.URL.EscapedPath()
	if location == "" {
		location = "/"
	}
	if encoded := query.Encode(); encoded != "" {
		location += "?" + encoded
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.auditAction(r, audit.Entry{Action: "preview.session_issued", Target: id})
	http.Redirect(w, r, location, http.StatusFound)
}

func (s *Server) previewHostFor(id string, port int) (string, bool) {
	parsed, err := url.Parse(s.previewURL(id, port))
	if err != nil || parsed.User != nil || parsed.Host == "" ||
		(parsed.Scheme != "https" && (!s.AllowInsecurePreview || parsed.Scheme != "http")) {
		return "", false
	}
	_, host, ok := s.previewHostForRequest(parsed.Host)
	return host, ok
}

func (s *Server) previewHostForRequest(rawHost string) (id, host string, ok bool) {
	rawHost = strings.ToLower(strings.TrimSpace(rawHost))
	if rawHost == "" || strings.ContainsAny(rawHost, " \t\r\n,/@") {
		return "", "", false
	}
	hostname := rawHost
	if strings.Contains(rawHost, ":") {
		var err error
		hostname, _, err = net.SplitHostPort(rawHost)
		if err != nil || hostname == "" {
			return "", "", false
		}
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s.PreviewDomain), "."))
	if domain == "" {
		return "", "", false
	}
	suffix := ".preview." + domain
	if !strings.HasSuffix(hostname, suffix) {
		return "", "", false
	}
	label := strings.TrimSuffix(hostname, suffix)
	if !strings.HasPrefix(label, "s-") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(label, "s-"), "-")
	if len(parts) != 2 || !safePreviewID(parts[0]) {
		return "", "", false
	}
	port, err := strconv.ParseUint(parts[1], 10, 16)
	if err != nil || port == 0 {
		return "", "", false
	}
	return strings.ToUpper(parts[0]), rawHost, true
}

func safePreviewID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func isHTTPSPreviewRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Traefik overwrites this header through the preview-only middleware after
	// TLS termination. X-Forwarded-* is deliberately not trusted because it is
	// client-controlled at the public edge.
	return r.Header.Get("X-Sandboxd-Preview-Gateway") == "1"
}

func previewCookie(r *http.Request) (string, bool) {
	var value string
	for _, cookie := range r.Cookies() {
		if cookie.Name != previewCookieName {
			continue
		}
		if value != "" || cookie.Value == "" || len(cookie.Value) > 256 {
			return "", false
		}
		value = cookie.Value
	}
	return value, value != ""
}

func validPreviewTarget(target runtimebackend.PreviewTarget) (*url.URL, error) {
	return validPreviewTargetForDomain(target, "cluster.local")
}

func (s *Server) validPreviewTarget(target runtimebackend.PreviewTarget) (*url.URL, error) {
	domain := s.PreviewClusterDomain
	if domain == "" {
		domain = "cluster.local"
	}
	return validPreviewTargetForDomain(target, domain)
}

// validProviderPreviewTarget binds a provider preview to the one Service the
// Kubernetes adapter creates for this persisted runtime namespace. Generic
// cluster-service validation is intentionally insufficient here: accepting a
// different Service would turn a compromised adapter response into an SSRF
// primitive inside the cluster.
func (s *Server) validProviderPreviewTarget(target runtimebackend.PreviewTarget, ref runtimebackend.SandboxRef) (*url.URL, error) {
	parsed, err := s.validPreviewTarget(target)
	if err != nil {
		return nil, err
	}
	namespace := ref.RuntimeID
	if !validProviderRuntimeNamespace(namespace) {
		return nil, errors.New("provider preview runtime namespace is invalid")
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s.PreviewClusterDomain), "."))
	if domain == "" {
		domain = "cluster.local"
	}
	expectedHost := "preview." + namespace + ".svc." + domain
	if !strings.EqualFold(parsed.Hostname(), expectedHost) ||
		parsed.Port() != strconv.Itoa(s.providerWebPort()) {
		return nil, errors.New("provider preview target does not match the sandbox service")
	}
	return parsed, nil
}

// validProviderRuntimeNamespace accepts the Kubernetes namespace form used as
// the durable runtime handle. Keeping this narrow prevents a corrupted row
// from changing the expected Service hostname structure.
func validProviderRuntimeNamespace(namespace string) bool {
	if len(namespace) == 0 || len(namespace) > 63 {
		return false
	}
	for i := range namespace {
		c := namespace[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
		if c == '-' && (i == 0 || i == len(namespace)-1) {
			return false
		}
	}
	return true
}

func validPreviewTargetForDomain(target runtimebackend.PreviewTarget, clusterDomain string) (*url.URL, error) {
	parsed, err := url.Parse(target.URL)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Host == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return nil, errors.New("invalid preview target")
	}
	host := strings.ToLower(parsed.Hostname())
	clusterDomain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(clusterDomain), "."))
	if clusterDomain == "" || !strings.HasSuffix(host, ".svc."+clusterDomain) || net.ParseIP(host) != nil {
		return nil, errors.New("preview target is not a cluster service")
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || port == 0 {
		return nil, errors.New("preview target port is invalid")
	}
	return parsed, nil
}

func (s *Server) newPreviewReverseProxy(target *url.URL) *httputil.ReverseProxy {
	transport := s.PreviewTransport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = target.Host
			request.Out.Header = sanitizedPreviewHeaders(request.In)
		},
		ModifyResponse: sanitizePreviewResponse,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			s.previewUnavailable(w)
		},
	}
}

func sanitizedPreviewHeaders(r *http.Request) http.Header {
	headers := make(http.Header)
	for name, values := range r.Header {
		if reservedPreviewHeader(name) {
			continue
		}
		headers[name] = append([]string(nil), values...)
	}
	var cookies []string
	for _, cookie := range r.Cookies() {
		if reservedPreviewCookie(cookie.Name) {
			continue
		}
		cookies = append(cookies, (&http.Cookie{Name: cookie.Name, Value: cookie.Value}).String())
	}
	if len(cookies) > 0 {
		headers.Set("Cookie", strings.Join(cookies, "; "))
	}
	return headers
}

func reservedPreviewHeader(name string) bool {
	name = strings.ToLower(name)
	if strings.HasPrefix(name, "x-forwarded-") || strings.HasPrefix(name, "x-sandbox") ||
		strings.HasPrefix(name, "x-auth") || strings.HasPrefix(name, "x-user") ||
		strings.HasPrefix(name, "x-identity") || strings.HasPrefix(name, "x-envoy") ||
		strings.HasPrefix(name, "x-traefik") {
		return true
	}
	switch name {
	case "authorization", "authentication", "proxy-authorization", "www-authenticate",
		"cookie", "forwarded", "x-real-ip", "x-api-key", "x-access-token",
		"x-bearer-token", "x-csrf-token", "x-original-uri", "x-rewrite-url",
		"referer", "preview-ticket", "ticket":
		return true
	}
	return false
}

func reservedPreviewCookie(name string) bool {
	name = strings.ToLower(name)
	if strings.HasPrefix(name, "__host-sandbox") || strings.HasPrefix(name, "__secure-sandbox") {
		return true
	}
	switch name {
	case previewCookieName, "sbx_session", "sandboxd_session", "preview_ticket",
		"auth", "authorization", "authentication", "access_token", "id_token",
		"auth_token", "session", "ticket":
		return true
	}
	return false
}

func sanitizePreviewResponse(response *http.Response) error {
	for name := range response.Header {
		if reservedPreviewHeader(name) {
			response.Header.Del(name)
		}
	}
	var allowed []string
	for _, value := range response.Header.Values("Set-Cookie") {
		name, _, found := strings.Cut(value, "=")
		if found && !reservedPreviewCookie(strings.TrimSpace(name)) {
			allowed = append(allowed, value)
		}
	}
	response.Header.Del("Set-Cookie")
	for _, value := range allowed {
		response.Header.Add("Set-Cookie", value)
	}
	response.Header.Del("Server")
	response.Header.Del("X-Powered-By")
	response.Header.Set("X-Content-Type-Options", "nosniff")
	response.Header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	response.Header.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	return nil
}

func (s *Server) previewNotFound(w http.ResponseWriter, r *http.Request) {
	s.auditAction(r, audit.Entry{Action: "preview.access_denied"})
	http.NotFound(w, r)
}

func (s *Server) previewUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "preview unavailable", http.StatusServiceUnavailable)
}
