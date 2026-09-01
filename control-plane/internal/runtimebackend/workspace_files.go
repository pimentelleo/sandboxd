package runtimebackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
)

var (
	ErrFileNotFound      = errors.New("runtime backend: workspace file not found")
	ErrFileIsDirectory   = errors.New("runtime backend: workspace path is a directory")
	ErrFileLimitRequired = errors.New("runtime backend: file operation limit is required")
	ErrFileLimitExceeded = errors.New("runtime backend: file operation limit exceeded")
	ErrSymlinkNotAllowed = errors.New("runtime backend: workspace symlinks are not allowed")
)

const fileReadBufferSize = 32 * 1024

// WorkspaceAdapter implements host-backed file access for the local loopback
// provider. Kubernetes providers implement WorkspaceFiles against their own
// storage API without receiving a host filesystem path.
var _ WorkspaceFiles = (*WorkspaceAdapter)(nil)

func (a *WorkspaceAdapter) ListFiles(ctx context.Context, ref SandboxRef, request ListFilesRequest) (ListFilesResult, error) {
	if request.Limit <= 0 {
		return ListFilesResult{}, ErrFileLimitRequired
	}
	root, err := a.openWorkspaceRoot(ref)
	if err != nil {
		return ListFilesResult{}, err
	}
	defer root.Close()

	info, err := lstatWorkspacePath(root, request.Path)
	if err != nil {
		return ListFilesResult{}, err
	}
	if !info.IsDir() {
		return ListFilesResult{}, ErrFileIsDirectory
	}
	directory, err := openWorkspaceDirectory(root, request.Path)
	if err != nil {
		return ListFilesResult{}, fileError(err)
	}
	defer directory.Close()
	info, err = directory.Stat()
	if err != nil {
		return ListFilesResult{}, err
	}
	if !info.IsDir() {
		return ListFilesResult{}, ErrFileIsDirectory
	}

	result := ListFilesResult{Entries: make([]FileInfo, 0, request.Limit)}
	if err := listWorkspaceDirectory(ctx, root, directory, request.Path, request.Recursive, &result, request.Limit); err != nil {
		if !errors.Is(err, errFileListComplete) {
			return ListFilesResult{}, err
		}
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		return result.Entries[i].Path.String() < result.Entries[j].Path.String()
	})
	return result, nil
}

