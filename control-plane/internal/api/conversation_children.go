package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/agentprompt"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/copilot"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/sandboxname"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

const (
	maxConcurrentConversationChildren      = 4
	maxConversationChildrenPerConversation = 4
	maxConversationChildrenGlobal          = 16
	conversationChildExecutionTimeout      = 20 * time.Minute
	conversationChildResultLimit           = 60 << 10
)

var (
	errConversationChildLimit       = errors.New("delegated task limit reached")
	errConversationChildUnavailable = errors.New("delegated tasks are unavailable")
	errConversationChildPatch       = errors.New("delegated task patch is unavailable")
)

// conversationChildRun owns the local cancellation signal for one isolated
// worker. The durable conversation_child row remains the source of truth.
type conversationChildRun struct {
	id     string
	ctx    context.Context
	cancel context.CancelFunc
}

// SpawnBackgroundTask is the parent-only Copilot tool entry point. The caller
// never controls the worker path, container target, model settings, or SDK
// session ID.
func (c *ConversationCoordinator) SpawnBackgroundTask(ctx context.Context, request copilot.BackgroundTaskRequest) (copilot.BackgroundTask, error) {
	if c == nil || c.server == nil || c.server.Store == nil || c.server.Copilot == nil ||
		c.server.usesRuntimeProvider() || c.server.Docker == nil || c.server.Loopback == nil || request.ConversationID == "" ||
		request.TurnID == "" || request.SandboxID == "" || strings.TrimSpace(request.Task) == "" {
		return copilot.BackgroundTask{}, errConversationChildUnavailable
	}

	conversation, err := c.server.Store.GetConversation(ctx, request.ConversationID)
	if err != nil {
		return copilot.BackgroundTask{}, err
	}
	if conversation.SandboxID != request.SandboxID || conversation.ArchivedAt.Valid ||
		!conversation.ActiveTurnID.Valid || conversation.ActiveTurnID.String != request.TurnID {
		return copilot.BackgroundTask{}, store.ErrConflict
	}
	parentTurn, err := c.server.Store.GetConversationTurn(ctx, request.TurnID)
	if err != nil {
		return copilot.BackgroundTask{}, err
	}
	if parentTurn.ConversationID != conversation.ID || parentTurn.Status != store.ConversationTurnRunning {
		return copilot.BackgroundTask{}, store.ErrConflict
	}

	childID := newULID()
	workspacePath, _, err := c.conversationChildWorkspacePaths(childID)
	if err != nil {
		return copilot.BackgroundTask{}, err
	}
	child := &store.ConversationChild{
		ID:             childID,
		ConversationID: request.ConversationID,
		ParentTurnID:   request.TurnID,
		Label:          request.Label,
		Prompt:         request.Task,
		WorkspacePath:  workspacePath,
	}

	// Store admission is serialized separately from the callback/run map. The
	// counts include queued records so a busy parent cannot create an unbounded
	// in-memory wait list behind the worker semaphore.
	c.childAdmissions.Lock()
	defer c.childAdmissions.Unlock()
	perConversation, err := c.server.Store.CountActiveConversationChildren(ctx, conversation.ID)
	if err != nil {
		return copilot.BackgroundTask{}, err
	}
	if perConversation >= maxConversationChildrenPerConversation {
		return copilot.BackgroundTask{}, errConversationChildLimit
	}
	global, err := c.server.Store.CountAllActiveConversationChildren(ctx)
	if err != nil {
		return copilot.BackgroundTask{}, err
	}
	if global >= maxConversationChildrenGlobal {
		return copilot.BackgroundTask{}, errConversationChildLimit
	}
	if err := c.server.Store.CreateConversationChild(ctx, child); err != nil {
		return copilot.BackgroundTask{}, err
	}
	c.startConversationChild(child.ID)
	c.notify(conversation.ID)
	return copilotBackgroundTask(child), nil
}

func (c *ConversationCoordinator) ListBackgroundTasks(ctx context.Context, conversationID string) ([]copilot.BackgroundTask, error) {
	if c == nil || c.server == nil || c.server.Store == nil {
		return nil, errConversationChildUnavailable
	}
	children, err := c.server.Store.ListConversationChildren(ctx, conversationID, 100)
	if err != nil {
		return nil, err
	}
	out := make([]copilot.BackgroundTask, 0, len(children))
	for _, child := range children {
		out = append(out, copilotBackgroundTask(child))
	}
	return out, nil
}

