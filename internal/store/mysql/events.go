package mysql

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"time"

	"compute-audit/internal/deepflow"
	"compute-audit/internal/store"
)

func (s *Store) PersistDeepFlowEvents(ctx context.Context, instanceID string, maxSnapshotAge time.Duration, events []deepflow.Event) error {
	if instanceID == "" || len(instanceID) > 128 {
		return fmt.Errorf("DeepFlow instance ID is required and must be at most 128 bytes")
	}
	if maxSnapshotAge <= 0 {
		return fmt.Errorf("snapshot max age must be positive")
	}
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin DeepFlow page transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, event := range events {
		sourceIP, err := nullableIP(event.SourceIP)
		if err != nil {
			return fmt.Errorf("invalid source IP for raw event %q: %w", event.RawEventID, err)
		}
		destinationIP, err := nullableIP(event.DestinationIP)
		if err != nil {
			return fmt.Errorf("invalid destination IP for raw event %q: %w", event.RawEventID, err)
		}
		var observerAgentID any
		if event.ObserverAgentID != "" {
			observerAgentID = event.ObserverAgentID
		}
		var serverPID any
		if event.ServerPID != nil {
			serverPID = *event.ServerPID
		}
		var durationMS any
		if event.DurationMS != nil {
			durationMS = *event.DurationMS
		}
		result, err := tx.ExecContext(ctx,
			`INSERT INTO audit_events
			 (deepflow_instance_id, tenant_id, raw_event_id, request_start, request_end,
			  observed_src_ip, destination_ip, observer_agent_id, server_pid, method, path,
			  status_code, duration_ms, observation_point, normalized_sha256)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE event_id = event_id`,
			instanceID, event.TenantID, event.RawEventID, event.RequestStart.UTC(), event.RequestEnd.UTC(),
			sourceIP, destinationIP, observerAgentID, serverPID, event.Method, event.Path,
			event.StatusCode, durationMS, event.ObservationPoint, event.NormalizedSHA256[:],
		)
		if err != nil {
			return fmt.Errorf("insert raw event %q: %w", event.RawEventID, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check raw event insert result: %w", err)
		}
		var eventID uint64
		if changed == 0 {
			var storedHash []byte
			if err := tx.QueryRowContext(ctx,
				`SELECT event_id, normalized_sha256 FROM audit_events
				 WHERE deepflow_instance_id = ? AND tenant_id = ? AND raw_event_id = ?`,
				instanceID, event.TenantID, event.RawEventID,
			).Scan(&eventID, &storedHash); err != nil {
				return fmt.Errorf("read existing raw event hash: %w", err)
			}
			if len(storedHash) != len(event.NormalizedSHA256) || subtle.ConstantTimeCompare(storedHash, event.NormalizedSHA256[:]) != 1 {
				return fmt.Errorf("%w: %s/%s/%s", store.ErrIdempotencyConflict, instanceID, event.TenantID, event.RawEventID)
			}
		} else {
			insertedID, err := result.LastInsertId()
			if err != nil {
				return fmt.Errorf("read inserted audit event ID: %w", err)
			}
			if insertedID <= 0 {
				return fmt.Errorf("read inserted audit event ID: driver returned invalid ID %d", insertedID)
			}
			eventID = uint64(insertedID)
		}
		if err := s.attributeDeepFlowEvent(ctx, tx, eventID, instanceID, event, maxSnapshotAge); err != nil {
			return fmt.Errorf("attribute raw event %q: %w", event.RawEventID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit DeepFlow page: %w", err)
	}
	return nil
}

func nullableIP(value string) (any, error) {
	if value == "" {
		return nil, nil
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return nil, fmt.Errorf("not an IP address")
	}
	return []byte(ip.To16()), nil
}

var _ interface {
	PersistDeepFlowEvents(context.Context, string, time.Duration, []deepflow.Event) error
} = (*Store)(nil)
