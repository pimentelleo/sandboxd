package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func TestEntraAuthorizationOwnershipAndAdministratorOverride(t *testing.T) {
	s := productionAuthorizationServer(t)
	owner := testEntraPrincipal("owner", auth.RoleUser)
	other := testEntraPrincipal("other", auth.RoleUser)
	admin := testEntraPrincipal("admin", auth.RoleAdmin)
	noRole := testEntraPrincipal("no-role", "")
	ownerID := seedAuthorizationPrincipal(t, s, owner)
	otherID := seedAuthorizationPrincipal(t, s, other)
	ownerApp := seedAuthorizationApp(t, s, "owner-app", owner, ownerID)
	otherApp := seedAuthorizationApp(t, s, "other-app", other, otherID)

	cases := []struct {
		name      string
		principal *auth.Principal
		path      string
		want      int
	}{
		{"owner can get own app", &owner, "/v1/apps/" + ownerApp.ID, http.StatusOK},
		{"other owner cannot get app", &other, "/v1/apps/" + ownerApp.ID, http.StatusNotFound},
		{"administrator can get all apps", &admin, "/v1/apps/" + ownerApp.ID, http.StatusOK},
		{"unauthenticated actor is rejected", nil, "/v1/apps/" + ownerApp.ID, http.StatusUnauthorized},
		{"actor without a sandboxd role is rejected", &noRole, "/v1/apps/" + ownerApp.ID, http.StatusForbidden},
		{"other app remains hidden", &owner, "/v1/apps/" + otherApp.ID, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveAuthorizationRequest(s, http.MethodGet, tc.path, "", tc.principal)
			if w.Code != tc.want {
				t.Fatalf("status = %d; want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestEntraAuthorizationFiltersListsAndIgnoresRequestedOwner(t *testing.T) {
	s := productionAuthorizationServer(t)
	owner := testEntraPrincipal("owner", auth.RoleUser)
	other := testEntraPrincipal("other", auth.RoleUser)
	admin := testEntraPrincipal("admin", auth.RoleAdmin)
	ownerID := seedAuthorizationPrincipal(t, s, owner)
	otherID := seedAuthorizationPrincipal(t, s, other)
	seedAuthorizationApp(t, s, "owner-app", owner, ownerID)
	seedAuthorizationApp(t, s, "other-app", other, otherID)

	cases := []struct {
		name      string
		principal auth.Principal
		path      string
		want      int
	}{
		{"owner sees only own apps", owner, "/v1/apps", 1},
		{"requested foreign external owner cannot expand list", owner, "/v1/apps?external_user_id=" + other.Subject(), 0},
		{"administrator sees all apps", admin, "/v1/apps", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveAuthorizationRequest(s, http.MethodGet, tc.path, "", &tc.principal)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
			}
			var body struct {
				Apps []v1App `json:"apps"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Apps) != tc.want {
				t.Fatalf("apps = %d; want %d; body=%s", len(body.Apps), tc.want, w.Body.String())
			}
		})
	}

	w := serveAuthorizationRequest(s, http.MethodPost, "/v1/apps",
		`{"name":"created","external_user_id":"`+other.Subject()+`"}`, &owner)
	if w.Code != http.StatusCreated {
		t.Fatalf("create app = %d; body=%s", w.Code, w.Body.String())
	}
	var created v1App
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	app, err := s.Store.GetApp(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if app.OwnerPrincipalID.String != ownerID || app.ExternalUserID.String != owner.Subject() {
		t.Fatalf("externally supplied owner was trusted: %#v", app)
	}
}

func TestEntraAuthorizationRestrictsGlobalSettings(t *testing.T) {
	s := productionAuthorizationServer(t)
	user := testEntraPrincipal("user", auth.RoleUser)
	admin := testEntraPrincipal("admin", auth.RoleAdmin)

	cases := []struct {
		name      string
		principal auth.Principal
		want      int
	}{
		{"user cannot read global settings", user, http.StatusForbidden},
		{"administrator can read global settings", admin, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveAuthorizationRequest(s, http.MethodGet, "/v1/settings", "", &tc.principal)
			if w.Code != tc.want {
				t.Fatalf("status = %d; want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func productionAuthorizationServer(t *testing.T) *Server {
	t.Helper()
	st := memStore(t)
	t.Cleanup(func() { _ = st.Close() })
	return &Server{
		Store: st,
		Auth:  auth.NewMiddleware(&auth.Config{Profile: auth.ProfileEntra, Entra: auth.EntraConfig{TenantID: "tenant"}}, nil, nil, nil),
	}
}

func testEntraPrincipal(oid string, role auth.Role) auth.Principal {
	p := auth.Principal{OID: oid, TenantID: "tenant"}
	if role != "" {
		p.Roles = []auth.Role{role}
	}
	return p
}

func seedAuthorizationPrincipal(t *testing.T, s *Server, principal auth.Principal) string {
	t.Helper()
	p := &store.Principal{
		ID:       "principal-" + principal.OID,
		Provider: "entra",
		TenantID: principal.TenantID,
		Subject:  principal.OID,
	}
	if err := s.Store.UpsertPrincipal(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func seedAuthorizationApp(t *testing.T, s *Server, id string, principal auth.Principal, principalID string) *store.App {
	t.Helper()
	app := &store.App{
		ID:               id,
		Name:             id,
		OwnerToken:       principal.Subject(),
		OwnerPrincipalID: nullStr(principalID),
		ExternalUserID:   nullStr(principal.Subject()),
	}
	if err := s.Store.CreateApp(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	return app
}

func serveAuthorizationRequest(s *Server, method, path, body string, principal *auth.Principal) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, reader)
	if principal != nil {
		r = r.WithContext(auth.WithActor(r.Context(), auth.Actor{
			Kind:        "user",
			Name:        principal.Subject(),
			PrincipalID: "principal-" + principal.OID,
			Principal:   principal,
		}))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
