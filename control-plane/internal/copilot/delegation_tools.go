package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	backgroundTaskToolResultLimit      = 60 << 10
	backgroundTaskToolTextLimit        = 52 << 10
	backgroundTaskToolPromptLimit      = 2 << 10
	backgroundTaskToolErrorLimit       = 1 << 10
	backgroundTaskToolChangedFileLimit = 8
	backgroundTaskToolPathLimit        = 256
	backgroundTaskToolTruncationSuffix = "\n[truncated]"
)

func delegationTools(delegate BackgroundDelegate, parent BackgroundTaskRequest, gate *mutationGate) []RuntimeTool {
	runner := delegationToolRunner{delegate: delegate, parent: parent, gate: gate}
	return []RuntimeTool{
		{
			Name:        "delegate_task",
			Description: "Delegate an independent task to an isolated background Copilot agent. It returns a task ID immediately and never changes this workspace directly.",
			Schema: strictSchema(map[string]any{
				"task":  stringSchema(64 << 10),
				"label": stringSchema(256),
			}, "task"),
			Handler: runner.spawn,
		},
		{
			Name:        "list_delegated_tasks",
			Description: "List isolated background tasks delegated from this conversation, newest first. Results are omitted from these compact summaries; use get_delegated_task for one task's full bounded result.",
			Schema:      strictSchema(map[string]any{}),
			Handler:     runner.list,
		},
		{
			Name:        "get_delegated_task",
			Description: "Get the current status, result, and changed file list for one delegated background task.",
			Schema:      strictSchema(map[string]any{"task_id": stringSchema(128)}, "task_id"),
			Handler:     runner.get,
		},
		{
			Name:        "read_delegated_change",
			Description: "Read one file change from a completed delegated task before deciding whether to apply it in this workspace.",
			Schema: strictSchema(map[string]any{
				"task_id": stringSchema(128),
				"path":    stringSchema(1024),
			}, "task_id", "path"),
			Handler: runner.readChange,
		},
		{
			Name:        "cancel_delegated_task",
			Description: "Cancel one queued or running delegated background task.",
			Schema:      strictSchema(map[string]any{"task_id": stringSchema(128)}, "task_id"),
			Handler:     runner.cancel,
		},
	}
}

type delegationToolRunner struct {
	delegate BackgroundDelegate
	parent   BackgroundTaskRequest
	gate     *mutationGate
}

func (r delegationToolRunner) spawn(args any) (string, error) {
	if !r.mayMutate() {
		return "", errMutationNotAllowed
	}
	values, err := objectArguments(args, map[string]int{"task": 64 << 10, "label": 256})
	if err != nil || strings.TrimSpace(values["task"]) == "" {
		return "", errors.New("invalid delegated task")
	}
	task, err := r.delegate.SpawnBackgroundTask(context.Background(), BackgroundTaskRequest{
		ConversationID: r.parent.ConversationID,
		TurnID:         r.parent.TurnID,
		SandboxID:      r.parent.SandboxID,
		Task:           strings.TrimSpace(values["task"]),
		Label:          strings.TrimSpace(values["label"]),
	})
	if err != nil {
		return "", err
	}
	return marshalBackgroundTask(task)
}

func (r delegationToolRunner) list(args any) (string, error) {
	if _, err := objectArguments(args, map[string]int{}); err != nil {
		return "", err
	}
	tasks, err := r.delegate.ListBackgroundTasks(context.Background(), r.parent.ConversationID)
	if err != nil {
		return "", err
	}
	return marshalBackgroundTaskList(tasks)
}

func (r delegationToolRunner) get(args any) (string, error) {
	values, err := objectArguments(args, map[string]int{"task_id": 128})
	if err != nil || values["task_id"] == "" {
		return "", errors.New("invalid delegated task")
	}
	task, err := r.delegate.GetBackgroundTask(context.Background(), r.parent.ConversationID, values["task_id"])
	if err != nil {
		return "", err
	}
	return marshalBackgroundTask(task)
}

