package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
)

func TestInterruptConversationTurnPersistsTerminalTaskResult(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const sandboxID = "01CONVERSATIONTESTSANDBOX"
	if err := st.Create(ctx, minimalSandbox(sandboxID, "sleep")); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	conversation := &Conversation{
		ID: "conversation-1", SandboxID: sandboxID, Agent: ConversationAgentGitHubCopilot,
		DefaultMode: ConversationModeInteractive,
	}
	if err := st.CreateConversation(ctx, conversation); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	turn, _, err := st.EnqueueConversationTurn(ctx, conversation.ID, "turn-1", "task-1",
		"make the dashboard clearer", ConversationModeInteractive, ConversationTurnSettings{})
	if err != nil {
		t.Fatalf("enqueue turn: %v", err)
	}
	if _, err := st.ClaimNextConversationTurn(ctx, conversation.ID); err != nil {
		t.Fatalf("claim turn: %v", err)
	}

	const interruption = "Copilot was interrupted because sandboxd restarted."
	if err := st.InterruptConversationTurn(ctx, turn.ID, interruption); err != nil {
		t.Fatalf("interrupt turn: %v", err)
	}
	task, err := st.GetTask(ctx, turn.TaskID)
	if err != nil {
		t.Fatalf("get interrupted task: %v", err)
	}
	if task.Status != ConversationTurnFailed || !task.ResultJSON.Valid {
		t.Fatalf("interrupted task = %#v", task)
	}
	var result runtime.TaskResult
	if err := json.Unmarshal([]byte(task.ResultJSON.String), &result); err != nil {
		t.Fatalf("decode interrupted result: %v", err)
	}
	if result.Status != runtime.TaskFailed || result.FailureReason != "provider_interrupted" ||
		result.ErrorMessage != interruption {
		t.Fatalf("interrupted result = %#v", result)
	}
}

func TestSnapshotActiveConversationIncludesSingleCursorForStoredState(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const sandboxID = "01SNAPSHOTTESTSANDBOX00000"
	if err := st.Create(ctx, minimalSandbox(sandboxID, "sleep")); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	conversation := &Conversation{
		ID: "conversation-snapshot", SandboxID: sandboxID, Agent: ConversationAgentGitHubCopilot,
		DefaultMode: ConversationModeInteractive,
	}
	if err := st.CreateConversation(ctx, conversation); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	turn, _, err := st.EnqueueConversationTurn(ctx, conversation.ID, "turn-snapshot", "task-snapshot",
		"build a form", ConversationModeInteractive, ConversationTurnSettings{
			Model: "gpt-5.3-codex", ReasoningEffort: "high", ContextTier: "long_context",
		})
	if err != nil {
		t.Fatalf("enqueue turn: %v", err)
	}
	if _, err := st.ClaimNextConversationTurn(ctx, conversation.ID); err != nil {
		t.Fatalf("claim turn: %v", err)
	}
	if err := st.AppendAssistantText(ctx, conversation.ID, turn.ID, "assistant-snapshot", "I will build it."); err != nil {
		t.Fatalf("append assistant text: %v", err)
	}

	snapshot, err := st.SnapshotActiveConversation(ctx, sandboxID)
	if err != nil {
		t.Fatalf("snapshot active conversation: %v", err)
	}
	cursor, err := st.LatestConversationEventID(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("latest event cursor: %v", err)
	}
	if snapshot.EventCursor != cursor {
		t.Fatalf("snapshot cursor = %d; want %d", snapshot.EventCursor, cursor)
	}
	if len(snapshot.Messages) != 2 || snapshot.Messages[1].Content != "I will build it." {
		t.Fatalf("snapshot messages = %#v", snapshot.Messages)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].Model != "gpt-5.3-codex" ||
		snapshot.Turns[0].ReasoningEffort != "high" || snapshot.Turns[0].ContextTier != "long_context" {
		t.Fatalf("snapshot model settings = %#v", snapshot.Turns)
	}
}