func (a *WorkspaceAdapter) ReadFile(ctx context.Context, ref SandboxRef, request ReadFileRequest) ([]byte, error) {
	if request.Path.IsRoot() {
		return nil, ErrFileIsDirectory
	}
	if request.MaxBytes <= 0 {
		return nil, ErrFileLimitRequired
	}
	root, err := a.openWorkspaceRoot(ref)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	if _, err := lstatWorkspacePath(root, request.Path); err != nil {
		return nil, err
	}
	file, err := openWorkspaceFile(root, request.Path)
	if err != nil {
		return nil, fileError(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, ErrFileIsDirectory
	}
	if info.Size() > request.MaxBytes {
		return nil, ErrFileLimitExceeded
	}

	data := make([]byte, 0, info.Size())
	buffer := make([]byte, fileReadBufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if int64(len(data)+count) > request.MaxBytes {
				return nil, ErrFileLimitExceeded
			}
			data = append(data, buffer[:count]...)
		}
		if readErr == io.EOF {
			return data, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func (a *WorkspaceAdapter) WriteFile(ctx context.Context, ref SandboxRef, request WriteFileRequest) (FileInfo, error) {
	if request.Path.IsRoot() {
		return FileInfo{}, ErrFileIsDirectory
	}
	if request.MaxBytes <= 0 {
		return FileInfo{}, ErrFileLimitRequired
	}
	if int64(len(request.Contents)) > request.MaxBytes {
		return FileInfo{}, ErrFileLimitExceeded
	}
	if err := ctx.Err(); err != nil {
		return FileInfo{}, err
	}
	root, err := a.openWorkspaceRoot(ref)
	if err != nil {
		return FileInfo{}, err
	}
	defer root.Close()

	parent := parentWorkspacePath(request.Path)
	if err := ensureWorkspaceDirectories(root, parent); err != nil {
		return FileInfo{}, err
	}
	if info, err := root.Lstat(workspaceName(request.Path)); err == nil {
		if info.Mode()&fs.ModeSymlink != 0 {
			return FileInfo{}, ErrSymlinkNotAllowed
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return FileInfo{}, err
	}

	if err := ctx.Err(); err != nil {
		return FileInfo{}, err
	}
	if err := replaceWorkspaceFile(root, request.Path, request.Contents); err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Path: request.Path, Type: FileTypeRegular, Size: int64(len(request.Contents))}, nil
}

func (a *WorkspaceAdapter) DeleteFile(ctx context.Context, ref SandboxRef, logical LogicalPath) error {
	if logical.IsRoot() {
		return ErrFileIsDirectory
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := a.openWorkspaceRoot(ref)
	if err != nil {
		return err
	}
	defer root.Close()

	info, err := lstatWorkspacePath(root, logical)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return ErrFileIsDirectory
	}
	return deleteWorkspaceFile(root, logical)
}

var errFileListComplete = errors.New("runtime backend: file list complete")

func (a *WorkspaceAdapter) openWorkspaceRoot(ref SandboxRef) (*os.Root, error) {
	paths, err := a.Paths(ref)
	if err != nil {
		return nil, err
	}
	// Root keeps a descriptor for the workspace, so later operations remain
	// confined even if a sandbox renames path components concurrently.
	root, err := os.OpenRoot(paths.Mount)
	if err != nil {
		return nil, fileError(err)
	}
	return root, nil
}

func lstatWorkspacePath(root *os.Root, logical LogicalPath) (fs.FileInfo, error) {
	current := "."
	parts := workspacePathParts(logical)
	for index, part := range parts {
		current = joinWorkspaceName(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return nil, fileError(err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, ErrSymlinkNotAllowed
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("runtime backend: workspace parent %q is not a directory", current)
		}
	}
	return root.Lstat(current)
}

func ensureWorkspaceDirectories(root *os.Root, logical LogicalPath) error {
	current := "."
	for _, part := range workspacePathParts(logical) {
		current = joinWorkspaceName(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := root.Mkdir(current, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, err = root.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return ErrSymlinkNotAllowed
		}
		if !info.IsDir() {
			return fmt.Errorf("runtime backend: workspace parent %q is not a directory", current)
		}
	}
	return nil
}

func listWorkspaceDirectory(ctx context.Context, root *os.Root, directory *os.File, current LogicalPath, recursive bool, result *ListFilesResult, limit int) error {
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		path, err := appendWorkspacePath(current, entry.Name())
		if err != nil {
			return err
		}
		// DirEntry.Info resolves from the process working directory when the
		// directory was opened through an fd. Use the workspace root instead.
		info, err := root.Lstat(workspaceName(path))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if len(result.Entries) == limit {
			result.Truncated = true
			return errFileListComplete
		}
		item := FileInfo{Path: path, Type: FileTypeRegular}
		if info.IsDir() {
			item.Type = FileTypeDirectory
		} else {
			item.Size = info.Size()
		}
		result.Entries = append(result.Entries, item)
		if !recursive || !info.IsDir() {
			continue
		}

		currentInfo, err := root.Lstat(workspaceName(path))
		if err != nil {
			return err
		}
		if currentInfo.Mode()&fs.ModeSymlink != 0 || !currentInfo.IsDir() {
			continue
		}
		child, err := openWorkspaceDirectory(root, path)
		if err != nil {
			return err
		}
		childInfo, err := child.Stat()
		if err != nil {
			child.Close()
			return err
		}
		if childInfo.IsDir() {
			err = listWorkspaceDirectory(ctx, root, child, path, recursive, result, limit)
		}
		closeErr := child.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func workspacePathParts(logical LogicalPath) []string {
	if logical.IsRoot() {
		return nil
	}
	return strings.Split(logical.String(), "/")
}

func parentWorkspacePath(logical LogicalPath) LogicalPath {
	index := strings.LastIndexByte(logical.String(), '/')
	if index < 0 {
		return LogicalPath{}
	}
	return LogicalPath{value: logical.String()[:index]}
}

func workspaceName(logical LogicalPath) string {
	if logical.IsRoot() {
		return "."
	}
	return logical.String()
}

func joinWorkspaceName(parent, name string) string {
	if parent == "." {
		return name
	}
	return parent + "/" + name
}

func appendWorkspacePath(parent LogicalPath, name string) (LogicalPath, error) {
	if parent.IsRoot() {
		return ParseLogicalPath(name)
	}
	return ParseLogicalPath(parent.String() + "/" + name)
}

func fileError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrFileNotFound
	}
	return err
}
