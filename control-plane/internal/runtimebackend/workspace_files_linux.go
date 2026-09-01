//go:build linux

package runtimebackend

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func replaceWorkspaceFile(root *os.Root, logical LogicalPath, contents []byte) error {
	directory, err := openWorkspaceDirectoryDescriptor(root, parentWorkspacePath(logical), true)
	if err != nil {
		return err
	}
	defer unix.Close(directory)

	var random [16]byte
	for range 10 {
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		temporary := ".runtimebackend-" + hex.EncodeToString(random[:])
		descriptor, err := unix.Openat(directory, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(descriptor), temporary)
		count, writeErr := file.Write(contents)
		if writeErr != nil {
			file.Close()
			unix.Unlinkat(directory, temporary, 0)
			return writeErr
		}
		if count != len(contents) {
			file.Close()
			unix.Unlinkat(directory, temporary, 0)
			return io.ErrShortWrite
		}
		if err := file.Close(); err != nil {
			unix.Unlinkat(directory, temporary, 0)
			return err
		}
		if err := rejectSymlinkAt(directory, fileName(logical)); err != nil {
			unix.Unlinkat(directory, temporary, 0)
			return err
		}
		if err := unix.Renameat(directory, temporary, directory, fileName(logical)); err != nil {
			unix.Unlinkat(directory, temporary, 0)
			return err
		}
		return nil
	}
	return errors.New("runtime backend: unable to create temporary workspace file")
}

func openWorkspaceFile(root *os.Root, logical LogicalPath) (*os.File, error) {
	directory, err := openWorkspaceDirectoryDescriptor(root, parentWorkspacePath(logical), false)
	if err != nil {
		return nil, err
	}
	descriptor, err := unix.Openat(directory, fileName(logical), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	unix.Close(directory)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrSymlinkNotAllowed
		}
		return nil, err
	}
	return os.NewFile(uintptr(descriptor), workspaceName(logical)), nil
}

func openWorkspaceDirectory(root *os.Root, logical LogicalPath) (*os.File, error) {
	descriptor, err := openWorkspaceDirectoryDescriptor(root, logical, false)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(descriptor), workspaceName(logical)), nil
}

func deleteWorkspaceFile(root *os.Root, logical LogicalPath) error {
	directory, err := openWorkspaceDirectoryDescriptor(root, parentWorkspacePath(logical), false)
	if err != nil {
		return err
	}
	defer unix.Close(directory)
	if err := rejectSymlinkAt(directory, fileName(logical)); err != nil {
		return err
	}
	return unix.Unlinkat(directory, fileName(logical), 0)
}

func rejectSymlinkAt(directory int, name string) error {
	var details unix.Stat_t
	if err := unix.Fstatat(directory, name, &details, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if details.Mode&unix.S_IFMT == unix.S_IFLNK {
		return ErrSymlinkNotAllowed
	}
	return nil
}

func openWorkspaceDirectoryDescriptor(root *os.Root, logical LogicalPath, create bool) (int, error) {
	base, err := root.Open(".")
	if err != nil {
		return -1, err
	}
	defer base.Close()
	directory, err := unix.Dup(int(base.Fd()))
	if err != nil {
		return -1, err
	}
	for _, part := range workspacePathParts(logical) {
		next, err := unix.Openat(directory, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if create && errors.Is(err, fs.ErrNotExist) {
			if err := unix.Mkdirat(directory, part, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				unix.Close(directory)
				return -1, err
			}
			next, err = unix.Openat(directory, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		unix.Close(directory)
		if err != nil {
			if errors.Is(err, unix.ELOOP) {
				return -1, ErrSymlinkNotAllowed
			}
			if errors.Is(err, unix.ENOTDIR) {
				return -1, fmt.Errorf("runtime backend: workspace parent %q is not a directory", part)
			}
			return -1, err
		}
		var details unix.Stat_t
		if err := unix.Fstat(next, &details); err != nil {
			unix.Close(next)
			return -1, err
		}
		if details.Mode&unix.S_IFMT == unix.S_IFLNK {
			unix.Close(next)
			return -1, ErrSymlinkNotAllowed
		}
		if details.Mode&unix.S_IFMT != unix.S_IFDIR {
			unix.Close(next)
			return -1, fmt.Errorf("runtime backend: workspace parent %q is not a directory", part)
		}
		directory = next
	}
	return directory, nil
}

func fileName(logical LogicalPath) string {
	parts := workspacePathParts(logical)
	return parts[len(parts)-1]
}
