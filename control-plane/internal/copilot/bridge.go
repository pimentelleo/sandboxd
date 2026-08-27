package copilot

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
)

const bridgeTaskPath = "/internal/copilot/tasks"
const bridgeCancelPath = "/internal/copilot/tasks/cancel"

// Handler returns the private bridge handler. The parent must mount it only on
// the control-plane's authenticated internal listener.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(m.ServeHTTP)
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == bridgeTaskPath:
		m.serveTask(w, r)
	case r.Method == http.MethodPost && r.URL.Path == bridgeCancelPath:
		m.serveCancel(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *Manager) serveTask(w http.ResponseWriter, r *http.Request) {
	var request TaskRequest
	if err := decodeJSON(r, &request); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
	writer := &ndjsonWriter{w: w}
	err := m.RunTask(r.Context(), request, writer.write)
	if err != nil {
		// No raw SDK/OAuth/Docker diagnostic is ever emitted to runtimed.
		writer.write(Envelope{Type: "error", Message: safeTaskError(err)})
	}
}

func (m *Manager) serveCancel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Capability string `json:"capability"`
	}
	if err := decodeJSON(r, &request); err != nil || len(request.Capability) < 32 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	m.CancelCapability(request.Capability)
	w.WriteHeader(http.StatusOK)
}

func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("missing body")
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxPrompt+maxSystem+4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("multiple values")
	}
	return nil
}

func safeTaskError(err error) string {
	switch {
	case errors.Is(err, ErrCapability):
		return "task authorization is invalid or expired"
	case errors.Is(err, ErrNotConnected):
		return "GitHub Copilot is not connected"
	default:
		return "Copilot task could not be completed"
	}
}

type ndjsonWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
}

func (w *ndjsonWriter) write(envelope Envelope) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// This type is internal and all call sites construct safe envelopes. Keep
	// this allowlist as a defense against future accidental event passthrough.
	switch envelope.Type {
	case "message", "tool", "usage", "error":
	default:
		return
	}
	if envelope.Type == "message" && envelope.Role != "agent" && envelope.Role != "agent_error" {
		return
	}
	// Paths are useful to a local SDK tool but not to this bridge. Do not let
	// any future event adapter turn a path (or a path-shaped secret) into a
	// runtimed event.
	envelope.Path = ""
	data, err := json.Marshal(envelope)
	if err != nil {
		return
	}
	_, _ = w.w.Write(append(data, '\n'))
	if flush, ok := w.w.(http.Flusher); ok {
		flush.Flush()
	}
}
