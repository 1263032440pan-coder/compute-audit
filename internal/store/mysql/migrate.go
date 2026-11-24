package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"compute-audit/migrations"
)

const migrationLockName = "compute-audit-schema-migrations"

func Migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	var lockAcquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 30)`, migrationLockName).Scan(&lockAcquired); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if !lockAcquired.Valid || lockAcquired.Int64 != 1 {
		return fmt.Errorf("timed out waiting for migration lock")
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, migrationLockName)
	}()

	if _, err := conn.ExecContext(ctx, `SET time_zone = '+00:00'`); err != nil {
		return fmt.Errorf("set migration session time zone: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(255) NOT NULL PRIMARY KEY,
		applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		return fmt.Errorf("list migration files: %w", err)
	}
	sort.Strings(files)
	for _, name := range files {
		var applied string
		err := conn.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = ?`, name).Scan(&applied)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", name, err)
		}

		contents, err := migrations.Files.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		for _, statement := range strings.Split(string(contents), ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply migration %s: %w", name, err)
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	return nil
}
