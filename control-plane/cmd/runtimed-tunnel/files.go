package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	maxFileBytes   int64 = 64 << 20
	maxFileEntries       = 10_000
	maxWireBytes   int64 = 96 << 20
)

var (
	errInvalidPath = errors.New("invalid logical path")
	errNotFound    = errors.New("file not found")
	errDirectory   = errors.New("path is a directory")
	errLimit       = errors.New("file operation limit exceeded")
	errSymlink     = errors.New("symlinks are not allowed")
	processLogName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type fileRequest struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Process   string `json:"process,omitempty"`
	Contents  []byte `json:"contents,omitempty"`
	MaxBytes  int64  `json:"max_bytes,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type fileResponse struct {
	Contents  []byte     `json:"contents,omitempty"`
	Lines     []string   `json:"lines,omitempty"`
	Info      *fileInfo  `json:"info,omitempty"`
	Entries   []fileInfo `json:"entries,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
	Error     *fileError `json:"error,omitempty"`
}

type fileInfo struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type fileError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func serveFileRequest(ctx context.Context, root string, input io.Reader, output io.Writer) error {
	var request fileRequest
	if err := json.NewDecoder(io.LimitReader(input, maxWireBytes)).Decode(&request); err != nil {
		return fmt.Errorf("decode file request: %w", err)
	}
	response := handleFileRequest(ctx, root, request)
	return json.NewEncoder(output).Encode(response)
}

func handleFileRequest(ctx context.Context, root string, request fileRequest) fileResponse {
	if err := ctx.Err(); err != nil {
		return errorResponse(err)
	}
	root, err := safeRoot(root)
	if err != nil {
		return errorResponse(err)
	}
	logical, err := parseLogicalPath(request.Path)
	if err != nil {
		return errorResponse(err)
	}
	switch request.Operation {
	case "list":
		entries, truncated, err := listFiles(ctx, root, logical, request.Recursive, request.Limit)
		if err != nil {
			return errorResponse(err)
		}
		return fileResponse{Entries: entries, Truncated: truncated}
	case "read":
		contents, err := readFile(ctx, root, logical, request.MaxBytes)
		if err != nil {
			return errorResponse(err)
		}
		return fileResponse{Contents: contents}
	case "write":
		info, err := writeFile(ctx, root, logical, request.Contents, request.MaxBytes)
		if err != nil {
			return errorResponse(err)
		}
		return fileResponse{Info: &info}
	case "delete":
		if err := deleteFile(ctx, root, logical); err != nil {
			return errorResponse(err)
		}
		return fileResponse{}
	case "tail_log":
		lines, err := tailProcessLog(ctx, root, request.Process, request.Limit, request.MaxBytes)
		if err != nil {
			return errorResponse(err)
		}
		return fileResponse{Lines: lines}
	default:
		return errorResponse(fmt.Errorf("unsupported file operation"))
	}
}

func safeRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", mapFileError(err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", mapFileError(err)
	}
	if !info.IsDir() {
		return "", errDirectory
	}
	return resolved, nil
}

