package mysql

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"compute-audit/internal/config"
	"compute-audit/internal/contract"
	"compute-audit/internal/store"
	driver "github.com/go-sql-driver/mysql"
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, cfg config.DatabaseConfig, password string) (*Store, error) {
	if password == "" {
		return nil, fmt.Errorf("database password environment variable is empty")
	}
	connMaxLifetime, err := time.ParseDuration(cfg.ConnMaxLifetime)
	if err != nil {
		return nil, fmt.Errorf("parse database connection lifetime: %w", err)
	}

	dsnConfig := driver.NewConfig()
	dsnConfig.User = cfg.User
	dsnConfig.Passwd = password
	dsnConfig.Net = "tcp"
	dsnConfig.Addr = cfg.Host
	dsnConfig.DBName = cfg.Name
	dsnConfig.ParseTime = true
	dsnConfig.Loc = time.UTC
	dsnConfig.Timeout = 5 * time.Second
	dsnConfig.ReadTimeout = 10 * time.Second
	dsnConfig.WriteTimeout = 10 * time.Second
	dsnConfig.Params = map[string]string{"charset": "utf8mb4"}

	db, err := sql.Open("mysql", dsnConfig.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("configure database connection: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to audit database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Migrate(ctx context.Context) error { return Migrate(ctx, s.db) }

func (s *Store) RegisterHost(ctx context.Context, hostID, hostname, arch, vendor string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO hosts (host_id, hostname, arch, vendor, status)
		 VALUES (?, ?, ?, ?, 'active')`,
		hostID, hostname, arch, vendor,
	)
	if err == nil {
		return nil
	}
	if isDuplicateKey(err) {
		return store.ErrHostExists
	}
	return fmt.Errorf("register host: %w", err)
}

func (s *Store) PersistSnapshot(ctx context.Context, batch contract.SnapshotBatch, payloadSHA256 [32]byte) (store.SnapshotResult, error) {
	statusJSON, err := json.Marshal(batch.Status)
	if err != nil {
		return store.SnapshotResult{}, fmt.Errorf("encode collector status: %w", err)
	}
	payloadJSON, err := json.Marshal(batch)
	if err != nil {
		return store.SnapshotResult{}, fmt.Errorf("encode snapshot payload: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.SnapshotResult{}, fmt.Errorf("begin snapshot transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var hostStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM hosts WHERE host_id = ? FOR UPDATE`, batch.HostID).Scan(&hostStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.SnapshotResult{}, store.ErrUnknownHost
		}
		return store.SnapshotResult{}, fmt.Errorf("read host registration: %w", err)
	}
	if hostStatus != "active" {
		return store.SnapshotResult{}, store.ErrInactiveHost
	}

	var storedHash []byte
	err = tx.QueryRowContext(ctx,
		`SELECT payload_sha256 FROM snapshot_batches WHERE host_id = ? AND batch_id = ? FOR UPDATE`,
		batch.HostID, batch.BatchID,
	).Scan(&storedHash)
	if err == nil {
		if len(storedHash) != len(payloadSHA256) || subtle.ConstantTimeCompare(storedHash, payloadSHA256[:]) != 1 {
			return store.SnapshotResult{}, store.ErrIdempotencyConflict
		}
		return store.SnapshotResult{Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return store.SnapshotResult{}, fmt.Errorf("check snapshot idempotency: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO snapshot_batches
		 (host_id, batch_id, boot_id, sequence, observed_at, schema_version, status, payload_sha256, payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		batch.HostID, batch.BatchID, batch.BootID, batch.Sequence, batch.ObservedAt.UTC(),
		batch.SchemaVersion, string(statusJSON), payloadSHA256[:], string(payloadJSON),
	)
	if err != nil {
		if isDuplicateKey(err) {
			return store.SnapshotResult{}, store.ErrIdempotencyConflict
		}
		return store.SnapshotResult{}, fmt.Errorf("insert snapshot batch: %w", err)
	}
	if err := persistSnapshotFacts(ctx, tx, batch); err != nil {
		return store.SnapshotResult{}, fmt.Errorf("persist normalized snapshot facts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE hosts SET last_seen = IF(last_seen IS NULL OR last_seen < ?, ?, last_seen) WHERE host_id = ?`,
		batch.ObservedAt.UTC(), batch.ObservedAt.UTC(), batch.HostID,
	); err != nil {
		return store.SnapshotResult{}, fmt.Errorf("update host last_seen: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.SnapshotResult{}, fmt.Errorf("commit snapshot batch: %w", err)
	}
	return store.SnapshotResult{}, nil
}

func isDuplicateKey(err error) bool {
	var mysqlErr *driver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
