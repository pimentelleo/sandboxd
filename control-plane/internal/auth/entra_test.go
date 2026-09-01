package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"
)

func testEntraConfig() EntraConfig {
	return EntraConfig{
		TenantID: "tenant-123", ClientID: "client-123", ClientSecret: "secret",
		RedirectURL:  "https://console.example.test/v1/auth/entra/callback",
		Issuer:       "https://login.example.test/tenant-123/v2.0",
		AuthorizeURL: "https://login.example.test/tenant-123/oauth2/v2.0/authorize",
		TokenURL:     "https://login.example.test/tenant-123/oauth2/v2.0/token",
		JWKSURL:      "https://login.example.test/tenant-123/discovery/v2.0/keys",
	}
}

func TestParseConfigEntraRequirements(t *testing.T) {
	env := map[string]string{
		"SANDBOXD_AUTH_PROFILE":        "entra",
		"SANDBOXD_ENTRA_TENANT_ID":     "tenant-123",
		"SANDBOXD_ENTRA_CLIENT_ID":     "client-123",
		"SANDBOXD_ENTRA_CLIENT_SECRET": "secret",
		"SANDBOXD_ENTRA_REDIRECT_URL":  "https://console.example.test/v1/auth/entra/callback",
	}
	cfg := ParseConfig(MapGetter(env))
	if cfg.Profile != ProfileEntra || !cfg.ProductionReady() {
		t.Fatalf("valid Entra config was rejected: profile=%q problem=%q", cfg.Profile, cfg.Problem)
	}
	if cfg.Entra.Issuer != "https://login.microsoftonline.com/tenant-123/v2.0" {
		t.Fatalf("issuer = %q", cfg.Entra.Issuer)
	}

	delete(env, "SANDBOXD_ENTRA_CLIENT_SECRET")
	cfg = ParseConfig(MapGetter(env))
	if cfg.ProductionReady() || cfg.Problem == "" {
		t.Fatalf("missing production credential must fail closed: %+v", cfg)
	}

	env["SANDBOXD_ENTRA_CLIENT_SECRET"] = "secret"
	env["SANDBOXD_API_AUTH_DISABLED"] = "true"
	cfg = ParseConfig(MapGetter(env))
	if cfg.ProductionReady() {
		t.Fatal("disabled production auth must fail closed")
	}
}

func TestPrincipalRecognizesOnlySandboxdRoles(t *testing.T) {
	claims := OIDCClaims{
		OID: "oid-1", TenantID: "tenant-1", Name: "Ada\nAdmin",
		PreferredUsername: "ada@example.test", Roles: []string{"other.role", string(RoleAdmin), string(RoleAdmin)},
	}
	principal, err := PrincipalFromOIDCClaims(claims)
	if err != nil {
		t.Fatal(err)
	}
	if !principal.HasRole(RoleAdmin) || principal.HasRole(RoleUser) || len(principal.Roles) != 1 {
		t.Fatalf("recognized roles = %#v", principal.Roles)
	}
	if principal.DisplayName != "" {
		t.Fatalf("unsafe display name was retained: %q", principal.DisplayName)
	}
	if _, err := PrincipalFromOIDCClaims(OIDCClaims{OID: "oid", TenantID: "tenant", Roles: []string{"unrelated"}}); err != ErrOIDCRole {
		t.Fatalf("unrecognized roles error = %v, want %v", err, ErrOIDCRole)
	}
}

type captureExchanger struct {
	codeVerifier string
	calls        int
}

func (x *captureExchanger) ExchangeAuthorizationCode(_ context.Context, _ EntraConfig, code, verifier string) (string, error) {
	x.calls++
	x.codeVerifier = verifier
	if code != "good-code" {
		return "", ErrOIDCProvider
	}
	return "id-token", nil
}

type expectedVerifier struct {
	expected OIDCValidation
	nonce    string
	calls    int
}

func (v *expectedVerifier) VerifyIDToken(_ context.Context, raw string, expected OIDCValidation) (OIDCClaims, error) {
	v.calls++
	if raw != "id-token" {
		return OIDCClaims{}, ErrOIDCToken
	}
	v.expected = expected
	return OIDCClaims{
		Issuer: expected.Issuer, Audience: []string{expected.Audience},
		ExpiresAt: expected.Now.Add(time.Hour), NotBefore: expected.Now.Add(-time.Minute),
		IssuedAt: expected.Now.Add(-time.Minute), Nonce: v.nonce,
		OID: "oid-1", TenantID: expected.TenantID, Name: "Ada", PreferredUsername: "ada@example.test",
		Roles: []string{string(RoleUser)},
	}, nil
}

type memoryLoginTransactions struct {
	mu    sync.Mutex
	now   func() time.Time
	items map[string]LoginTransaction
}

func (s *memoryLoginTransactions) CreateLoginTransaction(_ context.Context, transaction LoginTransaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = map[string]LoginTransaction{}
	}
	if _, exists := s.items[transaction.StateHash]; exists {
		return errors.New("duplicate transaction")
	}
	s.items[transaction.StateHash] = transaction
	return nil
}

func (s *memoryLoginTransactions) ConsumeLoginTransaction(_ context.Context, stateHash string) (*LoginTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	transaction, found := s.items[stateHash]
	if !found || !s.now().Before(transaction.ExpiresAt) {
		return nil, errors.New("transaction not found")
	}
	delete(s.items, stateHash)
	return &transaction, nil
}