func parseLogicalPath(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	if strings.ContainsRune(value, 0) || strings.Contains(value, `\`) ||
		strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || path.Clean(value) != value {
		return nil, errInvalidPath
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errInvalidPath
		}
	}
	return parts, nil
}

func listFiles(ctx context.Context, root string, parts []string, recursive bool, limit int) ([]fileInfo, bool, error) {
	if limit < 1 || limit > maxFileEntries {
		return nil, false, errLimit
	}
	directory, err := resolveExisting(root, parts)
	if err != nil {
		return nil, false, err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return nil, false, mapFileError(err)
	}
	if !info.IsDir() {
		return nil, false, errDirectory
	}
	entries := make([]fileInfo, 0, limit)
	truncated := false
	add := func(filename string, entry fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if len(entries) == limit {
			truncated = true
			return errLimit
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		item := fileInfo{Path: filepath.ToSlash(relative), Type: "file"}
		if entry.IsDir() {
			item.Type = "directory"
		} else {
			details, err := entry.Info()
			if err != nil {
				return err
			}
			item.Size = details.Size()
		}
		entries = append(entries, item)
		return nil
	}
	if recursive {
		err = filepath.WalkDir(directory, func(filename string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if filename == directory {
				return nil
			}
			return add(filename, entry)
		})
	} else {
		var directoryEntries []fs.DirEntry
		directoryEntries, err = os.ReadDir(directory)
		if err == nil {
			for _, entry := range directoryEntries {
				if err = add(filepath.Join(directory, entry.Name()), entry); err != nil {
					break
				}
			}
		}
	}
	if errors.Is(err, errLimit) && truncated {
		err = nil
	}
	if err != nil {
		return nil, false, mapFileError(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, truncated, nil
}

func readFile(ctx context.Context, root string, parts []string, limit int64) ([]byte, error) {
	if len(parts) == 0 {
		return nil, errDirectory
	}
	if limit < 1 || limit > maxFileBytes {
		return nil, errLimit
	}
	filename, err := resolveExisting(root, parts)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(filename)
	if err != nil {
		return nil, mapFileError(err)
	}
	if info.IsDir() {
		return nil, errDirectory
	}
	if info.Size() > limit {
		return nil, errLimit
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, mapFileError(err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func writeFile(ctx context.Context, root string, parts []string, contents []byte, limit int64) (fileInfo, error) {
	if len(parts) == 0 {
		return fileInfo{}, errDirectory
	}
	if limit < 1 || limit > maxFileBytes || int64(len(contents)) > limit {
		return fileInfo{}, errLimit
	}
	parent, err := ensureSafeParent(root, parts[:len(parts)-1])
	if err != nil {
		return fileInfo{}, err
	}
	filename := filepath.Join(parent, parts[len(parts)-1])
	if info, err := os.Lstat(filename); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return fileInfo{}, errSymlink
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fileInfo{}, err
	}
	temporary, err := os.CreateTemp(parent, ".runtimed-tunnel-*")
	if err != nil {
		return fileInfo{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fileInfo{}, err
	}
	if err := temporary.Close(); err != nil {
		return fileInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return fileInfo{}, err
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return fileInfo{}, err
	}
	return fileInfo{Path: strings.Join(parts, "/"), Type: "file", Size: int64(len(contents))}, nil
}

func deleteFile(ctx context.Context, root string, parts []string) error {
	if len(parts) == 0 {
		return errDirectory
	}
	filename, err := resolveExisting(root, parts)
	if err != nil {
		return err
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return mapFileError(err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return errSymlink
	}
	if info.IsDir() {
		return errDirectory
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return mapFileError(os.Remove(filename))
}

// tailProcessLog is deliberately narrower than readFile: callers cannot name a
// workspace path and can only read a capped tail of a runtimed-owned log.
func tailProcessLog(ctx context.Context, root, process string, tail int, maxBytes int64) ([]string, error) {
	if !processLogName.MatchString(process) {
		return nil, errInvalidPath
	}
	if tail < 1 || tail > 1_000 || maxBytes < 1 || maxBytes > 256<<10 {
		return nil, errLimit
	}
	filename, err := resolveExisting(root, []string{".runtimed", process + ".log"})
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(filename)
	if err != nil {
		return nil, mapFileError(err)
	}
	if info.IsDir() {
		return nil, errDirectory
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, mapFileError(err)
	}
	defer file.Close()

	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return []string{}, nil
	}
	lines := strings.Split(trimmed, "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return lines, nil
}

func resolveExisting(root string, parts []string) (string, error) {
	current := root
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", mapFileError(err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", errSymlink
		}
	}
	return current, nil
}

func ensureSafeParent(root string, parts []string) (string, error) {
	current := root
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", errSymlink
		}
		if !info.IsDir() {
			return "", errDirectory
		}
	}
	return current, nil
}

func mapFileError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return errNotFound
	}
	return err
}

func errorResponse(err error) fileResponse {
	code := "invalid_request"
	switch {
	case errors.Is(err, errNotFound):
		code = "not_found"
	case errors.Is(err, errDirectory):
		code = "is_directory"
	case errors.Is(err, errLimit):
		code = "limit_exceeded"
	case errors.Is(err, errSymlink):
		code = "symlink_not_allowed"
	}
	return fileResponse{Error: &fileError{Code: code, Message: err.Error()}}
}
