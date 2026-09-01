// v1_runtime_inspect.go — A1.5b: advisory runtime detection for an app's
// workspace. Read-only; it inspects workspace files host-side and returns
// suggestions/warnings. It NEVER runs app code, installs deps, touches a Git
// credential/token, or applies anything. Owner-scoped.
package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/detect"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/runtimebackend"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// GET /v1/apps/{id}/runtime-inspect
func (s *Server) v1RuntimeInspect(w http.ResponseWriter, r *http.Request) {
	app, err := s.Store.GetAppForOwner(r.Context(), r.PathValue("id"), tenantToken(r))
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such app")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "lookup failed")
		return
	}
	sb, err := s.Store.CurrentSandboxForApp(r.Context(), app.ID)
	if err != nil {
		// No sandbox => no workspace to inspect yet.
		writeJSON(w, http.StatusOK, detect.Result{
			ExistingManifest: &detect.ManifestSummary{Present: false},
			Suggestions:      []detect.Suggestion{},
			Warnings:         []string{"no workspace yet — create a sandbox for this app, then inspect"},
		})
		return
	}
	if s.usesRuntimeProvider() {
		if s.RuntimeFiles == nil {
			writeV1Err(w, http.StatusServiceUnavailable, "runtime_unavailable",
				"runtime provider file access is unavailable")
			return
		}
		files := &providerDetectFiles{
			ctx: r.Context(), files: s.RuntimeFiles, ref: s.runtimeRef(sb),
			entries: make(map[string]providerDetectEntry), maxBytes: s.providerFileReadLimit(),
		}
		result := detect.Inspect(files)
		if files.err != nil {
			writeV1Err(w, http.StatusServiceUnavailable, "runtime_unavailable",
				"runtime provider inspection access failed")
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	appDir := filepath.Join(sb.WorkspaceMnt, "workspace", "app")
	writeJSON(w, http.StatusOK, detect.Inspect(detect.OSFiles(appDir)))
}

// providerDetectFiles gives advisory detection a bounded, provider-owned
// workspace view. It intentionally cannot derive or access a host mount path.
type providerDetectFiles struct {
	ctx      context.Context
	files    runtimebackend.WorkspaceFiles
	ref      runtimebackend.SandboxRef
	entries  map[string]providerDetectEntry
	maxBytes int64
	err      error
}

type providerDetectEntry struct {
	contents []byte
	exists   bool
	readable bool
}

var _ detect.Files = (*providerDetectFiles)(nil)

func (f *providerDetectFiles) Read(rel string) ([]byte, bool) {
	entry := f.read(rel)
	return entry.contents, entry.readable
}

func (f *providerDetectFiles) Exists(rel string) bool {
	return f.read(rel).exists
}

func (f *providerDetectFiles) read(rel string) providerDetectEntry {
	if entry, ok := f.entries[rel]; ok {
		return entry
	}
	logical, err := providerAppPath(rel, true)
	if err != nil {
		f.recordError(err)
		return providerDetectEntry{}
	}
	contents, err := f.files.ReadFile(f.ctx, f.ref, runtimebackend.ReadFileRequest{
		Path: logical, MaxBytes: f.maxBytes,
	})
	entry := providerDetectEntry{}
	switch {
	case err == nil:
		entry = providerDetectEntry{contents: contents, exists: true, readable: true}
	case errors.Is(err, runtimebackend.ErrFileNotFound):
		// A missing optional detection file is normal.
	case errors.Is(err, runtimebackend.ErrFileIsDirectory), errors.Is(err, runtimebackend.ErrFileLimitExceeded):
		// These paths exist, but no bounded regular-file content is available.
		// This mirrors the local detector's Exists/Read split without turning an
		// oversized optional file into a host fallback.
		entry.exists = true
	default:
		f.recordError(err)
	}
	f.entries[rel] = entry
	return entry
}

func (f *providerDetectFiles) recordError(err error) {
	if f.err == nil {
		f.err = err
	}
}
