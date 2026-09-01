//go:build !linux

package runtimebackend

import (
	"io"
	"os"
)

func replaceWorkspaceFile(root *os.Root, logical LogicalPath, contents []byte) error {
	file, err := root.OpenFile(workspaceName(logical), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	count, writeErr := file.Write(contents)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if count != len(contents) {
		return io.ErrShortWrite
	}
	return closeErr
}

func openWorkspaceFile(root *os.Root, logical LogicalPath) (*os.File, error) {
	return root.Open(workspaceName(logical))
}

func openWorkspaceDirectory(root *os.Root, logical LogicalPath) (*os.File, error) {
	return root.Open(workspaceName(logical))
}

func deleteWorkspaceFile(root *os.Root, logical LogicalPath) error {
	return root.Remove(workspaceName(logical))
}
