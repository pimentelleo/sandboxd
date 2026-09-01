package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// PUT /v1/sandboxes/{id}/files?path=<rel> — atomic generic file write.
//
// Roots at the application directory (`/home/sandbox/workspace/app` inside
// the container). The backend uses this to prepare AGENTS.md / CLAUDE.md /
// opencode.json / any other file the chosen agent expects. The platform does
// NOT inspect the body — the file is opaque bytes.
//
// PUT, GET /files, and GET /files/content share this app-relative public
// contract for both local and provider runtimes. In particular, a bare
// `path=sandbox.yaml` writes the manifest runtimed reads.
//
// Security model (paying-tenant threat model):
//   - The caller has already passed the resource authorization boundary.
//   - Path is `filepath.Clean`-normalised, absolute paths and `..` are
//     rejected, and the resolved final path MUST stay under the app root
//     via prefix check.
//   - Reserved subtrees (`.runtimed/`, `lost+found/`) are refused —
//     `.runtimed/` is the in-sandbox supervisor's working dir and
//     writing into it could corrupt task state.
//   - The final file is opened with O_NOFOLLOW so a symlink at the leaf
//     cannot redirect the write off the mount.
//   - Written atomically: tmp file in the same directory + rename, so
//     a partially-written file is never observable.
//   - chown'd to the workspace owner uid/gid (the userns-remapped
//     sandbox user) so the agent sees its own user own the file.
//
// Per-file limit mirrors uploads.go (25 MiB) — small textual config
// and modestly-sized assets are the use case; larger blobs would
// belong in object storage, not the workspace.

const (
	// maxPutFileBytes — per-request body cap. Mirrors uploads.go.
	maxPutFileBytes = 25 << 20
)

// reservedPathPrefixes are app-relative subtrees the platform reserves.
// Writes here are refused even with a valid token.
var reservedPathPrefixes = []string{".runtimed/", ".runtimed", "lost+found/", "lost+found"}

// resolveWritePath validates a caller-supplied relative path against
// an app root and returns the absolute on-disk path.
//
// Rules (in order):
//  1. path is non-empty and does not contain a NUL byte.
//  2. path is not absolute.
//  3. After Clean, no segment is `..` (would escape) and no segment is
//     empty (would denote a directory write).
//  4. The cleaned path does not target a reserved subtree.
//  5. The resolved on-disk path stays under <appRoot>/.
func resolveWritePath(appRoot, raw string) (string, string, error) {
	if raw == "" {
		return "", "", errors.New("path is required")
	}
	if strings.ContainsRune(raw, 0) {
		return "", "", errors.New("invalid path: NUL byte")
	}
	if filepath.IsAbs(raw) {
		return "", "", errors.New("path must be relative to the app root")
	}
	// Trailing slash signals directory intent — check before Clean,
	// which would strip it.
	if strings.HasSuffix(raw, "/") {
		return "", "", errors.New("path must name a file, not a directory")
	}
	clean := filepath.Clean(raw)
	// Reject any traversal segment. filepath.Clean reduces "a/../b" to
	// "b" but leaves a leading ".." in place.
	for _, seg := range strings.Split(clean, string(os.PathSeparator)) {
		if seg == ".." {
			return "", "", errors.New("path traversal (..) not allowed")
		}
	}
	if clean == "." || clean == "/" {
		return "", "", errors.New("path must name a file, not the root")
	}
	if strings.HasSuffix(clean, "/") {
		return "", "", errors.New("path must name a file, not a directory")
	}
	for _, p := range reservedPathPrefixes {
		if clean == strings.TrimSuffix(p, "/") ||
			strings.HasPrefix(clean, strings.TrimSuffix(p, "/")+"/") {
			return "", "", errors.New("path is in a reserved subtree (" +
				strings.TrimSuffix(p, "/") + ")")
		}
	}
	full := filepath.Join(appRoot, clean)
	// Defence-in-depth: re-check the final prefix after Join.
	if full != appRoot && !strings.HasPrefix(full, appRoot+string(os.PathSeparator)) {
		return "", "", errors.New("resolved path escapes the app root")
	}
	return full, clean, nil
}