func (c *ConversationCoordinator) GetBackgroundTask(ctx context.Context, conversationID, id string) (copilot.BackgroundTask, error) {
	if c == nil || c.server == nil || c.server.Store == nil {
		return copilot.BackgroundTask{}, errConversationChildUnavailable
	}
	child, err := c.server.Store.GetConversationChildForConversation(ctx, conversationID, id)
	if err != nil {
		return copilot.BackgroundTask{}, err
	}
	return copilotBackgroundTask(child), nil
}

func (c *ConversationCoordinator) ReadBackgroundTaskChange(ctx context.Context, conversationID, id, requestedPath string) (copilot.BackgroundTaskChange, error) {
	if c == nil || c.server == nil || c.server.Store == nil {
		return copilot.BackgroundTaskChange{}, errConversationChildUnavailable
	}
	child, err := c.server.Store.GetConversationChildForConversation(ctx, conversationID, id)
	if err != nil {
		return copilot.BackgroundTaskChange{}, err
	}
	patch, err := child.Patch()
	if err != nil {
		return copilot.BackgroundTaskChange{}, errConversationChildPatch
	}
	for _, change := range patch.Changes {
		if change.Path == requestedPath {
			return copilot.BackgroundTaskChange{
				TaskID: child.ID, Path: change.Path, BaseSHA256: change.BaseSHA256,
				Content: change.Content, Deleted: change.Deleted, Mode: change.Mode,
			}, nil
		}
	}
	return copilot.BackgroundTaskChange{}, store.ErrNotFound
}

func (c *ConversationCoordinator) CancelBackgroundTask(ctx context.Context, conversationID, id string) (copilot.BackgroundTask, error) {
	if c == nil || c.server == nil || c.server.Store == nil {
		return copilot.BackgroundTask{}, errConversationChildUnavailable
	}
	child, err := c.server.Store.RequestConversationChildCancellation(ctx, conversationID, id)
	if err != nil {
		return copilot.BackgroundTask{}, err
	}
	c.mu.Lock()
	run := c.childRuns[id]
	c.mu.Unlock()
	if run != nil {
		run.cancel()
		if c.server.Copilot != nil {
			c.server.Copilot.CancelConversation(id)
		}
	} else {
		finished, finishErr := c.server.Store.FinishConversationChild(ctx, id,
			store.ConversationChildCancelled, "", "", store.ConversationChildPatchNone, nil)
		if finishErr == nil {
			child = finished
		} else if !errors.Is(finishErr, store.ErrConflict) {
			return copilot.BackgroundTask{}, finishErr
		}
		c.cleanupConversationChild(child)
	}
	c.notify(conversationID)
	return copilotBackgroundTask(child), nil
}

func (c *ConversationCoordinator) startConversationChild(id string) {
	if c == nil || c.server == nil || c.server.usesRuntimeProvider() || id == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &conversationChildRun{id: id, ctx: ctx, cancel: cancel}
	c.mu.Lock()
	if c.childRuns == nil {
		c.childRuns = make(map[string]*conversationChildRun)
	}
	if _, exists := c.childRuns[id]; exists {
		c.mu.Unlock()
		cancel()
		return
	}
	c.childRuns[id] = run
	c.mu.Unlock()
	go c.executeConversationChild(run)
}

