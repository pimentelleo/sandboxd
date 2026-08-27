package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

type v1Conversation struct {
	ID           string `json:"id"`
	SandboxID    string `json:"sandbox_id"`
	Agent        string `json:"agent"`
	State        string `json:"state"`
	DefaultMode  string `json:"default_mode"`
	ActiveTurnID string `json:"active_turn_id,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	ArchivedAt   string `json:"archived_at,omitempty"`
}

type v1ConversationTurn struct {
	ID           string `json:"id"`
	TaskID       string `json:"task_id"`
	Sequence     int64  `json:"sequence"`
	Prompt       string `json:"prompt"`
	Mode         string `json:"mode"`
	Status       string `json:"status"`
	ErrorMessage string `json:"error_message,omitempty"`
	CreatedAt    string `json:"created_at"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
}

type v1ConversationMessage struct {
	ID        string `json:"id"`
	TurnID    string `json:"turn_id"`
	Sequence  int64  `json:"sequence"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type v1ConversationInteraction struct {
	ID                string   `json:"id"`
	TurnID            string   `json:"turn_id"`
	Sequence          int64    `json:"sequence"`
	Type              string   `json:"type"`
	Status            string   `json:"status"`
	Question          string   `json:"question,omitempty"`
	Choices           []string `json:"choices"`
	AllowFreeform     bool     `json:"allow_freeform"`
	Summary           string   `json:"summary,omitempty"`
	Plan              string   `json:"plan,omitempty"`
	Actions           []string `json:"actions"`
	RecommendedAction string   `json:"recommended_action,omitempty"`
	Answer            string   `json:"answer,omitempty"`
	Approved          *bool    `json:"approved,omitempty"`
	SelectedAction    string   `json:"selected_action,omitempty"`
	Feedback          string   `json:"feedback,omitempty"`
	CreatedAt         string   `json:"created_at"`
	ResolvedAt        string   `json:"resolved_at,omitempty"`
}

type v1ConversationSnapshot struct {
	Conversation  *v1Conversation             `json:"conversation"`
	Turns         []v1ConversationTurn        `json:"turns"`
	Messages      []v1ConversationMessage     `json:"messages"`
	Interactions  []v1ConversationInteraction `json:"interactions"`
	EventCursor   int64                       `json:"event_cursor"`
	NextQueueSlot int                         `json:"next_queue_slot"`
}

type v1ConversationEvent struct {
	ID        int64           `json:"id"`
	Type      string          `json:"type"`
	TurnID    string          `json:"turn_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

type v1ConversationMessageReq struct {
	Prompt string `json:"prompt"`
	Mode   string `json:"mode,omitempty"`
}

type v1ConversationAnswerReq struct {
	Answer string `json:"answer"`
}

type v1ConversationPlanReq struct {
	Approved       *bool  `json:"approved"`
	SelectedAction string `json:"selected_action,omitempty"`
	Feedback       string `json:"feedback,omitempty"`
}

type v1ConversationResetReq struct {
	DefaultMode string `json:"default_mode,omitempty"`
}

func v1ConversationFromStore(conversation *store.Conversation) *v1Conversation {
	if conversation == nil {
		return nil
	}
	out := &v1Conversation{
		ID: conversation.ID, SandboxID: conversation.SandboxID, Agent: conversation.Agent,
		State: conversation.State, DefaultMode: conversation.DefaultMode,
		CreatedAt: conversation.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: conversation.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if conversation.ActiveTurnID.Valid {
		out.ActiveTurnID = conversation.ActiveTurnID.String
	}
	if conversation.LastError.Valid {
		out.LastError = conversation.LastError.String
	}
	if conversation.ArchivedAt.Valid {
		out.ArchivedAt = time.Unix(conversation.ArchivedAt.Int64, 0).UTC().Format(time.RFC3339)
	}
	return out
}

func v1ConversationSnapshotFromStore(snapshot *store.ConversationSnapshot) v1ConversationSnapshot {
	out := v1ConversationSnapshot{
		Conversation:  v1ConversationFromStore(snapshot.Conversation),
		Turns:         make([]v1ConversationTurn, 0, len(snapshot.Turns)),
		Messages:      make([]v1ConversationMessage, 0, len(snapshot.Messages)),
		Interactions:  make([]v1ConversationInteraction, 0, len(snapshot.Interactions)),
		EventCursor:   snapshot.EventCursor,
		NextQueueSlot: snapshot.NextQueueSlot,
	}
	for _, turn := range snapshot.Turns {
		item := v1ConversationTurn{
			ID: turn.ID, TaskID: turn.TaskID, Sequence: turn.Sequence, Prompt: turn.Prompt,
			Mode: turn.Mode, Status: turn.Status, CreatedAt: turn.CreatedAt.UTC().Format(time.RFC3339),
		}
		if turn.ErrorMessage.Valid {
			item.ErrorMessage = turn.ErrorMessage.String
		}
		if turn.StartedAt.Valid {
			item.StartedAt = time.Unix(turn.StartedAt.Int64, 0).UTC().Format(time.RFC3339)
		}
		if turn.FinishedAt.Valid {
			item.FinishedAt = time.Unix(turn.FinishedAt.Int64, 0).UTC().Format(time.RFC3339)
		}
		out.Turns = append(out.Turns, item)
	}
	for _, message := range snapshot.Messages {
		out.Messages = append(out.Messages, v1ConversationMessage{
			ID: message.ID, TurnID: message.TurnID, Sequence: message.Sequence, Role: message.Role,
			Content: message.Content, Status: message.Status,
			CreatedAt: message.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	for _, interaction := range snapshot.Interactions {
		item := v1ConversationInteraction{
			ID: interaction.ID, TurnID: interaction.TurnID, Sequence: interaction.Sequence,
			Type: interaction.Type, Status: interaction.Status, Question: interaction.Question,
			Choices: interaction.Choices, AllowFreeform: interaction.AllowFreeform,
			Summary: interaction.Summary, Plan: interaction.Plan, Actions: interaction.Actions,
			RecommendedAction: interaction.RecommendedAction,
			CreatedAt:         interaction.CreatedAt.UTC().Format(time.RFC3339),
		}
		if interaction.Answer.Valid {
			item.Answer = interaction.Answer.String
		}
		if interaction.Approved.Valid {
			value := interaction.Approved.Bool
			item.Approved = &value
		}
		if interaction.SelectedAction.Valid {
			item.SelectedAction = interaction.SelectedAction.String
		}
		if interaction.Feedback.Valid {
			item.Feedback = interaction.Feedback.String
		}
		if interaction.ResolvedAt.Valid {
			item.ResolvedAt = time.Unix(interaction.ResolvedAt.Int64, 0).UTC().Format(time.RFC3339)
		}
		out.Interactions = append(out.Interactions, item)
	}
	return out
}

// v1ConversationSnapshot returns durable state for the hosted Copilot chat.
// A sandbox without prior Copilot messages returns an empty transcript.
func (s *Server) v1ConversationSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.Get(r.Context(), id); err != nil {
		writeConversationStoreError(w, err)
		return
	}
	snapshot, err := s.Store.SnapshotActiveConversation(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, v1ConversationSnapshot{
			Turns: []v1ConversationTurn{}, Messages: []v1ConversationMessage{},
			Interactions: []v1ConversationInteraction{},
		})
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not load Copilot conversation")
		return
	}
	writeJSON(w, http.StatusOK, v1ConversationSnapshotFromStore(snapshot))
}

// v1ConversationSubmit accepts a message even while the current provider turn
// is active. The coordinator serializes it behind the active turn.
func (s *Server) v1ConversationSubmit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.copilotConversationReady(w) {
		return
	}
	var request v1ConversationMessageReq
	if err := decodeConversationJSON(w, r, &request); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mode, prompt := parseConversationMode(request.Mode, request.Prompt)
	if prompt == "" || len(prompt) > conversationPromptLimit || !validConversationMode(mode) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "prompt and a valid mode are required")
		return
	}
	coordinator := s.conversationCoordinator()
	turn, position, err := coordinator.Submit(r.Context(), id, prompt, mode)
	if err != nil {
		writeConversationActionError(w, err)
		return
	}
	s.auditAction(r, audit.Entry{
		Action: "conversation.message.create", Target: id,
		Detail: map[string]any{"turn_id": turn.ID, "mode": turn.Mode},
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": turn.ID, "task_id": turn.TaskID, "status": turn.Status, "mode": turn.Mode,
		"queue_position": position,
	})
}

