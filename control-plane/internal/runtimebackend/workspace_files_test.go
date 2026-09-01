package runtimebackend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestParseLogicalPath(t *testing.T) {
	valid, err := ParseLogicalPath("workspace/app/main.go")
	if err != nil || valid.String() != "workspace/app/main.go" {
		t.Fatalf("valid path = %q, %v", valid.String(), err)
	}
	for _, input := range []string{"/absolute", "parent/../file", "parent//file", "parent/./file", `parent\file`, "parent/"} {
		if _, err := ParseLogicalPath(input); err == nil {
			t.Errorf("ParseLogicalPath(%q) accepted an unsafe path", input)
		}
	}
	if root, err := ParseLogicalPath(""); err != nil || !root.IsRoot() {
		t.Fatalf("root path = %+v, %v", root, err)
	}
}

func TestWorkspaceAdapterFileOperationsAreBounded(t *testing.T) {
	root := t.TempDir()
	workspace := &fakeWorkspace{root: root}
	adapter, err := NewWorkspaceAdapter(workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref := SandboxRef{ID: "sandbox-1"}
	filename, err := ParseLogicalPath("workspace/app/main.go")
	if err != nil {
		t.Fatal(err)
	}
	written, err := adapter.WriteFile(context.Background(), ref, WriteFileRequest{Path: filename, Contents: []byte("hello"), MaxBytes: 5})
	if err != nil || written.Size != 5 {
		t.Fatalf("WriteFile = %+v, %v", written, err)
	}
	entries, err := adapter.ListFiles(context.Background(), ref, ListFilesRequest{Path: LogicalPath{}, Recursive: true, Limit: 10})
	if err != nil || len(entries.Entries) != 3 {
		t.Fatalf("ListFiles = %+v, %v", entries, err)
	}
	if _, err := adapter.WriteFile(context.Background(), ref, WriteFileRequest{Path: filename, Contents: []byte("world"), MaxBytes: 5}); err != nil {
		t.Fatalf("replacement WriteFile: %v", err)
	}
	contents, err := adapter.ReadFile(context.Background(), ref, ReadFileRequest{Path: filename, MaxBytes: 5})
	if err != nil || string(contents) != "world" {
		t.Fatalf("ReadFile = %q, %v", contents, err)
	}
	if _, err := adapter.ReadFile(context.Background(), ref, ReadFileRequest{Path: filename, MaxBytes: 4}); !errors.Is(err, ErrFileLimitExceeded) {
		t.Fatalf("short ReadFile error = %v", err)
	}
	if err := adapter.DeleteFile(context.Background(), ref, filename); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "workspace", "app", "main.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still exists: %v", err)
	}
}

func TestWorkspaceAdapterFileListingMarksOnlyOverflow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	adapter, err := NewWorkspaceAdapter(&fakeWorkspace{root: root})
	if err != nil {
		t.Fatal(err)
	}
	one, err := adapter.ListFiles(context.Background(), SandboxRef{ID: "sandbox-1"}, ListFilesRequest{Limit: 1})
	if err != nil || one.Truncated || len(one.Entries) != 1 {
		t.Fatalf("single entry list = %+v, %v", one, err)
	}
	if err := os.WriteFile(filepath.Join(root, "two"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	many, err := adapter.ListFiles(context.Background(), SandboxRef{ID: "sandbox-1"}, ListFilesRequest{Limit: 1})
	if err != nil || !many.Truncated || len(many.Entries) != 1 {
		t.Fatalf("overflow list = %+v, %v", many, err)
	}
}

func TestWorkspaceAdapterRejectsSymlinkPathComponents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("creating symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "linked-file")); err != nil {
		t.Fatal(err)
	}

	adapter, err := NewWorkspaceAdapter(&fakeWorkspace{root: root})
	if err != nil {
		t.Fatal(err)
	}
	ref := SandboxRef{ID: "sandbox-1"}
	linkedSecret, err := ParseLogicalPath("linked/secret")
	if err != nil {
		t.Fatal(err)
	}
	linkedFile, err := ParseLogicalPath("linked/new-file")
	if err != nil {
		t.Fatal(err)
	}
	linkedDirectory, err := ParseLogicalPath("linked")
	if err != nil {
		t.Fatal(err)
	}
	linkedFinal, err := ParseLogicalPath("linked-file")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := adapter.ReadFile(context.Background(), ref, ReadFileRequest{Path: linkedSecret, MaxBytes: 16}); !errors.Is(err, ErrSymlinkNotAllowed) {
		t.Fatalf("ReadFile through symlink = %v, want %v", err, ErrSymlinkNotAllowed)
	}
	if _, err := adapter.WriteFile(context.Background(), ref, WriteFileRequest{Path: linkedFile, Contents: []byte("inside"), MaxBytes: 16}); !errors.Is(err, ErrSymlinkNotAllowed) {
		t.Fatalf("WriteFile through symlink = %v, want %v", err, ErrSymlinkNotAllowed)
	}
	if _, err := adapter.ListFiles(context.Background(), ref, ListFilesRequest{Path: linkedDirectory, Limit: 10}); !errors.Is(err, ErrSymlinkNotAllowed) {
		t.Fatalf("ListFiles through symlink = %v, want %v", err, ErrSymlinkNotAllowed)
	}
	if err := adapter.DeleteFile(context.Background(), ref, linkedSecret); !errors.Is(err, ErrSymlinkNotAllowed) {
		t.Fatalf("DeleteFile through symlink = %v, want %v", err, ErrSymlinkNotAllowed)
	}
	if _, err := adapter.WriteFile(context.Background(), ref, WriteFileRequest{Path: linkedFinal, Contents: []byte("inside"), MaxBytes: 16}); !errors.Is(err, ErrSymlinkNotAllowed) {
		t.Fatalf("WriteFile to symlink = %v, want %v", err, ErrSymlinkNotAllowed)
	}
	if contents, err := os.ReadFile(filepath.Join(outside, "secret")); err != nil || string(contents) != "outside" {
		t.Fatalf("outside file = %q, %v", contents, err)
	}
}