func (c *ConversationCoordinator) executeConversationChild(run *conversationChildRun) {
	if c == nil || c.server == nil {
		return
	}
	defer c.clearConversationChildRun(run)
	if c.server.usesRuntimeProvider() {
		c.finishConversationChild(run.id, store.ConversationChildFailed, "",
			"Delegated Copilot workers are unavailable with the runtime provider.",
			store.ConversationChildPatchNone, nil)
		return
	}

	slots := c.conversationChildSlots()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-run.ctx.Done():
		c.finishConversationChild(run.id, store.ConversationChildCancelled, "", "",
			store.ConversationChildPatchNone, nil)
		return
	}

	child, err := c.server.Store.ClaimConversationChild(context.Background(), run.id)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			c.finishConversationChild(run.id, store.ConversationChildCancelled, "", "",
				store.ConversationChildPatchNone, nil)
		} else {
			c.logError("claim delegated Copilot task", err)
		}
		return
	}
	defer c.cleanupConversationChild(child)
	c.notify(child.ConversationID)

	executionCtx, cancel := context.WithTimeout(run.ctx, conversationChildExecutionTimeout)
	defer cancel()
	if err := c.provisionConversationChildWorkspace(executionCtx, child); err != nil {
		if !errors.Is(err, context.Canceled) {
			c.logError("prepare delegated Copilot workspace", err)
		}
		c.finishConversationChild(run.id, store.ConversationChildFailed, "",
			"Could not prepare the isolated delegated workspace.", store.ConversationChildPatchNone, nil)
		return
	}
	if executionCtx.Err() != nil {
		c.finishConversationChild(run.id, store.ConversationChildCancelled, "", "",
			store.ConversationChildPatchNone, nil)
		return
	}

	workerName := copilot.BackgroundWorkerContainerName(child.ID)
	if err := c.startConversationChildWorker(executionCtx, child, workerName); err != nil {
		if !errors.Is(err, context.Canceled) {
			c.logError("start delegated Copilot worker", err)
		}
		c.finishConversationChild(run.id, store.ConversationChildFailed, "",
			"Could not start the isolated delegated worker.", store.ConversationChildPatchNone, nil)
		return
	}
	child, err = c.server.Store.StartConversationChild(context.Background(), child.ID, workerName)
	if err != nil {
		c.logError("record delegated Copilot worker", err)
		c.finishConversationChild(run.id, store.ConversationChildFailed, "",
			"Could not start the isolated delegated worker.", store.ConversationChildPatchNone, nil)
		return
	}
	c.notify(child.ConversationID)

	collector := &conversationChildCollector{}
	err = c.server.Copilot.RunConversationTurn(executionCtx, copilot.ConversationTurnRequest{
		ConversationID:     child.ID,
		SandboxID:          c.childSandboxID(child),
		WorkspaceContainer: workerName,
		TurnID:             child.ID,
		Prompt:             child.Prompt,
		Mode:               copilot.ConversationModeAutopilot,
		Model:              child.Model,
		ReasoningEffort:    child.ReasoningEffort,
		ContextTier:        child.ContextTier,
		SystemPrompt:       c.conversationChildSystemPrompt(child),
		OnEvent:            collector.record,
	})
	status, result, failure := c.classifyConversationChildOutcome(executionCtx, err, collector.text())
	patchState, patch := c.captureConversationChildPatch(child)
	c.finishConversationChild(child.ID, status, result, failure, patchState, patch)
}

func (c *ConversationCoordinator) childSandboxID(child *store.ConversationChild) string {
	conversation, err := c.server.Store.GetConversation(context.Background(), child.ConversationID)
	if err != nil {
		return ""
	}
	return conversation.SandboxID
}

func (c *ConversationCoordinator) conversationChildSystemPrompt(_ *store.ConversationChild) string {
	return agentprompt.Render(agentprompt.Vars{
		AppDir:     "/home/sandbox/workspace/app",
		HealthPath: "/",
	}) + `

You are an isolated background worker helping another Copilot agent. Complete
the assigned task within this workspace copy. Do not ask the user questions;
choose a reasonable assumption if necessary. Do not use or expect delegation
tools. Your workspace is disposable and your changes are not applied anywhere
automatically. Finish with a concise account of the work, decisions, and any
limitations so the parent agent can review your result and patch.`
}

func (c *ConversationCoordinator) classifyConversationChildOutcome(ctx context.Context, err error, result string) (status, output, failure string) {
	if ctx.Err() != nil {
		return store.ConversationChildCancelled, "", ""
	}
	if err != nil {
		return store.ConversationChildFailed, result, "The delegated Copilot agent could not complete its task."
	}
	return store.ConversationChildSucceeded, result, ""
}

