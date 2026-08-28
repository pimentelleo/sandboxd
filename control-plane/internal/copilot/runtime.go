package copilot

import (
	"context"
	"errors"
	"sync"

	sdk "github.com/github/copilot-sdk/go"
)

// sdkRuntime is the only production use of the official SDK. It runs in the
// control-plane process and is never passed a sandbox workspace path.
type sdkRuntime struct {
	client                    *sdk.Client
	modelCatalogBaseDirectory string

	modelCatalogMu     sync.Mutex
	modelCatalogClient *sdk.Client
	modelCatalogToken  string
}

func (r *sdkRuntime) Create(ctx context.Context, config RuntimeConfig) (RuntimeSession, error) {
	return r.open(ctx, "", config)
}

func (r *sdkRuntime) Resume(ctx context.Context, id string, config RuntimeConfig) (RuntimeSession, error) {
	tools := sdkTools(config.Tools)
	correlation := newInteractionCorrelator()
	session, err := r.client.ResumeSession(ctx, id, sdkResumeSessionConfig(config, tools, correlation))
	if err != nil {
		correlation.close()
		return nil, err
	}
	return sdkSession{session: session, correlation: correlation}, nil
}

func (r *sdkRuntime) open(ctx context.Context, _ string, config RuntimeConfig) (RuntimeSession, error) {
	tools := sdkTools(config.Tools)
	correlation := newInteractionCorrelator()
	session, err := r.client.CreateSession(ctx, sdkSessionConfig(config, tools, correlation))
	if err != nil {
		correlation.close()
		return nil, err
	}
	return sdkSession{session: session, correlation: correlation}, nil
}

func sdkResumeSessionConfig(config RuntimeConfig, tools []sdk.Tool, correlation *interactionCorrelator) *sdk.ResumeSessionConfig {
	return &sdk.ResumeSessionConfig{
		ClientName: "sandboxd",
		Model:      config.Model, ReasoningEffort: config.ReasoningEffort, ContextTier: sdk.ContextTier(config.ContextTier),
		GitHubToken: config.Token, WorkingDirectory: config.Workdir,
		Tools: tools, AvailableTools: availableTools(config), SystemMessage: systemMessage(config.SystemPrompt),
		EnableConfigDiscovery: sdk.Bool(false), EnableOnDemandInstructionDiscovery: sdk.Bool(false),
		EnableFileHooks: sdk.Bool(false), EnableHostGitOperations: sdk.Bool(false),
		EnableSessionStore: sdk.Bool(false), EnableSkills: sdk.Bool(false),
		Streaming: sdk.Bool(true), ContinuePendingWork: sdk.Bool(config.ContinuePendingWork),
		OnEvent:               eventAdapter(config.OnEvent, correlation),
		OnUserInputRequest:    inputHandler(config.OnUserInputRequest, correlation),
		OnExitPlanModeRequest: planHandler(config.OnPlanRequest, correlation),
	}
}

func sdkSessionConfig(config RuntimeConfig, tools []sdk.Tool, correlation *interactionCorrelator) *sdk.SessionConfig {
	return &sdk.SessionConfig{
		ClientName: "sandboxd",
		Model:      config.Model, ReasoningEffort: config.ReasoningEffort, ContextTier: sdk.ContextTier(config.ContextTier),
		GitHubToken: config.Token, WorkingDirectory: config.Workdir,
		Tools: tools, AvailableTools: availableTools(config), SystemMessage: systemMessage(config.SystemPrompt),
		EnableConfigDiscovery: sdk.Bool(false), EnableOnDemandInstructionDiscovery: sdk.Bool(false),
		EnableFileHooks: sdk.Bool(false), EnableHostGitOperations: sdk.Bool(false),
		EnableSessionStore: sdk.Bool(false), EnableSkills: sdk.Bool(false),
		Streaming: sdk.Bool(true), IncludeSubAgentStreamingEvents: sdk.Bool(false),
		OnEvent:               eventAdapter(config.OnEvent, correlation),
		OnUserInputRequest:    inputHandler(config.OnUserInputRequest, correlation),
		OnExitPlanModeRequest: planHandler(config.OnPlanRequest, correlation),
	}
}

func (r *sdkRuntime) Delete(ctx context.Context, id string) error {
	return r.client.DeleteSession(ctx, id)
}

