package auth

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	entraStateTTL = 10 * time.Minute
	clockSkew     = 2 * time.Minute
)

var (
	ErrOIDCUnavailable = errors.New("OIDC is not configured")
	ErrOIDCState       = errors.New("OIDC state is invalid or expired")
	ErrOIDCProvider    = errors.New("OIDC provider denied authentication")
	ErrOIDCToken       = errors.New("OIDC token validation failed")
	ErrOIDCRole        = errors.New("OIDC principal has no console role")
)

// Role is an Entra app role sandboxd recognizes. No other role grants access.
type Role string

const (
	RoleUser  Role = "sandboxd.user"
	RoleAdmin Role = "sandboxd.admin"
)

// Principal is the authenticated Entra identity retained with an opaque
// server-side session. OID and TenantID are immutable directory identifiers;
// display fields are sanitized presentation metadata only.
type Principal struct {
	OID         string
	TenantID    string
	DisplayName string
	UPN         string
	Roles       []Role
}

func newOIDCHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Subject is stable across display-name or UPN changes and is suitable for
// ownership/audit attribution.
func (p Principal) Subject() string { return "entra:" + p.TenantID + ":" + p.OID }

func (p Principal) HasRole(role Role) bool {
	for _, got := range p.Roles {
		if got == role {
			return true
		}
	}
	return false
}

// SafePrincipal is the only Entra identity representation returned by the API.
type SafePrincipal struct {
	OID         string   `json:"oid"`
	TenantID    string   `json:"tenant_id"`
	DisplayName string   `json:"display_name,omitempty"`
	UPN         string   `json:"upn,omitempty"`
	Roles       []string `json:"roles"`
}

func (p Principal) Safe() SafePrincipal {
	roles := make([]string, 0, len(p.Roles))
	for _, role := range p.Roles {
		roles = append(roles, string(role))
	}
	return SafePrincipal{
		OID: p.OID, TenantID: p.TenantID, DisplayName: p.DisplayName, UPN: p.UPN, Roles: roles,
	}
}

// OIDCClaims is the verified subset of an Entra ID token needed to create a
// principal. TokenVerifier implementations must not return raw token material.
type OIDCClaims struct {
	Issuer            string
	Audience          []string
	AuthorizedParty   string
	ExpiresAt         time.Time
	NotBefore         time.Time
	IssuedAt          time.Time
	Nonce             string
	OID               string
	TenantID          string
	Name              string
	PreferredUsername string
	UPN               string
	Roles             []string
}

// OIDCValidation is the immutable expectation for a single ID-token
// validation. It lets production use a real JWKS verifier while tests inject a
// deterministic verifier without weakening the caller's checks.
type OIDCValidation struct {
	TenantID          string
	Issuer            string
	Audience          string
	ExpectedNonce     string
	ExpectedNonceHash string
	Now               time.Time
}

// TokenVerifier verifies an ID-token signature against JWKS and returns only
// parsed claims. Implementations are expected to validate OIDCValidation too;
// OIDCFlow validates the returned claims again before creating a session.
type TokenVerifier interface {
	VerifyIDToken(ctx context.Context, rawIDToken string, expected OIDCValidation) (OIDCClaims, error)
}

// CodeExchanger exchanges an authorization code and deliberately returns only
// the ID token needed by this login flow. Refresh/access tokens never enter a
// session, response, log, or principal.
type CodeExchanger interface {
	ExchangeAuthorizationCode(ctx context.Context, cfg EntraConfig, code, codeVerifier string) (string, error)
}

// LoginTransaction is the storage-neutral representation of a one-use OIDC
// login. State and nonce are one-way hashes and the PKCE verifier is encrypted.
// ReturnLocation is a validated same-origin path, never an absolute URL.
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
}

// LoginTransactionStore is fulfilled by the API adapter over store.Store.
// Keeping it narrow avoids an auth-to-store dependency and makes verifier
// tests deterministic.
type LoginTransactionStore interface {
	CreateLoginTransaction(context.Context, LoginTransaction) error
	ConsumeLoginTransaction(context.Context, string) (*LoginTransaction, error)
}

