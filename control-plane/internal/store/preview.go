package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PreviewTicket is a short-lived, single-use bootstrap credential. TokenHash is
// the SHA-256 hash of the opaque value returned to the browser; plaintext ticket
// values are never persisted.
type PreviewTicket struct {
	TokenHash               string
	SandboxID               string
	PrincipalID             string
	BrowserSessionTokenHash string
	PreviewHost             string
	AdminOverride           bool
	CreatedAt               time.Time
	ExpiresAt               time.Time
	ConsumedAt              sql.NullInt64
}

// PreviewSession is a host-bound opaque browser credential minted only while a
// valid PreviewTicket is consumed.
type PreviewSession struct {
	TokenHash               string
	SandboxID               string
	PrincipalID             string
	BrowserSessionTokenHash string
	PreviewHost             string
	AdminOverride           bool
	CreatedAt               time.Time
	ExpiresAt               time.Time
	RevokedAt               sql.NullInt64
}

func scanPreviewTicket(sc scanner) (*PreviewTicket, error) {
	var ticket PreviewTicket
	var createdAt, expiresAt int64
	if err := sc.Scan(
		&ticket.TokenHash, &ticket.SandboxID, &ticket.PrincipalID,
		&ticket.BrowserSessionTokenHash, &ticket.PreviewHost, &ticket.AdminOverride, &createdAt, &expiresAt,
		&ticket.ConsumedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	ticket.CreatedAt = time.Unix(createdAt, 0).UTC()
	ticket.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return &ticket, nil
}

func scanPreviewSession(sc scanner) (*PreviewSession, error) {
	var session PreviewSession
	var createdAt, expiresAt int64
	if err := sc.Scan(
		&session.TokenHash, &session.SandboxID, &session.PrincipalID,
		&session.BrowserSessionTokenHash, &session.PreviewHost, &session.AdminOverride, &createdAt, &expiresAt,
		&session.RevokedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	session.CreatedAt = time.Unix(createdAt, 0).UTC()
	session.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return &session, nil
}

func validPreviewBinding(tokenHash, sandboxID, principalID, browserSessionTokenHash, previewHost string, expiresAt time.Time) bool {
	return tokenHash != "" && sandboxID != "" && principalID != "" &&
		browserSessionTokenHash != "" && previewHost != "" && !expiresAt.IsZero()
}

// CreatePreviewTicket stores a hashed bootstrap ticket. Callers must authorize
// the principal against the sandbox before calling this method.
func (s *Store) CreatePreviewTicket(ctx context.Context, ticket PreviewTicket) error {
	if !validPreviewBinding(ticket.TokenHash, ticket.SandboxID, ticket.PrincipalID, ticket.BrowserSessionTokenHash, ticket.PreviewHost, ticket.ExpiresAt) {
		return errors.New("invalid preview ticket")
	}
	return s.submit(ctx, func(db *dialectDB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := lockPreviewPrincipal(ctx, tx, ticket.PrincipalID); err != nil {
			return err
		}
		now := s.now().Unix()
		if err := lockActivePreviewBrowserSession(ctx, tx, ticket.BrowserSessionTokenHash, ticket.PrincipalID, now); err != nil {
			return err
		}
		if ticket.CreatedAt.IsZero() {
			ticket.CreatedAt = time.Unix(now, 0).UTC()
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO preview_ticket
			    (token_hash, sandbox_id, principal_id, browser_session_token_hash, preview_host, admin_override, created_at, expires_at, consumed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			ticket.TokenHash, ticket.SandboxID, ticket.PrincipalID, ticket.BrowserSessionTokenHash, ticket.PreviewHost,
			ticket.AdminOverride, ticket.CreatedAt.Unix(), ticket.ExpiresAt.Unix())
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		return tx.Commit()
	})
}

// ConsumePreviewTicket atomically marks a ticket consumed and creates the
// host-bound session derived from it. A second caller can never receive the
// ticket's binding, including when production runs with multiple replicas.
func (s *Store) ConsumePreviewTicket(ctx context.Context, tokenHash, sessionTokenHash, sandboxID, previewHost string, sessionExpiresAt time.Time) (*PreviewSession, error) {
	if tokenHash == "" || sessionTokenHash == "" || sandboxID == "" || previewHost == "" || sessionExpiresAt.IsZero() {
		return nil, errors.New("invalid preview ticket consumption")
	}
	var session *PreviewSession
	err := s.submit(ctx, func(db *dialectDB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		var principalID, browserSessionTokenHash string
		if err := tx.QueryRowContext(ctx, `SELECT principal_id, browser_session_token_hash FROM preview_ticket WHERE token_hash=?`, tokenHash).
			Scan(&principalID, &browserSessionTokenHash); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// Lock the principal before the ticket so logout and ticket redemption
		// serialize on a common row across production replicas.
		if err := lockPreviewPrincipal(ctx, tx, principalID); err != nil {
			return err
		}
		ticket, err := scanPreviewTicket(tx.QueryRowContext(ctx, tx.ForUpdate(`
			SELECT token_hash, sandbox_id, principal_id, browser_session_token_hash, preview_host, admin_override, created_at, expires_at, consumed_at
			  FROM preview_ticket WHERE token_hash=?`), tokenHash))
		if err != nil {
			return err
		}
		now := s.now().Unix()
		if ticket.ConsumedAt.Valid || ticket.ExpiresAt.Unix() <= now ||
			ticket.SandboxID != sandboxID || ticket.PreviewHost != previewHost {
			return ErrNotFound
		}
		if ticket.BrowserSessionTokenHash != browserSessionTokenHash {
			return ErrNotFound
		}
		if err := lockActivePreviewBrowserSession(ctx, tx, ticket.BrowserSessionTokenHash, ticket.PrincipalID, now); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE preview_ticket SET consumed_at=?
			 WHERE token_hash=? AND consumed_at IS NULL AND expires_at>?`,
			now, tokenHash, now)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return ErrNotFound
		}
		session = &PreviewSession{
			TokenHash: sessionTokenHash, SandboxID: ticket.SandboxID, PrincipalID: ticket.PrincipalID,
			BrowserSessionTokenHash: ticket.BrowserSessionTokenHash, PreviewHost: ticket.PreviewHost, AdminOverride: ticket.AdminOverride,
			CreatedAt: time.Unix(now, 0).UTC(), ExpiresAt: sessionExpiresAt.UTC(),
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO preview_session
			    (token_hash, sandbox_id, principal_id, browser_session_token_hash, preview_host, admin_override, created_at, expires_at, revoked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			session.TokenHash, session.SandboxID, session.PrincipalID, session.BrowserSessionTokenHash, session.PreviewHost,
			session.AdminOverride, session.CreatedAt.Unix(), session.ExpiresAt.Unix()); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// GetActivePreviewSession returns a non-revoked host-bound session only while
// it remains valid. Callers still need to authorize its principal against the
// current sandbox owner before proxying.
func (s *Store) GetActivePreviewSession(ctx context.Context, tokenHash string) (*PreviewSession, error) {
	now := s.now().Unix()
	return scanPreviewSession(s.db.QueryRowContext(ctx, `
		SELECT ps.token_hash, ps.sandbox_id, ps.principal_id, ps.browser_session_token_hash,
		       ps.preview_host, ps.admin_override, ps.created_at, ps.expires_at, ps.revoked_at
		  FROM preview_session ps
		 WHERE ps.token_hash=? AND ps.revoked_at IS NULL AND ps.expires_at>?
		   AND EXISTS (
		       SELECT 1 FROM browser_session bs
		        WHERE bs.token_hash=ps.browser_session_token_hash
		          AND bs.principal_id=ps.principal_id
		          AND bs.revoked_at IS NULL
		          AND bs.expires_at>?
		   )`,
		tokenHash, now, now))
}

func lockPreviewPrincipal(ctx context.Context, tx *dialectTx, principalID string) error {
	var id string
	if err := tx.QueryRowContext(ctx, tx.ForUpdate(`SELECT id FROM principal WHERE id=?`), principalID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// lockActivePreviewBrowserSession keeps issuance, redemption, and logout from
// reviving preview authority after the originating console session is revoked.
func lockActivePreviewBrowserSession(ctx context.Context, tx *dialectTx, tokenHash, principalID string, now int64) error {
	var hash string
	if err := tx.QueryRowContext(ctx, tx.ForUpdate(`
		SELECT token_hash
		  FROM browser_session
		 WHERE token_hash=? AND principal_id=? AND revoked_at IS NULL AND expires_at>?`),
		tokenHash, principalID, now).Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// RevokePreviewAuthorityForPrincipal invalidates every active preview session
// and unredeemed bootstrap ticket issued to a principal. The principal lock
// serializes revocation with ticket issuance and redemption.
func (s *Store) RevokePreviewAuthorityForPrincipal(ctx context.Context, principalID string) error {
	if principalID == "" {
		return errors.New("principal id is required")
	}
	return s.submit(ctx, func(db *dialectDB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := lockPreviewPrincipal(ctx, tx, principalID); err != nil {
			return err
		}
		now := s.now().Unix()
		if _, err := tx.ExecContext(ctx, `
			UPDATE preview_ticket SET consumed_at=?
			 WHERE principal_id=? AND consumed_at IS NULL`,
			now, principalID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE preview_session SET revoked_at=?
			 WHERE principal_id=? AND revoked_at IS NULL`,
			now, principalID); err != nil {
			return err
		}
		return tx.Commit()
	})
}
