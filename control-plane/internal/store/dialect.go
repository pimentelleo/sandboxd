package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Provider identifies the database implementation used by a Store.
type Provider string

const (
	ProviderSQLite   Provider = "sqlite"
	ProviderPostgres Provider = "postgres"
)

// Profile is an explicit deployment intent. Production always uses PostgreSQL;
// a SQLite URL can never be selected by the production profile.
type Profile string

const (
	ProfileLocal      Profile = "local"
	ProfileProduction Profile = "production"
)

// Config selects a store provider. New callers must select either Provider or
// Profile deliberately. Open remains the SQLite compatibility entry point.
type Config struct {
	Provider      Provider
	Profile       Profile
	DSN           string
	MigrationsDir string
}

// ProviderForURL selects a provider only from an explicit database URL. Plain
// filesystem paths remain a SQLite-only concern of the legacy Open API.
func ProviderForURL(databaseURL string) (Provider, error) {
	lower := strings.ToLower(databaseURL)
	switch {
	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		return ProviderPostgres, nil
	case strings.HasPrefix(lower, "sqlite://"), strings.HasPrefix(lower, "file:"):
		return ProviderSQLite, nil
	default:
		return "", fmt.Errorf("unsupported database URL scheme")
	}
}

func (c Config) selectedProvider() (Provider, error) {
	var fromProfile Provider
	switch c.Profile {
	case "":
	case ProfileLocal:
		fromProfile = ProviderSQLite
	case ProfileProduction:
		fromProfile = ProviderPostgres
	default:
		return "", fmt.Errorf("unknown store profile %q", c.Profile)
	}
	if c.Provider == "" {
		if fromProfile == "" {
			return "", fmt.Errorf("store provider or profile is required")
		}
		return fromProfile, nil
	}
	if c.Provider != ProviderSQLite && c.Provider != ProviderPostgres {
		return "", fmt.Errorf("unknown store provider %q", c.Provider)
	}
	if fromProfile != "" && c.Provider != fromProfile {
		return "", fmt.Errorf("store provider %q conflicts with profile %q", c.Provider, c.Profile)
	}
	return c.Provider, nil
}

type dialectDB struct {
	*sql.DB
	provider Provider
}

func (db *dialectDB) bind(query string) string {
	return BindQuery(db.provider, query)
}

func (db *dialectDB) providerName() Provider { return db.provider }

func (db *dialectDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.DB.ExecContext(ctx, db.bind(query), args...)
}

func (db *dialectDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.DB.QueryContext(ctx, db.bind(query), args...)
}

func (db *dialectDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.DB.QueryRowContext(ctx, db.bind(query), args...)
}

func (db *dialectDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*dialectTx, error) {
	tx, err := db.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &dialectTx{Tx: tx, provider: db.provider}, nil
}

type dialectTx struct {
	*sql.Tx
	provider Provider
}

func (tx *dialectTx) bind(query string) string {
	return BindQuery(tx.provider, query)
}

func (tx *dialectTx) providerName() Provider { return tx.provider }

func (tx *dialectTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.Tx.ExecContext(ctx, tx.bind(query), args...)
}

func (tx *dialectTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.QueryContext(ctx, tx.bind(query), args...)
}

func (tx *dialectTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.Tx.QueryRowContext(ctx, tx.bind(query), args...)
}

// ForUpdate adds a row lock only where the provider can enforce it. SQLite
// retains its existing transaction semantics.
func (tx *dialectTx) ForUpdate(query string) string {
	if tx.provider == ProviderPostgres {
		return query + " FOR UPDATE"
	}
	return query
}

// BindQuery converts portable question-mark placeholders to PostgreSQL's
// positional form. It leaves literals, quoted identifiers, comments, and
// dollar-quoted PostgreSQL bodies untouched.
func BindQuery(provider Provider, query string) string {
	if provider != ProviderPostgres || !strings.Contains(query, "?") {
		return query
	}

	var out strings.Builder
	out.Grow(len(query) + 8)
	arg, i := 1, 0
	for i < len(query) {
		switch query[i] {
		case '\'':
			start := i
			i++
			for i < len(query) {
				if query[i] == '\'' {
					i++
					if i < len(query) && query[i] == '\'' {
						i++
						continue
					}
					break
				}
				i++
			}
			out.WriteString(query[start:i])
		case '"':
			start := i
			i++
			for i < len(query) {
				if query[i] == '"' {
					i++
					if i < len(query) && query[i] == '"' {
						i++
						continue
					}
					break
				}
				i++
			}
			out.WriteString(query[start:i])
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				start := i
				i += 2
				for i < len(query) && query[i] != '\n' {
					i++
				}
				out.WriteString(query[start:i])
				continue
			}
			out.WriteByte(query[i])
			i++
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				start := i
				i += 2
				for i+1 < len(query) && !(query[i] == '*' && query[i+1] == '/') {
					i++
				}
				if i+1 < len(query) {
					i += 2
				}
				out.WriteString(query[start:i])
				continue
			}
			out.WriteByte(query[i])
			i++
		case '$':
			if tag, end := dollarQuoteAt(query, i); end > i {
				closeAt := strings.Index(query[end:], tag)
				if closeAt >= 0 {
					stop := end + closeAt + len(tag)
					out.WriteString(query[i:stop])
					i = stop
					continue
				}
			}
			out.WriteByte(query[i])
			i++
		case '?':
			fmt.Fprintf(&out, "$%d", arg)
			arg++
			i++
		default:
			out.WriteByte(query[i])
			i++
		}
	}
	return out.String()
}

func dollarQuoteAt(query string, start int) (string, int) {
	end := start + 1
	for end < len(query) && ((query[end] >= 'a' && query[end] <= 'z') ||
		(query[end] >= 'A' && query[end] <= 'Z') ||
		(query[end] >= '0' && query[end] <= '9') || query[end] == '_') {
		end++
	}
	if end < len(query) && query[end] == '$' {
		return query[start : end+1], end + 1
	}
	return "", start
}
