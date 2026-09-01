package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBridgeUnixCopiesBothDirections(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runtimed.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()
		request, err := io.ReadAll(connection)
		if err != nil {
			serverDone <- err
			return
		}
		if string(request) != "request" {
			serverDone <- errUnexpectedRequest
			return
		}
		_, err = io.WriteString(connection, "response")
		serverDone <- err
	}()
	var response bytes.Buffer
	if err := bridgeUnix(context.Background(), socket, bytes.NewBufferString("request"), &response); err != nil {
		t.Fatalf("bridgeUnix: %v", err)
	}
	if response.String() != "response" {
		t.Fatalf("response = %q", response.String())
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestFileHelperRejectsUnsafePathsAndSymlinkEscape(t *testing.T) {
	for _, path := range []string{"/etc/passwd", "../escape", "a/../escape", `a\b`, "a//b"} {
		if _, err := parseLogicalPath(path); err == nil {
			t.Fatalf("unsafe logical path accepted: %q", path)
		}
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	response := handleFileRequest(context.Background(), root, fileRequest{Operation: "read", Path: "link/secret", MaxBytes: 10})
	if response.Error == nil || response.Error.Code != "symlink_not_allowed" {
		t.Fatalf("symlink escape response = %#v", response)
	}
}

func TestFileHelperEnforcesBoundsAndWritesInsideRoot(t *testing.T) {
	root := t.TempDir()
	write := handleFileRequest(context.Background(), root, fileRequest{
		Operation: "write", Path: "workspace/app/a.txt", Contents: []byte("hello"), MaxBytes: 5,
	})
	if write.Error != nil || write.Info == nil || write.Info.Size != 5 {
		t.Fatalf("write response = %#v", write)
	}
	read := handleFileRequest(context.Background(), root, fileRequest{Operation: "read", Path: "workspace/app/a.txt", MaxBytes: 4})
	if read.Error == nil || read.Error.Code != "limit_exceeded" {
		t.Fatalf("read bounds response = %#v", read)
	}
	if _, err := os.Stat(filepath.Join(root, "workspace", "app", "a.txt")); err != nil {
		t.Fatalf("write did not stay inside root: %v", err)
	}
}

var errUnexpectedRequest = &unexpectedRequestError{}

type unexpectedRequestError struct{}

func (*unexpectedRequestError) Error() string { return "unexpected bridge request" }

func TestTerminalFileOperationObservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := handleFileRequest(ctx, t.TempDir(), fileRequest{Operation: "list", Limit: 1})
	if response.Error == nil {
		t.Fatal("cancelled file request succeeded")
	}
}

func TestBridgeHonorsCancellation(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runtimed.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			defer connection.Close()
			_, _ = io.Copy(io.Discard, connection)
			time.Sleep(time.Second)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bridgeUnix(ctx, socket, bytes.NewBufferString("request"), io.Discard); err == nil {
		t.Fatal("cancelled bridge succeeded")
	}
}
