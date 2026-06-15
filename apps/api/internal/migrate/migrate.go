// Package migrate applies the SQL files in db/migrations on startup, in lexical
// order, each exactly once (tracked in schema_migrations).
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockKey is a fixed advisory-lock id so that with multiple API
// instances booting at once, exactly one runs DDL at a time.
const migrationLockKey int64 = 8675309

// Run locates db/migrations and executes every .sql file in lexical order,
// holding a session-level advisory lock for the duration so concurrent
// instances serialize rather than racing on DDL.
func Run(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := findMigrationsDir()
	if err != nil {
		return err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn for migration lock: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, "select pg_advisory_unlock($1)", migrationLockKey)
	}()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %q: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	// Track applied migrations so each file runs exactly once, in order.
	if _, err := conn.Exec(ctx,
		`create table if not exists schema_migrations (
			name text primary key,
			checksum text not null,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	for _, name := range files {
		sql, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read migration %q: %w", name, err)
		}
		sum := sha256.Sum256(sql)
		checksum := hex.EncodeToString(sum[:])

		var appliedChecksum string
		if err := conn.QueryRow(ctx,
			"select checksum from schema_migrations where name = $1", name,
		).Scan(&appliedChecksum); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check migration %q: %w", name, err)
		} else if err == nil {
			if appliedChecksum != checksum {
				return fmt.Errorf("migration %q was modified after it was applied", name)
			}
			continue
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			"insert into schema_migrations (name, checksum) values ($1, $2)", name, checksum,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %q: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %q: %w", name, err)
		}
		log.Printf("applied migration %s", name)
	}
	return nil
}

// findMigrationsDir honors UFO_MIGRATIONS_DIR, otherwise walks up from the
// working directory looking for db/migrations (so it works whether the binary
// runs from apps/api or the repo root).
func findMigrationsDir() (string, error) {
	if d := os.Getenv("UFO_MIGRATIONS_DIR"); d != "" {
		return d, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(cwd, "db", "migrations")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return "", fmt.Errorf("could not locate db/migrations (set UFO_MIGRATIONS_DIR)")
		}
		cwd = parent
	}
}
