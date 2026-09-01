package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

type migrationStoreStub struct {
	closed bool
}

func (s *migrationStoreStub) Close() error {
	s.closed = true
	return nil
}

func productionMigrationEnvironment(databaseURL string) environmentLookup {
	return func(key string) string {
		switch key {
		case "SANDBOXD_PLATFORM":
			return "kubernetes"
		case "DATABASE_URL":
			return databaseURL
		default:
			return ""
		}
	}
}

func TestMigrateCommandRecognition(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{args: nil, want: false},
		{args: []string{"version"}, want: false},
		{args: []string{"migrate"}, want: true},
		{args: []string{"migrate", "--unexpected"}, want: true},
	} {
		if got := isMigrateCommand(tc.args); got != tc.want {
			t.Errorf("isMigrateCommand(%q) = %t, want %t", tc.args, got, tc.want)
		}
	}
}

func TestMigrateCommandUsesOnlyConfiguredMigrationStore(t *testing.T) {
	var opened store.Config
	stub := &migrationStoreStub{}
	var stdout, stderr bytes.Buffer

	exitCode := runMigrate(
		nil,
		productionMigrationEnvironment("postgres://db.example/sandboxd"),
		func(_ context.Context, config store.Config) (migrationStore, error) {
			opened = config
			return stub, nil
		},
		"/migrations",
		&stdout,
		&stderr,
	)

	if exitCode != 0 {
		t.Fatalf("runMigrate exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if opened.Profile != store.ProfileProduction {
		t.Fatalf("profile = %q, want production", opened.Profile)
	}
	if opened.Provider != "" {
		t.Fatalf("provider = %q, want profile-selected provider", opened.Provider)
	}
	if opened.DSN != "postgres://db.example/sandboxd" {
		t.Fatalf("migration opener received unexpected database URL")
	}
	if opened.MigrationsDir != "/migrations" {
		t.Fatalf("migrations dir = %q", opened.MigrationsDir)
	}
	if !stub.closed {
		t.Fatal("migration store was not closed")
	}
	if got := stdout.String(); got != "migrate: complete\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestMigrateCommandRequiresExplicitKubernetesPlatform(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
	}{
		{name: "omitted"},
		{name: "local", platform: "local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			called := false
			exitCode := runMigrate(
				nil,
				func(key string) string {
					switch key {
					case "SANDBOXD_PLATFORM":
						return tc.platform
					case "DATABASE_URL":
						return "postgresql://db.example/sandboxd"
					default:
						return ""
					}
				},
				func(context.Context, store.Config) (migrationStore, error) {
					called = true
					return nil, errors.New("must not open implicit production configuration")
				},
				"/migrations",
				&stdout,
				&stderr,
			)

			if exitCode != 2 {
				t.Fatalf("runMigrate exit code = %d, want 2", exitCode)
			}
			if called {
				t.Fatal("migration opener called without explicit Kubernetes platform")
			}
			if got := stderr.String(); got != "migrate: PostgreSQL persistence requires SANDBOXD_PLATFORM=kubernetes\n" {
				t.Fatalf("stderr = %q", got)
			}
			if stdout.Len() != 0 {
				t.Fatalf("unexpected stdout = %q", stdout.String())
			}
		})
	}
}

func TestMigrateCommandRejectsMissingOrInvalidProductionDatabaseURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{name: "missing", want: "DATABASE_URL must be configured"},
		{name: "sqlite", url: "file:/var/lib/sandboxd/state/sandboxd.db", want: "DATABASE_URL must be a PostgreSQL URL"},
		{name: "unknown scheme", url: "mysql://db.example/sandboxd", want: "DATABASE_URL must be a PostgreSQL URL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			called := false
			exitCode := runMigrate(
				nil,
				productionMigrationEnvironment(tc.url),
				func(context.Context, store.Config) (migrationStore, error) {
					called = true
					return nil, errors.New("must not open invalid configuration")
				},
				"/migrations",
				&stdout,
				&stderr,
			)

			if exitCode != 2 {
				t.Fatalf("runMigrate exit code = %d, want 2", exitCode)
			}
			if called {
				t.Fatal("migration opener called for invalid configuration")
			}
			if got := stderr.String(); got != "migrate: "+tc.want+"\n" {
				t.Fatalf("stderr = %q", got)
			}
			if stdout.Len() != 0 {
				t.Fatalf("unexpected stdout = %q", stdout.String())
			}
		})
	}
}

func TestMigrateCommandDoesNotExposeStoreErrorDetails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runMigrate(
		nil,
		productionMigrationEnvironment("postgres://db.example/sandboxd"),
		func(context.Context, store.Config) (migrationStore, error) {
			return nil, errors.New("connection rejected: test-only sensitive detail")
		},
		"/migrations",
		&stdout,
		&stderr,
	)

	if exitCode != 1 {
		t.Fatalf("runMigrate exit code = %d, want 1", exitCode)
	}
	if got := stderr.String(); got != "migrate: unable to apply database migrations\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestConfiguredStoreConfigDoesNotSelectProductionFromDatabaseURLAlone(t *testing.T) {
	config, err := configuredStoreConfig(func(key string) string {
		if key == "DATABASE_URL" {
			return "postgresql://db.example/sandboxd"
		}
		return ""
	}, "/local.db", "/migrations")
	if err != nil {
		t.Fatalf("configuredStoreConfig returned error: %v", err)
	}
	if config.Profile != "" || config.Provider != store.ProviderSQLite {
		t.Fatal("DATABASE_URL alone must retain the local SQLite profile")
	}
}

func TestConfiguredStoreConfigUsesProductionOnlyForExplicitKubernetesPlatform(t *testing.T) {
	config, err := configuredStoreConfig(func(key string) string {
		switch key {
		case "SANDBOXD_PLATFORM":
			return "kubernetes"
		case "DATABASE_URL":
			return "postgresql://db.example/sandboxd"
		default:
			return ""
		}
	}, "/local.db", "/migrations")
	if err != nil {
		t.Fatalf("configuredStoreConfig returned error: %v", err)
	}
	if config.Profile != store.ProfileProduction || config.Provider != "" {
		t.Fatal("expected production profile with provider selected by explicit platform")
	}
}

func TestConfiguredStoreConfigPreservesLocalSQLiteFallback(t *testing.T) {
	config, err := configuredStoreConfig(func(key string) string {
		if key == "SANDBOXD_DB" {
			return "custom.db"
		}
		return ""
	}, "default.db", "/migrations")
	if err != nil {
		t.Fatalf("configuredStoreConfig returned error: %v", err)
	}
	if config.Provider != store.ProviderSQLite || config.Profile != "" {
		t.Fatal("expected local SQLite configuration when DATABASE_URL is not configured")
	}
	if config.DSN != "file:custom.db?_journal=WAL&_busy_timeout=5000&_fk=1" {
		t.Fatalf("local DSN = %q", config.DSN)
	}
}
