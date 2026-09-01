package auth

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Profile selects the interactive console authentication authority.
type Profile string

const (
	// ProfileLocal keeps the single-password console login used by local installs.
	ProfileLocal Profile = "local"
	// ProfileEntra requires a single Microsoft Entra tenant for console login.
	ProfileEntra Profile = "entra"
	// ProfileInvalid is deliberately not a usable profile. Middleware rejects
	// protected requests when this is selected.
	ProfileInvalid Profile = "invalid"
)

// LocalAuthMode selects whether the local profile uses the legacy shared
// password or durable email/password accounts.
type LocalAuthMode string

const (
	LocalAuthModePassword LocalAuthMode = "password"
	LocalAuthModeAccounts LocalAuthMode = "accounts"
)

// EntraConfig contains the private and public configuration required for the
// Entra authorization-code flow. It is never serialized or logged.
type EntraConfig struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Issuer       string
	AuthorizeURL string
	TokenURL     string
	JWKSURL      string
}

// Config is the reloadable auth configuration. It is held behind an
// atomic.Pointer in Middleware so a SIGHUP can swap it without locks.
type Config struct {
	APITokens       []NamedToken      // SANDBOXD_API_TOKENS
	PreviewSecrets  map[string]string // SANDBOXD_PREVIEW_TOKEN_SECRETS: kid -> secret
	AuthRedirectURL string            // SANDBOXD_AUTH_REDIRECT_URL
	Disabled        bool              // SANDBOXD_API_AUTH_DISABLED rollback path
	Profile         Profile
	LocalAuthMode   LocalAuthMode
	Entra           EntraConfig
	// Problem is deliberately generic: it is safe to expose as an availability
	// state but never includes a secret or an environment value.
	Problem string
}

// ParseConfig builds a Config from a key->value getter. At startup the
// getter is os.Getenv (systemd has already loaded the EnvironmentFile
// into the process environment); on SIGHUP it is a map populated by
// LoadEnvFile, because the process environment is stale by then.
func ParseConfig(get func(string) string) *Config {
	c := &Config{
		APITokens:       parseNamedPairs(get("SANDBOXD_API_TOKENS")),
		PreviewSecrets:  pairsToMap(get("SANDBOXD_PREVIEW_TOKEN_SECRETS")),
		AuthRedirectURL: strings.TrimSpace(get("SANDBOXD_AUTH_REDIRECT_URL")),
		Profile:         ProfileLocal,
		LocalAuthMode:   LocalAuthModePassword,
	}
	switch strings.ToLower(strings.TrimSpace(get("SANDBOXD_API_AUTH_DISABLED"))) {
	case "", "0", "false", "no":
		c.Disabled = false
	default:
		c.Disabled = true
	}
	switch strings.ToLower(strings.TrimSpace(get("SANDBOXD_AUTH_PROFILE"))) {
	case "", string(ProfileLocal):
		c.Profile = ProfileLocal
		switch strings.ToLower(strings.TrimSpace(get("SANDBOXD_LOCAL_AUTH_MODE"))) {
		case "", string(LocalAuthModePassword):
			c.LocalAuthMode = LocalAuthModePassword
		case string(LocalAuthModeAccounts):
			c.LocalAuthMode = LocalAuthModeAccounts
			if c.Disabled {
				c.Problem = "local account authentication cannot be disabled"
			} else if len(c.APITokens) != 0 {
				c.Problem = "local account authentication does not allow API tokens"
			}
		default:
			c.Problem = "unsupported local authentication mode"
		}
	case string(ProfileEntra):
		c.Profile = ProfileEntra
		c.Entra = entraConfigFrom(get)
		if c.Disabled {
			c.Problem = "production authentication cannot be disabled"
		} else if err := c.Entra.Validate(); err != nil {
			c.Problem = "production authentication is not configured"
		}
	default:
		c.Profile = ProfileInvalid
		c.Problem = "unsupported authentication profile"
	}
	return c
}

