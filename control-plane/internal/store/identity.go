package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// LocalPrincipalProvider and LocalPrincipalTenant identify durable local
	// account identities without conflating them with Entra directory claims.
	LocalPrincipalProvider = "local"
	LocalPrincipalTenant   = "sandboxd-local"
)

// Principal is an authenticated owner. Provider, tenant ID, and subject are
// immutable identity coordinates; display metadata is refreshable.
type Principal struct {
	ID          string
	Provider    string
	TenantID    string
	Subject     string
	DisplayName sql.NullString
	Email       sql.NullString
	Roles       []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const principalCols = `id, provider, tenant_id, subject, display_name, email, roles, created_at, updated_at`

func scanPrincipal(sc scanner) (*Principal, error) {
	var p Principal
	var roles string
	var createdAt, updatedAt int64
	if err := sc.Scan(&p.ID, &p.Provider, &p.TenantID, &p.Subject, &p.DisplayName,
		&p.Email, &roles, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(roles), &p.Roles); err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(createdAt, 0).UTC()
	p.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &p, nil
}

// UpsertPrincipal creates or refreshes a principal identity. Its ID is
// preserved on a provider/tenant/subject conflict so callers can safely retry
// an Entra claims refresh.
func (s *Store) UpsertPrincipal(ctx context.Context, principal *Principal) error {
	if principal == nil || principal.ID == "" || principal.Provider == "" ||
		principal.TenantID == "" || principal.Subject == "" {
		return errors.New("invalid principal")
	}
	roles, err := json.Marshal(principal.Roles)
	if err != nil {
		return err
	}
	return s.submit(ctx, func(db *dialectDB) error {
		now := s.now().Unix()
		_, err := db.ExecContext(ctx, `
			INSERT INTO principal
			    (id, provider, tenant_id, subject, display_name, email, roles, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(provider, tenant_id, subject) DO UPDATE SET
			    display_name=excluded.display_name, email=excluded.email, roles=excluded.roles, updated_at=excluded.updated_at`,
			principal.ID, principal.Provider, principal.TenantID, principal.Subject,
			principal.DisplayName, principal.Email, string(roles), now, now)
		if err != nil {
			return err
		}
		stored, err := scanPrincipal(db.QueryRowContext(ctx,
			`SELECT `+principalCols+` FROM principal WHERE provider=? AND tenant_id=? AND subject=?`,
			principal.Provider, principal.TenantID, principal.Subject))
		if err != nil {
			return err
		}
		*principal = *stored
		return nil
	})
}

// GetPrincipal returns the identity registered for provider tenant and subject.
func (s *Store) GetPrincipal(ctx context.Context, provider, tenantID, subject string) (*Principal, error) {
	return scanPrincipal(s.db.QueryRowContext(ctx,
		`SELECT `+principalCols+` FROM principal WHERE provider=? AND tenant_id=? AND subject=?`,
		provider, tenantID, subject))
}

// GetPrincipalByID returns an identity by its stable internal ID.
func (s *Store) GetPrincipalByID(ctx context.Context, id string) (*Principal, error) {
	return scanPrincipal(s.db.QueryRowContext(ctx,
		`SELECT `+principalCols+` FROM principal WHERE id=?`, id))
}

// LocalAccount is a local email/password credential paired with its durable
// principal. PasswordHash is deliberately available only to authentication
// code; API responses must never serialize this type.
type LocalAccount struct {
	Principal    Principal
	PasswordHash string
}

func scanLocalAccount(sc scanner) (*LocalAccount, error) {
	var account LocalAccount
	var roles string
	var createdAt, updatedAt int64
	if err := sc.Scan(
		&account.Principal.ID,
		&account.Principal.Provider,
		&account.Principal.TenantID,
		&account.Principal.Subject,
		&account.Principal.DisplayName,
		&account.Principal.Email,
		&roles,
		&createdAt,
		&updatedAt,
		&account.PasswordHash,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(roles), &account.Principal.Roles); err != nil {
		return nil, err
	}
	account.Principal.CreatedAt = time.Unix(createdAt, 0).UTC()
	account.Principal.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return &account, nil
}

const localAccountSelectCols = `p.id, p.provider, p.tenant_id, p.subject, p.display_name, p.email,
	p.roles, p.created_at, p.updated_at, a.password_hash`

// LocalAccountsExist reports whether the one-time initial administrator has
// already been created.
func (s *Store) LocalAccountsExist(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM local_account`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetLocalAccount returns the credential for a normalized local email address.
func (s *Store) GetLocalAccount(ctx context.Context, email string) (*LocalAccount, error) {
	return scanLocalAccount(s.db.QueryRowContext(ctx, `
		SELECT `+localAccountSelectCols+`
		  FROM local_account a
		  JOIN principal p ON p.id=a.principal_id
		 WHERE p.provider=? AND p.tenant_id=? AND p.subject=?`,
		LocalPrincipalProvider, LocalPrincipalTenant, email))
}

// GetLocalAccountByPrincipal returns a credential only when its principal is a
// local account. It is used to resolve an opaque browser session.
func (s *Store) GetLocalAccountByPrincipal(ctx context.Context, principalID string) (*LocalAccount, error) {
	return scanLocalAccount(s.db.QueryRowContext(ctx, `
		SELECT `+localAccountSelectCols+`
		  FROM local_account a
		  JOIN principal p ON p.id=a.principal_id
		 WHERE a.principal_id=? AND p.provider=? AND p.tenant_id=?`,
		principalID, LocalPrincipalProvider, LocalPrincipalTenant))
}

// CreateInitialLocalAccount atomically creates the sole first local account.
// The first account has both user and administrator roles; later accounts are
// created through CreateLocalAccount and receive only the user role.
func (s *Store) CreateInitialLocalAccount(ctx context.Context, email, passwordHash string) (*Principal, error) {
	return s.createLocalAccount(ctx, email, passwordHash, []string{"sandboxd.user", "sandboxd.admin"}, true)
}

// CreateLocalAccount creates an additional local user account. The caller is
// responsible for authorizing the administrator-only provisioning operation.
func (s *Store) CreateLocalAccount(ctx context.Context, email, passwordHash string) (*Principal, error) {
	return s.createLocalAccount(ctx, email, passwordHash, []string{"sandboxd.user"}, false)
}

func (s *Store) createLocalAccount(ctx context.Context, email, passwordHash string, roles []string, initial bool) (*Principal, error) {
	if email == "" || passwordHash == "" {
		return nil, errors.New("local account email and password hash are required")
	}
	principalID, err := newLocalPrincipalID()
	if err != nil {
		return nil, err
	}
	encodedRoles, err := json.Marshal(roles)
	if err != nil {
		return nil, err
	}
	var created *Principal
	err = s.submit(ctx, func(db *dialectDB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if initial {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM local_account`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return ErrConflict
			}
		}
		now := s.now().Unix()
		principal := &Principal{
			ID:          principalID,
			Provider:    LocalPrincipalProvider,
			TenantID:    LocalPrincipalTenant,
			Subject:     email,
			DisplayName: sql.NullString{String: email, Valid: true},
			Email:       sql.NullString{String: email, Valid: true},
			Roles:       append([]string(nil), roles...),
			CreatedAt:   time.Unix(now, 0).UTC(),
			UpdatedAt:   time.Unix(now, 0).UTC(),
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO principal
			    (id, provider, tenant_id, subject, display_name, email, roles, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			principal.ID, principal.Provider, principal.TenantID, principal.Subject,
			principal.DisplayName, principal.Email, string(encodedRoles), now, now); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO local_account (principal_id, password_hash, created_at, updated_at)
			VALUES (?, ?, ?, ?)`, principal.ID, passwordHash, now, now); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		created = principal
		return nil
	})
	return created, err
}

// UpdateLocalAccountPassword conditionally replaces a local credential and
// revokes every console and preview session belonging to that account. The
// expected password hash closes the check/update race at the database boundary.
func (s *Store) UpdateLocalAccountPassword(ctx context.Context, principalID, expectedHash, newHash string) error {
	if principalID == "" || expectedHash == "" || newHash == "" {
		return errors.New("local account password update is incomplete")
	}
	return s.submit(ctx, func(db *dialectDB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		now := s.now().Unix()
		result, err := tx.ExecContext(ctx, `
			UPDATE local_account SET password_hash=?, updated_at=?
			 WHERE principal_id=? AND password_hash=?`,
			newHash, now, principalID, expectedHash)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE browser_session SET revoked_at=?
			 WHERE principal_id=? AND revoked_at IS NULL`, now, principalID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE preview_ticket SET consumed_at=?
			 WHERE principal_id=? AND consumed_at IS NULL`, now, principalID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE preview_session SET revoked_at=?
			 WHERE principal_id=? AND revoked_at IS NULL`, now, principalID); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func newLocalPrincipalID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "local-" + hex.EncodeToString(value[:]), nil
}