// LoginStart is the safe result of beginning an authorization-code flow.
type LoginStart struct {
	AuthorizationURL string
	ExpiresAt        time.Time
}

// OIDCFlow manages durable, one-use state/PKCE/nonce challenges. State never
// resides in persistent storage in plaintext.
type OIDCFlow struct {
	cfg          EntraConfig
	exchanger    CodeExchanger
	verifier     TokenVerifier
	transactions LoginTransactionStore
	now          func() time.Time
}

// NewOIDCFlow constructs a production flow. Nil collaborators use the
// standard-library HTTPS token exchanger and JWKS verifier; tests can inject
// either collaborator.
func NewOIDCFlow(cfg EntraConfig, exchanger CodeExchanger, verifier TokenVerifier, transactions LoginTransactionStore) *OIDCFlow {
	if exchanger == nil {
		exchanger = httpCodeExchanger{client: newOIDCHTTPClient()}
	}
	if verifier == nil {
		verifier = NewJWKSVerifier(cfg.JWKSURL, nil)
	}
	return &OIDCFlow{
		cfg: cfg, exchanger: exchanger, verifier: verifier, transactions: transactions, now: time.Now,
	}
}

// Persistent reports whether login transactions can be shared across replicas.
func (f *OIDCFlow) Persistent() bool {
	return f != nil && f.transactions != nil
}

// Begin generates a one-use state, S256 PKCE challenge, and nonce, persists
// the transaction, then returns the Entra authorize URL.
func (f *OIDCFlow) Begin(ctx context.Context) (LoginStart, error) {
	return f.BeginWithReturn(ctx, "/")
}

