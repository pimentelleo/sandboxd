package store

import (
	"context"
	"encoding/json"
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