func (s *Server) v1ConversationAnswer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.copilotConversationReady(w) {
		return
	}
	var request v1ConversationAnswerReq
	if err := decodeConversationJSON(w, r, &request); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	interaction, err := s.conversationCoordinator().Answer(
		r.Context(), id, r.PathValue("interactionId"), request.Answer, nil, "", "")
	if err != nil {
		writeConversationActionError(w, err)
		return
	}
	s.auditAction(r, audit.Entry{Action: "conversation.input.answer", Target: id,
		Detail: map[string]any{"interaction_id": interaction.ID}})
	writeJSON(w, http.StatusOK, map[string]any{"id": interaction.ID, "status": interaction.Status})
}

func (s *Server) v1ConversationPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.copilotConversationReady(w) {
		return
	}
	var request v1ConversationPlanReq
	if err := decodeConversationJSON(w, r, &request); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if request.Approved == nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "approved is required")
		return
	}
	interaction, err := s.conversationCoordinator().Answer(
		r.Context(), id, r.PathValue("interactionId"), "", request.Approved,
		request.SelectedAction, request.Feedback)
	if err != nil {
		writeConversationActionError(w, err)
		return
	}
	s.auditAction(r, audit.Entry{Action: "conversation.plan.respond", Target: id,
		Detail: map[string]any{"interaction_id": interaction.ID, "approved": *request.Approved}})
	writeJSON(w, http.StatusOK, map[string]any{"id": interaction.ID, "status": interaction.Status})
}