// ListModels uses an isolated SDK runtime so the live conversation client
// never shares catalog authentication state with an account replacement.
func (r *sdkRuntime) ListModels(ctx context.Context, token string) ([]ModelInfo, error) {
	if token == "" {
		return nil, ErrNotConnected
	}
	r.modelCatalogMu.Lock()
	defer r.modelCatalogMu.Unlock()

	if r.modelCatalogClient == nil || r.modelCatalogToken != token {
		if r.modelCatalogClient != nil {
			_ = r.modelCatalogClient.Stop()
		}
		r.modelCatalogClient = sdk.NewClient(&sdk.ClientOptions{
			Mode: sdk.ModeEmpty, BaseDirectory: r.modelCatalogBaseDirectory,
			WorkingDirectory: r.modelCatalogBaseDirectory, GitHubToken: token,
			UseLoggedInUser: sdk.Bool(false), LogLevel: "error",
		})
		r.modelCatalogToken = token
	}
	if err := r.modelCatalogClient.Start(ctx); err != nil {
		return nil, err
	}
	models, err := r.modelCatalogClient.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		item := ModelInfo{
			ID: model.ID, Name: model.Name,
			SupportedReasoningEfforts: append([]string(nil), model.SupportedReasoningEfforts...),
			DefaultReasoningEffort:    model.DefaultReasoningEffort,
		}
		if model.Capabilities.Limits.MaxContextWindowTokens != nil {
			limit := *model.Capabilities.Limits.MaxContextWindowTokens
			item.MaxContextWindowTokens = &limit
		}
		out = append(out, item)
	}
	return out, nil
}

// InvalidateModelCatalog removes the SDK's in-process catalog cache after a
// credential change. The next lookup starts a private runtime with the new PAT.
func (r *sdkRuntime) InvalidateModelCatalog() {
	r.modelCatalogMu.Lock()
	client := r.modelCatalogClient
	r.modelCatalogClient = nil
	r.modelCatalogToken = ""
	r.modelCatalogMu.Unlock()
	if client != nil {
		_ = client.Stop()
	}
}

type sdkSession struct {
	session     *sdk.Session
	correlation *interactionCorrelator
}

func (s sdkSession) ID() string { return s.session.SessionID }
func (s sdkSession) Send(ctx context.Context, prompt string) error {
	_, err := s.session.SendPrompt(ctx, prompt)
	return err
}
func (s sdkSession) SendMessage(ctx context.Context, message RuntimeMessage) error {
	_, err := s.session.Send(ctx, sdk.MessageOptions{
		Prompt: message.Prompt, AgentMode: sdk.AgentMode(message.Mode),
	})
	return err
}
func (s sdkSession) Abort(ctx context.Context) error { return s.session.Abort(ctx) }
func (s sdkSession) Disconnect() error {
	if s.correlation != nil {
		s.correlation.close()
	}
	return s.session.Disconnect()
}

func availableTools(config RuntimeConfig) []string {
	tools := sdk.NewToolSet()
	if config.OnUserInputRequest != nil {
		tools.AddBuiltIn("ask_user")
	}
	if config.OnPlanRequest != nil {
		tools.AddBuiltIn("exit_plan_mode")
	}
	if config.Tools == nil {
		for _, name := range []string{"list_files", "read_file", "search_files", "write_file", "run_command"} {
			tools.AddCustom(name)
		}
	} else {
		for _, tool := range config.Tools {
			tools.AddCustom(tool.Name)
		}
	}
	return tools.ToSlice()
}

func systemMessage(content string) *sdk.SystemMessageConfig {
	if content == "" {
		return nil
	}
	return &sdk.SystemMessageConfig{Mode: "append", Content: content}
}

func sdkTools(tools []RuntimeTool) []sdk.Tool {
	out := make([]sdk.Tool, 0, len(tools))
	for _, tool := range tools {
		t := tool
		out = append(out, sdk.Tool{
			Name: t.Name, Description: t.Description, Parameters: t.Schema,
			SkipPermission: true, Defer: sdk.ToolDeferNever,
			Handler: func(inv sdk.ToolInvocation) (sdk.ToolResult, error) {
				result, err := t.Handler(inv.Arguments)
				if err != nil {
					if errors.Is(err, errMutationNotAllowed) {
						return sdk.ToolResult{ResultType: "failure", Error: errMutationNotAllowed.Error()}, nil
					}
					return sdk.ToolResult{ResultType: "failure", Error: "tool operation failed"}, nil
				}
				return sdk.ToolResult{ResultType: "success", TextResultForLLM: result}, nil
			},
		})
	}
	return out
}

