package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeShellStartupFilesConvertsCRLF(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	if err := os.WriteFile(profile, []byte(". /etc/profile.d/sandbox-env.sh\r\n. ~/.bashrc\r\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	bashrc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bashrc, []byte("[ -f /etc/profile.d/sandbox-env.sh ] && . /etc/profile.d/sandbox-env.sh\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := normalizeShellStartupFiles(home); err != nil {
		t.Fatalf("normalizeShellStartupFiles: %v", err)
	}

	got, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if want := ". /etc/profile.d/sandbox-env.sh\n. ~/.bashrc\n"; string(got) != want {
		t.Errorf("profile = %q, want %q", got, want)
	}
	info, err := os.Stat(profile)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("profile mode = %#o, want %#o", got, 0o640)
	}
	got, err = os.ReadFile(bashrc)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[ -f /etc/profile.d/sandbox-env.sh ] && . /etc/profile.d/sandbox-env.sh\n"; string(got) != want {
		t.Errorf("bashrc = %q, want %q", got, want)
	}
}

func TestNormalizeShellStartupFilesLeavesLFAndMissingFilesUntouched(t *testing.T) {
	home := t.TempDir()
	if err := normalizeShellStartupFiles(home); err != nil {
		t.Fatalf("missing files: %v", err)
	}

	path := filepath.Join(home, ".profile")
	want := ". /etc/profile.d/sandbox-env.sh\n"
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	bashrc := filepath.Join(home, ".bashrc")
	bashrcWant := "export PS1='sandbox'\n"
	if err := os.WriteFile(bashrc, []byte(bashrcWant), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := normalizeShellStartupFiles(home); err != nil {
		t.Fatalf("LF files: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("profile = %q, want %q", got, want)
	}
	got, err = os.ReadFile(bashrc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != bashrcWant {
		t.Errorf("bashrc = %q, want %q", got, bashrcWant)
	}
}

func TestNormalizeShellStartupFilesDoesNotFollowProfileSymlinks(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "custom-profile")
	want := ". /etc/profile.d/sandbox-env.sh\r\n"
	if err := os.WriteFile(target, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".profile")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := normalizeShellStartupFiles(home); err != nil {
		t.Fatalf("normalizeShellStartupFiles: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("symlink target = %q, want unchanged %q", got, want)
	}
}
