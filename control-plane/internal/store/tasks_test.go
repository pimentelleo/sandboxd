package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestTaskTimeoutRoundTrip guards the timeout_s column wiring (migration
// 0012 + the CreateTask insert and the GetTask / ListRunningTasks
// scans): a persisted timeout must survive a read back, so the boot-time
// reconciler can re-attach a watcher with the right streaming window.
func TestTaskTimeoutRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.CreateTask(ctx, &Task{
		TaskID: "01TASKTIMEOUT0000000000001", SandboxID: "01SBX00000000000000000001",
		Agent: "opencode", Prompt: "p", TimeoutS: 3600,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	got, err := st.GetTask(ctx, "01TASKTIMEOUT0000000000001")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.TimeoutS != 3600 {
		t.Errorf("GetTask timeout_s = %d; want 3600", got.TimeoutS)
	}

	running, err := st.ListRunningTasks(ctx)
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(running) != 1 || running[0].TimeoutS != 3600 {
		t.Errorf("ListRunningTasks timeout_s = %+v; want one row with 3600", running)
	}

	// Default stays 0 when unset (omitted timeout_s → runtimed default).
	if err := st.CreateTask(ctx, &Task{
		TaskID: "01TASKNOTIMEOUT00000000001", SandboxID: "01SBX00000000000000000001",
		Agent: "opencode", Prompt: "p",
	}); err != nil {
		t.Fatalf("create task 2: %v", err)
	}
	got2, _ := st.GetTask(ctx, "01TASKNOTIMEOUT00000000001")
	if got2.TimeoutS != 0 {
		t.Errorf("default timeout_s = %d; want 0", got2.TimeoutS)
	}
}

func TestFinishTaskClaimsRunningTaskOnce(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	task := &Task{
		TaskID: "01TASKFINISH00000000000001", SandboxID: "01SBX00000000000000000001",
		Agent: "opencode", Prompt: "p",
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := st.FinishTask(ctx, task.TaskID, "succeeded", `{"status":"succeeded"}`); err != nil {
		t.Fatalf("finish task: %v", err)
	}
	if err := st.FinishTask(ctx, task.TaskID, "failed", `{"status":"failed"}`); !errors.Is(err, ErrTaskNotRunning) {
		t.Fatalf("second finish = %v; want ErrTaskNotRunning", err)
	}
	got, err := st.GetTask(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "succeeded" || !got.ResultJSON.Valid || got.ResultJSON.String != `{"status":"succeeded"}` {
		t.Fatalf("terminal task was overwritten: %#v", got)
	}
}

func TestFinishTaskWithLeaseRejectsStaleWatcher(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	st.now = func() time.Time { return now }
	task := &Task{
		TaskID: "01TASKLEASE00000000000001", SandboxID: "01SBX00000000000000000001",
		Agent: "opencode", Prompt: "p",
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	first, err := st.AcquireOperationLease(ctx, LeaseResourceTask, task.TaskID, "replica-a", time.Minute)
	if err != nil {
		t.Fatalf("acquire first lease: %v", err)
	}
	now = now.Add(time.Minute + time.Millisecond)
	second, err := st.AcquireOperationLease(ctx, LeaseResourceTask, task.TaskID, "replica-b", time.Minute)
	if err != nil {
		t.Fatalf("acquire second lease: %v", err)
	}
	if err := st.FinishTaskWithLease(ctx, *first, task.TaskID, "failed", `{"status":"failed"}`); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale watcher finish = %v; want ErrLeaseLost", err)
	}
	if err := st.FinishTaskWithLease(ctx, *second, task.TaskID, "succeeded", `{"status":"succeeded"}`); err != nil {
		t.Fatalf("current watcher finish: %v", err)
	}
	got, err := st.GetTask(ctx, task.TaskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "succeeded" || !got.ResultJSON.Valid || got.ResultJSON.String != `{"status":"succeeded"}` {
		t.Fatalf("stale watcher changed terminal task: %#v", got)
	}
}
