package copilot

import (
	"context"
	"errors"
	"strings"
	"time"
)

// IssueCapability grants exactly one private task request for the sandbox/task
// pair. Capabilities are opaque, unguessable, and intentionally not persisted.
func (m *Manager) IssueCapability(sandboxID, taskID string, ttl time.Duration) (string, error) {
	if !validIdentifier(sandboxID) || !validIdentifier(taskID) || ttl <= 0 {
		return "", errors.New("invalid capability request")
	}
	token, err := randomOpaque(32)
	if err != nil {
		return "", errors.New("unable to issue task capability")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked(m.cfg.Now())
	account := ""
	if m.credential != nil {
		account = m.credential.Account
	}
	m.capabilities[token] = capability{
		sandboxID: sandboxID, taskID: taskID, account: account, generation: m.generations[sandboxID],
		expiresAt: m.cfg.Now().Add(ttl),
	}
	return token, nil
}

// CancelCapability is idempotent. It removes an unconsumed capability or aborts
// its associated active task without revealing whether it ever existed.
func (m *Manager) CancelCapability(token string) {
	m.mu.Lock()
	cap, ok := m.capabilities[token]
	if ok {
		delete(m.capabilities, token)
	}
	active, running := m.active[token]
	if running {
		delete(m.active, token)
	}
	m.mu.Unlock()
	if ok && cap.consumed && running {
		active.cancel()
		if active.session != nil {
			_ = active.session.Abort(context.Background())
		}
	}
}

// CleanupSandbox revokes all capabilities, aborts active tasks, and removes the
// durable SDK session reference. SDK deletion is best effort by design.
func (m *Manager) CleanupSandbox(sandboxID string) error {
	m.mu.Lock()
	m.generations[sandboxID]++
	var active []activeTask
	for token, cap := range m.capabilities {
		if cap.sandboxID == sandboxID {
			delete(m.capabilities, token)
			if task, ok := m.active[token]; ok {
				delete(m.active, token)
				active = append(active, task)
			}
		}
	}
	sessionID := m.sessions[sandboxID]
	_, hadSession := m.sessions[sandboxID]
	delete(m.sessions, sandboxID)
	err := m.persistLocked()
	if err != nil && hadSession {
		m.sessions[sandboxID] = sessionID
	}
	m.mu.Unlock()
	for _, task := range active {
		task.cancel()
		if task.session != nil {
			_ = task.session.Abort(context.Background())
		}
	}
	if err == nil && sessionID != "" {
		_ = m.runtime.Delete(context.Background(), sessionID)
	}
	return err
}

// CleanupExpired removes expired unconsumed capabilities. It is safe to call
// from a periodic reaper.
func (m *Manager) CleanupExpired() {
	m.mu.Lock()
	m.cleanupLocked(m.cfg.Now())
	m.mu.Unlock()
}

func (m *Manager) cleanupLocked(now time.Time) {
	for token, cap := range m.capabilities {
		// A consumed capability belongs to an active task and remains valid until
		// that task finishes or is explicitly canceled.
		if !cap.consumed && !now.Before(cap.expiresAt) {
			delete(m.capabilities, token)
		}
	}
}

func (m *Manager) consumeCapability(token string) (capability, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked(m.cfg.Now())
	cap, ok := m.capabilities[token]
	if !ok || cap.consumed || !m.cfg.Now().Before(cap.expiresAt) {
		delete(m.capabilities, token)
		return capability{}, ErrCapability
	}
	cap.consumed = true
	m.capabilities[token] = cap
	return cap, nil
}

// capabilityMatchesAccount confirms that a consumed capability still belongs to
// the account that configured it. Credential replacement invalidates all
// capabilities, including one that was already consumed while its SDK session
// was being opened.
func (m *Manager) capabilityMatchesAccount(token string, expected capability) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.capabilities[token]
	if !ok || !current.consumed || current.sandboxID != expected.sandboxID ||
		current.taskID != expected.taskID || current.account == "" ||
		current.account != expected.account || m.credential == nil ||
		m.credential.Account != current.account {
		delete(m.capabilities, token)
		return false
	}
	return true
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return !strings.Contains(value, "..")
}
