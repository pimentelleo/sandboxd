package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPreviewTicketIsHostBoundSingleUseAndExpires(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertPrincipal(ctx, &Principal{
		ID: "principal-owner", Provider: "entra", TenantID: "tenant", Subject: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, &Sandbox{
		ID: "01OWNER", Status: "stopped", Image: "image", WorkspaceImg: "img", WorkspaceMnt: "mnt",
		OwnerPrincipalID: nullIfEmpty("principal-owner"),
	}); err != nil {
		t.Fatal(err)
	}
	const browserSessionTokenHash = "browser-session"
	if err := st.CreateBrowserSession(ctx, BrowserSession{
		TokenHash: browserSessionTokenHash, PrincipalID: "principal-owner", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	host := "s-01owner-3000.preview.example.test"
	if err := st.CreatePreviewTicket(ctx, PreviewTicket{
		TokenHash: "ticket", SandboxID: "01OWNER", PrincipalID: "principal-owner",
		BrowserSessionTokenHash: browserSessionTokenHash, PreviewHost: host, ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumePreviewTicket(ctx, "ticket", "session-wrong-host", "01OWNER",
		"s-01owner-3001.preview.example.test", time.Now().Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong host consumption error = %v; want not found", err)
	}
	session, err := st.ConsumePreviewTicket(ctx, "ticket", "session", "01OWNER", host, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if session.PreviewHost != host || session.SandboxID != "01OWNER" ||
		session.BrowserSessionTokenHash != browserSessionTokenHash {
		t.Fatalf("session binding = %#v", session)
	}
	if _, err := st.ConsumePreviewTicket(ctx, "ticket", "session-two", "01OWNER", host, time.Now().Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay error = %v; want not found", err)
	}

	if err := st.CreatePreviewTicket(ctx, PreviewTicket{
		TokenHash: "expired", SandboxID: "01OWNER", PrincipalID: "principal-owner",
		BrowserSessionTokenHash: browserSessionTokenHash, PreviewHost: host, ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumePreviewTicket(ctx, "expired", "expired-session", "01OWNER", host, time.Now().Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired ticket error = %v; want not found", err)
	}
	if err := st.CreatePreviewTicket(ctx, PreviewTicket{
		TokenHash: "unredeemed", SandboxID: "01OWNER", PrincipalID: "principal-owner",
		BrowserSessionTokenHash: browserSessionTokenHash, PreviewHost: host, ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.RevokePreviewAuthorityForPrincipal(ctx, "principal-owner"); err != nil {
		t.Fatalf("revoke principal preview authority: %v", err)
	}
	if _, err := st.GetActivePreviewSession(ctx, "session"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked preview session = %v; want not found", err)
	}
	if _, err := st.ConsumePreviewTicket(ctx, "unredeemed", "unredeemed-session", "01OWNER", host, time.Now().Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked preview ticket = %v; want not found", err)
	}
}

func TestPreviewAuthorityCannotOutliveBrowserSession(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertPrincipal(ctx, &Principal{
		ID: "principal-owner", Provider: "entra", TenantID: "tenant", Subject: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, &Sandbox{
		ID: "01OWNER", Status: "stopped", Image: "image", WorkspaceImg: "img", WorkspaceMnt: "mnt",
		OwnerPrincipalID: nullIfEmpty("principal-owner"),
	}); err != nil {
		t.Fatal(err)
	}
	const browserSessionTokenHash = "browser-session"
	if err := st.CreateBrowserSession(ctx, BrowserSession{
		TokenHash: browserSessionTokenHash, PrincipalID: "principal-owner", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	host := "s-01owner-3000.preview.example.test"
	if err := st.CreatePreviewTicket(ctx, PreviewTicket{
		TokenHash: "session-ticket", SandboxID: "01OWNER", PrincipalID: "principal-owner",
		BrowserSessionTokenHash: browserSessionTokenHash, PreviewHost: host, ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumePreviewTicket(ctx, "session-ticket", "preview-session", "01OWNER", host, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("create preview session: %v", err)
	}
	if err := st.CreatePreviewTicket(ctx, PreviewTicket{
		TokenHash: "ticket", SandboxID: "01OWNER", PrincipalID: "principal-owner",
		BrowserSessionTokenHash: browserSessionTokenHash, PreviewHost: host, ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeBrowserSession(ctx, browserSessionTokenHash); err != nil {
		t.Fatalf("revoke browser session: %v", err)
	}
	if _, err := st.GetActivePreviewSession(ctx, "preview-session"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("preview session remained active after browser logout: %v; want not found", err)
	}
	if _, err := st.ConsumePreviewTicket(ctx, "ticket", "preview-session", "01OWNER", host, time.Now().Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ticket redeemed after browser logout: %v; want not found", err)
	}
	if err := st.CreatePreviewTicket(ctx, PreviewTicket{
		TokenHash: "new-ticket", SandboxID: "01OWNER", PrincipalID: "principal-owner",
		BrowserSessionTokenHash: browserSessionTokenHash, PreviewHost: host, ExpiresAt: time.Now().Add(time.Minute),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ticket issued after browser logout: %v; want not found", err)
	}
}
