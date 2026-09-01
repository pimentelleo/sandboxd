package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProviderURLAndProfileSelection(t *testing.T) {
	tests := []struct {
		url  string
		want Provider
	}{
		{"postgres://user:pass@db.example/sandboxd", ProviderPostgres},
		{"postgresql://user:pass@db.example/sandboxd", ProviderPostgres},
		{"sqlite://sandboxd.db", ProviderSQLite},
		{"file:sandboxd.db?_fk=1", ProviderSQLite},
	}
	for _, test := range tests {
		got, err := ProviderForURL(test.url)
		if err != nil || got != test.want {
			t.Fatalf("ProviderForURL(%q) = %q, %v; want %q, nil", test.url, got, err, test.want)
		}
	}
	if _, err := ProviderForURL("sandboxd.db"); err == nil {
		t.Fatal("plain path unexpectedly selected a provider")
	}
	if got, err := (Config{Profile: ProfileProduction}).selectedProvider(); err != nil || got != ProviderPostgres {
		t.Fatalf("production profile = %q, %v; want postgres, nil", got, err)
	}
	if _, err := (Config{Provider: ProviderSQLite, Profile: ProfileProduction}).selectedProvider(); err == nil {
		t.Fatal("SQLite production profile conflict was accepted")
	}
}

func TestBindQueryPostgresSkipsQuotedTextAndComments(t *testing.T) {
	query := "SELECT '?', \"?\", $tag$?$tag$, value FROM t -- ?\nWHERE a=? AND b='it''s ?' /* ? */ AND c=?"
	want := "SELECT '?', \"?\", $tag$?$tag$, value FROM t -- ?\nWHERE a=$1 AND b='it''s ?' /* ? */ AND c=$2"
	if got := BindQuery(ProviderPostgres, query); got != want {
		t.Fatalf("BindQuery() = %q; want %q", got, want)
	}
	if got := BindQuery(ProviderSQLite, query); got != query {
		t.Fatalf("SQLite binding changed query: %q", got)
	}
}

func TestPostgresLeaseQueriesUseDatabaseClock(t *testing.T) {
	for _, query := range []string{postgresAcquireLease, postgresHeartbeatLease} {
		if !strings.Contains(query, "clock_timestamp()") {
			t.Fatalf("PostgreSQL lease query does not use the database clock: %s", query)
		}
		if strings.Contains(BindQuery(ProviderPostgres, query), "?") {
			t.Fatalf("PostgreSQL lease query has an unbound placeholder: %s", query)
		}
	}
	if !strings.Contains(postgresAcquireLease, "RETURNING acquired_at, heartbeat_at, expires_at") {
		t.Fatalf("acquire query does not return database-derived timestamps: %s", postgresAcquireLease)
	}
}

func TestConversationSnapshotTransactionOptions(t *testing.T) {
	if got := conversationSnapshotTxOptions(ProviderSQLite); !got.ReadOnly || got.Isolation != sql.LevelDefault {
		t.Fatalf("SQLite snapshot options = %#v; want read-only default isolation", got)
	}
	if got := conversationSnapshotTxOptions(ProviderPostgres); !got.ReadOnly || got.Isolation != sql.LevelRepeatableRead {
		t.Fatalf("PostgreSQL snapshot options = %#v; want read-only repeatable-read", got)
	}
}