func TestConversationChildInheritsTurnSettingsAndExposesBoundedPatch(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const sandboxID = "01CHILDLIFECYCLETESTSANDBOX"
	if err := st.Create(ctx, minimalSandbox(sandboxID, "sleep")); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	conversation := &Conversation{
		ID: "conversation-child", SandboxID: sandboxID, Agent: ConversationAgentGitHubCopilot,
		DefaultMode: ConversationModeInteractive,
	}
	if err := st.CreateConversation(ctx, conversation); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	turn, _, err := st.EnqueueConversationTurn(ctx, conversation.ID, "turn-child", "task-child",
		"coordinate the work", ConversationModeInteractive, ConversationTurnSettings{
			Model: "gpt-5.6-terra", ReasoningEffort: "max", ContextTier: "long_context",
		})
	if err != nil {
		t.Fatalf("enqueue turn: %v", err)
	}
	if _, err := st.ClaimNextConversationTurn(ctx, conversation.ID); err != nil {
		t.Fatalf("claim turn: %v", err)
	}

	child := &ConversationChild{
		ID: "child-1", ConversationID: conversation.ID, ParentTurnID: turn.ID,
		Label: "Add tests", Prompt: "Write focused tests.", WorkspacePath: "/private/child-1",
	}
	if err := st.CreateConversationChild(ctx, child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	if child.Model != "gpt-5.6-terra" || child.ReasoningEffort != "max" || child.ContextTier != "long_context" {
		t.Fatalf("child settings = %#v", child)
	}
	if _, err := st.ClaimConversationChild(ctx, child.ID); err != nil {
		t.Fatalf("claim child: %v", err)
	}
	if _, err := st.StartConversationChild(ctx, child.ID, "sandboxd-child-child-1"); err != nil {
		t.Fatalf("start child: %v", err)
	}
	if _, err := st.FinishConversationChild(ctx, child.ID, ConversationChildSucceeded, "done", "",
		ConversationChildPatchAvailable, &ConversationChildPatch{Changes: []ConversationChildChange{{
			Path: "../secret", Content: "no",
		}}}); err == nil {
		t.Fatal("unsafe delegated patch was accepted")
	}
	finished, err := st.FinishConversationChild(ctx, child.ID, ConversationChildSucceeded, "done", "",
		ConversationChildPatchAvailable, &ConversationChildPatch{Changes: []ConversationChildChange{{
			Path: "test/app_test.go", BaseSHA256: "abc", Content: "package app",
		}}})
	if err != nil {
		t.Fatalf("finish child: %v", err)
	}
	if finished.Status != ConversationChildSucceeded || finished.PatchState != ConversationChildPatchAvailable {
		t.Fatalf("finished child = %#v", finished)
	}
	if _, err := st.RequestConversationChildCancellation(ctx, conversation.ID, child.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel terminal child error = %v; want conflict", err)
	}

	cancelling := &ConversationChild{
		ID: "child-cancelling", ConversationID: conversation.ID, ParentTurnID: turn.ID,
		Label: "Cancelled work", Prompt: "This result must not be retained.", WorkspacePath: "/private/child-cancelling",
	}
	if err := st.CreateConversationChild(ctx, cancelling); err != nil {
		t.Fatalf("create cancelling child: %v", err)
	}
	if _, err := st.ClaimConversationChild(ctx, cancelling.ID); err != nil {
		t.Fatalf("claim cancelling child: %v", err)
	}
	if _, err := st.StartConversationChild(ctx, cancelling.ID, "sandboxd-child-child-cancelling"); err != nil {
		t.Fatalf("start cancelling child: %v", err)
	}
	if _, err := st.RequestConversationChildCancellation(ctx, conversation.ID, cancelling.ID); err != nil {
		t.Fatalf("request child cancellation: %v", err)
	}
	cancelled, err := st.FinishConversationChild(ctx, cancelling.ID, ConversationChildSucceeded, "late result", "",
		ConversationChildPatchAvailable, &ConversationChildPatch{Changes: []ConversationChildChange{{
			Path: "should-not-survive.txt", BaseSHA256: "abc", Content: "late patch",
		}}})
	if err != nil {
		t.Fatalf("finish cancelling child: %v", err)
	}
	if cancelled.Status != ConversationChildCancelled || cancelled.Result != "" ||
		cancelled.PatchState != ConversationChildPatchNone || cancelled.PatchJSON != "" {
		t.Fatalf("cancelled child retained completion data: %#v", cancelled)
	}

	snapshot, err := st.SnapshotActiveConversation(ctx, sandboxID)
	if err != nil {
		t.Fatalf("snapshot conversation: %v", err)
	}
	if len(snapshot.Children) != 2 || snapshot.Children[0].ID != child.ID ||
		snapshot.Children[0].ChangedFiles()[0] != "test/app_test.go" {
		t.Fatalf("snapshot children = %#v", snapshot.Children)
	}
}
