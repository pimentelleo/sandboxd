package api

import (
	"context"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// storeResolver implements auth.CredentialResolver over the configured store: it
// maps session cookies and API keys to the (single, shared) tenant. Wired into
// the auth middleware in main.go.
type storeResolver struct{ st *store.Store }

// NewStoreResolver returns an auth.CredentialResolver backed by st.
func NewStoreResolver(st *store.Store) auth.CredentialResolver { return storeResolver{st: st} }

func (r storeResolver) ResolveSession(ctx context.Context, cookieValue string) (string, bool) {
	if r.st == nil || cookieValue == "" {
		return "", false
	}
	h := console.HashToken(cookieValue)
	owner, expires, found, err := r.st.LookupSession(ctx, h)
	if err != nil || !found || time.Now().Unix() > expires {
		return "", false
	}
	_ = r.st.TouchSession(ctx, h, time.Now().Unix())
	return owner, true
}

func (r storeResolver) ResolveEntraSession(ctx context.Context, cookieValue string) (*auth.EntraSession, bool) {
	if r.st == nil || cookieValue == "" {
		return nil, false
	}
	hash := console.HashToken(cookieValue)
	session, err := r.st.GetActiveBrowserSession(ctx, hash)
	if err != nil {
		return nil, false
	}
	// Touch is a second active-state check, closing the lookup/use gap for a
	// session revoked concurrently by logout.
	if err := r.st.TouchBrowserSession(ctx, hash); err != nil {
		return nil, false
	}
	stored, err := r.st.GetPrincipalByID(ctx, session.PrincipalID)
	if err != nil || stored.Provider != "entra" {
		return nil, false
	}
	principal, err := auth.PrincipalFromOIDCClaims(auth.OIDCClaims{
		OID: stored.Subject, TenantID: stored.TenantID, Name: stored.DisplayName.String,
		PreferredUsername: stored.Email.String, Roles: stored.Roles,
	})
	if err != nil {
		return nil, false
	}
	return &auth.EntraSession{
		PrincipalID:             stored.ID,
		BrowserSessionTokenHash: hash,
		Principal:               principal,
	}, true
}

func (r storeResolver) ResolveLocalAccountSession(ctx context.Context, cookieValue string) (*auth.LocalAccountSession, bool) {
	if r.st == nil || cookieValue == "" {
		return nil, false
	}
	hash := console.HashToken(cookieValue)
	session, err := r.st.GetActiveBrowserSession(ctx, hash)
	if err != nil {
		return nil, false
	}
	// This second active-state check prevents a concurrent logout or password
	// change from extending a session that was already revoked.
	if err := r.st.TouchBrowserSession(ctx, hash); err != nil {
		return nil, false
	}
	account, err := r.st.GetLocalAccountByPrincipal(ctx, session.PrincipalID)
	if err != nil {
		return nil, false
	}
	roles := make([]auth.Role, 0, len(account.Principal.Roles))
	for _, role := range account.Principal.Roles {
		roles = append(roles, auth.Role(role))
	}
	if len(roles) == 0 {
		return nil, false
	}
	return &auth.LocalAccountSession{
		PrincipalID:             account.Principal.ID,
		BrowserSessionTokenHash: hash,
		Subject:                 account.Principal.Subject,
		DisplayName:             account.Principal.DisplayName.String,
		Email:                   account.Principal.Email.String,
		Roles:                   roles,
	}, true
}

func (r storeResolver) ResolveAPIKey(ctx context.Context, presented string) (string, bool) {
	if r.st == nil || presented == "" {
		return "", false
	}
	id, found, err := r.st.LookupAPIKey(ctx, console.HashToken(presented))
	if err != nil || !found {
		return "", false
	}
	_ = r.st.TouchAPIKey(ctx, id, time.Now().Unix())
	return store.DefaultTenant, true // single shared tenant
}

type entraLoginTransactionStore struct{ st *store.Store }

// NewEntraLoginTransactionStore adapts durable store transactions to the
// deliberately narrow auth interface.
func NewEntraLoginTransactionStore(st *store.Store) auth.LoginTransactionStore {
	if st == nil {
		return nil
	}
	return entraLoginTransactionStore{st: st}
}

func (r storeResolver) LoginTransactionStore() auth.LoginTransactionStore {
	return NewEntraLoginTransactionStore(r.st)
}

func (s entraLoginTransactionStore) CreateLoginTransaction(ctx context.Context, transaction auth.LoginTransaction) error {
	return s.st.CreateLoginTransaction(ctx, store.LoginTransaction{
		ID:                 transaction.ID,
		Provider:           transaction.Provider,
		StateHash:          transaction.StateHash,
		NonceHash:          transaction.NonceHash,
		VerifierCiphertext: transaction.VerifierCiphertext,
		VerifierNonce:      transaction.VerifierNonce,
		RedirectURI:        transaction.RedirectURI,
		ReturnLocation:     transaction.ReturnLocation,
		CreatedAt:          transaction.CreatedAt,
		ExpiresAt:          transaction.ExpiresAt,
	})
}

func (s entraLoginTransactionStore) ConsumeLoginTransaction(ctx context.Context, stateHash string) (*auth.LoginTransaction, error) {
	transaction, err := s.st.ConsumeLoginTransaction(ctx, stateHash)
	if err != nil {
		return nil, err
	}
	return &auth.LoginTransaction{
		ID:                 transaction.ID,
		Provider:           transaction.Provider,
		StateHash:          transaction.StateHash,
		NonceHash:          transaction.NonceHash,
		VerifierCiphertext: transaction.VerifierCiphertext,
		VerifierNonce:      transaction.VerifierNonce,
		RedirectURI:        transaction.RedirectURI,
		ReturnLocation:     transaction.ReturnLocation,
		CreatedAt:          transaction.CreatedAt,
		ExpiresAt:          transaction.ExpiresAt,
	}, nil
}
