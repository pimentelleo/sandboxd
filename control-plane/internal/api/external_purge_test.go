package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func TestPurgeOneUsesPersistedContainerID(t *testing.T) {
	ctx := context.Background()
	id := "01M14B2Z6T97D5ZBCQRVZ1463E"
	containerID := "c6097a82c9fd"
	st := memStore(t)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Create(ctx, &store.Sandbox{ID: id, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunning(ctx, id, containerID, ""); err != nil {
		t.Fatal(err)
	}

	lb := loopback.New()
	lb.Root = t.TempDir()
	workspace, _ := lb.Paths(id)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	dockerBin, calls := purgeDockerStub(t)
	server := &Server{
		Store:    st,
		Docker:   &docker.Client{Bin: dockerBin},
		Loopback: lb,
	}

	if _, _, err := server.purgeOne(ctx, id); err != nil {
		t.Fatalf("purgeOne: %v", err)
	}
	if _, err := st.Get(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sandbox row remains after purge: %v", err)
	}
	got, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "inspect --format {{json .}} "+containerID) {
		t.Fatalf("inspect did not use persisted container ID:\n%s", got)
	}
	if strings.Contains(string(got), "s-"+id) {
		t.Fatalf("inspect used uppercase container name:\n%s", got)
	}
}

func purgeDockerStub(t *testing.T) (bin, calls string) {
	t.Helper()
	calls = filepath.Join(t.TempDir(), "docker-calls")
	t.Setenv("SANDBOXD_PURGE_DOCKER_CALLS", calls)
	if runtime.GOOS == "windows" {
		bin = filepath.Join(t.TempDir(), "docker.cmd")
		script := "@echo off\r\n" +
			"echo %*>>\"%SANDBOXD_PURGE_DOCKER_CALLS%\"\r\n" +
			"if \"%1\"==\"inspect\" (\r\n" +
			"  echo Error: no such object 1>&2\r\n" +
			"  exit /b 1\r\n" +
			")\r\n" +
			"exit /b 0\r\n"
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return bin, calls
	}
	bin = filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$SANDBOXD_PURGE_DOCKER_CALLS\"\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  echo 'Error: no such object' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}
