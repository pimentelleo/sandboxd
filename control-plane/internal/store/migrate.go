package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migrate applies any *.sql files in migrationsDir whose numeric prefix
// isn't recorded in the migration table. No down migrations in v1.
// Filenames must be NNNN_name.sql.
func Migrate(ctx context.Context, db *sql.DB, migrationsDir string) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS migration (
			id         INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("ensure migration table: %w", err)
	}

	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT id FROM migration`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		applied[id] = true
	}
	rows.Close()

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir %s: %w", migrationsDir, err)
	}
	type mig struct {
		id   int
		name string
		path string
	}
	var migs []mig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		under := strings.IndexByte(e.Name(), '_')
		if under <= 0 {
			return fmt.Errorf("migration filename must be NNNN_name.sql: %s", e.Name())
		}
		id, err := strconv.Atoi(e.Name()[:under])
		if err != nil {
			return fmt.Errorf("migration id parse %s: %w", e.Name(), err)
		}
		migs = append(migs, mig{id: id, name: e.Name(), path: filepath.Join(migrationsDir, e.Name())})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].id < migs[j].id })

	for _, m := range migs {
		if applied[m.id] {
			continue
		}
		body, err := os.ReadFile(m.path)
		if err != nil {
			return fmt.Errorf("read %s: %w", m.path, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO migration (id, name, applied_at) VALUES (?, ?, ?)`,
			m.id, m.name, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", m.name, err)
		}
	}
	return nil
}

const postgresMigrationAdvisoryLockID int64 = 831765977202319467

type migrationDB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

// MigratePostgres applies the PostgreSQL-only migration stream. It intentionally
// never reads the SQLite migration directory, preventing an implicit
// SQLite-to-production migration. A session-scoped advisory lock serializes the
// singleton migration Job across control-plane replicas without locking tables
// used by normal application traffic.
func MigratePostgres(ctx context.Context, db *sql.DB, migrationsDir string) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, postgresMigrationAdvisoryLockID); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, unlockErr := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, postgresMigrationAdvisoryLockID); unlockErr != nil && err == nil {
			err = fmt.Errorf("release migration advisory lock: %w", unlockErr)
		}
	}()

	return migrateWithBinding(ctx, conn, filepath.Join(migrationsDir, "postgres"), ProviderPostgres)
}

func migrateWithBinding(ctx context.Context, db migrationDB, migrationsDir string, provider Provider) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS migration (
			id         BIGINT PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at BIGINT NOT NULL
		)`); err != nil {
		return fmt.Errorf("ensure migration table: %w", err)
	}
	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT id FROM migration`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		applied[id] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir %s: %w", migrationsDir, err)
	}
	type migration struct {
		id   int
		name string
		path string
	}
	var migrations []migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		under := strings.IndexByte(entry.Name(), '_')
		if under <= 0 {
			return fmt.Errorf("migration filename must be NNNN_name.sql: %s", entry.Name())
		}
		id, err := strconv.Atoi(entry.Name()[:under])
		if err != nil {
			return fmt.Errorf("migration id parse %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{id: id, name: entry.Name(), path: filepath.Join(migrationsDir, entry.Name())})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].id < migrations[j].id })

	for _, migration := range migrations {
		if applied[migration.id] {
			continue
		}
		body, err := os.ReadFile(migration.path)
		if err != nil {
			return fmt.Errorf("read %s: %w", migration.name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var recorded bool
		if err := tx.QueryRowContext(ctx,
			BindQuery(provider, `SELECT EXISTS(SELECT 1 FROM migration WHERE id=?)`), migration.id).Scan(&recorded); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("check %s: %w", migration.name, err)
		}
		if recorded {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit skipped %s: %w", migration.name, err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", migration.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			BindQuery(provider, `INSERT INTO migration (id, name, applied_at) VALUES (?, ?, ?)`),
			migration.id, migration.name, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record %s: %w", migration.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", migration.name, err)
		}
	}
	return nil
}
