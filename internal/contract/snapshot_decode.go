package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
)

const MaxSnapshotBodyBytes = 2 << 20

// DecodeSnapshot enforces the wire-size limit, rejects unknown JSON fields,
// and validates all required identities before a batch reaches persistence.
func DecodeSnapshot(r io.Reader) (SnapshotBatch, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxSnapshotBodyBytes+1))
	if err != nil {
		return SnapshotBatch{}, fmt.Errorf("read snapshot body: %w", err)
	}
	return DecodeSnapshotBytes(body)
}

func DecodeSnapshotBytes(body []byte) (SnapshotBatch, error) {
	if len(body) > MaxSnapshotBodyBytes {
		return SnapshotBatch{}, fmt.Errorf("snapshot body exceeds %d bytes", MaxSnapshotBodyBytes)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	var batch SnapshotBatch
	if err := dec.Decode(&batch); err != nil {
		return SnapshotBatch{}, fmt.Errorf("decode snapshot: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		if err == nil {
			return SnapshotBatch{}, fmt.Errorf("decode snapshot: multiple JSON values")
		}
		return SnapshotBatch{}, fmt.Errorf("decode snapshot trailer: %w", err)
	}
	if err := batch.Validate(); err != nil {
		return SnapshotBatch{}, err
	}
	return batch, nil
}

func (b SnapshotBatch) Validate() error {
	if b.SchemaVersion != SnapshotSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", b.SchemaVersion)
	}
	if !validIdentifier(b.BatchID, 191) {
		return fmt.Errorf("batch_id is required and must be at most 191 characters")
	}
	if !validIdentifier(b.HostID, 128) {
		return fmt.Errorf("host_id is required and must be at most 128 characters")
	}
	if !validIdentifier(b.BootID, 128) {
		return fmt.Errorf("boot_id is required and must be at most 128 characters")
	}
	if b.Sequence == 0 {
		return fmt.Errorf("sequence must be greater than zero")
	}
	if b.ObservedAt.IsZero() {
		return fmt.Errorf("observed_at is required")
	}
	if err := validateCollectorState("status.nvidia", b.Status.NVIDIA); err != nil {
		return err
	}
	if err := validateCollectorState("status.ascend", b.Status.Ascend); err != nil {
		return err
	}
	if err := validateCollectorState("status.process", b.Status.Process); err != nil {
		return err
	}

	if len(b.Processes)+len(b.Services)+len(b.Bindings) > 50000 {
		return fmt.Errorf("snapshot contains too many records")
	}
	if err := validateProcesses(b.Processes); err != nil {
		return err
	}
	if err := validateServices(b.Services); err != nil {
		return err
	}
	serviceIDs := make(map[string]struct{}, len(b.Services))
	for _, service := range b.Services {
		serviceIDs[service.ID] = struct{}{}
	}
	for i, process := range b.Processes {
		if process.ServiceInstanceID == "" {
			continue
		}
		if _, exists := serviceIDs[process.ServiceInstanceID]; !exists {
			return fmt.Errorf("processes[%d].service_instance_id does not exist in services", i)
		}
	}
	if err := validateBindings(b.Bindings); err != nil {
		return err
	}
	processIDs := make(map[processKey]struct{}, len(b.Processes))
	for _, process := range b.Processes {
		processIDs[processKey{pid: process.PID, startTicks: process.StartTicks}] = struct{}{}
	}
	for i, binding := range b.Bindings {
		if _, exists := processIDs[processKey{pid: binding.PID, startTicks: binding.StartTicks}]; !exists {
			return fmt.Errorf("bindings[%d] does not reference a process in this snapshot", i)
		}
	}
	return nil
}

func validateProcesses(processes []Process) error {
	seen := make(map[processKey]struct{}, len(processes))
	for i, process := range processes {
		if process.PID <= 0 || process.StartTicks == 0 {
			return fmt.Errorf("processes[%d] requires positive pid and start_ticks", i)
		}
		key := processKey{pid: process.PID, startTicks: process.StartTicks}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("processes[%d] duplicates pid/start_ticks", i)
		}
		seen[key] = struct{}{}
		if process.ServiceInstanceID != "" && !validIdentifier(process.ServiceInstanceID, 191) {
			return fmt.Errorf("processes[%d].service_instance_id is invalid", i)
		}
	}
	return nil
}

func validateServices(services []Service) error {
	seen := make(map[string]struct{}, len(services))
	for i, service := range services {
		if !validIdentifier(service.ID, 191) || !validServiceType(service.Type) {
			return fmt.Errorf("services[%d] requires a valid id and type", i)
		}
		if _, ok := seen[service.ID]; ok {
			return fmt.Errorf("services[%d] duplicates id", i)
		}
		seen[service.ID] = struct{}{}
		for j, socket := range service.Listen {
			if socket.PID <= 0 || socket.Port == 0 || net.ParseIP(socket.IP) == nil {
				return fmt.Errorf("services[%d].listen[%d] requires pid, IP address, and port", i, j)
			}
		}
	}
	return nil
}

func validateBindings(bindings []DeviceBinding) error {
	seen := make(map[bindingKey]struct{}, len(bindings))
	for i, binding := range bindings {
		if !validIdentifier(binding.DeviceKey, 191) || binding.PID <= 0 || binding.StartTicks == 0 {
			return fmt.Errorf("bindings[%d] requires device_key, pid, and start_ticks", i)
		}
		key := bindingKey{deviceKey: binding.DeviceKey, pid: binding.PID, startTicks: binding.StartTicks}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("bindings[%d] duplicates device/process identity", i)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateCollectorState(field string, state CollectorState) error {
	switch state {
	case CollectorOK, CollectorPartial, CollectorFailed, CollectorNotApplicable, CollectorStale, CollectorSourceTimeUnknown:
		return nil
	default:
		return fmt.Errorf("%s has unsupported value %q", field, state)
	}
}

func validServiceType(value string) bool {
	switch value {
	case "docker", "systemd", "process":
		return true
	default:
		return false
	}
}

func validIdentifier(value string, max int) bool {
	return value != "" && len(value) <= max && strings.TrimSpace(value) == value
}

type processKey struct {
	pid        int
	startTicks uint64
}

type bindingKey struct {
	deviceKey  string
	pid        int
	startTicks uint64
}
