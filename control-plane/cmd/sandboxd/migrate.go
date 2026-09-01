package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

type environmentLookup func(string) string

type migrationStore interface {
	Close() error
}

type migrationStoreOpener func(context.Context, store.Config) (migrationStore, error)

func isMigrateCommand(args []string) bool {
	return len(args) > 0 && args[0] == "migrate"
}

func openMigrationStore(ctx context.Context, config store.Config) (migrationStore, error) {
	return store.OpenWithConfig(ctx, config)
}

// runMigrate applies the explicitly selected Kubernetes PostgreSQL migration
// stream and exits. It intentionally receives an opener so this command can be
// tested without a database and cannot acquire daemon-only dependencies.
func runMigrate(
	args []string,
	getenv environmentLookup,
	open migrationStoreOpener,
	migrations string,
	stdout, stderr io.Writer,
) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "migrate: does not accept arguments")
		return 2
	}

	config, err := productionStoreConfig(getenv, migrations)
	if err != nil {
		fmt.Fprintln(stderr, "migrate:", err)
		return 2
	}
	st, err := open(context.Background(), config)
	if err != nil {
		// Store errors can contain driver-specific details. Keep command output
		// useful without exposing a database URL or its credentials.
		fmt.Fprintln(stderr, "migrate: unable to apply database migrations")
		return 1
	}
	if err := st.Close(); err != nil {
		fmt.Fprintln(stderr, "migrate: unable to finish database migrations")
		return 1
	}
	fmt.Fprintln(stdout, "migrate: complete")
	return 0
}

// configuredStoreConfig is shared by daemon startup and tests. The platform
// selection, not DATABASE_URL's mere presence, chooses production persistence.
func configuredStoreConfig(getenv environmentLookup, dbPath, migrations string) (store.Config, error) {
	platform, err := configuredPlatform(getenv)
	if err != nil {
		return store.Config{}, err
	}
	if platform == platformKubernetes {
		return productionStoreConfig(getenv, migrations)
	}
	return store.Config{
		Provider:      store.ProviderSQLite,
		DSN:           fmt.Sprintf("file:%s?_journal=WAL&_busy_timeout=5000&_fk=1", getenvOr(getenv, "SANDBOXD_DB", dbPath)),
		MigrationsDir: migrations,
	}, nil
}

func productionStoreConfig(getenv environmentLookup, migrations string) (store.Config, error) {
	platform, err := configuredPlatform(getenv)
	if err != nil {
		return store.Config{}, err
	}
	if platform != platformKubernetes {
		return store.Config{}, errors.New("PostgreSQL persistence requires SANDBOXD_PLATFORM=kubernetes")
	}
	databaseURL := strings.TrimSpace(getenv("DATABASE_URL"))
	if databaseURL == "" {
		return store.Config{}, errors.New("DATABASE_URL must be configured")
	}
	provider, err := store.ProviderForURL(databaseURL)
	if err != nil || provider != store.ProviderPostgres {
		return store.Config{}, errors.New("DATABASE_URL must be a PostgreSQL URL")
	}
	return store.Config{
		Profile:       store.ProfileProduction,
		DSN:           databaseURL,
		MigrationsDir: migrations,
	}, nil
}

func getenvOr(getenv environmentLookup, key, fallback string) string {
	if value := getenv(key); value != "" {
		return value
	}
	return fallback
}

func configuredMigrationsDir() string {
	migrations := envDefault("SANDBOXD_MIGRATIONS", migrationsDir)
	if _, err := os.Stat(migrations); err != nil {
		if exe, e := os.Executable(); e == nil {
			alt := filepath.Join(filepath.Dir(exe), "..", "..", "migrations")
			if _, e2 := os.Stat(alt); e2 == nil {
				return alt
			}
		}
	}
	return migrations
}
