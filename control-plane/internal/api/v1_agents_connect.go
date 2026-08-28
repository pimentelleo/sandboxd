// v1_agents_connect.go — Phase 10B: connect a coding-agent provider by one of
// two owner-supplied methods, generalized across the registry (opencode,
// claude-code, codex):
//
//   - IMPORT (subscription / OAuth): the owner runs `<cli> login` on their own
//     machine and pastes the resulting credential bundle. It is stored verbatim
//     at the provider's HOME-relative credential file — opaque, never parsed.
//   - API KEY: the owner pastes a provider API key. It is stored opaquely in the
//     provider's key file; at task time runtimed injects it as the provider's
//     one allowlisted key env var (see agentauth.APIKeyEnv).
//
// Each connect fully replaces the provider's auth dir, so a provider holds
// exactly one method at a time. No token is ever logged or returned.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/agentauth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/copilot"
)

// provider validates {provider} against the registry, returning it and writing a
// 404 when unknown. Guards every connect endpoint.
func (s *Server) agentProvider(w http.ResponseWriter, r *http.Request) (agentauth.Provider, bool) {
	if s.AgentAuth == nil {
		writeErr(w, http.StatusServiceUnavailable, "agent auth not configured")
		return agentauth.Provider{}, false
	}
	p, ok := agentauth.Get(r.PathValue("provider"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown provider")
		return agentauth.Provider{}, false
	}
	return p, true
}

// POST /v1/agents/{provider}/import
// Body: {"credentials": "<contents of the CLI's login credential file>"}.
// Stored verbatim (opaque) at the provider's credential file; never parsed.
func (s *Server) v1AgentImport(w http.ResponseWriter, r *http.Request) {
	p, ok := s.agentProvider(w, r)
	if !ok {
		return
	}
	rel, ok := agentauth.CredentialFile(p.ID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "provider does not support credential import")
		return
	}
	var body struct {
		Credentials string `json:"credentials"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&body); err != nil || body.Credentials == "" {
		writeErr(w, http.StatusBadRequest, "missing credentials")
		return
	}
	if err := s.AgentAuth.ImportCredential(p.ID, rel, []byte(body.Credentials)); err != nil {
		writeErr(w, http.StatusBadRequest, "could not import credentials")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": p.ID, "status": "connected", "method": "oauth"})
}

// POST /v1/agents/{provider}/api-key
// Body: {"api_key": "<provider api key>"}. Stored opaquely; runtimed injects it
// as the provider's key env var at task time.
func (s *Server) v1AgentAPIKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.agentProvider(w, r)
	if !ok {
		return
	}
	if _, ok := agentauth.APIKeyEnv(p.ID); !ok {
		writeErr(w, http.StatusBadRequest, "provider does not support API-key auth")
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil || body.APIKey == "" {
		writeErr(w, http.StatusBadRequest, "missing api_key")
		return
	}
	if err := s.AgentAuth.ImportCredential(p.ID, agentauth.APIKeyFile, []byte(body.APIKey)); err != nil {
		writeErr(w, http.StatusBadRequest, "could not store api key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": p.ID, "status": "connected", "method": "api_key"})
}

// POST /v1/agents/claude-code/oauth/start — returns the authorize URL the user
// opens in a browser. They approve and paste the resulting code back to /finish.
func (s *Server) v1AgentOAuthStart(w http.ResponseWriter, _ *http.Request) {
	if s.AgentOAuth == nil {
		writeErr(w, http.StatusServiceUnavailable, "oauth not configured")
		return
	}
	authURL, err := s.AgentOAuth.Start()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start login")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authorize_url": authURL})
}

// POST /v1/agents/claude-code/oauth/finish — body {"code":"<code#state>"}.
// Exchanges the code for tokens and stores them; the value is never echoed.
func (s *Server) v1AgentOAuthFinish(w http.ResponseWriter, r *http.Request) {
	if s.AgentOAuth == nil {
		writeErr(w, http.StatusServiceUnavailable, "oauth not configured")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil || body.Code == "" {
		writeErr(w, http.StatusBadRequest, "missing code")
		return
	}
	if err := s.AgentOAuth.Finish(body.Code); err != nil {
		writeErr(w, http.StatusBadRequest, "could not complete login: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": "claude-code", "status": "connected", "method": "oauth"})
}

// POST /v1/agents/github-copilot/pat stores a fine-grained GitHub personal
// access token in the control plane. It is write-only and is never mounted in
// a sandbox.
func (s *Server) v1GitHubCopilotPAT(w http.ResponseWriter, r *http.Request) {
	if s.Copilot == nil {
		writeErr(w, http.StatusServiceUnavailable, "GitHub Copilot is unavailable")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Token == "" {
		writeErr(w, http.StatusBadRequest, "missing token")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	status, err := s.Copilot.ConnectPAT(r.Context(), body.Token)
	if errors.Is(err, copilot.ErrInvalidPAT) {
		writeErr(w, http.StatusBadRequest, "a fine-grained GitHub personal access token is required")
		return
	}
	if errors.Is(err, copilot.ErrCredentialChanged) {
		writeErr(w, http.StatusConflict, "GitHub Copilot connection changed; retry")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not validate GitHub Copilot token")
		return
	}
	if coordinator := s.conversationCoordinator(); coordinator != nil {
		coordinator.InterruptAll(r.Context(),
			"Copilot was interrupted because the GitHub Copilot credential changed.")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": "github-copilot", "status": "connected",
		"method": status.Method, "account": status.Account,
	})
}

// GET /v1/agents/github-copilot/models exposes the connected account's
// allowlisted model catalog. It never returns tokens, SDK state, billing, or
// policy metadata.
func (s *Server) v1GitHubCopilotModels(w http.ResponseWriter, r *http.Request) {
	if s.Copilot == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "GitHub Copilot is unavailable")
		return
	}
	models, err := s.Copilot.ListModels(r.Context())
	switch {
	case errors.Is(err, copilot.ErrNotConnected), errors.Is(err, copilot.ErrCredentialChanged):
		writeV1Err(w, http.StatusConflict, "agent_not_connected", "GitHub Copilot is not connected")
	case errors.Is(err, copilot.ErrModelCatalogUnavailable):
		writeV1Err(w, http.StatusServiceUnavailable, "model_catalog_unavailable", "Copilot model choices are temporarily unavailable")
	case err != nil:
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not load Copilot model choices")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"models": models})
	}
}

// POST /v1/agents/{provider}/disconnect — deletes the stored auth dir.
func (s *Server) v1AgentDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("provider") == "github-copilot" {
		if s.Copilot == nil {
			writeErr(w, http.StatusServiceUnavailable, "GitHub Copilot is unavailable")
			return
		}
		if err := s.Copilot.Disconnect(); err != nil {
			writeErr(w, http.StatusInternalServerError, "could not disconnect GitHub Copilot")
			return
		}
		if coordinator := s.conversationCoordinator(); coordinator != nil {
			coordinator.InterruptAll(r.Context(),
				"Copilot was interrupted because the GitHub Copilot credential was disconnected.")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p, ok := s.agentProvider(w, r)
	if !ok {
		return
	}
	if err := s.AgentAuth.Delete(p.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not disconnect")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