// SetAppOwnerPrincipal attaches an authenticated owner without changing the
// legacy tenant token. ownerToken keeps this safe for current API callers.
func (s *Store) SetAppOwnerPrincipal(ctx context.Context, appID, ownerToken, principalID string) error {
	return s.submit(ctx, func(db *dialectDB) error {
		if _, err := scanPrincipal(db.QueryRowContext(ctx,
			`SELECT `+principalCols+` FROM principal WHERE id=?`, principalID)); err != nil {
			return err
		}
		result, err := db.ExecContext(ctx,
			`UPDATE app SET owner_principal_id=?, updated_at=? WHERE id=? AND owner_token=?`,
			principalID, s.now().Unix(), appID, ownerToken)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// GetAppForPrincipal is an identity-scoped app lookup. A non-owned app is
// deliberately indistinguishable from an absent app.
func (s *Store) GetAppForPrincipal(ctx context.Context, appID, principalID string) (*App, error) {
	return scanApp(s.db.QueryRowContext(ctx,
		`SELECT `+appSelectCols+` FROM app WHERE id=? AND owner_principal_id=?`, appID, principalID))
}

// ListAppsForPrincipal lists only apps assigned to an authenticated principal.
func (s *Store) ListAppsForPrincipal(ctx context.Context, principalID string) ([]*App, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+appSelectCols+` FROM app WHERE owner_principal_id=? ORDER BY created_at DESC`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apps []*App
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

// SetSandboxOwnerPrincipal attaches an authenticated owner to a sandbox.
func (s *Store) SetSandboxOwnerPrincipal(ctx context.Context, sandboxID, principalID string) error {
	return s.submit(ctx, func(db *dialectDB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := scanPrincipal(tx.QueryRowContext(ctx,
			`SELECT `+principalCols+` FROM principal WHERE id=?`, principalID)); err != nil {
			return err
		}
		var currentOwner sql.NullString
		if err := tx.QueryRowContext(ctx, tx.ForUpdate(
			`SELECT owner_principal_id FROM sandbox WHERE id=?`), sandboxID).Scan(&currentOwner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if currentOwner.Valid && currentOwner.String != principalID {
			return ErrConflict
		}
		var durableOwner string
		err = tx.QueryRowContext(ctx,
			`SELECT principal_id FROM workspace_principal_owner WHERE sandbox_id=?`, sandboxID).Scan(&durableOwner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case durableOwner != principalID:
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE sandbox SET owner_principal_id=?, updated_at=? WHERE id=?`,
			principalID, s.now().Unix(), sandboxID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspace_principal_owner (sandbox_id, principal_id, created_at)
			VALUES (?, ?, ?)
			ON CONFLICT(sandbox_id) DO NOTHING`,
			sandboxID, principalID, s.now().Unix()); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// ListSandboxesForPrincipal lists sandboxes that an authenticated principal may
// operate. It retains the ordinary list ordering and includes requested ports.
func (s *Store) ListSandboxesForPrincipal(ctx context.Context, principalID string) ([]*Sandbox, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sandboxSelectCols+` FROM sandbox WHERE owner_principal_id=? ORDER BY created_at DESC`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sandboxes []*Sandbox
	for rows.Next() {
		sandbox, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		sandbox.Ports, err = s.portsFor(ctx, sandbox.ID)
		if err != nil {
			return nil, err
		}
		sandboxes = append(sandboxes, sandbox)
	}
	return sandboxes, rows.Err()
}

// GetSandboxForPrincipal returns a sandbox only when it belongs to principalID.
func (s *Store) GetSandboxForPrincipal(ctx context.Context, sandboxID, principalID string) (*Sandbox, error) {
	sb, err := scanSandbox(s.db.QueryRowContext(ctx,
		`SELECT `+sandboxSelectCols+` FROM sandbox WHERE id=? AND owner_principal_id=?`,
		sandboxID, principalID))
	if err != nil {
		return nil, err
	}
	sb.Ports, err = s.portsFor(ctx, sb.ID)
	return sb, err
}

// GetWorkspacePrincipalOwner returns the durable identity binding for a
// workspace. It remains available after a sandbox is deleted and is removed
// only by PurgeSandbox.
func (s *Store) GetWorkspacePrincipalOwner(ctx context.Context, sandboxID string) (string, error) {
	var principalID string
	err := s.db.QueryRowContext(ctx,
		`SELECT principal_id FROM workspace_principal_owner WHERE sandbox_id=?`, sandboxID).Scan(&principalID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return principalID, err
}

// BrowserSession is an opaque browser credential. TokenHash must be a hash of
// a generated cookie; the plaintext cookie is never persisted.
type BrowserSession struct {
	TokenHash   string
	PrincipalID string
	CreatedAt   time.Time
	LastUsedAt  time.Time
	ExpiresAt   time.Time
	RevokedAt   sql.NullInt64
}

func scanBrowserSession(sc scanner) (*BrowserSession, error) {
	var session BrowserSession
	var createdAt, lastUsedAt, expiresAt int64
	if err := sc.Scan(&session.TokenHash, &session.PrincipalID, &createdAt, &lastUsedAt,
		&expiresAt, &session.RevokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	session.CreatedAt = time.Unix(createdAt, 0).UTC()
	session.LastUsedAt = time.Unix(lastUsedAt, 0).UTC()
	session.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return &session, nil
}

// CreateBrowserSession persists an already-hashed opaque session token.
func (s *Store) CreateBrowserSession(ctx context.Context, session BrowserSession) error {
	if session.TokenHash == "" || session.PrincipalID == "" || session.ExpiresAt.IsZero() {
		return errors.New("invalid browser session")
	}
	return s.submit(ctx, func(db *dialectDB) error {
		now := s.now().Unix()
		if session.CreatedAt.IsZero() {
			session.CreatedAt = time.Unix(now, 0).UTC()
		}
		if session.LastUsedAt.IsZero() {
			session.LastUsedAt = session.CreatedAt
		}
		_, err := db.ExecContext(ctx, `
			INSERT INTO browser_session
			    (token_hash, principal_id, created_at, last_used_at, expires_at, revoked_at)
			VALUES (?, ?, ?, ?, ?, NULL)`,
			session.TokenHash, session.PrincipalID, session.CreatedAt.Unix(),
			session.LastUsedAt.Unix(), session.ExpiresAt.Unix())
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

// GetActiveBrowserSession returns a non-revoked, non-expired browser session.
func (s *Store) GetActiveBrowserSession(ctx context.Context, tokenHash string) (*BrowserSession, error) {
	return scanBrowserSession(s.db.QueryRowContext(ctx, `
		SELECT token_hash, principal_id, created_at, last_used_at, expires_at, revoked_at
		  FROM browser_session
		 WHERE token_hash=? AND revoked_at IS NULL AND expires_at>?`,
		tokenHash, s.now().Unix()))
}

// TouchBrowserSession records use only while the session remains active.
func (s *Store) TouchBrowserSession(ctx context.Context, tokenHash string) error {
	return s.submit(ctx, func(db *dialectDB) error {
		result, err := db.ExecContext(ctx, `
			UPDATE browser_session SET last_used_at=?
			 WHERE token_hash=? AND revoked_at IS NULL AND expires_at>?`,
			s.now().Unix(), tokenHash, s.now().Unix())
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RevokeBrowserSession invalidates a session without retaining its plaintext.
func (s *Store) RevokeBrowserSession(ctx context.Context, tokenHash string) error {
	return s.submit(ctx, func(db *dialectDB) error {
		result, err := db.ExecContext(ctx,
			`UPDATE browser_session SET revoked_at=? WHERE token_hash=? AND revoked_at IS NULL`,
			s.now().Unix(), tokenHash)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RevokeBrowserSessionsForPrincipal invalidates every active browser session
// for one principal. It is used by the Entra "sign out everywhere" flow.
func (s *Store) RevokeBrowserSessionsForPrincipal(ctx context.Context, principalID string) error {
	if principalID == "" {
		return errors.New("principal id is required")
	}
	return s.submit(ctx, func(db *dialectDB) error {
		_, err := db.ExecContext(ctx,
			`UPDATE browser_session SET revoked_at=? WHERE principal_id=? AND revoked_at IS NULL`,
			s.now().Unix(), principalID)
		return err
	})
}

// LoginTransaction contains only hashed state/nonces and encrypted PKCE
// verifier material. Encryption and decryption stay in the auth layer.
type LoginTransaction struct {
	ID                 string
	Provider           string
	StateHash          string
	NonceHash          string
	VerifierCiphertext []byte
	VerifierNonce      []byte
	RedirectURI        string
	ReturnLocation     string
	CreatedAt          time.Time
	ExpiresAt          time.Time
	ConsumedAt         sql.NullInt64
}

func scanLoginTransaction(sc scanner) (*LoginTransaction, error) {
	var transaction LoginTransaction
	var createdAt, expiresAt int64
	if err := sc.Scan(&transaction.ID, &transaction.Provider, &transaction.StateHash,
		&transaction.NonceHash, &transaction.VerifierCiphertext, &transaction.VerifierNonce,
		&transaction.RedirectURI, &transaction.ReturnLocation, &createdAt, &expiresAt, &transaction.ConsumedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	transaction.CreatedAt = time.Unix(createdAt, 0).UTC()
	transaction.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return &transaction, nil
}

// CreateLoginTransaction stores a one-time OAuth transaction. Callers must
// hash state/nonce and encrypt the verifier before invoking this method.
func (s *Store) CreateLoginTransaction(ctx context.Context, transaction LoginTransaction) error {
	if transaction.ID == "" || transaction.Provider == "" || transaction.StateHash == "" ||
		transaction.NonceHash == "" || len(transaction.VerifierCiphertext) == 0 ||
		len(transaction.VerifierNonce) == 0 || transaction.RedirectURI == "" || transaction.ReturnLocation == "" || transaction.ExpiresAt.IsZero() {
		return errors.New("invalid login transaction")
	}
	return s.submit(ctx, func(db *dialectDB) error {
		createdAt := transaction.CreatedAt
		if createdAt.IsZero() {
			createdAt = s.now().UTC()
		}
		_, err := db.ExecContext(ctx, `
			INSERT INTO login_transaction
			    (id, provider, state_hash, nonce_hash, verifier_ciphertext, verifier_nonce,
			     redirect_uri, return_location, created_at, expires_at, consumed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			transaction.ID, transaction.Provider, transaction.StateHash, transaction.NonceHash,
			transaction.VerifierCiphertext, transaction.VerifierNonce, transaction.RedirectURI,
			transaction.ReturnLocation, createdAt.Unix(), transaction.ExpiresAt.Unix())
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

// ConsumeLoginTransaction atomically consumes a non-expired OAuth transaction.
// A replay, expiry, or unknown state yields ErrNotFound so callers do not leak
// which condition rejected a browser callback.
func (s *Store) ConsumeLoginTransaction(ctx context.Context, stateHash string) (*LoginTransaction, error) {
	var consumed *LoginTransaction
	err := s.submit(ctx, func(db *dialectDB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		transaction, err := scanLoginTransaction(tx.QueryRowContext(ctx, tx.ForUpdate(`
			SELECT id, provider, state_hash, nonce_hash, verifier_ciphertext, verifier_nonce,
			       redirect_uri, return_location, created_at, expires_at, consumed_at
			  FROM login_transaction WHERE state_hash=?`), stateHash))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrNotFound
			}
			return err
		}
		if transaction.ConsumedAt.Valid || transaction.ExpiresAt.Unix() <= s.now().Unix() {
			return ErrNotFound
		}
		now := s.now().Unix()
		result, err := tx.ExecContext(ctx, `
			UPDATE login_transaction SET consumed_at=?
			 WHERE id=? AND consumed_at IS NULL AND expires_at>?`,
			now, transaction.ID, now)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrNotFound
		}
		transaction.ConsumedAt = sql.NullInt64{Int64: now, Valid: true}
		consumed = transaction
		return tx.Commit()
	})
	return consumed, err
}

type LeaseResource string

const (
	LeaseResourceSandbox      LeaseResource = "sandbox"
	LeaseResourceConversation LeaseResource = "conversation"
	LeaseResourceTask         LeaseResource = "task"
)

var (
	ErrLeaseHeld = errors.New("operation lease is held")
	ErrLeaseLost = errors.New("operation lease is no longer held")
)

const postgresLeaseClock = `WITH lease_clock AS (
	SELECT FLOOR(EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT AS now_ms
)`

const postgresAcquireLease = postgresLeaseClock + `
INSERT INTO operation_lease
    (resource_type, resource_id, holder_id, token, acquired_at, heartbeat_at, expires_at)
SELECT ?, ?, ?, ?, now_ms, now_ms, now_ms + ? FROM lease_clock
ON CONFLICT(resource_type, resource_id) DO UPDATE SET
    holder_id=excluded.holder_id, token=excluded.token, acquired_at=excluded.acquired_at,
    heartbeat_at=excluded.heartbeat_at, expires_at=excluded.expires_at
WHERE operation_lease.expires_at <= (SELECT now_ms FROM lease_clock)
RETURNING acquired_at, heartbeat_at, expires_at`

const postgresHeartbeatLease = postgresLeaseClock + `
UPDATE operation_lease
   SET heartbeat_at=lease_clock.now_ms, expires_at=lease_clock.now_ms + ?
  FROM lease_clock
 WHERE resource_type=? AND resource_id=? AND holder_id=? AND token=?
   AND expires_at > lease_clock.now_ms
RETURNING acquired_at, heartbeat_at, expires_at`

// OperationLease is a database-backed ownership fence for work performed by a
// control-plane replica. Token must accompany every heartbeat and release.
type OperationLease struct {
	Resource    LeaseResource
	ResourceID  string
	HolderID    string
	Token       string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
	ExpiresAt   time.Time
}

func newLeaseToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate lease token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validLeaseResource(resource LeaseResource) bool {
	return resource == LeaseResourceSandbox || resource == LeaseResourceConversation || resource == LeaseResourceTask
}

// AcquireOperationLease takes a lease only when it is absent or expired. The
// single conditional statement is a compare-and-set on both providers.
func (s *Store) AcquireOperationLease(ctx context.Context, resource LeaseResource, resourceID, holderID string, ttl time.Duration) (*OperationLease, error) {
	if !validLeaseResource(resource) || resourceID == "" || holderID == "" || ttl <= 0 {
		return nil, errors.New("invalid operation lease")
	}
	token, err := newLeaseToken()
	if err != nil {
		return nil, err
	}
	var acquired *OperationLease
	err = s.submit(ctx, func(db *dialectDB) error {
		if db.providerName() == ProviderPostgres {
			var acquiredAt, heartbeatAt, expiresAt int64
			err := db.QueryRowContext(ctx, postgresAcquireLease,
				resource, resourceID, holderID, token, ttl.Milliseconds()).
				Scan(&acquiredAt, &heartbeatAt, &expiresAt)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLeaseHeld
			}
			if err != nil {
				return err
			}
			acquired = &OperationLease{
				Resource: resource, ResourceID: resourceID, HolderID: holderID, Token: token,
				AcquiredAt:  time.UnixMilli(acquiredAt).UTC(),
				HeartbeatAt: time.UnixMilli(heartbeatAt).UTC(),
				ExpiresAt:   time.UnixMilli(expiresAt).UTC(),
			}
			return nil
		}
		now := s.now().UTC()
		expiresAt := now.Add(ttl)
		result, err := db.ExecContext(ctx, `
			INSERT INTO operation_lease
			    (resource_type, resource_id, holder_id, token, acquired_at, heartbeat_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(resource_type, resource_id) DO UPDATE SET
			    holder_id=excluded.holder_id, token=excluded.token, acquired_at=excluded.acquired_at,
			    heartbeat_at=excluded.heartbeat_at, expires_at=excluded.expires_at
			WHERE operation_lease.expires_at <= ?`,
			resource, resourceID, holderID, token, now.UnixMilli(), now.UnixMilli(), expiresAt.UnixMilli(), now.UnixMilli())
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrLeaseHeld
		}
		acquired = &OperationLease{
			Resource: resource, ResourceID: resourceID, HolderID: holderID, Token: token,
			AcquiredAt: now, HeartbeatAt: now, ExpiresAt: expiresAt,
		}
		return nil
	})
	return acquired, err
}

// HeartbeatOperationLease extends a lease only when the same holder and token
// still own an unexpired row.
func (s *Store) HeartbeatOperationLease(ctx context.Context, lease OperationLease, ttl time.Duration) (*OperationLease, error) {
	if !validLeaseResource(lease.Resource) || lease.ResourceID == "" || lease.HolderID == "" || lease.Token == "" || ttl <= 0 {
		return nil, errors.New("invalid operation lease")
	}
	var renewed *OperationLease
	err := s.submit(ctx, func(db *dialectDB) error {
		if db.providerName() == ProviderPostgres {
			var acquiredAt, heartbeatAt, expiresAt int64
			err := db.QueryRowContext(ctx, postgresHeartbeatLease,
				ttl.Milliseconds(), lease.Resource, lease.ResourceID, lease.HolderID, lease.Token).
				Scan(&acquiredAt, &heartbeatAt, &expiresAt)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLeaseLost
			}
			if err != nil {
				return err
			}
			lease.AcquiredAt = time.UnixMilli(acquiredAt).UTC()
			lease.HeartbeatAt = time.UnixMilli(heartbeatAt).UTC()
			lease.ExpiresAt = time.UnixMilli(expiresAt).UTC()
			renewed = &lease
			return nil
		}
		now := s.now().UTC()
		expiresAt := now.Add(ttl)
		result, err := db.ExecContext(ctx, `
			UPDATE operation_lease SET heartbeat_at=?, expires_at=?
			 WHERE resource_type=? AND resource_id=? AND holder_id=? AND token=? AND expires_at>?`,
			now.UnixMilli(), expiresAt.UnixMilli(), lease.Resource, lease.ResourceID,
			lease.HolderID, lease.Token, now.UnixMilli())
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrLeaseLost
		}
		lease.HeartbeatAt = now
		lease.ExpiresAt = expiresAt
		renewed = &lease
		return nil
	})
	return renewed, err
}

// ReleaseOperationLease releases only the fenced lease holder's row.
func (s *Store) ReleaseOperationLease(ctx context.Context, lease OperationLease) error {
	if !validLeaseResource(lease.Resource) || lease.ResourceID == "" || lease.HolderID == "" || lease.Token == "" {
		return errors.New("invalid operation lease")
	}
	return s.submit(ctx, func(db *dialectDB) error {
		result, err := db.ExecContext(ctx, `
			DELETE FROM operation_lease
			 WHERE resource_type=? AND resource_id=? AND holder_id=? AND token=?`,
			lease.Resource, lease.ResourceID, lease.HolderID, lease.Token)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrLeaseLost
		}
		return nil
	})
}
