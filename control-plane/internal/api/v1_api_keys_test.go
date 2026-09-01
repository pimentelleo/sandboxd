package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
)

func TestAPIKeysCRUD(t *testing.T) {
	h, _ := authTestServer(t)
	// establish a console session
	w := doAuth(t, h, "POST", "/v1/auth/setup", `{"password":"correcthorse"}`, "", "")
	if w.Code != 204 {
		t.Fatalf("setup: %d", w.Code)
	}

	cookie := sessionFrom(t, w)

	// create → 201 with plaintext key + prefix
	w = doAuth(t, h, "POST", "/v1/api-keys", `{"name":"ci-bot"}`, cookie, "")
	if w.Code != 201 {
		t.Fatalf("create: want 201 got %d %s", w.Code, w.Body.String())
	}
	var created struct{ ID, Name, Prefix, Key string }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, "sk_") || created.Prefix == "" || created.ID == "" {
		t.Fatalf("create payload: %+v", created)
	}

	// list → metadata only, no key/hash
	w = doAuth(t, h, "GET", "/v1/api-keys", "", cookie, "")
	if w.Code != 200 {
		t.Fatalf("list: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "ci-bot") || strings.Contains(body, created.Key) {
		t.Fatalf("list leaked or missing: %s", body)
	}

	// the created key authenticates as a SERVICE actor → cannot mint keys (403)
	if w := doAuth(t, h, "POST", "/v1/api-keys", `{"name":"evil"}`, "", created.Key); w.Code != 403 {
		t.Fatalf("service actor minting key: want 403 got %d", w.Code)
	}
	// but the created key DOES authorize an ordinary protected route
	if w := doAuth(t, h, "GET", "/v1/apps", "", "", created.Key); w.Code == 401 {
		t.Fatalf("valid api key rejected on /v1/apps: %d", w.Code)
	}

	// duplicate name → 409
	if w := doAuth(t, h, "POST", "/v1/api-keys", `{"name":"ci-bot"}`, cookie, ""); w.Code != 409 {
		t.Fatalf("dup name: want 409 got %d", w.Code)
	}

	// delete → 204, then 404
	if w := doAuth(t, h, "DELETE", "/v1/api-keys/"+created.ID, "", cookie, ""); w.Code != 204 {
		t.Fatalf("delete: want 204 got %d", w.Code)
	}
	if w := doAuth(t, h, "DELETE", "/v1/api-keys/"+created.ID, "", cookie, ""); w.Code != 404 {
		t.Fatalf("re-delete: want 404 got %d", w.Code)
	}
	// the revoked key no longer authenticates
	if w := doAuth(t, h, "GET", "/v1/apps", "", "", created.Key); w.Code != 401 {
		t.Fatalf("revoked key still works: %d", w.Code)
	}
}

func TestEntraAPIKeysRequireAdministrator(t *testing.T) {
	userHandler, userServer, userVerifier := newEntraAuthTestServer(t, []string{string(auth.RoleUser)})
	userCookie := completeEntraLogin(t, userHandler, userVerifier)

	_, hash, prefix, err := console.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := userServer.Store.CreateAPIKey(context.Background(), "global-key", "global", hash, prefix, 1); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/api-keys", ""},
		{http.MethodPost, "/v1/api-keys", `{"name":"user-key"}`},
		{http.MethodDelete, "/v1/api-keys/global-key", ""},
	} {
		if w := doAuthWithCookie(t, userHandler, request.method, request.path, request.body,
			auth.EntraSessionCookie, userCookie, ""); w.Code != http.StatusForbidden {
			t.Fatalf("ordinary user %s %s: want 403 got %d %s", request.method, request.path, w.Code, w.Body.String())
		}
	}
	keys, err := userServer.Store.ListAPIKeys(context.Background())
	if err != nil || len(keys) != 1 || keys[0].ID != "global-key" {
		t.Fatalf("ordinary user changed global API keys: %#v, %v", keys, err)
	}

	adminHandler, _, adminVerifier := newEntraAuthTestServer(t, []string{string(auth.RoleAdmin)})
	adminCookie := completeEntraLogin(t, adminHandler, adminVerifier)
	created := doAuthWithCookie(t, adminHandler, http.MethodPost, "/v1/api-keys", `{"name":"admin-key"}`,
		auth.EntraSessionCookie, adminCookie, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("administrator create: want 201 got %d %s", created.Code, created.Body.String())
	}
	var adminKey v1APIKey
	if err := json.Unmarshal(created.Body.Bytes(), &adminKey); err != nil || adminKey.ID == "" {
		t.Fatalf("administrator create payload = %s, %v", created.Body.String(), err)
	}
	if w := doAuthWithCookie(t, adminHandler, http.MethodGet, "/v1/api-keys", "",
		auth.EntraSessionCookie, adminCookie, ""); w.Code != http.StatusOK {
		t.Fatalf("administrator list: want 200 got %d %s", w.Code, w.Body.String())
	}
	if w := doAuthWithCookie(t, adminHandler, http.MethodDelete, "/v1/api-keys/"+adminKey.ID, "",
		auth.EntraSessionCookie, adminCookie, ""); w.Code != http.StatusNoContent {
		t.Fatalf("administrator delete: want 204 got %d %s", w.Code, w.Body.String())
	}
}
