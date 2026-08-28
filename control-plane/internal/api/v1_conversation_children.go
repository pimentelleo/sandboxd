package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// v1ConversationChildren lists the isolated background tasks for the active
// Copilot conversation. Workers and their private workspaces are never exposed.
func (s *Server) v1ConversationChildren(w http.ResponseWriter, r *http.Request) {
	conversation, err := s.activeConversationForChildRequest(r, w)
	if err != nil {
		return
	}
	children, err := s.Store.ListConversationChildren(r.Context(), conversation.ID, 100)
	if err != nil {
		writeConversationChildError(w, err)
		return
	}
	out := make([]v1ConversationChild, 0, len(children))
	for _, child := range children {
		out = append(out, v1ConversationChildFromStore(child))
	}
	writeJSON(w, http.StatusOK, map[string]any{"children": out})
}

func (s *Server) v1ConversationChild(w http.ResponseWriter, r *http.Request) {
	conversation, err := s.activeConversationForChildRequest(r, w)
	if err != nil {
		return
	}
	child, err := s.Store.GetConversationChildForConversation(r.Context(), conversation.ID, r.PathValue("childId"))
	if err != nil {
		writeConversationChildError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v1ConversationChildFromStore(child))
}

// v1ConversationChildChange returns one bounded replacement/deletion record.
// The client may display it for review but no endpoint applies it automatically.
func (s *Server) v1ConversationChildChange(w http.ResponseWriter, r *http.Request) {
	conversation, err := s.activeConversationForChildRequest(r, w)
	if err != nil {
		return
	}
	child, err := s.Store.GetConversationChildForConversation(r.Context(), conversation.ID, r.PathValue("childId"))
	if err != nil {
		writeConversationChildError(w, err)
		return
	}
	path := r.PathValue("path")
	if !validConversationChildChangePath(path) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid delegated change path")
		return
	}
	patch, err := child.Patch()
	if err != nil {
		writeConversationChildError(w, errConversationChildPatch)
		return
	}
	for _, change := range patch.Changes {
		if change.Path == path {
			writeJSON(w, http.StatusOK, v1ConversationChildChange{
				TaskID:     child.ID,
				Path:       change.Path,
				BaseSHA256: change.BaseSHA256,
				Content:    change.Content,
				Deleted:    change.Deleted,
				Mode:       change.Mode,
			})
			return
		}
	}
	writeConversationChildError(w, store.ErrNotFound)
}

func (s *Server) v1ConversationChildCancel(w http.ResponseWriter, r *http.Request) {
	conversation, err := s.activeConversationForChildRequest(r, w)
	if err != nil {
		return
	}
	coordinator := s.conversationCoordinator()
	if coordinator == nil {
		writeConversationChildError(w, errConversationChildUnavailable)
		return
	}
	task, err := coordinator.CancelBackgroundTask(r.Context(), conversation.ID, r.PathValue("childId"))
	if err != nil {
		writeConversationChildError(w, err)
		return
	}
	child, err := s.Store.GetConversationChildForConversation(r.Context(), conversation.ID, task.ID)
	if err != nil {
		writeConversationChildError(w, err)
		return
	}
	s.auditAction(r, audit.Entry{
		Action: "conversation.child.cancel",
		Target: r.PathValue("id"),
		Detail: map[string]any{"child_id": child.ID, "parent_turn_id": child.ParentTurnID},
	})
	writeJSON(w, http.StatusOK, v1ConversationChildFromStore(child))
}

func (s *Server) activeConversationForChildRequest(r *http.Request, w http.ResponseWriter) (*store.Conversation, error) {
	if _, err := s.Store.Get(r.Context(), r.PathValue("id")); err != nil {
		writeConversationStoreError(w, err)
		return nil, err
	}
	conversation, err := s.Store.GetActiveConversation(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no Copilot conversation for this sandbox")
		return nil, err
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not load Copilot conversation")
		return nil, err
	}
	return conversation, nil
}

func validConversationChildChangePath(path string) bool {
	if path == "" || len(path) > 1024 || strings.ContainsRune(path, 0) ||
		strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func writeConversationChildError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeV1Err(w, http.StatusNotFound, "not_found", "no such delegated task or change")
	case errors.Is(err, store.ErrConflict):
		writeV1Err(w, http.StatusConflict, "conflict", "the delegated task changed; refresh and try again")
	case errors.Is(err, errConversationChildLimit):
		writeV1Err(w, http.StatusTooManyRequests, "delegated_task_limit", "the delegated task limit has been reached")
	case errors.Is(err, errConversationChildPatch):
		writeV1Err(w, http.StatusConflict, "patch_unavailable", "a reviewable delegated task patch is unavailable")
	case errors.Is(err, errConversationChildUnavailable):
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "delegated tasks are unavailable")
	default:
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not update the delegated task")
	}
}
