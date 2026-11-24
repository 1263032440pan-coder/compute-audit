package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"

	"compute-audit/internal/contract"
)

const insertChunkSize = 250

func persistSnapshotFacts(ctx context.Context, tx *sql.Tx, batch contract.SnapshotBatch) error {
	serviceRows := make([][]any, 0, len(batch.Services))
	for _, service := range batch.Services {
		serviceRows = append(serviceRows, []any{batch.HostID, batch.BatchID, service.ID, service.Type, service.Name})
	}
	if err := insertRows(ctx, tx,
		"INSERT INTO snapshot_services (host_id, batch_id, service_id, service_type, service_name) VALUES ",
		5, serviceRows,
	); err != nil {
		return fmt.Errorf("persist snapshot services: %w", err)
	}

	processRows := make([][]any, 0, len(batch.Processes))
	for _, process := range batch.Processes {
		var serviceID any
		if process.ServiceInstanceID != "" {
			serviceID = process.ServiceInstanceID
		}
		processRows = append(processRows, []any{
			batch.HostID,
			batch.BatchID,
			process.PID,
			process.PPID,
			process.StartTicks,
			serviceID,
			process.NetNS,
			process.CGroup,
		})
	}
	if err := insertRows(ctx, tx,
		"INSERT INTO snapshot_processes (host_id, batch_id, pid, ppid, start_ticks, service_id, netns, cgroup) VALUES ",
		8, processRows,
	); err != nil {
		return fmt.Errorf("persist snapshot processes: %w", err)
	}

	listenerRows := make([][]any, 0)
	for _, service := range batch.Services {
		for _, listener := range service.Listen {
			ip := net.ParseIP(listener.IP)
			if ip == nil {
				return fmt.Errorf("service %q has an invalid listener IP", service.ID)
			}
			listenerRows = append(listenerRows, []any{
				batch.HostID,
				batch.BatchID,
				service.ID,
				listener.PID,
				[]byte(ip.To16()),
				listener.Port,
				listener.NetNS,
			})
		}
	}
	if err := insertRows(ctx, tx,
		"INSERT INTO snapshot_listeners (host_id, batch_id, service_id, pid, ip, port, netns) VALUES ",
		7, listenerRows,
	); err != nil {
		return fmt.Errorf("persist snapshot listeners: %w", err)
	}

	bindingRows := make([][]any, 0, len(batch.Bindings))
	for _, binding := range batch.Bindings {
		var memoryBytes any
		if binding.MemoryBytes != nil {
			memoryBytes = *binding.MemoryBytes
		}
		bindingRows = append(bindingRows, []any{
			batch.HostID,
			batch.BatchID,
			binding.DeviceKey,
			binding.PID,
			binding.StartTicks,
			memoryBytes,
		})
	}
	if err := insertRows(ctx, tx,
		"INSERT INTO snapshot_device_bindings (host_id, batch_id, device_key, pid, start_ticks, memory_bytes) VALUES ",
		6, bindingRows,
	); err != nil {
		return fmt.Errorf("persist snapshot device bindings: %w", err)
	}
	return nil
}

func insertRows(ctx context.Context, tx *sql.Tx, prefix string, columns int, rows [][]any) error {
	if columns <= 0 {
		return fmt.Errorf("insert column count must be positive")
	}
	rowPlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?,", columns), ",") + ")"
	for start := 0; start < len(rows); start += insertChunkSize {
		end := min(start+insertChunkSize, len(rows))
		var query strings.Builder
		query.Grow(len(prefix) + (end-start)*len(rowPlaceholder+","))
		query.WriteString(prefix)
		args := make([]any, 0, (end-start)*columns)
		for i := start; i < end; i++ {
			if len(rows[i]) != columns {
				return fmt.Errorf("row %d has %d values, expected %d", i, len(rows[i]), columns)
			}
			if i > start {
				query.WriteByte(',')
			}
			query.WriteString(rowPlaceholder)
			args = append(args, rows[i]...)
		}
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			return err
		}
	}
	return nil
}