// ProductionReady reports whether the selected production profile can safely
// start an Entra authorization-code flow.
func (c *Config) ProductionReady() bool {
	return c != nil && c.Profile == ProfileEntra && c.Problem == "" && c.Entra.Validate() == nil
}

// LocalAccountsReady reports whether the account-backed local profile can
// safely issue identity-scoped sessions.
func (c *Config) LocalAccountsReady() bool {
	return c != nil && c.Profile == ProfileLocal && c.LocalAuthMode == LocalAuthModeAccounts &&
		c.Problem == "" && !c.Disabled && len(c.APITokens) == 0
}

func entraConfigFrom(get func(string) string) EntraConfig {
	tenantID := strings.TrimSpace(get("SANDBOXD_ENTRA_TENANT_ID"))
	base := "https://login.microsoftonline.com/" + tenantID
	return EntraConfig{
		TenantID:     tenantID,
		ClientID:     strings.TrimSpace(get("SANDBOXD_ENTRA_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(get("SANDBOXD_ENTRA_CLIENT_SECRET")),
		RedirectURL:  strings.TrimSpace(get("SANDBOXD_ENTRA_REDIRECT_URL")),
		Issuer:       envOrDefault(get("SANDBOXD_ENTRA_ISSUER"), base+"/v2.0"),
		AuthorizeURL: envOrDefault(get("SANDBOXD_ENTRA_AUTHORIZE_URL"), base+"/oauth2/v2.0/authorize"),
		TokenURL:     envOrDefault(get("SANDBOXD_ENTRA_TOKEN_URL"), base+"/oauth2/v2.0/token"),
		JWKSURL:      envOrDefault(get("SANDBOXD_ENTRA_JWKS_URL"), base+"/discovery/v2.0/keys"),
	}
}

func envOrDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

// Validate checks the configuration without disclosing why a production
// profile is unavailable. All provider endpoints must be HTTPS so a
// configuration typo cannot downgrade the code or client credential exchange.
func (c EntraConfig) Validate() error {
	if !safeTenantID(c.TenantID) || c.ClientID == "" || c.ClientSecret == "" {
		return fmt.Errorf("incomplete Entra configuration")
	}
	if !validHTTPSURL(c.RedirectURL) || !validHTTPSURL(c.Issuer) ||
		!validHTTPSURL(c.AuthorizeURL) || !validHTTPSURL(c.TokenURL) || !validHTTPSURL(c.JWKSURL) {
		return fmt.Errorf("invalid Entra endpoint configuration")
	}
	return nil
}

func validHTTPSURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}

func safeTenantID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// LoadEnvFile parses a systemd EnvironmentFile-style file (KEY=value
// per line, `#` comments, optional surrounding quotes on the value)
// into a map. Used by the SIGHUP reload path.
func LoadEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		m[key] = val
	}
	return m, nil
}

// MapGetter adapts a map to the func(string)string ParseConfig wants.
func MapGetter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// BuildRedirectURL substitutes {sandbox_id} and {return} (URL-encoded)
// into the SANDBOXD_AUTH_REDIRECT_URL template.
func BuildRedirectURL(template, sandboxID, returnURL string) string {
	return strings.NewReplacer(
		"{sandbox_id}", url.QueryEscape(sandboxID),
		"{return}", url.QueryEscape(returnURL),
	).Replace(template)
}

// parseNamedPairs splits a "name=value,name=value" list. Whitespace
// around each element and around `name`/`value` is trimmed; empty
// elements and elements without an `=` are skipped.
func parseNamedPairs(s string) []NamedToken {
	var out []NamedToken
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		out = append(out, NamedToken{
			Name:  strings.TrimSpace(part[:eq]),
			Token: strings.TrimSpace(part[eq+1:]),
		})
	}
	return out
}

func pairsToMap(s string) map[string]string {
	m := map[string]string{}
	for _, nt := range parseNamedPairs(s) {
		m[nt.Name] = nt.Token
	}
	return m
}