func TestWorkspaceAdapterConfinementSurvivesComponentReplacement(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	parent := filepath.Join(root, "replace")
	parking := filepath.Join(root, "replace-parking")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "escaped"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "outside-only"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "symlink-check")); err != nil {
		t.Skipf("creating symlink: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "symlink-check")); err != nil {
		t.Fatal(err)
	}

	adapter, err := NewWorkspaceAdapter(&fakeWorkspace{root: root})
	if err != nil {
		t.Fatal(err)
	}
	target, err := ParseLogicalPath("replace/escaped")
	if err != nil {
		t.Fatal(err)
	}
	targetDirectory, err := ParseLogicalPath("replace")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var replacements sync.WaitGroup
	var swaps atomic.Int64
	replacements.Add(1)
	go func() {
		defer replacements.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Rename(parent, parking) != nil {
				continue
			}
			if os.Symlink(outside, parent) == nil {
				swaps.Add(1)
				_ = os.Remove(parent)
			}
			_ = os.Rename(parking, parent)
		}
	}()

	ref := SandboxRef{ID: "sandbox-1"}
	for range 1000 {
		_, _ = adapter.WriteFile(context.Background(), ref, WriteFileRequest{Path: target, Contents: []byte("inside"), MaxBytes: 16})
		contents, err := adapter.ReadFile(context.Background(), ref, ReadFileRequest{Path: target, MaxBytes: 16})
		if err == nil && string(contents) == "outside" {
			close(stop)
			replacements.Wait()
			t.Fatal("ReadFile escaped the workspace after component replacement")
		}
		listed, err := adapter.ListFiles(context.Background(), ref, ListFilesRequest{Path: targetDirectory, Recursive: true, Limit: 16})
		if err == nil {
			for _, entry := range listed.Entries {
				if entry.Path.String() == "replace/outside-only" {
					close(stop)
					replacements.Wait()
					t.Fatal("ListFiles escaped the workspace after component replacement")
				}
			}
		}
	}
	close(stop)
	replacements.Wait()
	if swaps.Load() == 0 {
		t.Fatal("component replacement did not run")
	}

	contents, err := os.ReadFile(filepath.Join(outside, "escaped"))
	if err != nil || string(contents) != "outside" {
		t.Fatalf("outside file = %q, %v", contents, err)
	}
}

type remoteWorkspaceContract struct{}

func (remoteWorkspaceContract) Paths(SandboxRef) (WorkspacePaths, error) {
	return WorkspacePaths{Storage: "remote-volume/sandbox-1", Mount: "/workspace"}, nil
}
func (remoteWorkspaceContract) Provision(context.Context, SandboxRef) error { return nil }
func (remoteWorkspaceContract) ProvisionFromTemplate(context.Context, SandboxRef, string) error {
	return nil
}
func (remoteWorkspaceContract) Release(context.Context, SandboxRef) error { return nil }
func (remoteWorkspaceContract) Exists(SandboxRef) (bool, error)           { return true, nil }
func (remoteWorkspaceContract) ListFiles(context.Context, SandboxRef, ListFilesRequest) (ListFilesResult, error) {
	return ListFilesResult{}, nil
}
func (remoteWorkspaceContract) ReadFile(context.Context, SandboxRef, ReadFileRequest) ([]byte, error) {
	return nil, nil
}
func (remoteWorkspaceContract) WriteFile(context.Context, SandboxRef, WriteFileRequest) (FileInfo, error) {
	return FileInfo{}, nil
}
func (remoteWorkspaceContract) DeleteFile(context.Context, SandboxRef, LogicalPath) error { return nil }

var _ WorkspaceManager = remoteWorkspaceContract{}
var _ WorkspaceFiles = remoteWorkspaceContract{}