// BeginWithReturn persists a validated same-origin location for the post-login
// redirect. Invalid locations collapse to "/" and cannot create an open redirect.
func (f *OIDCFlow) BeginWithReturn(ctx context.Context, returnLocation string) (LoginStart, error) {
	if f == nil || !f.Persistent() || f.cfg.Validate() != nil {
		return LoginStart{}, ErrOIDCUnavailable
	}
	state, err := randomURLValue(32)
	if err != nil {
		return LoginStart{}, err
	}
	verifier, err := randomURLValue(48)
	if err != nil {
		return LoginStart{}, err
	}
	nonce, err := randomURLValue(32)
	if err != nil {
		return LoginStart{}, err
	}
	id, err := randomURLValue(24)
	if err != nil {
		return LoginStart{}, err
	}
	now := f.now().UTC()
	exp := now.Add(entraStateTTL)
	ciphertext, verifierNonce, err := f.encryptVerifier(verifier)
	if err != nil {
		return LoginStart{}, ErrOIDCUnavailable
	}
	if err := f.transactions.CreateLoginTransaction(ctx, LoginTransaction{
		ID:                 id,
		Provider:           "entra",
		StateHash:          valueHash(state),
		NonceHash:          valueHash(nonce),
		VerifierCiphertext: ciphertext,
		VerifierNonce:      verifierNonce,
		RedirectURI:        f.cfg.RedirectURL,
		ReturnLocation:     safeReturnLocation(returnLocation),
		CreatedAt:          now,
		ExpiresAt:          exp,
	}); err != nil {
		return LoginStart{}, ErrOIDCUnavailable
	}

	challengeHash := sha256.Sum256([]byte(verifier))
	u, err := url.Parse(f.cfg.AuthorizeURL)
	if err != nil {
		return LoginStart{}, ErrOIDCUnavailable
	}
	q := u.Query()
	q.Set("client_id", f.cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", f.cfg.RedirectURL)
	q.Set("response_mode", "query")
	q.Set("scope", "openid profile email")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challengeHash[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return LoginStart{AuthorizationURL: u.String(), ExpiresAt: exp}, nil
}

// Complete consumes state before exchanging the code, then verifies the
// returned ID token and turns it into a recognized Entra principal.
func (f *OIDCFlow) Complete(ctx context.Context, state, code string) (Principal, error) {
	principal, _, err := f.CompleteWithReturn(ctx, state, code)
	return principal, err
}

// CompleteWithReturn returns the verified principal and its persisted safe
// return location. The transaction is consumed before token exchange so any
// failed callback remains one-use.
func (f *OIDCFlow) CompleteWithReturn(ctx context.Context, state, code string) (Principal, string, error) {
	if f == nil || !f.Persistent() || f.cfg.Validate() != nil {
		return Principal{}, "", ErrOIDCUnavailable
	}
	if state == "" {
		return Principal{}, "", ErrOIDCState
	}
	transaction, err := f.transactions.ConsumeLoginTransaction(ctx, valueHash(state))
	if err != nil || transaction == nil || transaction.Provider != "entra" ||
		transaction.RedirectURI != f.cfg.RedirectURL || !safeStoredReturnLocation(transaction.ReturnLocation) {
		return Principal{}, "", ErrOIDCState
	}
	verifier, err := f.decryptVerifier(transaction.VerifierCiphertext, transaction.VerifierNonce)
	if err != nil {
		return Principal{}, "", ErrOIDCState
	}
	if code == "" {
		return Principal{}, "", ErrOIDCProvider
	}
	expected := OIDCValidation{
		TenantID: f.cfg.TenantID, Issuer: f.cfg.Issuer, Audience: f.cfg.ClientID,
		ExpectedNonceHash: transaction.NonceHash, Now: f.now().UTC(),
	}
	idToken, err := f.exchanger.ExchangeAuthorizationCode(ctx, f.cfg, code, verifier)
	if err != nil || idToken == "" {
		return Principal{}, "", ErrOIDCProvider
	}
	claims, err := f.verifier.VerifyIDToken(ctx, idToken, expected)
	if err != nil {
		return Principal{}, "", ErrOIDCToken
	}
	if err := ValidateOIDCClaims(claims, expected); err != nil {
		return Principal{}, "", ErrOIDCToken
	}
	principal, err := PrincipalFromOIDCClaims(claims)
	if err != nil {
		return Principal{}, "", err
	}
	return principal, transaction.ReturnLocation, nil
}

func (f *OIDCFlow) transactionAEAD() (cipher.AEAD, error) {
	key := sha256.Sum256([]byte("sandboxd/entra/login-transaction/v1\x00" + f.cfg.ClientSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (f *OIDCFlow) transactionAAD() []byte {
	return []byte("sandboxd/entra/login-transaction/v1\x00" + f.cfg.TenantID + "\x00" + f.cfg.ClientID + "\x00" + f.cfg.RedirectURL)
}

func (f *OIDCFlow) encryptVerifier(verifier string) ([]byte, []byte, error) {
	aead, err := f.transactionAEAD()
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return aead.Seal(nil, nonce, []byte(verifier), f.transactionAAD()), nonce, nil
}

func (f *OIDCFlow) decryptVerifier(ciphertext, nonce []byte) (string, error) {
	aead, err := f.transactionAEAD()
	if err != nil || len(nonce) != aead.NonceSize() {
		return "", ErrOIDCState
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, f.transactionAAD())
	if err != nil || len(plaintext) == 0 {
		return "", ErrOIDCState
	}
	return string(plaintext), nil
}

func valueHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func safeReturnLocation(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Fragment != "" ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return "/"
	}
	decodedPath, err := url.PathUnescape(u.EscapedPath())
	if err != nil || strings.HasPrefix(decodedPath, "//") || strings.Contains(decodedPath, "\\") {
		return "/"
	}
	return u.String()
}

func safeStoredReturnLocation(raw string) bool {
	return safeReturnLocation(raw) == raw
}

func randomURLValue(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidateOIDCClaims enforces issuer, tenant, audience, nonce, and temporal
// bounds independently of the signature verifier.
func ValidateOIDCClaims(c OIDCClaims, expected OIDCValidation) error {
	now := expected.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if c.Issuer != expected.Issuer || c.TenantID != expected.TenantID || !contains(c.Audience, expected.Audience) {
		return errors.New("issuer, tenant, or audience mismatch")
	}
	if len(c.Audience) > 1 && c.AuthorizedParty != expected.Audience {
		return errors.New("authorized party mismatch")
	}
	if c.ExpiresAt.IsZero() || !now.Before(c.ExpiresAt) ||
		(!c.NotBefore.IsZero() && c.NotBefore.After(now.Add(clockSkew))) ||
		c.IssuedAt.IsZero() || c.IssuedAt.After(now.Add(clockSkew)) {
		return errors.New("invalid token time claims")
	}
	nonceMatches := expected.ExpectedNonce != "" &&
		subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(expected.ExpectedNonce)) == 1
	nonceHashMatches := expected.ExpectedNonceHash != "" &&
		subtle.ConstantTimeCompare([]byte(valueHash(c.Nonce)), []byte(expected.ExpectedNonceHash)) == 1
	if !nonceMatches && !nonceHashMatches {
		return errors.New("nonce mismatch")
	}
	return nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if subtle.ConstantTimeCompare([]byte(value), []byte(target)) == 1 {
			return true
		}
	}
	return false
}

// PrincipalFromOIDCClaims accepts only sandboxd's two app roles. Unknown roles
// cannot accidentally turn into application access.
func PrincipalFromOIDCClaims(c OIDCClaims) (Principal, error) {
	if !safeIdentifier(c.OID) || !safeIdentifier(c.TenantID) {
		return Principal{}, errors.New("invalid immutable principal identifier")
	}
	roles := recognizedRoles(c.Roles)
	if len(roles) == 0 {
		return Principal{}, ErrOIDCRole
	}
	return Principal{
		OID: c.OID, TenantID: c.TenantID, DisplayName: safePresentation(c.Name),
		UPN: safePresentation(firstNonEmpty(c.PreferredUsername, c.UPN)), Roles: roles,
	}, nil
}

func recognizedRoles(values []string) []Role {
	seen := map[Role]bool{}
	var out []Role
	for _, raw := range values {
		role := Role(raw)
		if (role == RoleUser || role == RoleAdmin) && !seen[role] {
			seen[role] = true
			out = append(out, role)
		}
	}
	return out
}

func safeIdentifier(s string) bool {
	return safeTenantID(s)
}

func safePresentation(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 256 {
		return ""
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ""
		}
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func roleStrings(roles []Role) []string {
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		out = append(out, string(role))
	}
	return out
}

type httpCodeExchanger struct{ client *http.Client }

func (x httpCodeExchanger) ExchangeAuthorizationCode(ctx context.Context, cfg EntraConfig, code, codeVerifier string) (string, error) {
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {cfg.RedirectURL},
		"code_verifier": {codeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := x.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if err != nil || res.StatusCode < 200 || res.StatusCode > 299 {
		return "", ErrOIDCProvider
	}
	var payload struct {
		IDToken string `json:"id_token"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.IDToken == "" {
		return "", ErrOIDCProvider
	}
	return payload.IDToken, nil
}

// JWKSVerifier validates Entra RS256 ID tokens with a fixed, configured JWKS
// endpoint. It caches keys briefly and refreshes once on an unknown key ID.
type JWKSVerifier struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	refresh time.Time
}

func NewJWKSVerifier(jwksURL string, client *http.Client) *JWKSVerifier {
	if client == nil {
		client = newOIDCHTTPClient()
	}
	return &JWKSVerifier{url: jwksURL, client: client, now: time.Now, keys: make(map[string]*rsa.PublicKey)}
}

func (v *JWKSVerifier) VerifyIDToken(ctx context.Context, raw string, expected OIDCValidation) (OIDCClaims, error) {
	header, claims, input, signature, err := parseJWT(raw)
	if err != nil || header.Alg != "RS256" || header.Kid == "" {
		return OIDCClaims{}, ErrOIDCToken
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return OIDCClaims{}, ErrOIDCToken
	}
	sum := sha256.Sum256([]byte(input))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], signature) != nil {
		return OIDCClaims{}, ErrOIDCToken
	}
	if err := ValidateOIDCClaims(claims, expected); err != nil {
		return OIDCClaims{}, ErrOIDCToken
	}
	return claims, nil
}

func (v *JWKSVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	now := v.now()
	v.mu.Lock()
	key := v.keys[kid]
	fresh := now.Before(v.refresh)
	v.mu.Unlock()
	if key != nil && fresh {
		return key, nil
	}
	if err := v.fetch(ctx); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if key = v.keys[kid]; key == nil {
		return nil, errors.New("unknown signing key")
	}
	return key, nil
}

func (v *JWKSVerifier) fetch(ctx context.Context) error {
	u, err := url.Parse(v.url)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("invalid JWKS URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return err
	}
	res, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1024*1024))
	if err != nil || res.StatusCode != http.StatusOK {
		return errors.New("could not load JWKS")
	}
	var payload struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return errors.New("invalid JWKS")
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, jwk := range payload.Keys {
		if jwk.Kty != "RSA" || jwk.Kid == "" {
			continue
		}
		n, nErr := base64.RawURLEncoding.DecodeString(jwk.N)
		e, eErr := base64.RawURLEncoding.DecodeString(jwk.E)
		if nErr != nil || eErr != nil || len(n) == 0 || len(n) > 1024 || len(e) == 0 || len(e) > 4 {
			continue
		}
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 {
			continue
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		if exponent < 3 || exponent%2 == 0 {
			continue
		}
		keys[jwk.Kid] = &rsa.PublicKey{N: modulus, E: exponent}
	}
	if len(keys) == 0 {
		return errors.New("JWKS has no usable RSA keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.refresh = v.now().Add(time.Hour)
	v.mu.Unlock()
	return nil
}

type jwtClaims struct {
	Issuer            string          `json:"iss"`
	Audience          json.RawMessage `json:"aud"`
	AuthorizedParty   string          `json:"azp"`
	ExpiresAt         int64           `json:"exp"`
	NotBefore         int64           `json:"nbf"`
	IssuedAt          int64           `json:"iat"`
	Nonce             string          `json:"nonce"`
	OID               string          `json:"oid"`
	TenantID          string          `json:"tid"`
	Name              string          `json:"name"`
	PreferredUsername string          `json:"preferred_username"`
	UPN               string          `json:"upn"`
	Roles             json.RawMessage `json:"roles"`
}

func parseJWT(raw string) (jwtHeader, OIDCClaims, string, []byte, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || anyEmpty(parts) {
		return jwtHeader{}, OIDCClaims{}, "", nil, errors.New("malformed JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtHeader{}, OIDCClaims{}, "", nil, err
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtHeader{}, OIDCClaims{}, "", nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtHeader{}, OIDCClaims{}, "", nil, err
	}
	var header jwtHeader
	var rawClaims jwtClaims
	if json.Unmarshal(headerRaw, &header) != nil || json.Unmarshal(claimsRaw, &rawClaims) != nil {
		return jwtHeader{}, OIDCClaims{}, "", nil, errors.New("invalid JWT JSON")
	}
	audience, err := stringsClaim(rawClaims.Audience)
	if err != nil {
		return jwtHeader{}, OIDCClaims{}, "", nil, err
	}
	roles, err := stringsClaim(rawClaims.Roles)
	if err != nil {
		return jwtHeader{}, OIDCClaims{}, "", nil, err
	}
	return header, OIDCClaims{
		Issuer: rawClaims.Issuer, Audience: audience, AuthorizedParty: rawClaims.AuthorizedParty,
		ExpiresAt: unixTime(rawClaims.ExpiresAt),
		NotBefore: unixTime(rawClaims.NotBefore), IssuedAt: unixTime(rawClaims.IssuedAt),
		Nonce: rawClaims.Nonce, OID: rawClaims.OID, TenantID: rawClaims.TenantID,
		Name: rawClaims.Name, PreferredUsername: rawClaims.PreferredUsername, UPN: rawClaims.UPN, Roles: roles,
	}, parts[0] + "." + parts[1], sig, nil
}

func stringsClaim(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}
	return many, nil
}

func unixTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(value, 0).UTC()
}

func anyEmpty(values []string) bool {
	for _, value := range values {
		if value == "" {
			return true
		}
	}
	return false
}

func (p Principal) String() string {
	return fmt.Sprintf("%s/%s", p.TenantID, p.OID)
}
