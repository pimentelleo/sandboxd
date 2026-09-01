package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// handleProviderWakeJSON is the provider equivalent of wake.Handler's JSON
// endpoint. It intentionally performs no host admission, TCP probing, Docker
// inspection, or loopback access: the provider owns all of those concerns.
func (s *Server) handleProviderWakeJSON(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	started := time.Now()
	status := "creating"
	err := s.withRuntimeLease(r.Context(), id, func(ctx context.Context) error {
		sb, err := s.Store.Get(ctx, id)
		if err != nil {
			return err
		}
		if sb.Status == "error" {
			return fmt.Errorf("%w: sandbox is in an error state", ErrRuntimeUnavailable)
		}
		if err := s.RuntimeLifecycle.Start(ctx, s.runtimeRef(sb)); err != nil {
			return err
		}
		live, err := s.RuntimeLifecycle.Inspect(ctx, s.runtimeRef(sb))
		if err != nil {
			return err
		}
		status = runtimeStatus(live.State)
		return s.persistRuntimeState(ctx, id, live)
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeRuntimeProviderError(w, err)
		return
	}
	_ = s.Store.BumpLastActive(r.Context(), id, time.Now().UTC())
	writeJSON(w, http.StatusOK, map[string]any{
		"id":               id,
		"status":           status,
		"wake_duration_ms": time.Since(started).Milliseconds(),
	})
}
