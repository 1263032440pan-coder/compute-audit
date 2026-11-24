package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"compute-audit/internal/contract"
	"compute-audit/internal/deepflow"
)

type deviceEvidence struct {
	deviceKey      string
	evidence       string
	collectorState contract.CollectorState
}

type eventAttribution struct {
	hostID           any
	snapshotBatchID  any
	snapshotObserved any
	processStart     any
	serviceID        any
	status           string
	confidence       string
	reason           string
	devices          []deviceEvidence
}

func (s *Store) attributeDeepFlowEvent(ctx context.Context, tx *sql.Tx, eventID uint64, instanceID string, event deepflow.Event, maxSnapshotAge time.Duration) error {
	result := eventAttribution{status: "observer_unmapped", confidence: "unknown", reason: "observer_unmapped"}
	if event.ObserverAgentID != "" {
		rows, err := tx.QueryContext(ctx,
			`SELECT host_id FROM observer_bindings
			 WHERE deepflow_instance_id = ? AND observer_agent_id = ?
			   AND valid_from <= ? AND (valid_to IS NULL OR valid_to > ?)
			 ORDER BY valid_from DESC LIMIT 2`,
			instanceID, event.ObserverAgentID, event.RequestStart.UTC(), event.RequestStart.UTC(),
		)
		if err != nil {
			return fmt.Errorf("resolve observer host: %w", err)
		}
		var hosts []string
		for rows.Next() {
			var hostID string
			if err := rows.Scan(&hostID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read observer host mapping: %w", err)
			}
			hosts = append(hosts, hostID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate observer host mappings: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close observer host mappings: %w", err)
		}
		switch len(hosts) {
		case 0:
		case 1:
			result.hostID = hosts[0]
		default:
			result.status = "observer_ambiguous"
			result.reason = "observer_ambiguous"
		}
	}
	if result.hostID != nil && result.status != "observer_ambiguous" {
		if event.ServerPID == nil {
			result.status = "server_pid_missing"
			result.reason = "server_pid_missing"
		} else {
			if err := s.resolveProcessSnapshot(ctx, tx, &result, event, maxSnapshotAge); err != nil {
				return err
			}
		}
	}
	return persistAttribution(ctx, tx, eventID, result)
}

func (s *Store) resolveProcessSnapshot(ctx context.Context, tx *sql.Tx, result *eventAttribution, event deepflow.Event, maxSnapshotAge time.Duration) error {
	hostID, ok := result.hostID.(string)
	if !ok || hostID == "" {
		return fmt.Errorf("observer mapping has no host ID")
	}
	var batchID string
	var observedAt time.Time
	var statusJSON []byte
	var startTicks sql.NullInt64
	var serviceID sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT sb.batch_id, sb.observed_at, sb.status, sp.start_ticks, sp.service_id
		 FROM snapshot_batches AS sb
		 LEFT JOIN snapshot_processes AS sp
		   ON sp.host_id = sb.host_id AND sp.batch_id = sb.batch_id AND sp.pid = ?
		 WHERE sb.host_id = ? AND sb.observed_at <= ?
		 ORDER BY sb.observed_at DESC, sb.received_at DESC
		 LIMIT 1`,
		*event.ServerPID, hostID, event.RequestStart.UTC(),
	).Scan(&batchID, &observedAt, &statusJSON, &startTicks, &serviceID)
	if err != nil {
		if err == sql.ErrNoRows {
			result.status = "snapshot_missing"
			result.reason = "snapshot_missing"
			return nil
		}
		return fmt.Errorf("find process snapshot: %w", err)
	}
	result.snapshotBatchID = batchID
	result.snapshotObserved = observedAt.UTC()
	if event.RequestStart.Sub(observedAt) > maxSnapshotAge {
		result.status = "snapshot_stale"
		result.reason = "snapshot_stale"
		return nil
	}
	var collectorStatus contract.CollectorStatus
	if err := json.Unmarshal(statusJSON, &collectorStatus); err != nil {
		return fmt.Errorf("decode snapshot collector status: %w", err)
	}
	if !startTicks.Valid {
		if collectorStatus.Process == contract.CollectorOK {
			result.status = "process_not_found"
			result.reason = "process_not_found"
		} else if collectorStatus.Process == contract.CollectorFailed {
			result.status = "process_collection_failed"
			result.reason = "process_collection_failed"
		} else {
			result.status = "process_snapshot_incomplete"
			result.reason = "process_snapshot_incomplete"
		}
		return nil
	}
	start := uint64(startTicks.Int64)
	if start == 0 {
		return fmt.Errorf("snapshot process has zero start_ticks")
	}
	result.processStart = start
	service := ""
	if serviceID.Valid && serviceID.String != "" {
		result.serviceID = serviceID.String
		service = serviceID.String
	}

	devices, err := snapshotServiceDevices(ctx, tx, hostID, batchID, int(*event.ServerPID), start, service, collectorStatus)
	if err != nil {
		return err
	}
	result.devices = devices
	if len(devices) == 0 {
		if result.serviceID == nil {
			result.status = "service_unknown"
			result.reason = "service_unknown"
			return nil
		}
		result.status, result.reason = emptyBindingStatus(collectorStatus)
		return nil
	}
	result.status = "attribution_partial"
	result.reason = "device_source_or_process_snapshot_incomplete"
	result.confidence = "unknown"
	for _, device := range devices {
		if device.collectorState != contract.CollectorOK {
			continue
		}
		if device.evidence == "observed_process" {
			result.status = "attributed"
			result.reason = "server_process_binding_observed"
			result.confidence = "observed_process"
			return nil
		}
		if result.confidence == "unknown" {
			result.status = "attributed"
			result.reason = "same_service_binding_observed"
			result.confidence = "service_group"
		}
	}
	return nil
}

func snapshotServiceDevices(ctx context.Context, tx *sql.Tx, hostID, batchID string, pid int, startTicks uint64, serviceID string, statuses contract.CollectorStatus) ([]deviceEvidence, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT b.device_key, b.pid
		 FROM snapshot_device_bindings AS b
		 JOIN snapshot_processes AS p
		   ON p.host_id = b.host_id AND p.batch_id = b.batch_id
		  AND p.pid = b.pid AND p.start_ticks = b.start_ticks
		 WHERE b.host_id = ? AND b.batch_id = ?
		   AND ((p.pid = ? AND p.start_ticks = ?) OR (p.service_id = ? AND ? <> ''))
		 ORDER BY b.device_key, b.pid`,
		hostID, batchID, pid, startTicks, serviceID, serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("query service device bindings: %w", err)
	}
	byDevice := make(map[string]deviceEvidence)
	for rows.Next() {
		var deviceKey string
		var bindingPID int
		if err := rows.Scan(&deviceKey, &bindingPID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read service device binding: %w", err)
		}
		state := collectorStatusForDevice(deviceKey, statuses)
		evidence := "service_group"
		if bindingPID == pid {
			evidence = "observed_process"
		}
		switch state {
		case contract.CollectorSourceTimeUnknown:
			evidence = "source_time_unknown"
		case contract.CollectorPartial:
			evidence = "collector_partial"
		case contract.CollectorFailed, contract.CollectorStale:
			evidence = "collector_unavailable"
		}
		candidate := deviceEvidence{deviceKey: deviceKey, evidence: evidence, collectorState: state}
		_, exists := byDevice[deviceKey]
		if !exists || (candidate.collectorState == contract.CollectorOK && candidate.evidence == "observed_process") {
			byDevice[deviceKey] = candidate
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate service device bindings: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close service device bindings: %w", err)
	}
	devices := make([]deviceEvidence, 0, len(byDevice))
	for _, device := range byDevice {
		devices = append(devices, device)
	}
	return devices, nil
}

func collectorStatusForDevice(deviceKey string, statuses contract.CollectorStatus) contract.CollectorState {
	switch {
	case strings.HasPrefix(deviceKey, "nvidia:"):
		return statuses.NVIDIA
	case strings.HasPrefix(deviceKey, "ascend:"):
		return statuses.Ascend
	default:
		return contract.CollectorFailed
	}
}

func emptyBindingStatus(statuses contract.CollectorStatus) (status, reason string) {
	applicable := 0
	for _, state := range []contract.CollectorState{statuses.NVIDIA, statuses.Ascend} {
		if state == contract.CollectorNotApplicable {
			continue
		}
		applicable++
		switch state {
		case contract.CollectorFailed, contract.CollectorStale:
			return "collector_unavailable", "collector_unavailable"
		case contract.CollectorSourceTimeUnknown:
			return "source_time_unknown", "source_time_unknown"
		case contract.CollectorPartial:
			return "collector_partial", "collector_partial"
		}
	}
	if applicable == 0 {
		return "no_accelerator_collector", "no_accelerator_collector"
	}
	return "no_observed_binding", "no_observed_binding"
}

func persistAttribution(ctx context.Context, tx *sql.Tx, eventID uint64, result eventAttribution) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_event_attributions WHERE event_id = ?`, eventID); err != nil {
		return fmt.Errorf("replace previous event attribution: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_event_attributions
		 (event_id, host_id, snapshot_batch_id, snapshot_observed_at, process_start_ticks, service_id, status, confidence, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, result.hostID, result.snapshotBatchID, result.snapshotObserved, result.processStart, result.serviceID, result.status, result.confidence, result.reason,
	); err != nil {
		return fmt.Errorf("insert event attribution: %w", err)
	}
	for _, device := range result.devices {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audit_event_devices (event_id, device_key, evidence, collector_state)
			 VALUES (?, ?, ?, ?)`,
			eventID, device.deviceKey, device.evidence, device.collectorState,
		); err != nil {
			return fmt.Errorf("insert event device evidence: %w", err)
		}
	}
	return nil
}
