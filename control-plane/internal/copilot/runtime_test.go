package copilot

import (
	"strings"
	"testing"
)

func TestAvailableToolsExposeNativeInteractionsOnlyWhenHandlersExist(t *testing.T) {
	legacy := availableTools(RuntimeConfig{})
	for _, tool := range []string{"builtin:ask_user", "builtin:exit_plan_mode"} {
		if includesTool(legacy, tool) {
			t.Fatalf("legacy task tools unexpectedly include %q: %#v", tool, legacy)
		}
	}

	input := availableTools(RuntimeConfig{
		OnUserInputRequest: func(RuntimeInputRequest) (RuntimeInputResponse, error) {
			return RuntimeInputResponse{}, nil
		},
	})
	if !includesTool(input, "builtin:ask_user") || includesTool(input, "builtin:exit_plan_mode") {
		t.Fatalf("input tool configuration = %#v", input)
	}

	plan := availableTools(RuntimeConfig{
		OnPlanRequest: func(RuntimePlanRequest) (RuntimePlanResponse, error) {
			return RuntimePlanResponse{}, nil
		},
	})
	if includesTool(plan, "builtin:ask_user") || !includesTool(plan, "builtin:exit_plan_mode") {
		t.Fatalf("plan tool configuration = %#v", plan)
	}

	conversation := availableTools(RuntimeConfig{
		OnUserInputRequest: func(RuntimeInputRequest) (RuntimeInputResponse, error) {
			return RuntimeInputResponse{}, nil
		},
		OnPlanRequest: func(RuntimePlanRequest) (RuntimePlanResponse, error) {
			return RuntimePlanResponse{}, nil
		},
	})
	for _, tool := range []string{"builtin:ask_user", "builtin:exit_plan_mode"} {
		if !includesTool(conversation, tool) {
			t.Fatalf("conversation tools do not include %q: %#v", tool, conversation)
		}
	}
	for _, tool := range []string{
		"custom:list_files",
		"custom:read_file",
		"custom:search_files",
		"custom:write_file",
		"custom:run_command",
	} {
		if !includesTool(conversation, tool) {
			t.Fatalf("conversation tools do not include %q: %#v", tool, conversation)
		}
	}
}

func TestConversationSystemPromptRequiresNativeClarification(t *testing.T) {
	prompt := conversationSystemPrompt("base instructions")
	if !strings.Contains(prompt, "native ask_user tool") ||
		!strings.Contains(prompt, "Never present a blocking question only as normal assistant text") {
		t.Fatalf("conversation instructions do not require native clarification: %q", prompt)
	}
}

func includesTool(tools []string, wanted string) bool {
	for _, tool := range tools {
		if tool == wanted {
			return true
		}
	}
	return false
}
