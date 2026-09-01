package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// Authorization is deliberately enforced at the route boundary. Handlers retain
// their owner-token queries for legacy local installs, while this policy binds
// principal-backed requests to an immutable principal before any handler can
// inspect a resource.
type authorizationContext struct {
	principalID string
	ownerToken  string
	admin       bool
}

type authorizationContextKey struct{}

func authorizationFrom(ctx context.Context) authorizationContext {
	v, _ := ctx.Value(authorizationContextKey{}).(authorizationContext)
	return v
}

func withAuthorization(r *http.Request, a authorizationContext) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authorizationContextKey{}, a))
}

func (s *Server) productionAuthorization() bool {
	cfg := s.authCfg()
	return cfg.Profile == auth.ProfileEntra || cfg.LocalAccountsReady()
}

func actorOwnerToken(r *http.Request) string {
	return auth.ActorFrom(r.Context()).Name
}

func principalID(r *http.Request) string {
	return authorizationFrom(r.Context()).principalID
}

func requestIsAdmin(r *http.Request) bool {
	return authorizationFrom(r.Context()).admin
}

func (s *Server) authorizeRequest(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if !s.productionAuthorization() || authzExemptPath(r.URL.Path) {
		return r, true
	}

	actor := auth.ActorFrom(r.Context())
	if actor.Kind != "user" || actor.PrincipalID == "" || actor.Name == "" {
		s.writeAuthorizationError(w, r, http.StatusUnauthorized, "unauthorized")
		return r, false
	}
	cfg := s.authCfg()
	if cfg.Profile == auth.ProfileEntra {
		principal := actor.Principal
		if principal == nil || principal.OID == "" || principal.TenantID == "" ||
			actor.Name != principal.Subject() || cfg.Entra.TenantID != principal.TenantID {
			s.writeAuthorizationError(w, r, http.StatusUnauthorized, "unauthorized")
			return r, false
		}
	}
	admin := actor.HasRole(auth.RoleAdmin)
	if !admin && !actor.HasRole(auth.RoleUser) {
		s.writeAuthorizationError(w, r, http.StatusForbidden, "forbidden")
		return r, false
	}
	id := actor.PrincipalID
	if cfg.Profile == auth.ProfileEntra {
		var err error
		id, err = s.principalForActor(r.Context(), *actor.Principal)
		if err != nil {
			s.writeAuthorizationError(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return r, false
		}
	}
	adminOnly := authzAdminOnlyPath(cfg, r.URL.Path)
	if adminOnly && !admin {
		s.writeAuthorizationError(w, r, http.StatusForbidden, "forbidden")
		return r, false
	}

	// Global administrator controls do not need an ownership lookup. This keeps
	// settings available even during an identity-store outage, without allowing
	// ordinary users to bypass it.
	if adminOnly {
		return withAuthorization(r, authorizationContext{admin: true}), true
	}

	if s.Store == nil {
		s.writeAuthorizationError(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return r, false
	}
	r = withAuthorization(r, authorizationContext{principalID: id, admin: admin})

	if appID := authorizationAppID(r); appID != "" {
		app, err := s.Store.GetApp(r.Context(), appID)
		if errors.Is(err, store.ErrNotFound) {
			s.writeAuthorizationNotFound(w, r)
			return r, false
		}
		if err != nil {
			s.writeAuthorizationError(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return r, false
		}
		if !admin && (!app.OwnerPrincipalID.Valid || app.OwnerPrincipalID.String != id) {
			s.writeAuthorizationNotFound(w, r)
			return r, false
		}
		a := authorizationFrom(r.Context())
		a.ownerToken = app.OwnerToken
		r = withAuthorization(r, a)
		return r, true
	}

	if taskID := authorizationTaskID(r); taskID != "" {
		task, err := s.Store.GetTask(r.Context(), taskID)
		if errors.Is(err, store.ErrNotFound) {
			s.writeAuthorizationNotFound(w, r)
			return r, false
		}
		if err != nil {
			s.writeAuthorizationError(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return r, false
		}
		return s.authorizeSandboxRequest(w, r, task.SandboxID)
	}

	if snapshotID := authorizationSnapshotID(r); snapshotID != "" {
		snap, err := s.Store.GetSnapshot(r.Context(), snapshotID)
		if errors.Is(err, store.ErrNotFound) {
			s.writeAuthorizationNotFound(w, r)
			return r, false
		}
		if err != nil {
			s.writeAuthorizationError(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return r, false
		}
		if !admin && (!snap.OwnerPrincipalID.Valid || snap.OwnerPrincipalID.String != id) {
			s.writeAuthorizationNotFound(w, r)
			return r, false
		}
		a := authorizationFrom(r.Context())
		a.ownerToken = snap.OwnerToken
		return withAuthorization(r, a), true
	}

	if credentialID := authorizationGitCredentialID(r); credentialID != "" {
		var owner string
		err := s.Store.DB().QueryRowContext(r.Context(),
			s.authorizationQuery(`SELECT owner_token FROM git_credential WHERE id = ?`), credentialID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			s.writeAuthorizationNotFound(w, r)
			return r, false
		}
		if err != nil {
			s.writeAuthorizationError(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return r, false
		}
		if !admin && owner != actorOwnerToken(r) {
			s.writeAuthorizationNotFound(w, r)
			return r, false
		}
		a := authorizationFrom(r.Context())
		a.ownerToken = owner
		return withAuthorization(r, a), true
	}

	if sandboxID := authorizationSandboxID(r); sandboxID != "" {
		return s.authorizeSandboxRequest(w, r, sandboxID)
	}
	return r, true
}

func (s *Server) authorizeSandboxRequest(w http.ResponseWriter, r *http.Request, sandboxID string) (*http.Request, bool) {
	a := authorizationFrom(r.Context())
	sb, err := s.Store.Get(r.Context(), sandboxID)
	if err == nil {
		if !a.admin && (!sb.OwnerPrincipalID.Valid || sb.OwnerPrincipalID.String != a.principalID) {
			s.writeAuthorizationNotFound(w, r)
			return r, false
		}
		a.ownerToken = s.sandboxOwnerToken(r.Context(), sb)
		return withAuthorization(r, a), true
	}
	if !errors.Is(err, store.ErrNotFound) {
		s.writeAuthorizationError(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return r, false
	}

	// Legacy workspace snapshot operations intentionally remain available after
	// their sandbox row is deleted. The durable workspace principal binding is
	// the ownership source for that narrow case.
	owner, err := s.Store.GetWorkspacePrincipalOwner(r.Context(), sandboxID)
	if errors.Is(err, store.ErrNotFound) || (!a.admin && owner != a.principalID) {
		s.writeAuthorizationNotFound(w, r)
		return r, false
	}
	if err != nil {
		s.writeAuthorizationError(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return r, false
	}
	return r, true
}

func (s *Server) sandboxOwnerToken(ctx context.Context, sb *store.Sandbox) string {
	if sb.AppID.Valid {
		if app, err := s.Store.GetApp(ctx, sb.AppID.String); err == nil {
			return app.OwnerToken
		}
	}
	if sb.ExternalUserID.Valid {
		return sb.ExternalUserID.String
	}
	return ""
}

func (s *Server) principalForActor(ctx context.Context, p auth.Principal) (string, error) {
	stored, err := s.Store.GetPrincipal(ctx, "entra", p.TenantID, p.OID)
	if err == nil {
		return stored.ID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return s.upsertEntraPrincipal(ctx, p)
}

func (s *Server) creationOwner(r *http.Request, appID string) (principal, ownerToken string, err error) {
	principal = principalID(r)
	if principal == "" {
		return "", "", store.ErrNotFound
	}
	if appID == "" {
		return principal, actorOwnerToken(r), nil
	}
	app, err := s.Store.GetApp(r.Context(), appID)
	if err != nil {
		return "", "", err
	}
	if !requestIsAdmin(r) && (!app.OwnerPrincipalID.Valid || app.OwnerPrincipalID.String != principal) {
		return "", "", store.ErrNotFound
	}
	if !app.OwnerPrincipalID.Valid {
		return "", "", store.ErrNotFound
	}
	return app.OwnerPrincipalID.String, app.OwnerToken, nil
}

func (s *Server) writeCreationAuthorizationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.writeAuthorizationNotFound(w, r)
		return
	}
	s.writeAuthorizationError(w, r, http.StatusServiceUnavailable, "authorization unavailable")
}

func (s *Server) creationExternalUserID(r *http.Request, supplied string) string {
	if s.productionAuthorization() {
		return actorOwnerToken(r)
	}
	return supplied
}

func (s *Server) appsForRequest(r *http.Request) ([]*store.App, error) {
	externalUserID := r.URL.Query().Get("external_user_id")
	if !s.productionAuthorization() {
		return s.Store.ListAppsForOwner(r.Context(), tenantToken(r), externalUserID)
	}

	var apps []*store.App
	var err error
	if requestIsAdmin(r) {
		rows, err := s.Store.DB().QueryContext(r.Context(), `SELECT id FROM app ORDER BY created_at DESC`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			app, err := s.Store.GetApp(r.Context(), id)
			if err != nil {
				return nil, err
			}
			apps = append(apps, app)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	} else {
		apps, err = s.Store.ListAppsForPrincipal(r.Context(), principalID(r))
		if err != nil {
			return nil, err
		}
	}
	if externalUserID == "" {
		return apps, nil
	}
	filtered := make([]*store.App, 0, len(apps))
	for _, app := range apps {
		if app.ExternalUserID.Valid && app.ExternalUserID.String == externalUserID {
			filtered = append(filtered, app)
		}
	}
	return filtered, nil
}

func (s *Server) snapshotsForRequest(r *http.Request) ([]*store.Snapshot, error) {
	if !s.productionAuthorization() {
		return s.Store.ListSnapshotsByOwner(r.Context(), tenantToken(r))
	}
	var snapshots []*store.Snapshot
	if requestIsAdmin(r) {
		rows, err := s.Store.DB().QueryContext(r.Context(), `SELECT id FROM snapshot ORDER BY created_at DESC`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			snapshot, err := s.Store.GetSnapshot(r.Context(), id)
			if err != nil {
				return nil, err
			}
			snapshots = append(snapshots, snapshot)
		}
		return snapshots, rows.Err()
	}
	rows, err := s.Store.DB().QueryContext(r.Context(),
		s.authorizationQuery(`SELECT id FROM snapshot WHERE owner_principal_id = ? ORDER BY created_at DESC`),
		principalID(r))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		snapshot, err := s.Store.GetSnapshot(r.Context(), id)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

func (s *Server) appSnapshotsForRequest(r *http.Request, appID string) ([]*store.Snapshot, error) {
	if !s.productionAuthorization() || !requestIsAdmin(r) {
		if !s.productionAuthorization() {
			return s.Store.ListSnapshotsByApp(r.Context(), tenantToken(r), appID)
		}
		rows, err := s.Store.DB().QueryContext(r.Context(), s.authorizationQuery(
			`SELECT id FROM snapshot WHERE source_app_id = ? AND owner_principal_id = ? ORDER BY created_at DESC`),
			appID, principalID(r))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var snapshots []*store.Snapshot
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			snapshot, err := s.Store.GetSnapshot(r.Context(), id)
			if err != nil {
				return nil, err
			}
			snapshots = append(snapshots, snapshot)
		}
		return snapshots, rows.Err()
	}
	rows, err := s.Store.DB().QueryContext(r.Context(),
		`SELECT id FROM snapshot WHERE source_app_id = ? ORDER BY created_at DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []*store.Snapshot
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		snapshot, err := s.Store.GetSnapshot(r.Context(), id)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

func (s *Server) gitCredentialsForRequest(r *http.Request) ([]*store.GitCredential, error) {
	if !s.productionAuthorization() || !requestIsAdmin(r) {
		return s.Store.ListGitCredentials(r.Context(), actorOwnerToken(r))
	}
	rows, err := s.Store.DB().QueryContext(r.Context(), `SELECT DISTINCT owner_token FROM git_credential`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var credentials []*store.GitCredential
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owned, err := s.Store.ListGitCredentials(r.Context(), owner)
		if err != nil {
			return nil, err
		}
		credentials = append(credentials, owned...)
	}
	return credentials, rows.Err()
}

func (s *Server) authorizeSnapshotTemplate(r *http.Request, imagePath string) error {
	if !s.productionAuthorization() || imagePath == "" {
		return nil
	}
	var id string
	err := s.Store.DB().QueryRowContext(r.Context(),
		s.authorizationQuery(`SELECT id FROM snapshot WHERE image_path = ?`), imagePath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = s.snapshotForTenant(r, id)
	return err
}

func (s *Server) authorizationQuery(query string) string {
	return store.BindQuery(s.Store.Provider(), query)
}

func (s *Server) writeAuthorizationError(w http.ResponseWriter, r *http.Request, code int, message string) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		writeV1Err(w, code, map[int]string{http.StatusUnauthorized: "unauthorized", http.StatusForbidden: "forbidden", http.StatusServiceUnavailable: "unavailable"}[code], message)
		return
	}
	writeErr(w, code, message)
}

func (s *Server) writeAuthorizationNotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		writeV1Err(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func authzExemptPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/llm.txt", "/preview-auth", "/forward-auth",
		"/v1/auth/status", "/v1/auth/login", "/v1/auth/setup",
		"/v1/auth/entra/login", "/v1/auth/entra/callback":
		return true
	}
	return false
}

func authzAdminOnlyPath(cfg *auth.Config, path string) bool {
	apiKeysEnabled := cfg == nil || cfg.LocalAuthMode != auth.LocalAuthModeAccounts
	return path == "/v1/settings" || strings.HasPrefix(path, "/v1/agents/") ||
		path == "/v1/agents" || strings.HasPrefix(path, "/v1/upgrade") ||
		(apiKeysEnabled && (path == "/v1/api-keys" || strings.HasPrefix(path, "/v1/api-keys/"))) ||
		path == "/v1/auth/accounts" ||
		strings.HasPrefix(path, "/external-users/") || strings.HasPrefix(path, "/external-projects/") ||
		strings.HasSuffix(path, "/claim")
}

func authorizationAppID(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/v1/apps/") {
		return r.PathValue("id")
	}
	return ""
}

func authorizationSandboxID(r *http.Request) string {
	switch {
	case strings.HasPrefix(r.URL.Path, "/sandbox/"):
		return r.PathValue("id")
	case strings.HasPrefix(r.URL.Path, "/wake/"):
		return r.PathValue("id")
	case strings.HasPrefix(r.URL.Path, "/v1/sandboxes/"):
		return r.PathValue("id")
	}
	return ""
}

func authorizationSnapshotID(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/v1/snapshots/") {
		return r.PathValue("id")
	}
	return ""
}

func authorizationTaskID(r *http.Request) string {
	if r.URL.Path == "/v1/tasks/"+r.PathValue("id")+"/events" {
		return r.PathValue("id")
	}
	return ""
}

func authorizationGitCredentialID(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/v1/git-credentials/") {
		return r.PathValue("id")
	}
	return ""
}