func (r delegationToolRunner) readChange(args any) (string, error) {
	values, err := objectArguments(args, map[string]int{"task_id": 128, "path": 1024})
	if err != nil || values["task_id"] == "" || values["path"] == "" {
		return "", errors.New("invalid delegated change")
	}
	change, err := r.delegate.ReadBackgroundTaskChange(context.Background(), r.parent.ConversationID,
		values["task_id"], values["path"])
	if err != nil {
		return "", err
	}
	return marshalBackgroundTaskChange(change)
}

func (r delegationToolRunner) cancel(args any) (string, error) {
	if !r.mayMutate() {
		return "", errMutationNotAllowed
	}
	values, err := objectArguments(args, map[string]int{"task_id": 128})
	if err != nil || values["task_id"] == "" {
		return "", errors.New("invalid delegated task")
	}
	task, err := r.delegate.CancelBackgroundTask(context.Background(), r.parent.ConversationID, values["task_id"])
	if err != nil {
		return "", err
	}
	return marshalBackgroundTask(task)
}

func (r delegationToolRunner) mayMutate() bool {
	return r.gate == nil || r.gate.Allowed()
}

func marshalBackgroundTask(task BackgroundTask) (string, error) {
	task = compactBackgroundTask(task)
	raw, err := json.Marshal(task)
	if err != nil {
		return "", err
	}
	if len(raw) <= backgroundTaskToolResultLimit {
		return string(raw), nil
	}

	result := task.Result
	task.Result = ""
	raw, err = json.Marshal(task)
	if err != nil {
		return "", err
	}
	if len(raw) > backgroundTaskToolResultLimit {
		task = minimalBackgroundTask(task)
	}
	task.Result, err = fitBackgroundTaskString(result, func(candidate string) (int, error) {
		task.Result = candidate
		raw, marshalErr := json.Marshal(task)
		return len(raw), marshalErr
	})
	if err != nil {
		return "", err
	}
	raw, err = json.Marshal(task)
	if err != nil {
		return "", err
	}
	if len(raw) > backgroundTaskToolResultLimit {
		return "", errors.New("delegated task result exceeds tool limit")
	}
	return string(raw), nil
}

type backgroundTaskList struct {
	Tasks     []backgroundTaskListItem `json:"tasks"`
	Truncated bool                     `json:"truncated,omitempty"`
}

type backgroundTaskListItem struct {
	ID               string `json:"id"`
	Label            string `json:"label,omitempty"`
	Task             string `json:"task,omitempty"`
	Status           string `json:"status"`
	PatchState       string `json:"patch_state"`
	ChangedFileCount int    `json:"changed_file_count"`
}

func marshalBackgroundTaskList(tasks []BackgroundTask) (string, error) {
	response := backgroundTaskList{Tasks: make([]backgroundTaskListItem, 0, len(tasks))}
	for i := len(tasks) - 1; i >= 0; i-- {
		task := tasks[i]
		response.Tasks = append(response.Tasks, backgroundTaskListItem{
			ID:               truncateBackgroundTaskText(task.ID, 128),
			Label:            truncateBackgroundTaskText(task.Label, 128),
			Task:             truncateBackgroundTaskText(task.Task, 128),
			Status:           truncateBackgroundTaskText(task.Status, 64),
			PatchState:       truncateBackgroundTaskText(task.PatchState, 64),
			ChangedFileCount: len(task.ChangedFiles),
		})
	}
	if raw, err := json.Marshal(response); err != nil {
		return "", err
	} else if len(raw) <= backgroundTaskToolResultLimit {
		return string(raw), nil
	}

	for i := range response.Tasks {
		response.Tasks[i].Label = truncateBackgroundTaskText(response.Tasks[i].Label, 32)
		response.Tasks[i].Task = truncateBackgroundTaskText(response.Tasks[i].Task, 32)
	}
	response.Truncated = true
	for len(response.Tasks) > 0 {
		raw, err := json.Marshal(response)
		if err != nil {
			return "", err
		}
		if len(raw) <= backgroundTaskToolResultLimit {
			return string(raw), nil
		}
		response.Tasks = response.Tasks[:len(response.Tasks)-1]
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return "", err
	}
	if len(raw) > backgroundTaskToolResultLimit {
		return "", errors.New("delegated task list exceeds tool limit")
	}
	return string(raw), nil
}