func TestOIDCFlowUsesOneTimeStatePKCEAndNonce(t *testing.T) {
	now := time.Date(2026, 8, 28, 18, 0, 0, 0, time.UTC)
	exchanger := &captureExchanger{}
	verifier := &expectedVerifier{}
	transactions := &memoryLoginTransactions{now: func() time.Time { return now }}
	flow := NewOIDCFlow(testEntraConfig(), exchanger, verifier, transactions)
	flow.now = func() time.Time { return now }

	start, err := flow.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("missing OIDC anti-forgery parameter: %s", start.AuthorizationURL)
	}
	transaction := transactions.items[valueHash(q.Get("state"))]
	if transaction.StateHash == q.Get("state") || transaction.NonceHash == q.Get("nonce") ||
		transaction.ReturnLocation != "/" || len(transaction.VerifierCiphertext) == 0 {
		t.Fatalf("unsafe persisted transaction: %#v", transaction)
	}
	verifier.nonce = q.Get("nonce")

	// A second flow instance represents another control-plane replica using the
	// same durable transaction store and Entra configuration.
	replica := NewOIDCFlow(testEntraConfig(), exchanger, verifier, transactions)
	replica.now = func() time.Time { return now }
	principal, err := replica.Complete(context.Background(), q.Get("state"), "good-code")
	if err != nil {
		t.Fatal(err)
	}
	if principal.OID != "oid-1" || !principal.HasRole(RoleUser) {
		t.Fatalf("principal = %+v", principal)
	}
	sum := sha256.Sum256([]byte(exchanger.codeVerifier))
	if got, want := q.Get("code_challenge"), base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
		t.Fatalf("PKCE challenge = %q, want %q", got, want)
	}
	if verifier.expected.ExpectedNonceHash != valueHash(q.Get("nonce")) {
		t.Fatalf("nonce hash = %q", verifier.expected.ExpectedNonceHash)
	}
	if _, err := flow.Complete(context.Background(), q.Get("state"), "good-code"); err != ErrOIDCState {
		t.Fatalf("replayed state error = %v, want %v", err, ErrOIDCState)
	}
	if exchanger.calls != 1 {
		t.Fatalf("exchange calls = %d, want 1", exchanger.calls)
	}
}

func TestOIDCFlowRejectsExpiredStateAndUnsafeReturn(t *testing.T) {
	now := time.Date(2026, 8, 28, 18, 0, 0, 0, time.UTC)
	transactions := &memoryLoginTransactions{now: func() time.Time { return now }}
	flow := NewOIDCFlow(testEntraConfig(), &captureExchanger{}, &expectedVerifier{}, transactions)
	flow.now = func() time.Time { return now }

	start, err := flow.BeginWithReturn(context.Background(), "https://attacker.example")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(entraStateTTL)
	if _, err := flow.Complete(context.Background(), u.Query().Get("state"), "good-code"); err != ErrOIDCState {
		t.Fatalf("expired transaction error = %v, want %v", err, ErrOIDCState)
	}
}

func TestSafeReturnLocationRejectsAuthorityAmbiguities(t *testing.T) {
	for _, raw := range []string{
		"https://attacker.example", "//attacker.example", `/\attacker.example`, `/%2f%2fattacker.example`,
		`/%5cattacker.example`, "/safe#fragment",
	} {
		if got := safeReturnLocation(raw); got != "/" {
			t.Fatalf("safeReturnLocation(%q) = %q, want /", raw, got)
		}
	}
	if got := safeReturnLocation("/apps?tab=mine"); got != "/apps?tab=mine" {
		t.Fatalf("safe return location = %q", got)
	}
}

func TestOIDCClaimsRejectIssuerTenantAudienceNonceAndTime(t *testing.T) {
	now := time.Now().UTC()
	expected := OIDCValidation{TenantID: "tenant", Issuer: "https://issuer", Audience: "client", ExpectedNonce: "nonce", Now: now}
	claims := OIDCClaims{
		Issuer: expected.Issuer, Audience: []string{expected.Audience}, TenantID: expected.TenantID, Nonce: expected.ExpectedNonce,
		ExpiresAt: now.Add(time.Hour), NotBefore: now.Add(-time.Minute), IssuedAt: now.Add(-time.Minute),
	}
	if err := ValidateOIDCClaims(claims, expected); err != nil {
		t.Fatal(err)
	}
	claims.Audience = []string{"other"}
	if err := ValidateOIDCClaims(claims, expected); err == nil {
		t.Fatal("wrong audience accepted")
	}
	claims.Audience = []string{expected.Audience}
	claims.ExpiresAt = now.Add(-time.Second)
	if err := ValidateOIDCClaims(claims, expected); err == nil {
		t.Fatal("expired claim accepted")
	}
	claims.ExpiresAt = now.Add(time.Hour)
	claims.Audience = []string{expected.Audience, "another-client"}
	if err := ValidateOIDCClaims(claims, expected); err == nil {
		t.Fatal("multiple audiences without matching azp accepted")
	}
	claims.AuthorizedParty = expected.Audience
	if err := ValidateOIDCClaims(claims, expected); err != nil {
		t.Fatalf("matching azp rejected: %v", err)
	}
}
