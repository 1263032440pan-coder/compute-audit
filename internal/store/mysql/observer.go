package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"compute-audit/internal/store"
)

func (s *Store) RegisterObserverBinding(ctx context.Context, instanceID, agentID, hostID string, validFrom time.Time, validTo *time.Time) error {
	if instanceID == "" || len(instanceID) > 128 || strings.TrimSpace(instanceID) != instanceID {
		return fmt.Errorf("DeepFlow instance ID is required and must be at most 128 bytes")
	}
	if agentID == "" || len(agentID) > 191 || strings.TrimSpace(agentID) != agentID {
		return fmt.Errorf("observer agent ID is required and must be at most 191 bytes")
	}
	if hostID == "" || len(hostID) > 128 || strings.TrimSpace(hostID) != hostID {
		return fmt.Errorf("host ID is required and must be at most 128 bytes")
	}
	if validFrom.IsZero() || (validTo != nil && !validTo.After(validFrom)) {
		return fmt.Errorf("observer mapping validity range is invalid")
	}
	validFrom = validFrom.UTC()
	if validTo != nil {
		to := validTo.UTC()
		validTo = &to
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin observer mapping transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var hostStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM hosts WHERE host_id = ? FOR UPDATE`, hostID).Scan(&hostStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrUnknownHost
		}
		return fmt.Errorf("read observer host: %w", err)
	}
	if hostStatus != "active" {
		return store.ErrInactiveHost
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT host_id, valid_from, valid_to FROM observer_bindings
		 WHERE deepflow_instance_id = ? AND observer_agent_id = ? FOR UPDATE`,
		instanceID, agentID,
	)
	if err != nil {
		return fmt.Errorf("check observer mapping overlap: %w", err)
	}
	for rows.Next() {
		var existingHost string
		var existingFrom time.Time
		var existingTo sql.NullTime
		if err := rows.Scan(&existingHost, &existingFrom, &existingTo); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read observer mapping: %w", err)
		}
		if existingHost == hostID && existingFrom.Equal(validFrom) && sameNullableTime(existingTo, validTo) {
			_ = rows.Close()
			return nil
		}
		if rangesOverlap(validFrom, validTo, existingFrom, existingTo) {
			_ = rows.Close()
			return store.ErrObserverBindingConflict
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate observer mappings: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close observer mapping rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO observer_bindings (deepflow_instance_id, observer_agent_id, host_id, valid_from, valid_to)
		 VALUES (?, ?, ?, ?, ?)`,
		instanceID, agentID, hostID, validFrom, nullableTime(validTo),
	); err != nil {
		return fmt.Errorf("insert observer mapping: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit observer mapping: %w", err)
	}
	return nil
}

func (s *Store) CloseObserverBinding(ctx context.Context, instanceID, agentID string, validTo time.Time) error {
	if instanceID == "" || len(instanceID) > 128 || strings.TrimSpace(instanceID) != instanceID {
		return fmt.Errorf("DeepFlow instance ID is required and must be at most 128 bytes")
	}
	if agentID == "" || len(agentID) > 191 || strings.TrimSpace(agentID) != agentID {
		return fmt.Errorf("observer agent ID is required and must be at most 191 bytes")
	}
	if validTo.IsZero() {
		return fmt.Errorf("observer mapping close time is required")
	}
	validTo = validTo.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin observer mapping close transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT host_id, valid_from FROM observer_bindings
		 WHERE deepflow_instance_id = ? AND observer_agent_id = ? AND valid_to IS NULL FOR UPDATE`,
		instanceID, agentID,
	)
	if err != nil {
		return fmt.Errorf("find open observer mapping: %w", err)
	}
	type openMapping struct {
		hostID    string
		validFrom time.Time
	}
	var mappings []openMapping
	for rows.Next() {
		var mapping openMapping
		if err := rows.Scan(&mapping.hostID, &mapping.validFrom); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read open observer mapping: %w", err)
		}
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate open observer mapping: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close open observer mappings: %w", err)
	}
	if len(mappings) != 1 {
		return fmt.Errorf("expected exactly one open observer mapping, found %d", len(mappings))
	}
	if !validTo.After(mappings[0].validFrom) {
		return fmt.Errorf("mapping close time must be after valid_from")
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE observer_bindings SET valid_to = ?
		 WHERE deepflow_instance_id = ? AND observer_agent_id = ? AND host_id = ? AND valid_from = ? AND valid_to IS NULL`,
		validTo, instanceID, agentID, mappings[0].hostID, mappings[0].validFrom,
	); err != nil {
		return fmt.Errorf("close observer mapping: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit observer mapping close: %w", err)
	}
	return nil
}

func rangesOverlap(from time.Time, to *time.Time, existingFrom time.Time, existingTo sql.NullTime) bool {
	if to != nil && !existingFrom.Before(*to) {
		return false
	}
	return !existingTo.Valid || existingTo.Time.After(from)
}

func sameNullableTime(existing sql.NullTime, target *time.Time) bool {
	if !existing.Valid || target == nil {
		return !existing.Valid && target == nil
	}
	return existing.Time.Equal(*target)
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}