func marshalBackgroundTaskChange(change BackgroundTaskChange) (string, error) {
	change.TaskID = truncateBackgroundTaskText(change.TaskID, 128)
	change.BaseSHA256 = truncateBackgroundTaskText(change.BaseSHA256, 128)
	change.Content = truncateBackgroundTaskText(change.Content, 48<<10)
	raw, err := json.Marshal(change)
	if err != nil {
		return "", err
	}
	if len(raw) <= backgroundTaskToolResultLimit {
		return string(raw), nil
	}

	content := change.Content
	change.Content = ""
	change.Content, err = fitBackgroundTaskString(content, func(candidate string) (int, error) {
		change.Content = candidate
		raw, marshalErr := json.Marshal(change)
		return len(raw), marshalErr
	})
	if err != nil {
		return "", err
	}
	raw, err = json.Marshal(change)
	if err != nil {
		return "", err
	}
	if len(raw) > backgroundTaskToolResultLimit {
		return "", errors.New("delegated change exceeds tool limit")
	}
	return string(raw), nil
}

func compactBackgroundTask(task BackgroundTask) BackgroundTask {
	task.ID = truncateBackgroundTaskText(task.ID, 128)
	task.ParentTurnID = truncateBackgroundTaskText(task.ParentTurnID, 128)
	task.Label = truncateBackgroundTaskText(task.Label, 128)
	task.Task = truncateBackgroundTaskText(task.Task, backgroundTaskToolPromptLimit)
	task.Model = truncateBackgroundTaskText(task.Model, 128)
	task.ReasoningEffort = truncateBackgroundTaskText(task.ReasoningEffort, 64)
	task.ContextTier = truncateBackgroundTaskText(task.ContextTier, 64)
	task.Status = truncateBackgroundTaskText(task.Status, 64)
	task.Result = truncateBackgroundTaskText(task.Result, backgroundTaskToolTextLimit)
	task.ErrorMessage = truncateBackgroundTaskText(task.ErrorMessage, backgroundTaskToolErrorLimit)
	task.PatchState = truncateBackgroundTaskText(task.PatchState, 64)
	if len(task.ChangedFiles) > backgroundTaskToolChangedFileLimit {
		task.ChangedFiles = task.ChangedFiles[:backgroundTaskToolChangedFileLimit]
	}
	for i := range task.ChangedFiles {
		task.ChangedFiles[i] = truncateBackgroundTaskText(task.ChangedFiles[i], backgroundTaskToolPathLimit)
	}
	return task
}

func minimalBackgroundTask(task BackgroundTask) BackgroundTask {
	return BackgroundTask{
		ID:         truncateBackgroundTaskText(task.ID, 64),
		Status:     truncateBackgroundTaskText(task.Status, 32),
		PatchState: truncateBackgroundTaskText(task.PatchState, 32),
	}
}

func fitBackgroundTaskString(value string, measure func(string) (int, error)) (string, error) {
	size, err := measure(value)
	if err != nil {
		return "", err
	}
	if size <= backgroundTaskToolResultLimit {
		return value, nil
	}
	emptySize, err := measure("")
	if err != nil {
		return "", err
	}
	if emptySize > backgroundTaskToolResultLimit {
		return "", errors.New("delegated task metadata exceeds tool limit")
	}

	best := ""
	for low, high := 1, len(value); low <= high; {
		mid := low + (high-low)/2
		candidate := truncateBackgroundTaskText(value, mid)
		size, err := measure(candidate)
		if err != nil {
			return "", err
		}
		if size <= backgroundTaskToolResultLimit {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return best, nil
}

func truncateBackgroundTaskText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= len(backgroundTaskToolTruncationSuffix) {
		return ""
	}
	end := limit - len(backgroundTaskToolTruncationSuffix)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + backgroundTaskToolTruncationSuffix
}
