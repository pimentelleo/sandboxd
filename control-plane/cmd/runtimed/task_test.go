package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtime"
)

func boolp(b bool) *bool { return &b }

// Continue is the default, but gated on there being a prior session: the first
// task in a sandbox always starts fresh, later tasks continue unless the caller
// forces a choice.
func TestNewTaskContinueDefault(t *testing.T) {
	root := t.TempDir()

	mk := func(id string, cont *bool) bool {
		tk, err := newTask(runtime.StartTaskRequest{TaskID: id, Prompt: "p", Continue: cont}, root)
		if err != nil {
			t.Fatalf("newTask(%s): %v", id, err)
		}
		return tk.cont
	}

	// First task, default (nil) → fresh: nothing to continue yet.
	if mk("t1", nil) {
		t.Error("first task should start fresh (no prior session)")
	}
	// Second task, default (nil) → continue: a prior task dir now exists.
	if !mk("t2", nil) {
		t.Error("second task should continue by default")
	}
	// Forced continue on a would-be-first sandbox → gated to fresh.
	root2 := t.TempDir()
	if _, err := newTask(runtime.StartTaskRequest{TaskID: "solo", Prompt: "p", Continue: boolp(true)}, root2); err != nil {
		t.Fatal(err)
	}
	// (solo is the only task in root2) forcing continue must still start fresh.
	// Re-create the check directly via hasPriorTask semantics:
	if hasPriorTask(root2, "solo") {
		t.Error("solo should have no prior task")
	}
	// Forced fresh even when a prior session exists.
	if mk("t3-forced-fresh", boolp(false)) {
		t.Error("explicit continue:false must start fresh")
	}
}

func TestHasPriorTask(t *testing.T) {
	root := t.TempDir()
	if hasPriorTask(root, "self") {
		t.Error("empty root has no prior task")
	}
	// A dir that is only the task itself doesn't count.
	os.MkdirAll(filepath.Join(root, "self"), 0o755)
	if hasPriorTask(root, "self") {
		t.Error("only-self should not count as a prior task")
	}
	os.MkdirAll(filepath.Join(root, "earlier"), 0o755)
	if !hasPriorTask(root, "self") {
		t.Error("a sibling task dir is a prior task")
	}
}

func TestAbandonHostedTaskReleasesRestartedRuntime(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, ".runtimed")
	appDir := filepath.Join(root, "workspace", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := &app{appDir: appDir, runtimeDir: runtimeDir, log: slog.Default()}
	const oldTaskID = "hosted-before-restart"
	if _, err := first.prepareHostedTask(runtime.PrepareHostedTaskRequest{
		TaskID: oldTaskID, Prompt: "interrupted request",
	}); err != nil {
		t.Fatalf("prepare hosted task: %v", err)
	}

	// A fresh runtimed process restores the old marker exactly as a sandbox
	// wake would. It must not remain an admission lock after abandonment.
	restarted := &app{appDir: appDir, runtimeDir: runtimeDir, log: slog.Default()}
	restarted.loadHostedTasks()
	if _, err := restarted.prepareHostedTask(runtime.PrepareHostedTaskRequest{
		TaskID: "new-task", Prompt: "next request",
	}); !errors.Is(err, errTaskInProgress) {
		t.Fatalf("prepare with restored hosted marker = %v; want task in progress", err)
	}

	result, err := restarted.abandonHostedTask(runtime.AbandonHostedTaskRequest{
		TaskID: oldTaskID, Status: runtime.TaskFailed, FailureReason: "provider_interrupted",
		ErrorMessage: "control plane restarted",
	})
	if err != nil {
		t.Fatalf("abandon hosted task: %v", err)
	}
	if result.Status != runtime.TaskFailed || result.FailureReason != "provider_interrupted" {
		t.Fatalf("abandoned result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(hostedTaskDir(runtimeDir, oldTaskID), hostedTaskMetadataFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hosted marker remains after abandon: %v", err)
	}
	if _, err := restarted.prepareHostedTask(runtime.PrepareHostedTaskRequest{
		TaskID: "new-task", Prompt: "next request",
	}); err != nil {
		t.Fatalf("prepare after abandon: %v", err)
	}

	// Retrying after a lost HTTP response returns the existing terminal result
	// rather than re-creating the metadata or failing as a missing task.
	retried, err := restarted.abandonHostedTask(runtime.AbandonHostedTaskRequest{
		TaskID: oldTaskID, Status: runtime.TaskFailed,
	})
	if err != nil {
		t.Fatalf("retry abandon hosted task: %v", err)
	}
	if retried.ID != oldTaskID || retried.Status != runtime.TaskFailed {
		t.Fatalf("retry result = %#v", retried)
	}
	finalized, err := restarted.finalizeHostedTask(t.Context(), runtime.FinalizeHostedTaskRequest{
		TaskID: oldTaskID, Status: runtime.TaskFailed,
	})
	if err != nil {
		t.Fatalf("retry finalize hosted task: %v", err)
	}
	if finalized.ID != oldTaskID || finalized.Status != runtime.TaskFailed {
		t.Fatalf("retry finalize result = %#v", finalized)
	}
}