func TestSQLiteIdentityOwnershipAndLoginTransaction(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	t.Cleanup(func() { _ = store.Close() })
	if store.Provider() != ProviderSQLite {
		t.Fatalf("Open provider = %q; want sqlite", store.Provider())
	}

	first := &Principal{
		ID: "principal-1", Provider: "entra", TenantID: "tenant-1", Subject: "oid-1",
		Roles: []string{"sandboxd.user"},
	}
	second := &Principal{ID: "principal-2", Provider: "entra", TenantID: "tenant-1", Subject: "oid-2"}
	if err := store.UpsertPrincipal(ctx, first); err != nil {
		t.Fatalf("upsert first principal: %v", err)
	}
	if err := store.UpsertPrincipal(ctx, second); err != nil {
		t.Fatalf("upsert second principal: %v", err)
	}
	first.Roles = []string{"sandboxd.user", "sandboxd.admin"}
	if err := store.UpsertPrincipal(ctx, first); err != nil {
		t.Fatalf("refresh first principal roles: %v", err)
	}
	if persisted, err := store.GetPrincipal(ctx, "entra", first.TenantID, first.Subject); err != nil ||
		persisted.ID != first.ID || strings.Join(persisted.Roles, ",") != "sandboxd.user,sandboxd.admin" {
		t.Fatalf("persisted principal = %#v, %v", persisted, err)
	}
	app := &App{ID: "app-1", OwnerToken: DefaultTenant, Name: "Scoped", Description: ""}
	if err := store.CreateApp(ctx, app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := store.SetAppOwnerPrincipal(ctx, app.ID, DefaultTenant, first.ID); err != nil {
		t.Fatalf("set app owner: %v", err)
	}
	if _, err := store.GetAppForPrincipal(ctx, app.ID, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-principal app lookup = %v; want ErrNotFound", err)
	}
	apps, err := store.ListAppsForPrincipal(ctx, first.ID)
	if err != nil || len(apps) != 1 || apps[0].ID != app.ID {
		t.Fatalf("principal app list = %#v, %v", apps, err)
	}

	sandbox := &Sandbox{
		ID: "sandbox-1", Status: "creating", Image: "base", WorkspaceImg: "image",
		WorkspaceMnt: "mount", MemoryHigh: "4G",
	}
	if err := store.Create(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := store.SetSandboxOwnerPrincipal(ctx, sandbox.ID, first.ID); err != nil {
		t.Fatalf("set sandbox owner: %v", err)
	}
	if owner, err := store.GetWorkspacePrincipalOwner(ctx, sandbox.ID); err != nil || owner != first.ID {
		t.Fatalf("workspace principal owner = %q, %v; want %q, nil", owner, err, first.ID)
	}
	if _, err := store.GetSandboxForPrincipal(ctx, sandbox.ID, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-principal sandbox lookup = %v; want ErrNotFound", err)
	}
	sandboxes, err := store.ListSandboxesForPrincipal(ctx, first.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].ID != sandbox.ID {
		t.Fatalf("principal sandbox list = %#v, %v", sandboxes, err)
	}
	if err := store.Delete(ctx, sandbox.ID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if owner, err := store.GetWorkspacePrincipalOwner(ctx, sandbox.ID); err != nil || owner != first.ID {
		t.Fatalf("deleted workspace owner = %q, %v; want %q, nil", owner, err, first.ID)
	}
	conflictingReuse := &Sandbox{
		ID: "sandbox-1", Status: "creating", Image: "base", WorkspaceImg: "image",
		WorkspaceMnt: "mount", MemoryHigh: "4G",
		OwnerPrincipalID: sql.NullString{String: second.ID, Valid: true},
	}
	if err := store.Create(ctx, conflictingReuse); !errors.Is(err, ErrConflict) {
		t.Fatalf("reuse with another principal = %v; want ErrConflict", err)
	}
	if owner, err := store.GetWorkspacePrincipalOwner(ctx, sandbox.ID); err != nil || owner != first.ID {
		t.Fatalf("conflicting reuse changed owner = %q, %v; want %q, nil", owner, err, first.ID)
	}
	reused := &Sandbox{
		ID: "sandbox-1", Status: "creating", Image: "base", WorkspaceImg: "image",
		WorkspaceMnt: "mount", MemoryHigh: "4G",
	}
	if err := store.Create(ctx, reused); err != nil {
		t.Fatalf("reuse with durable owner: %v", err)
	}
	if !reused.OwnerPrincipalID.Valid || reused.OwnerPrincipalID.String != first.ID {
		t.Fatalf("reused sandbox owner = %#v; want %q", reused.OwnerPrincipalID, first.ID)
	}
	if err := store.SetSandboxOwnerPrincipal(ctx, reused.ID, second.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("ownership transfer = %v; want ErrConflict", err)
	}

	now := time.Unix(1_700_000_000, 0).UTC()
	store.now = func() time.Time { return now }
	browserSession := BrowserSession{
		TokenHash: "sha256-cookie-token", PrincipalID: first.ID, ExpiresAt: now.Add(time.Hour),
	}
	if err := store.CreateBrowserSession(ctx, browserSession); err != nil {
		t.Fatalf("create browser session: %v", err)
	}
	if active, err := store.GetActiveBrowserSession(ctx, browserSession.TokenHash); err != nil || active.PrincipalID != first.ID {
		t.Fatalf("active browser session = %#v, %v", active, err)
	}
	if err := store.RevokeBrowserSession(ctx, browserSession.TokenHash); err != nil {
		t.Fatalf("revoke browser session: %v", err)
	}
	if _, err := store.GetActiveBrowserSession(ctx, browserSession.TokenHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked browser session = %v; want ErrNotFound", err)
	}
	otherBrowserSession := BrowserSession{
		TokenHash: "sha256-other-cookie-token", PrincipalID: first.ID, ExpiresAt: now.Add(time.Hour),
	}
	if err := store.CreateBrowserSession(ctx, otherBrowserSession); err != nil {
		t.Fatalf("create second browser session: %v", err)
	}
	if err := store.RevokeBrowserSessionsForPrincipal(ctx, first.ID); err != nil {
		t.Fatalf("revoke all browser sessions: %v", err)
	}
	if _, err := store.GetActiveBrowserSession(ctx, otherBrowserSession.TokenHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("globally revoked browser session = %v; want ErrNotFound", err)
	}
	transaction := LoginTransaction{
		ID: "login-1", Provider: "entra", StateHash: "state-hash", NonceHash: "nonce-hash",
		VerifierCiphertext: []byte("sealed"), VerifierNonce: []byte("nonce"),
		RedirectURI: "https://example.test/callback", ReturnLocation: "/apps?tab=mine", ExpiresAt: now.Add(time.Minute),
	}
	if err := store.CreateLoginTransaction(ctx, transaction); err != nil {
		t.Fatalf("create login transaction: %v", err)
	}
	consumed, err := store.ConsumeLoginTransaction(ctx, transaction.StateHash)
	if err != nil || !consumed.ConsumedAt.Valid || string(consumed.VerifierCiphertext) != "sealed" ||
		consumed.ReturnLocation != "/apps?tab=mine" {
		t.Fatalf("consume login transaction = %#v, %v", consumed, err)
	}
	if _, err := store.ConsumeLoginTransaction(ctx, transaction.StateHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replayed transaction = %v; want ErrNotFound", err)
	}
	expired := transaction
	expired.ID, expired.StateHash = "login-expired", "expired-state-hash"
	expired.ExpiresAt = now.Add(-time.Second)
	if err := store.CreateLoginTransaction(ctx, expired); err != nil {
		t.Fatalf("create expired transaction: %v", err)
	}
	if _, err := store.ConsumeLoginTransaction(ctx, expired.StateHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired transaction = %v; want ErrNotFound", err)
	}
}

func TestOperationLeaseExpiryAndFence(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Unix(1_700_000_000, 0).UTC()
	store.now = func() time.Time { return now }

	first, err := store.AcquireOperationLease(ctx, LeaseResourceSandbox, "sandbox-1", "replica-a", time.Minute)
	if err != nil {
		t.Fatalf("acquire first lease: %v", err)
	}
	if _, err := store.AcquireOperationLease(ctx, LeaseResourceSandbox, "sandbox-1", "replica-b", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second acquire = %v; want ErrLeaseHeld", err)
	}
	now = now.Add(time.Minute + time.Millisecond)
	second, err := store.AcquireOperationLease(ctx, LeaseResourceSandbox, "sandbox-1", "replica-b", time.Minute)
	if err != nil {
		t.Fatalf("acquire expired lease: %v", err)
	}
	if _, err := store.HeartbeatOperationLease(ctx, *first, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old holder heartbeat = %v; want ErrLeaseLost", err)
	}
	if renewed, err := store.HeartbeatOperationLease(ctx, *second, time.Minute); err != nil || !renewed.ExpiresAt.After(now) {
		t.Fatalf("heartbeat second = %#v, %v", renewed, err)
	}
	if err := store.ReleaseOperationLease(ctx, *second); err != nil {
		t.Fatalf("release second lease: %v", err)
	}
	if err := store.ReleaseOperationLease(ctx, *second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("repeat release = %v; want ErrLeaseLost", err)
	}
}
