// v1_api_keys.go — programmatic API keys managed from the console. Each key is
// shown once at creation (its sha256 is stored, never the plaintext) and, in the
// default single-tenant deployment, authenticates as the shared tenant. Local
// console users may manage keys; Entra requires an administrator because that
// pool is globally scoped. A leaked key cannot mint more keys.
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

type v1APIKey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

func toV1APIKey(k *store.APIKey) v1APIKey {
	out := v1APIKey{
		ID:        k.ID,
		Name:      k.Name,
		Prefix:    k.Prefix,
		CreatedAt: k.CreatedAt.Format(time.RFC3339),
	}
	if k.LastUsedAt != nil {
		out.LastUsedAt = k.LastUsedAt.Format(time.RFC3339)
	}
	return out
}

// requireAPIKeyManager keeps local console behavior unchanged while preventing
// a tenant-scoped Entra user from managing the globally scoped API-key pool.
func (s *Server) requireAPIKeyManager(w http.ResponseWriter, r *http.Request) bool {
	if s.authCfg().LocalAuthMode == auth.LocalAuthModeAccounts {
		writeV1Err(w, http.StatusNotFound, "not_found", "API keys are not available with local account authentication")
		return false
	}
	actor := auth.ActorFrom(r.Context())
	if actor.Kind != "user" {
		writeV1Err(w, http.StatusForbidden, "forbidden", "API key management requires a console login")
		return false
	}
	if s.authCfg().Profile == auth.ProfileEntra &&
		(actor.Principal == nil || !actor.Principal.HasRole(auth.RoleAdmin)) {
		writeV1Err(w, http.StatusForbidden, "forbidden", "API key management requires an administrator console login")
		return false
	}
	return true
}

// POST /v1/api-keys {name} — mint a key; returns the plaintext ONCE.
func (s *Server) v1CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	if !s.requireAPIKeyManager(w, r) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if !validLabel(name, 64) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "name must be 1-64 printable characters")
		return
	}
	plain, hash, prefix, err := console.NewToken()
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not generate key")
		return
	}
	id := newULID()
	if err := s.Store.CreateAPIKey(r.Context(), id, name, hash, prefix, time.Now().Unix()); err != nil {
		if err == store.ErrConflict {
			writeV1Err(w, http.StatusConflict, "conflict", "a key with that name already exists")
			return
		}
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not store key")
		return
	}
	s.auditAPIKey(r, "api_key.created", id, name)
	// The plaintext key is returned exactly once — it cannot be retrieved again.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": name, "prefix": prefix, "key": plain,
	})
}

// GET /v1/api-keys — list metadata (never the hash or plaintext).
func (s *Server) v1ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	if !s.requireAPIKeyManager(w, r) {
		return
	}
	keys, err := s.Store.ListAPIKeys(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not list keys")
		return
	}
	out := make([]v1APIKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, toV1APIKey(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// DELETE /v1/api-keys/{id} — revoke a key.
func (s *Server) v1DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	if !s.requireAPIKeyManager(w, r) {
		return
	}
	id := r.PathValue("id")
	deleted, err := s.Store.DeleteAPIKey(r.Context(), id)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not delete key")
		return
	}
	if !deleted {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	s.auditAPIKey(r, "api_key.revoked", id, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditAPIKey(r *http.Request, action, id, name string) {
	if s.Audit == nil {
		return
	}
	actor := auth.ActorFrom(r.Context())
	detail := map[string]any{}
	if name != "" {
		detail["name"] = name
	}
	s.Audit.Write(r.Context(), audit.Entry{
		ActorKind: actor.Kind,
		ActorName: actor.Name,
		Action:    action,
		Target:    id,
		Detail:    detail,
	})
}