func (s *Server) v1ConversationCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.copilotConversationReady(w) {
		return
	}
	turn, err := s.conversationCoordinator().Cancel(r.Context(), id)
	if err != nil {
		writeConversationActionError(w, err)
		return
	}
	s.auditAction(r, audit.Entry{Action: "conversation.cancel", Target: id,
		Detail: map[string]any{"turn_id": turn.ID}})
	writeJSON(w, http.StatusAccepted, map[string]any{"id": turn.ID, "status": store.ConversationTurnCancelling})
}

func (s *Server) v1ConversationReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.copilotConversationReady(w) {
		return
	}
	var request v1ConversationResetReq
	if err := decodeConversationJSON(w, r, &request); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if request.DefaultMode == "" {
		request.DefaultMode = store.ConversationModeInteractive
	}
	conversation, err := s.conversationCoordinator().Reset(r.Context(), id, request.DefaultMode)
	if err != nil {
		writeConversationActionError(w, err)
		return
	}
	s.auditAction(r, audit.Entry{Action: "conversation.reset", Target: id})
	writeJSON(w, http.StatusCreated, map[string]any{"conversation": v1ConversationFromStore(conversation)})
}

// v1ConversationEvents replays persisted events after a cursor, then waits for
// a coordinator notification. The snapshot endpoint supplies the cursor used
// to close the load-to-subscribe race.
func (s *Server) v1ConversationEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.Get(r.Context(), id); err != nil {
		writeConversationStoreError(w, err)
		return
	}
	conversation, err := s.Store.GetActiveConversation(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no Copilot conversation for this sandbox")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not load Copilot conversation")
		return
	}
	after, err := conversationEventCursor(r)
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid event cursor")
		return
	}
	coordinator := s.conversationCoordinator()
	if coordinator == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "GitHub Copilot conversations are unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeV1Err(w, http.StatusInternalServerError, "internal", "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		updates := coordinator.subscribe(conversation.ID)
		events, err := s.Store.ListConversationEvents(r.Context(), conversation.ID, after, 200)
		if err != nil {
			return
		}
		for _, event := range events {
			item := v1ConversationEvent{
				ID: event.ID, Type: event.Type, Payload: event.Payload,
				CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339),
			}
			if event.TurnID.Valid {
				item.TurnID = event.TurnID.String
			}
			raw, err := json.Marshal(item)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.ID, raw); err != nil {
				return
			}
			after = event.ID
		}
		if len(events) != 0 {
			flusher.Flush()
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-updates:
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) copilotConversationReady(w http.ResponseWriter) bool {
	if s.Copilot == nil || s.conversationCoordinator() == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "GitHub Copilot conversations are unavailable")
		return false
	}
	if !s.Copilot.Status().Connected {
		writeV1Err(w, http.StatusConflict, "agent_not_connected", "GitHub Copilot is not connected")
		return false
	}
	return true
}

func decodeConversationJSON(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, conversationPromptLimit+1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid json")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid request")
	}
	return nil
}

func parseConversationMode(requestedMode, prompt string) (string, string) {
	prompt = strings.TrimSpace(prompt)
	for _, command := range []struct {
		prefix string
		mode   string
	}{
		{prefix: "/plan", mode: store.ConversationModePlan},
		{prefix: "/interactive", mode: store.ConversationModeInteractive},
		{prefix: "/autopilot", mode: store.ConversationModeAutopilot},
	} {
		if prompt == command.prefix {
			return command.mode, ""
		}
		if strings.HasPrefix(prompt, command.prefix+" ") {
			return command.mode, strings.TrimSpace(strings.TrimPrefix(prompt, command.prefix))
		}
	}
	if requestedMode == "" {
		requestedMode = store.ConversationModeInteractive
	}
	return requestedMode, prompt
}

func conversationEventCursor(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("after")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cursor < 0 {
		return 0, errors.New("invalid cursor")
	}
	return cursor, nil
}

func writeConversationStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such sandbox")
		return
	}
	writeV1Err(w, http.StatusInternalServerError, "internal", "could not load Copilot conversation")
}

func writeConversationActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeV1Err(w, http.StatusNotFound, "not_found", "no such sandbox or Copilot interaction")
	case errors.Is(err, store.ErrConflict):
		writeV1Err(w, http.StatusConflict, "conflict", "the Copilot conversation changed; refresh and try again")
	case errors.Is(err, errInteractionUnavailable):
		writeV1Err(w, http.StatusConflict, "interaction_unavailable", "the Copilot interaction is no longer active")
	case errors.Is(err, errConversationUnavailable):
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "GitHub Copilot conversations are unavailable")
	case errors.Is(err, errInvalidConversation):
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid Copilot conversation request")
	case errors.Is(err, errSandboxUnavailable):
		writeV1Err(w, http.StatusBadGateway, "sandbox_unavailable", "the sandbox is unavailable")
	default:
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not update the Copilot conversation")
	}
}