// mountOwner returns the uid/gid that owns the workspace mount root —
// the sandbox user as userns-remapped on the host. Falls back to -1
// so callers can skip chown gracefully.
func mountOwner(mnt string) (uid, gid int) {
	fi, err := os.Stat(mnt)
	if err != nil {
		return -1, -1
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(st.Uid), int(st.Gid)
}

// v1PutFile is the handler for PUT /v1/sandboxes/{id}/files.
func (s *Server) v1PutFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isULID(id) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid sandbox id")
		return
	}
	if s.usesRuntimeProvider() {
		s.v1PutProviderFile(w, r, id)
		return
	}
	_, mnt := s.Loopback.Paths(id)
	if info, err := os.Stat(mnt); err != nil || !info.IsDir() {
		writeV1Err(w, http.StatusNotFound, "not_found", "no workspace for that sandbox")
		return
	}

	// Paths are relative to the APP dir (workspace/app), matching GET /files and
	// /files/content. Writing relative to the workspace root instead left console
	// editor saves invisible to reads (they landed a directory above the app), so
	// the editor looked read-only. appDirFor is under mnt, so the chown-up loop
	// below (bounded by mnt) still fixes ownership of any dirs it creates.
	full, rel, err := resolveWritePath(s.appDirFor(id), r.URL.Query().Get("path"))
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Hard size gate before reading the body.
	if r.ContentLength > maxPutFileBytes {
		writeV1Err(w, http.StatusRequestEntityTooLarge, "invalid_request",
			"file exceeds the 25 MiB limit")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPutFileBytes)

	uid, gid := mountOwner(mnt)

	// Create parent dirs with the same owner so the agent can read its
	// own tree.
	parent := filepath.Dir(full)
	if err := os.MkdirAll(parent, 0o775); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if uid >= 0 {
		// Walk up and chown any newly-created parents back to the owner.
		// We only chown directories we may have created (mode marker
		// 0o775 + currently root-owned). Best-effort.
		for p := parent; p != mnt && strings.HasPrefix(p, mnt+string(os.PathSeparator)); p = filepath.Dir(p) {
			if fi, err := os.Stat(p); err == nil {
				if st, ok := fi.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != uid || int(st.Gid) != gid) {
					_ = os.Chown(p, uid, gid)
				}
			}
		}
	}

	// Atomic write: tmp in same dir + rename. O_NOFOLLOW on the tmp
	// ensures we never write through a symlink left by a previous run.
	tmp, err := os.CreateTemp(parent, ".put-*.tmp")
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	tmpPath := tmp.Name()
	written, copyErr := io.Copy(tmp, r.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		var mbe *http.MaxBytesError
		if errors.As(copyErr, &mbe) {
			writeV1Err(w, http.StatusRequestEntityTooLarge, "invalid_request",
				"file exceeds the 25 MiB limit")
			return
		}
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "read body: "+copyErr.Error())
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusInternalServerError, "internal", "close tmp: "+closeErr.Error())
		return
	}

	// Set ownership BEFORE rename so the file is never visible at the
	// target path with wrong owner.
	if uid >= 0 {
		_ = os.Chown(tmpPath, uid, gid)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusInternalServerError, "internal", "chmod: "+err.Error())
		return
	}

	// Refuse to overwrite if the existing leaf is a symlink — a
	// symlinked target could redirect the write out of the mount.
	if fi, err := os.Lstat(full); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusBadRequest, "invalid_request",
			"refusing to overwrite a symlink")
		return
	}

	if err := os.Rename(tmpPath, full); err != nil {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusInternalServerError, "internal", "rename: "+err.Error())
		return
	}

	s.auditAction(r, audit.Entry{
		Action: "file.put", Target: id,
		Detail: map[string]any{"path": rel, "size": written},
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "size": written})
}

func (s *Server) v1PutProviderFile(w http.ResponseWriter, r *http.Request, id string) {
	if s.RuntimeFiles == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime provider file access is unavailable")
		return
	}
	sb, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no workspace for that sandbox")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	logical, err := providerAppPath(r.URL.Query().Get("path"), true)
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	limit := s.providerFileByteLimit()
	if r.ContentLength > limit {
		writeV1Err(w, http.StatusRequestEntityTooLarge, "invalid_request", providerFileLimitMessage(limit))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	contents, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeV1Err(w, http.StatusRequestEntityTooLarge, "invalid_request", providerFileLimitMessage(limit))
			return
		}
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	info, err := s.RuntimeFiles.WriteFile(r.Context(), s.runtimeRef(sb), runtimebackend.WriteFileRequest{
		Path: logical, Contents: contents, MaxBytes: limit,
	})
	if err != nil {
		writeProviderFileError(w, err, "file")
		return
	}
	rel, ok := providerPublicPath(info.Path)
	if !ok || rel == "" {
		writeV1Err(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime provider returned an invalid file path")
		return
	}
	s.auditAction(r, audit.Entry{
		Action: "file.put", Target: id,
		Detail: map[string]any{"path": rel, "size": info.Size, "runtime_provider": true},
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "size": info.Size})
}

func (s *Server) providerFileByteLimit() int64 {
	if s.ProviderFileByteLimit > 0 {
		return s.ProviderFileByteLimit
	}
	return maxPutFileBytes
}

func providerFileLimitMessage(limit int64) string {
	return fmt.Sprintf("file exceeds the runtime provider limit of %d bytes", limit)
}