func (c *ConversationCoordinator) provisionConversationChildWorkspace(ctx context.Context, child *store.ConversationChild) (err error) {
	if c.server.usesRuntimeProvider() || c.server.Loopback == nil || c.server.Docker == nil {
		return errConversationChildUnavailable
	}
	conversation, err := c.server.Store.GetConversation(ctx, child.ConversationID)
	if err != nil {
		return err
	}
	if err := c.ensureSandboxRunning(ctx, conversation.SandboxID); err != nil {
		return err
	}
	sandbox, err := c.server.Store.Get(ctx, conversation.SandboxID)
	if err != nil {
		return err
	}
	parentContainer := sandboxname.Reference(sandbox.ID, sandbox.ContainerID.String)
	_, sourceRoot := c.server.Loopback.Paths(conversation.SandboxID)
	sourceApp := filepath.Join(sourceRoot, "workspace", "app")
	workerRoot, baselineRoot, err := c.conversationChildWorkspacePaths(child.ID)
	if err != nil {
		return err
	}
	if filepath.Clean(workerRoot) != filepath.Clean(child.WorkspacePath) ||
		!pathUnderRoot(filepath.Clean(sourceApp), c.server.Loopback.Root) {
		return errConversationChildUnavailable
	}
	if info, statErr := os.Stat(sourceApp); statErr != nil || !info.IsDir() {
		if statErr != nil {
			return statErr
		}
		return errors.New("sandbox workspace app is unavailable")
	}

	if c.server.Locks != nil {
		c.server.Locks.Lock(conversation.SandboxID)
		defer c.server.Locks.Unlock(conversation.SandboxID)
	}
	pauseCtx, pauseCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := c.server.Docker.Pause(pauseCtx, parentContainer); err != nil {
		pauseCancel()
		return fmt.Errorf("pause parent sandbox: %w", err)
	}
	pauseCancel()
	paused := true
	defer func() {
		if !paused {
			return
		}
		unpauseCtx, unpauseCancel := context.WithTimeout(context.Background(), 30*time.Second)
		unpauseErr := c.server.Docker.Unpause(unpauseCtx, parentContainer)
		unpauseCancel()
		if unpauseErr != nil {
			c.logError("unpause parent sandbox after delegated copy", unpauseErr)
			if err == nil {
				err = fmt.Errorf("unpause parent sandbox: %w", unpauseErr)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.removeConversationChildWorkspace(child); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(workerRoot, "workspace", "app"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(baselineRoot, 0o755); err != nil {
		return err
	}
	if err := copyTreeExcluding(sourceApp, filepath.Join(workerRoot, "workspace", "app"), conversationChildIgnoredNames); err != nil {
		return fmt.Errorf("copy child workspace: %w", err)
	}
	if err := copyTreeExcluding(sourceApp, baselineRoot, conversationChildIgnoredNames); err != nil {
		return fmt.Errorf("copy child baseline: %w", err)
	}
	if err := c.server.Loopback.NormalizeOwnership(workerRoot); err != nil {
		return fmt.Errorf("normalize child workspace: %w", err)
	}
	return nil
}

func (c *ConversationCoordinator) startConversationChildWorker(ctx context.Context, child *store.ConversationChild, workerName string) error {
	if c.server == nil || c.server.usesRuntimeProvider() || c.server.Docker == nil {
		return errConversationChildUnavailable
	}
	conversation, err := c.server.Store.GetConversation(ctx, child.ConversationID)
	if err != nil {
		return err
	}
	sandbox, err := c.server.Store.Get(ctx, conversation.SandboxID)
	if err != nil {
		return err
	}
	workerRoot, _, err := c.conversationChildWorkspacePaths(child.ID)
	if err != nil {
		return err
	}
	if filepath.Clean(workerRoot) != filepath.Clean(child.WorkspacePath) {
		return errConversationChildUnavailable
	}
	volumes := []string{workerRoot + ":/home/sandbox"}
	if c.server.DNSResolvConf != "" {
		volumes = append(volumes, c.server.DNSResolvConf+":/etc/resolv.conf:ro")
	}
	_, err = c.server.Docker.Run(ctx, docker.RunSpec{
		Name:     workerName,
		Hostname: workerName,
		// Delegated workers need outbound access for package installs but must
		// not join the sandbox/Traefik network or gain preview reachability.
		Network:     "",
		Userns:      c.server.Userns,
		Runtime:     c.server.Runtime,
		ReadOnly:    true,
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		CPUShares:   50,
		Memory:      "4g",
		MemorySwap:  "4g",
		PidsLimit:   512,
		Ulimits:     []string{"nofile=65536:65536"},
		Tmpfs:       []string{"/tmp:size=256m", "/var/tmp:size=64m"},
		Volumes:     volumes,
		Labels:      []string{"sandboxd.delegated-worker=true"},
		Image:       sandbox.Image,
		Cmd:         []string{"sleep", "infinity"},
	})
	return err
}

func (c *ConversationCoordinator) captureConversationChildPatch(child *store.ConversationChild) (string, *store.ConversationChildPatch) {
	if c == nil || c.server == nil || c.server.usesRuntimeProvider() {
		return store.ConversationChildPatchUnavailable, nil
	}
	workerRoot, baselineRoot, err := c.conversationChildWorkspacePaths(child.ID)
	if err != nil || filepath.Clean(workerRoot) != filepath.Clean(child.WorkspacePath) {
		return store.ConversationChildPatchUnavailable, nil
	}
	state, patch, err := buildConversationChildPatch(baselineRoot, filepath.Join(workerRoot, "workspace", "app"))
	if err != nil {
		c.logError("capture delegated Copilot patch", err)
		return store.ConversationChildPatchUnavailable, nil
	}
	return state, patch
}

func (c *ConversationCoordinator) finishConversationChild(id, status, result, failure, patchState string, patch *store.ConversationChildPatch) {
	child, err := c.server.Store.GetConversationChild(context.Background(), id)
	if err != nil {
		c.logError("load delegated Copilot task for completion", err)
		return
	}
	if conversationChildTerminal(child.Status) {
		return
	}
	if child.Status == store.ConversationChildCancelling {
		status, result, failure, patchState, patch = store.ConversationChildCancelled, "", "",
			store.ConversationChildPatchNone, nil
	}
	finished, err := c.server.Store.FinishConversationChild(context.Background(), id, status,
		result, failure, patchState, patch)
	if err != nil && !errors.Is(err, store.ErrConflict) {
		c.logError("finish delegated Copilot task", err)
		return
	}
	if finished != nil {
		c.notify(finished.ConversationID)
	} else {
		c.notify(child.ConversationID)
	}
}

func (c *ConversationCoordinator) clearConversationChildRun(run *conversationChildRun) {
	if c == nil || run == nil {
		return
	}
	c.mu.Lock()
	if c.childRuns[run.id] == run {
		delete(c.childRuns, run.id)
	}
	c.mu.Unlock()
}

func (c *ConversationCoordinator) cleanupConversationChild(child *store.ConversationChild) {
	if c == nil || child == nil {
		return
	}
	if c.server != nil && c.server.usesRuntimeProvider() {
		return
	}
	if c.server != nil && c.server.Docker != nil {
		name := child.WorkerContainer
		if name == "" {
			name = copilot.BackgroundWorkerContainerName(child.ID)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := c.server.Docker.Remove(cleanupCtx, name); err != nil {
			c.logError("remove delegated Copilot worker", err)
		}
		cancel()
	}
	if c.server != nil && c.server.Copilot != nil {
		if err := c.server.Copilot.CleanupConversation(child.ID); err != nil {
			c.logError("remove delegated Copilot session", err)
		}
	}
	if err := c.removeConversationChildWorkspace(child); err != nil {
		c.logError("remove delegated Copilot workspace", err)
	}
}

func (c *ConversationCoordinator) conversationChildWorkspacePaths(id string) (workerRoot, baselineRoot string, err error) {
	if c == nil || c.server == nil || c.server.usesRuntimeProvider() || c.server.Loopback == nil || !isULID(id) ||
		c.server.Loopback.Root == "" {
		return "", "", errConversationChildUnavailable
	}
	root := filepath.Clean(c.server.Loopback.Root)
	workersRoot := filepath.Join(root, "_delegates")
	baselinesRoot := filepath.Join(root, "_delegate-baselines")
	workerRoot = filepath.Join(workersRoot, id)
	baselineRoot = filepath.Join(baselinesRoot, id)
	if !pathUnderRoot(workerRoot, workersRoot) || !pathUnderRoot(baselineRoot, baselinesRoot) {
		return "", "", errConversationChildUnavailable
	}
	return workerRoot, baselineRoot, nil
}

func (c *ConversationCoordinator) removeConversationChildWorkspace(child *store.ConversationChild) error {
	if child == nil {
		return nil
	}
	if c != nil && c.server != nil && c.server.usesRuntimeProvider() {
		return nil
	}
	workerRoot, baselineRoot, err := c.conversationChildWorkspacePaths(child.ID)
	if err != nil {
		return err
	}
	if child.WorkspacePath != "" && filepath.Clean(child.WorkspacePath) != filepath.Clean(workerRoot) {
		return errConversationChildUnavailable
	}
	if err := os.RemoveAll(workerRoot); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(baselineRoot); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (c *ConversationCoordinator) recoverConversationChildren(ctx context.Context) {
	if c == nil || c.server == nil || c.server.Store == nil {
		return
	}
	if c.server.usesRuntimeProvider() {
		children, err := c.server.Store.InterruptAllActiveConversationChildren(ctx,
			"Delegated Copilot work is unavailable with the runtime provider.")
		if err != nil {
			c.logError("recover delegated Copilot tasks", err)
			return
		}
		for _, child := range children {
			c.notify(child.ConversationID)
		}
		return
	}
	children, err := c.server.Store.InterruptAllActiveConversationChildren(ctx,
		"Delegated Copilot work was interrupted because sandboxd restarted.")
	if err != nil {
		c.logError("recover delegated Copilot tasks", err)
		return
	}
	for _, child := range children {
		c.stopConversationChild(child)
		c.notify(child.ConversationID)
	}
}

func (c *ConversationCoordinator) interruptConversationChildren(ctx context.Context, conversationID, message string) {
	if c == nil || c.server == nil || c.server.Store == nil {
		return
	}
	children, err := c.server.Store.InterruptActiveConversationChildren(ctx, conversationID, message)
	if err != nil {
		c.logError("interrupt delegated Copilot tasks", err)
		return
	}
	for _, child := range children {
		c.stopConversationChild(child)
		c.notify(child.ConversationID)
	}
}

func (c *ConversationCoordinator) interruptAllConversationChildren(ctx context.Context, message string) {
	if c == nil || c.server == nil || c.server.Store == nil {
		return
	}
	children, err := c.server.Store.InterruptAllActiveConversationChildren(ctx, message)
	if err != nil {
		c.logError("interrupt all delegated Copilot tasks", err)
		return
	}
	for _, child := range children {
		c.stopConversationChild(child)
		c.notify(child.ConversationID)
	}
}

func (c *ConversationCoordinator) stopConversationChild(child *store.ConversationChild) {
	if c != nil && c.server != nil && c.server.usesRuntimeProvider() {
		c.mu.Lock()
		run := c.childRuns[child.ID]
		c.mu.Unlock()
		if run != nil {
			run.cancel()
			if c.server.Copilot != nil {
				c.server.Copilot.CancelConversation(child.ID)
			}
		}
		return
	}
	c.mu.Lock()
	run := c.childRuns[child.ID]
	c.mu.Unlock()
	if run != nil {
		run.cancel()
		if c.server.Copilot != nil {
			c.server.Copilot.CancelConversation(child.ID)
		}
		return
	}
	c.cleanupConversationChild(child)
}

func copilotBackgroundTask(child *store.ConversationChild) copilot.BackgroundTask {
	if child == nil {
		return copilot.BackgroundTask{}
	}
	task := copilot.BackgroundTask{
		ID: child.ID, ParentTurnID: child.ParentTurnID, Label: child.Label, Task: child.Prompt,
		Model: child.Model, ReasoningEffort: child.ReasoningEffort, ContextTier: child.ContextTier,
		Status: child.Status, Result: child.Result, PatchState: child.PatchState,
		ChangedFiles: child.ChangedFiles(),
	}
	if child.ErrorMessage.Valid {
		task.ErrorMessage = child.ErrorMessage.String
	}
	return task
}

type conversationChildCollector struct {
	textBuilder strings.Builder
}

func (c *conversationChildCollector) record(event copilot.Envelope) {
	if event.Type != "message" || event.Text == "" || c.textBuilder.Len() >= conversationChildResultLimit {
		return
	}
	remaining := conversationChildResultLimit - c.textBuilder.Len()
	if len(event.Text) > remaining {
		event.Text = event.Text[:remaining]
	}
	c.textBuilder.WriteString(event.Text)
}

func (c *conversationChildCollector) text() string {
	return c.textBuilder.String()
}

var conversationChildIgnoredNames = func() map[string]bool {
	ignored := make(map[string]bool, len(snapshotIgnoreDirs)+5)
	for name := range snapshotIgnoreDirs {
		ignored[name] = true
	}
	// The worker cannot safely use the parent's repository metadata or runtime
	// socket. It still receives all project source files and lockfiles.
	ignored[".git"] = true
	ignored[".runtimed"] = true
	// A worker uses a private workspace copy, so app-local credentials must not
	// cross this boundary with project source.
	ignored[".env*"] = true
	ignored[".npmrc"] = true
	ignored[".netrc"] = true
	ignored[".pypirc"] = true
	return ignored
}()

type conversationChildFileState struct {
	kind    fs.FileMode
	mode    fs.FileMode
	sha256  string
	present bool
}

func buildConversationChildPatch(baselineRoot, workerRoot string) (string, *store.ConversationChildPatch, error) {
	baseline, err := collectConversationChildFiles(baselineRoot)
	if err != nil {
		return "", nil, err
	}
	worker, err := collectConversationChildFiles(workerRoot)
	if err != nil {
		return "", nil, err
	}
	paths := make(map[string]struct{}, len(baseline)+len(worker))
	for path := range baseline {
		paths[path] = struct{}{}
	}
	for path := range worker {
		paths[path] = struct{}{}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)

	patch := &store.ConversationChildPatch{}
	totalPatchBytes := 0
	for _, path := range ordered {
		before, hadBefore := baseline[path]
		after, hasAfter := worker[path]
		if sameConversationChildFile(before, hadBefore, after, hasAfter) {
			continue
		}
		if (hadBefore && before.kind != 0) || (hasAfter && after.kind != 0) {
			return "", nil, errConversationChildPatch
		}
		change := store.ConversationChildChange{Path: path}
		if hadBefore {
			change.BaseSHA256 = before.sha256
		}
		if !hasAfter {
			change.Deleted = true
			patch.Changes = append(patch.Changes, change)
			totalPatchBytes += len(change.Path) + len(change.BaseSHA256)
			if len(patch.Changes) > store.MaxConversationChildPatchFiles ||
				totalPatchBytes > store.MaxConversationChildPatchBytes {
				return "", nil, errConversationChildPatch
			}
			continue
		}
		if after.mode != 0 {
			change.Mode = uint32(after.mode)
		}
		content, err := os.ReadFile(filepath.Join(workerRoot, filepath.FromSlash(path)))
		if err != nil {
			return "", nil, err
		}
		if len(content) > store.MaxConversationChildPatchFileBytes || !utf8.Valid(content) ||
			strings.IndexByte(string(content), 0) >= 0 {
			return "", nil, errConversationChildPatch
		}
		change.Content = string(content)
		patch.Changes = append(patch.Changes, change)
		totalPatchBytes += len(change.Path) + len(change.BaseSHA256) + len(change.Content)
		if len(patch.Changes) > store.MaxConversationChildPatchFiles ||
			totalPatchBytes > store.MaxConversationChildPatchBytes {
			return "", nil, errConversationChildPatch
		}
	}
	if len(patch.Changes) == 0 {
		return store.ConversationChildPatchNone, nil, nil
	}
	raw, err := json.Marshal(patch)
	if err != nil || len(raw) > store.MaxConversationChildPatchBytes {
		return "", nil, errConversationChildPatch
	}
	return store.ConversationChildPatchAvailable, patch, nil
}

func collectConversationChildFiles(root string) (map[string]conversationChildFileState, error) {
	files := make(map[string]conversationChildFileState)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if treeEntryIgnored(conversationChildIgnoredNames, entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !validConversationChildPatchPath(rel) {
			return errConversationChildPatch
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		state := conversationChildFileState{
			kind:    info.Mode() & fs.ModeType,
			mode:    info.Mode().Perm(),
			present: true,
		}
		switch {
		case info.Mode().IsRegular():
			digest, err := sha256File(path)
			if err != nil {
				return err
			}
			state.sha256 = digest
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			digest := sha256.Sum256([]byte(target))
			state.sha256 = hex.EncodeToString(digest[:])
		default:
			state.sha256 = "special"
		}
		files[rel] = state
		return nil
	})
	return files, err
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func sameConversationChildFile(before conversationChildFileState, hadBefore bool, after conversationChildFileState, hasAfter bool) bool {
	return hadBefore == hasAfter && (!hadBefore ||
		(before.kind == after.kind && before.mode == after.mode && before.sha256 == after.sha256))
}

func validConversationChildPatchPath(value string) bool {
	if value == "" || len(value) > 1024 || strings.HasPrefix(value, "/") ||
		strings.Contains(value, "\\") || strings.ContainsRune(value, 0) {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func (c *ConversationCoordinator) conversationChildSlots() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.childSlots == nil {
		c.childSlots = make(chan struct{}, maxConcurrentConversationChildren)
	}
	return c.childSlots
}

func conversationChildTerminal(status string) bool {
	switch status {
	case store.ConversationChildSucceeded, store.ConversationChildFailed,
		store.ConversationChildCancelled, store.ConversationChildInterrupted:
		return true
	default:
		return false
	}
}