func eventAdapter(send func(RuntimeEvent), correlation *interactionCorrelator) sdk.SessionEventHandler {
	return func(event sdk.SessionEvent) {
		switch data := event.Data.(type) {
		case *sdk.AssistantMessageData:
			emitRuntimeEvent(send, RuntimeEvent{Type: "message", Text: data.Content})
		case *sdk.AssistantMessageDeltaData:
			emitRuntimeEvent(send, RuntimeEvent{Type: "message_delta", Text: data.DeltaContent})
		case *sdk.AssistantUsageData:
			emitRuntimeEvent(send, RuntimeEvent{Type: "usage", Input: value(data.InputTokens), Output: value(data.OutputTokens),
				Reasoning: value(data.ReasoningTokens), CacheRead: value(data.CacheReadTokens), CacheWrite: value(data.CacheWriteTokens)})
		case *sdk.ToolExecutionStartData:
			emitRuntimeEvent(send, RuntimeEvent{Type: "tool_start", ToolCallID: data.ToolCallID, ToolName: data.ToolName})
		case *sdk.ToolExecutionCompleteData:
			emitRuntimeEvent(send, RuntimeEvent{Type: "tool_complete", ToolCallID: data.ToolCallID, Success: data.Success})
		case *sdk.AssistantIdleData:
			emitRuntimeEvent(send, RuntimeEvent{Type: "idle"})
		case *sdk.SessionErrorData:
			// Session errors are handled by the manager, which exposes only a
			// generic failure to callers and keeps provider diagnostics private.
			emitRuntimeEvent(send, RuntimeEvent{Type: "error", Text: data.Message})
		case *sdk.UserInputRequestedData:
			if correlation != nil {
				correlation.pushInput(data.RequestID)
			}
			emitRuntimeEvent(send, RuntimeEvent{
				Type: "user_input_requested", RequestID: data.RequestID,
				Question: data.Question, Choices: append([]string(nil), data.Choices...),
				AllowFreeform: valueBool(data.AllowFreeform),
			})
		case *sdk.ExitPlanModeRequestedData:
			actions := make([]string, 0, len(data.Actions))
			for _, action := range data.Actions {
				actions = append(actions, string(action))
			}
			if correlation != nil {
				correlation.pushPlan(data.RequestID)
			}
			emitRuntimeEvent(send, RuntimeEvent{
				Type: "plan_requested", RequestID: data.RequestID,
				Summary: data.Summary, Plan: data.PlanContent, Actions: actions,
				RecommendedAction: string(data.RecommendedAction),
			})
		}
	}
}

func emitRuntimeEvent(send func(RuntimeEvent), event RuntimeEvent) {
	if send != nil {
		send(event)
	}
}

func value(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func valueBool(v *bool) bool {
	return v != nil && *v
}

func inputHandler(handler func(RuntimeInputRequest) (RuntimeInputResponse, error), correlation *interactionCorrelator) sdk.UserInputHandler {
	if handler == nil {
		return nil
	}
	return func(request sdk.UserInputRequest, _ sdk.UserInputInvocation) (sdk.UserInputResponse, error) {
		requestID, ok := correlation.takeInput()
		if !ok {
			return sdk.UserInputResponse{}, errors.New("Copilot input request was interrupted")
		}
		response, err := handler(RuntimeInputRequest{
			RequestID: requestID, Question: request.Question,
			Choices:       append([]string(nil), request.Choices...),
			AllowFreeform: valueBool(request.AllowFreeform),
		})
		if err != nil {
			return sdk.UserInputResponse{}, err
		}
		return sdk.UserInputResponse{Answer: response.Answer, WasFreeform: response.WasFreeform}, nil
	}
}

func planHandler(handler func(RuntimePlanRequest) (RuntimePlanResponse, error), correlation *interactionCorrelator) sdk.ExitPlanModeRequestHandler {
	if handler == nil {
		return nil
	}
	return func(request sdk.ExitPlanModeRequest, _ sdk.ExitPlanModeInvocation) (sdk.ExitPlanModeResult, error) {
		requestID, ok := correlation.takePlan()
		if !ok {
			return sdk.ExitPlanModeResult{}, errors.New("Copilot plan request was interrupted")
		}
		response, err := handler(RuntimePlanRequest{
			RequestID: requestID, Summary: request.Summary, Plan: request.PlanContent,
			Actions:           append([]string(nil), request.Actions...),
			RecommendedAction: request.RecommendedAction,
		})
		if err != nil {
			return sdk.ExitPlanModeResult{}, err
		}
		return sdk.ExitPlanModeResult{
			Approved: response.Approved, SelectedAction: response.SelectedAction,
			Feedback: response.Feedback,
		}, nil
	}
}

// The official SDK invokes direct callbacks without request IDs while emitting
// matching requested events with IDs. Its per-session event dispatcher is FIFO;
// these small FIFO channels preserve that ordering without placing a provider ID
// in a public response. Disconnect closes them so a stranded callback returns.
type interactionCorrelator struct {
	inputIDs chan string
	planIDs  chan string
	closed   chan struct{}
	once     sync.Once
}

func newInteractionCorrelator() *interactionCorrelator {
	return &interactionCorrelator{
		inputIDs: make(chan string, 8),
		planIDs:  make(chan string, 8),
		closed:   make(chan struct{}),
	}
}

func (c *interactionCorrelator) pushInput(id string) { c.push(c.inputIDs, id) }
func (c *interactionCorrelator) pushPlan(id string)  { c.push(c.planIDs, id) }

func (c *interactionCorrelator) push(queue chan string, id string) {
	if id == "" {
		return
	}
	select {
	case queue <- id:
	case <-c.closed:
	}
}

func (c *interactionCorrelator) takeInput() (string, bool) { return c.take(c.inputIDs) }
func (c *interactionCorrelator) takePlan() (string, bool)  { return c.take(c.planIDs) }

func (c *interactionCorrelator) take(queue <-chan string) (string, bool) {
	select {
	case id := <-queue:
		return id, true
	case <-c.closed:
		return "", false
	}
}

func (c *interactionCorrelator) close() {
	c.once.Do(func() { close(c.closed) })
}
