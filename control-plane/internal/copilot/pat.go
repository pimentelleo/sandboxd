package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ConnectPAT verifies and stores a fine-grained GitHub personal access token.
// Replacing a token removes all retained state so no task can continue under
// the prior credential.
func (m *Manager) ConnectPAT(ctx context.Context, rawToken string) (Status, error) {
	token := strings.TrimSpace(rawToken)
	if !isFineGrainedPAT(token) {
		return Status{}, ErrInvalidPAT
	}

	m.mu.Lock()
	generation := m.credentialGeneration
	m.mu.Unlock()

	account, err := m.githubAccount(ctx, token)
	if err != nil {
		return Status{}, errors.New("unable to verify GitHub personal access token")
	}

	m.mu.Lock()
	if m.credentialGeneration != generation {
		m.mu.Unlock()
		return Status{}, ErrCredentialChanged
	}
	credential := &credential{PersonalAccessToken: token, Account: account}
	previousCredential := m.credential
	previousSessions := m.sessions
	previousCapabilities := m.capabilities
	previousActive := m.active
	previousActiveConversations := m.activeConversations
	m.credential = credential
	m.sessions = make(map[string]string)
	m.capabilities = make(map[string]capability)
	m.active = make(map[string]activeTask)
	m.activeConversations = make(map[string]activeTask)
	if err := m.persistLocked(); err != nil {
		m.credential = previousCredential
		m.sessions = previousSessions
		m.capabilities = previousCapabilities
		m.active = previousActive
		m.activeConversations = previousActiveConversations
		m.mu.Unlock()
		return Status{}, errors.New("unable to store GitHub personal access token")
	}
	m.credentialGeneration++
	m.mu.Unlock()

	for _, task := range previousActive {
		task.cancel()
		if task.session != nil {
			_ = task.session.Abort(context.Background())
		}
	}
	for _, task := range previousActiveConversations {
		task.cancel()
		if task.session != nil {
			_ = task.session.Abort(context.Background())
		}
	}
	for _, sessionID := range previousSessions {
		if sessionID != "" {
			_ = m.runtime.Delete(context.Background(), sessionID)
		}
	}
	return Status{Connected: true, Account: account, Method: "github-pat"}, nil
}

func isFineGrainedPAT(token string) bool {
	const prefix = "github_pat_"
	if !strings.HasPrefix(token, prefix) || len(token) > 1024 || len(token) < len(prefix)+20 {
		return false
	}
	for _, char := range token[len(prefix):] {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_') {
			return false
		}
	}
	return true
}

func (m *Manager) githubAccount(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.UserURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", errors.New("GitHub user endpoint error")
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&user); err != nil {
		return "", err
	}
	login := user.Login
	if !usefulLogin(login) {
		return "", errors.New("GitHub account has no usable login")
	}
	return login, nil
}

func usefulLogin(login string) bool {
	if login == "" || len(login) > 255 || strings.TrimSpace(login) != login {
		return false
	}
	for _, r := range login {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (m *Manager) tokenForTask() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credential == nil || m.credential.PersonalAccessToken == "" {
		return "", ErrNotConnected
	}
	return m.credential.PersonalAccessToken, nil
}
