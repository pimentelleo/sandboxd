package copilot

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	ContextTierDefault     = "default"
	ContextTierLongContext = "long_context"
)

type modelCatalogFetch struct {
	generation uint64
	done       chan struct{}
}

// ListModels returns only safe, authenticated-account model metadata. The
// manager caches successful responses, while the SDK runtime keeps its own
// private connection isolated from conversation sessions.
func (m *Manager) ListModels(ctx context.Context) ([]ModelInfo, error) {
	m.mu.Lock()
	connected := m.credential != nil && m.credential.PersonalAccessToken != ""
	m.mu.Unlock()
	if !connected {
		return nil, ErrNotConnected
	}
	catalogRuntime, ok := m.runtime.(ModelCatalogRuntime)
	if !ok {
		return nil, ErrModelCatalogUnavailable
	}

	for {
		m.mu.Lock()
		if m.credential == nil || m.credential.PersonalAccessToken == "" {
			m.mu.Unlock()
			return nil, ErrNotConnected
		}
		if m.modelCatalog != nil {
			models := cloneModelCatalog(m.modelCatalog)
			m.mu.Unlock()
			return models, nil
		}
		generation := m.credentialGeneration
		token := m.credential.PersonalAccessToken
		if fetch := m.modelCatalogFetch; fetch != nil && fetch.generation == generation {
			done := fetch.done
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		fetch := &modelCatalogFetch{generation: generation, done: make(chan struct{})}
		m.modelCatalogFetch = fetch
		m.mu.Unlock()

		raw, err := catalogRuntime.ListModels(ctx, token)
		models, sanitizeErr := sanitizeModelCatalog(raw)
		if err == nil {
			err = sanitizeErr
		}

		m.mu.Lock()
		current := m.modelCatalogFetch == fetch
		changed := m.credentialGeneration != generation || m.credential == nil ||
			m.credential.PersonalAccessToken != token
		if current {
			m.modelCatalogFetch = nil
			close(fetch.done)
		}
		if !changed && err == nil {
			m.modelCatalog = cloneModelCatalog(models)
		}
		m.mu.Unlock()

		if changed {
			return nil, ErrCredentialChanged
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrModelCatalogUnavailable, err)
		}
		return models, nil
	}
}

// ValidateModelSelection normalizes a turn's immutable model controls against
// the connected account's current model catalog. The provider remains the
// authority for compatibility beyond this catalog.
func (m *Manager) ValidateModelSelection(ctx context.Context, model, reasoningEffort, contextTier string) (ModelSelection, error) {
	selection := ModelSelection{
		Model:           strings.TrimSpace(model),
		ReasoningEffort: strings.TrimSpace(reasoningEffort),
		ContextTier:     strings.TrimSpace(contextTier),
	}
	if selection.ContextTier == "" {
		selection.ContextTier = ContextTierDefault
	}
	if len(selection.Model) > 256 || len(selection.ReasoningEffort) > 64 ||
		len(selection.ContextTier) > 64 || !validContextTier(selection.ContextTier) {
		return ModelSelection{}, ErrInvalidModelSelection
	}
	if selection.Model == "" {
		if selection.ReasoningEffort != "" || selection.ContextTier == ContextTierLongContext {
			return ModelSelection{}, ErrInvalidModelSelection
		}
		return selection, nil
	}

	models, err := m.ListModels(ctx)
	if err != nil {
		return ModelSelection{}, err
	}
	for _, available := range models {
		if available.ID != selection.Model {
			continue
		}
		if selection.ReasoningEffort != "" && !contains(available.SupportedReasoningEfforts, selection.ReasoningEffort) {
			return ModelSelection{}, ErrInvalidModelSelection
		}
		return selection, nil
	}
	return ModelSelection{}, ErrInvalidModelSelection
}

func (m *Manager) invalidateModelCatalogLocked() {
	m.modelCatalog = nil
	if m.modelCatalogFetch != nil {
		close(m.modelCatalogFetch.done)
		m.modelCatalogFetch = nil
	}
}

func (m *Manager) invalidateRuntimeModelCatalog() {
	if runtime, ok := m.runtime.(ModelCatalogRuntime); ok {
		runtime.InvalidateModelCatalog()
	}
}

func validContextTier(tier string) bool {
	return tier == ContextTierDefault || tier == ContextTierLongContext
}

func sanitizeModelCatalog(raw []ModelInfo) ([]ModelInfo, error) {
	models := make([]ModelInfo, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, rawModel := range raw {
		id := strings.TrimSpace(rawModel.ID)
		if id == "" || len(id) > 256 || id != rawModel.ID {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}

		name := strings.TrimSpace(rawModel.Name)
		if name == "" || len(name) > 512 {
			name = id
		}
		efforts := make([]string, 0, len(rawModel.SupportedReasoningEfforts))
		for _, rawEffort := range rawModel.SupportedReasoningEfforts {
			effort := strings.TrimSpace(rawEffort)
			if effort == "" || len(effort) > 64 || effort != rawEffort || contains(efforts, effort) {
				continue
			}
			efforts = append(efforts, effort)
		}
		defaultEffort := rawModel.DefaultReasoningEffort
		if !contains(efforts, defaultEffort) {
			defaultEffort = ""
		}
		item := ModelInfo{
			ID: id, Name: name, SupportedReasoningEfforts: efforts,
			DefaultReasoningEffort: defaultEffort,
		}
		if rawModel.MaxContextWindowTokens != nil && *rawModel.MaxContextWindowTokens > 0 {
			limit := *rawModel.MaxContextWindowTokens
			item.MaxContextWindowTokens = &limit
		}
		models = append(models, item)
	}
	if len(raw) != 0 && len(models) == 0 {
		return nil, errors.New("model catalog contained no valid entries")
	}
	return models, nil
}

func cloneModelCatalog(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		item := model
		item.SupportedReasoningEfforts = append([]string(nil), model.SupportedReasoningEfforts...)
		if model.MaxContextWindowTokens != nil {
			limit := *model.MaxContextWindowTokens
			item.MaxContextWindowTokens = &limit
		}
		out = append(out, item)
	}
	return out
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
