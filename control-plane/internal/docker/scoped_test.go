package docker

import (
	"strings"
	"testing"
	"time"
)

func scopedRequest() ScopedExecRequest {
	return ScopedExecRequest{
		Container:   "s-sandbox_123",
		User:        "sandbox",
		Workdir:     "/home/sandbox/workspace/app",
		Command:     []string{"sh", "-lc", "echo hello"},
		Timeout:     1500 * time.Millisecond,
		OutputLimit: 1024,
	}
}

func TestScopedExecArgsPreserveNonInteractiveScope(t *testing.T) {
	request := scopedRequest()
	args := scopedExecArgs(request)
	want := []string{
		"exec", "--user", "sandbox", "--workdir", "/home/sandbox/workspace/app",
		"s-sandbox_123", "timeout", "--signal=TERM", "--kill-after=5s", "2s",
		"--", "sh", "-lc", "echo hello",
	}
	if got, expected := strings.Join(args, "\x00"), strings.Join(want, "\x00"); got != expected {
		t.Fatalf("scoped args = %#v, want %#v", args, want)
	}
	if strings.Contains(strings.Join(args, "\x00"), "-t") {
		t.Fatalf("scoped docker exec must not allocate a TTY: %#v", args)
	}

	request.Stdin = []byte("input")
	args = scopedExecArgs(request)
	if got := strings.Join(args, "\x00"); !strings.Contains(got, "\x00--interactive\x00") {
		t.Fatalf("stdin request did not enable stdin: %#v", args)
	}
	if strings.Contains(strings.Join(args, "\x00"), "-t") {
		t.Fatalf("stdin request allocated a TTY: %#v", args)
	}
}

func TestValidateScopedExecRejectsUnsafeRequests(t *testing.T) {
	cases := []struct {
		name string
		edit func(*ScopedExecRequest)
	}{
		{"container injection", func(r *ScopedExecRequest) { r.Container = "s-safe;id" }},
		{"user newline", func(r *ScopedExecRequest) { r.User = "sandbox\nroot" }},
		{"relative workdir", func(r *ScopedExecRequest) { r.Workdir = "workspace" }},
		{"missing command", func(r *ScopedExecRequest) { r.Command = nil }},
		{"NUL command", func(r *ScopedExecRequest) { r.Command = []string{"echo\x00unsafe"} }},
		{"missing timeout", func(r *ScopedExecRequest) { r.Timeout = 0 }},
		{"missing output limit", func(r *ScopedExecRequest) { r.OutputLimit = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := scopedRequest()
			tc.edit(&request)
			if err := validateScopedExec(request); err == nil {
				t.Fatal("unsafe request was accepted")
			}
		})
	}
}

func TestBoundedExecOutputCapsCombinedStreams(t *testing.T) {
	output := newBoundedExecOutput(5)
	if n, err := output.stdoutWriter().Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("stdout write = %d, %v", n, err)
	}
	if n, err := output.stderrWriter().Write([]byte("def")); err != nil || n != 3 {
		t.Fatalf("stderr write = %d, %v", n, err)
	}
	if got := output.stdout.String() + output.stderr.String(); got != "abcde" {
		t.Fatalf("retained output = %q, want %q", got, "abcde")
	}
}
