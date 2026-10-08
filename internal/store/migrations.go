package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"settlekit/migrations"
)

func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := pool.Begin(bounded)
	if err != nil {
		return fmt.Errorf("begin migrations: %w", err)
	}
	defer func() {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer rollbackCancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err := tx.Exec(bounded, `SELECT pg_advisory_xact_lock(836428120081)`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err := tx.Exec(bounded, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var applied string
		err := tx.QueryRow(bounded, `SELECT version FROM schema_migrations WHERE version=$1`, entry.Name()).Scan(&applied)
		if err == nil {
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", entry.Name(), err)
		}
		body, err := fs.ReadFile(migrations.Files, entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(bounded, string(body), pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(bounded, `INSERT INTO schema_migrations(version) VALUES($1)`, entry.Name()); err != nil {
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}
	}
	if err := tx.Commit(bounded); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}
