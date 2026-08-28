package main

import (
	"bytes"
	"os"
	"path/filepath"
)

// normalizeShellStartupFiles repairs CRLF line endings in the managed startup
// files copied into persistent sandbox homes. A CRLF source line makes POSIX sh
// look for an environment file whose path ends in a carriage return.
func normalizeShellStartupFiles(home string) error {
	for _, name := range []string{".profile", ".bashrc"} {
		if err := normalizeShellStartupFile(filepath.Join(home, name)); err != nil {
			return err
		}
	}
	return nil
}

func normalizeShellStartupFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	normalized := bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	if bytes.Equal(content, normalized) {
		return nil
	}
	return os.WriteFile(path, normalized, info.Mode().Perm())
}
