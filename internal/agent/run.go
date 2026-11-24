package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"compute-audit/internal/agent/collector/ascend"
	"compute-audit/internal/agent/collector/nvidia"
	"compute-audit/internal/agent/discovery"
	"compute-audit/internal/agent/procfs"
	"compute-audit/internal/agent/reporter"
	"compute-audit/internal/agent/spool"
	"compute-audit/internal/contract"
)

const (
	procRoot          = "/proc"
	maxProcesses      = 16000
	maxDeviceBindings = 16000
)

func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	interval, err := time.ParseDuration(cfg.Interval)
	if err != nil || interval <= 0 {
		return fmt.Errorf("invalid agent interval %q", cfg.Interval)
	}
	queue, err := spool.Open(cfg.Spool.Directory, cfg.Spool.MaxBytes)
	if err != nil {
		return err
	}
	reportClient, err := reporter.New(cfg.Report.URL, cfg.Report.CAFile, cfg.Report.CertFile, cfg.Report.KeyFile)
	if err != nil {
		return err
	}
	bootID, err := readBootID()
	if err != nil {
		return err
	}
	notify := make(chan struct{}, 1)
	go reporter.RunLoop(ctx, queue, reportClient, notify, logger)

	collect := func() {
		if ctx.Err() != nil {
			return
		}
		batch, err := collectSnapshot(ctx, cfg, bootID, queue)
		if err != nil {
			logger.Error("snapshot collection failed", "host_id", cfg.HostID, "error", err)
			return
		}
		if ctx.Err() != nil {
			return
		}
		_, err = queue.Enqueue(batch)
		if err != nil {
			logger.Error("could not persist snapshot", "host_id", cfg.HostID, "batch_id", batch.BatchID, "error", err)
			return
		}
		logger.Info("snapshot persisted to local spool", "host_id", cfg.HostID, "batch_id", batch.BatchID, "sequence", batch.Sequence)
		select {
		case notify <- struct{}{}:
		default:
		}
	}

	collect()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			collect()
		}
	}
}

func collectSnapshot(ctx context.Context, cfg Config, bootID string, queue *spool.Queue) (contract.SnapshotBatch, error) {
	sequence, err := queue.NextSequence(bootID)
	if err != nil {
		return contract.SnapshotBatch{}, err
	}
	batchIDBytes := make([]byte, 16)
	if _, err := rand.Read(batchIDBytes); err != nil {
		return contract.SnapshotBatch{}, fmt.Errorf("create batch id: %w", err)
	}
	batch := contract.SnapshotBatch{
		SchemaVersion: contract.SnapshotSchemaVersion,
		BatchID:       hex.EncodeToString(batchIDBytes),
		HostID:        cfg.HostID,
		BootID:        bootID,
		ObservedAt:    time.Now().UTC(),
		Sequence:      sequence,
		Status: contract.CollectorStatus{
			NVIDIA:  contract.CollectorNotApplicable,
			Ascend:  contract.CollectorNotApplicable,
			Process: contract.CollectorFailed,
		},
		Processes: make([]contract.Process, 0),
		Services:  make([]contract.Service, 0),
		Bindings:  make([]contract.DeviceBinding, 0),
	}
	processes, failed, scanErr := procfs.Scan(procRoot)
	if scanErr != nil {
		batch.Status.Process = contract.CollectorFailed
	} else {
		batch.Status.Process = contract.CollectorOK
		if failed > 0 {
			batch.Status.Process = contract.CollectorPartial
		}
	}

	var nvidiaBindings []nvidia.Binding
	var ascendBindings []ascend.Binding
	if cfg.Vendor == "nvidia" || cfg.Vendor == "mixed" {
		timeout, _ := time.ParseDuration(cfg.NVIDIA.Timeout)
		collected, collectErr := (nvidia.Collector{Command: cfg.NVIDIA.SmiPath, Timeout: timeout}).Collect(ctx)
		nvidiaBindings = collected
		batch.Status.NVIDIA = collectorState(collectErr, len(collected))
	}
	if cfg.Vendor == "ascend" || cfg.Vendor == "mixed" {
		timeout, _ := time.ParseDuration(cfg.Ascend.Timeout)
		mapping := ascend.Mapping{
			MetricName:      cfg.Ascend.MetricName,
			PIDLabel:        cfg.Ascend.PIDLabel,
			PIDFromValue:    cfg.Ascend.PIDFromValue,
			DeviceLabels:    cfg.Ascend.DeviceLabels,
			MemoryLabel:     cfg.Ascend.MemoryLabel,
			MemoryFromValue: cfg.Ascend.MemoryFromValue,
			MemoryUnit:      cfg.Ascend.MemoryUnit,
			SourceTimeKnown: cfg.Ascend.SourceTimeKnown,
		}
		result, collectErr := ascend.Collect(ctx, cfg.HostID, cfg.Ascend.MetricsURL, mapping, timeout, nil)
		ascendBindings = result.Bindings
		switch {
		case collectErr != nil:
			batch.Status.Ascend = collectorState(collectErr, len(ascendBindings))
		case !result.SourceTimeKnown:
			batch.Status.Ascend = contract.CollectorSourceTimeUnknown
		default:
			batch.Status.Ascend = contract.CollectorOK
		}
	}

	if scanErr == nil {
		devicePIDs := make(map[int]struct{}, len(nvidiaBindings)+len(ascendBindings))
		for _, binding := range nvidiaBindings {
			devicePIDs[binding.PID] = struct{}{}
		}
		for _, binding := range ascendBindings {
			devicePIDs[binding.PID] = struct{}{}
		}
		// Prefer processes with accelerator bindings so the payload cap does not
		// discard the identities needed for attribution.
		sort.Slice(processes, func(i, j int) bool {
			_, leftBound := devicePIDs[processes[i].PID]
			_, rightBound := devicePIDs[processes[j].PID]
			if leftBound != rightBound {
				return leftBound
			}
			return processes[i].PID < processes[j].PID
		})
		if len(processes) > maxProcesses {
			processes = processes[:maxProcesses]
			batch.Status.Process = contract.CollectorPartial
		}
		processByPID := make(map[int]procfs.ProcessInfo, len(processes))
		services := make(map[string]contract.Service)
		batch.Processes = make([]contract.Process, 0, len(processes))
		for _, process := range processes {
			service := discovery.Resolve(cfg.HostID, process)
			services[service.ID] = service
			processByPID[process.PID] = process
			batch.Processes = append(batch.Processes, contract.Process{
				PID:               process.PID,
				PPID:              process.PPID,
				StartTicks:        process.StartTicks,
				ServiceInstanceID: service.ID,
				NetNS:             process.NetNS,
				CGroup:            truncate(process.CGroup, 2048),
			})
		}
		batch.Services = make([]contract.Service, 0, len(services))
		for _, service := range services {
			batch.Services = append(batch.Services, service)
		}
		sort.Slice(batch.Services, func(i, j int) bool { return batch.Services[i].ID < batch.Services[j].ID })
		validatedPIDs := make(map[int]bool, len(devicePIDs))
		for _, binding := range nvidiaBindings {
			batch.Bindings = append(batch.Bindings, contract.DeviceBinding{DeviceKey: binding.DeviceKey, PID: binding.PID, MemoryBytes: binding.MemoryBytes})
		}
		batch.Bindings = appendBindings(batch.Bindings, processByPID, validatedPIDs, &batch.Status.NVIDIA, &batch.Status.Process)
		for _, binding := range ascendBindings {
			batch.Bindings = append(batch.Bindings, contract.DeviceBinding{DeviceKey: binding.DeviceKey, PID: binding.PID, MemoryBytes: binding.MemoryBytes})
		}
		batch.Bindings = appendBindings(batch.Bindings, processByPID, validatedPIDs, &batch.Status.Ascend, &batch.Status.Process)
		if len(batch.Bindings) > maxDeviceBindings {
			batch.Bindings = batch.Bindings[:maxDeviceBindings]
			if batch.Status.NVIDIA == contract.CollectorOK || batch.Status.NVIDIA == contract.CollectorSourceTimeUnknown {
				batch.Status.NVIDIA = contract.CollectorPartial
			}
			if batch.Status.Ascend == contract.CollectorOK || batch.Status.Ascend == contract.CollectorSourceTimeUnknown {
				batch.Status.Ascend = contract.CollectorPartial
			}
		}
	} else {
		if len(nvidiaBindings) > 0 {
			batch.Status.NVIDIA = contract.CollectorPartial
		}
		if len(ascendBindings) > 0 {
			batch.Status.Ascend = contract.CollectorPartial
		}
	}
	return fitSnapshot(batch)
}

func appendBindings(destination []contract.DeviceBinding, processes map[int]procfs.ProcessInfo, validated map[int]bool, collectorState *contract.CollectorState, processState *contract.CollectorState) []contract.DeviceBinding {
	resolved := destination[:0]
	for _, binding := range destination {
		process, ok := processes[binding.PID]
		if ok {
			valid, checked := validated[binding.PID]
			if !checked {
				current, err := procfs.Read(procRoot, binding.PID)
				valid = err == nil && current.StartTicks == process.StartTicks
				validated[binding.PID] = valid
			}
			ok = valid
		}
		if !ok {
			if *collectorState == contract.CollectorOK || *collectorState == contract.CollectorSourceTimeUnknown {
				*collectorState = contract.CollectorPartial
			}
			if *processState == contract.CollectorOK {
				*processState = contract.CollectorPartial
			}
			continue
		}
		binding.StartTicks = process.StartTicks
		resolved = append(resolved, binding)
	}
	return resolved
}

func fitSnapshot(batch contract.SnapshotBatch) (contract.SnapshotBatch, error) {
	payload, err := json.Marshal(batch)
	if err != nil {
		return contract.SnapshotBatch{}, fmt.Errorf("encode snapshot batch: %w", err)
	}
	if len(payload) <= contract.MaxSnapshotBodyBytes {
		return batch, nil
	}
	for i := range batch.Processes {
		batch.Processes[i].CGroup = ""
	}
	if batch.Status.Process == contract.CollectorOK {
		batch.Status.Process = contract.CollectorPartial
	}
	payload, err = json.Marshal(batch)
	if err != nil {
		return contract.SnapshotBatch{}, fmt.Errorf("encode compact snapshot batch: %w", err)
	}
	if len(payload) <= contract.MaxSnapshotBodyBytes {
		return batch, nil
	}

	originalProcessCount := len(batch.Processes)
	low, high := 0, originalProcessCount
	for low < high {
		middle := low + (high-low+1)/2
		candidate := snapshotPrefix(batch, middle)
		markTrimmedStatus(batch, &candidate)
		encoded, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			return contract.SnapshotBatch{}, fmt.Errorf("encode trimmed snapshot batch: %w", marshalErr)
		}
		if len(encoded) <= contract.MaxSnapshotBodyBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	originalBatch := batch
	batch = snapshotPrefix(originalBatch, low)
	markTrimmedStatus(originalBatch, &batch)
	encoded, err := json.Marshal(batch)
	if err != nil {
		return contract.SnapshotBatch{}, fmt.Errorf("encode final snapshot batch: %w", err)
	}
	if len(encoded) > contract.MaxSnapshotBodyBytes {
		return contract.SnapshotBatch{}, fmt.Errorf("snapshot metadata exceeds %d bytes", contract.MaxSnapshotBodyBytes)
	}
	return batch, nil
}

func markTrimmedStatus(original contract.SnapshotBatch, candidate *contract.SnapshotBatch) {
	if len(candidate.Processes) < len(original.Processes) {
		candidate.Status.Process = contract.CollectorPartial
	}
	nvidiaBefore, ascendBefore := countBindings(original.Bindings)
	nvidiaAfter, ascendAfter := countBindings(candidate.Bindings)
	if nvidiaAfter < nvidiaBefore && (candidate.Status.NVIDIA == contract.CollectorOK || candidate.Status.NVIDIA == contract.CollectorSourceTimeUnknown) {
		candidate.Status.NVIDIA = contract.CollectorPartial
	}
	if ascendAfter < ascendBefore && (candidate.Status.Ascend == contract.CollectorOK || candidate.Status.Ascend == contract.CollectorSourceTimeUnknown) {
		candidate.Status.Ascend = contract.CollectorPartial
	}
}

func countBindings(bindings []contract.DeviceBinding) (nvidia, ascend int) {
	for _, binding := range bindings {
		if strings.HasPrefix(binding.DeviceKey, "nvidia:") {
			nvidia++
		} else if strings.HasPrefix(binding.DeviceKey, "ascend:") {
			ascend++
		}
	}
	return nvidia, ascend
}

func snapshotPrefix(batch contract.SnapshotBatch, count int) contract.SnapshotBatch {
	candidate := batch
	if count < 0 {
		count = 0
	}
	if count > len(batch.Processes) {
		count = len(batch.Processes)
	}
	candidate.Processes = append([]contract.Process(nil), batch.Processes[:count]...)
	processes := make(map[processIdentity]struct{}, count)
	services := make(map[string]struct{}, count)
	for _, process := range candidate.Processes {
		processes[processIdentity{pid: process.PID, start: process.StartTicks}] = struct{}{}
		if process.ServiceInstanceID != "" {
			services[process.ServiceInstanceID] = struct{}{}
		}
	}
	candidate.Bindings = make([]contract.DeviceBinding, 0, len(batch.Bindings))
	for _, binding := range batch.Bindings {
		if _, exists := processes[processIdentity{pid: binding.PID, start: binding.StartTicks}]; exists {
			candidate.Bindings = append(candidate.Bindings, binding)
		}
	}
	candidate.Services = make([]contract.Service, 0, len(batch.Services))
	for _, service := range batch.Services {
		if _, exists := services[service.ID]; exists {
			candidate.Services = append(candidate.Services, service)
		}
	}
	return candidate
}

type processIdentity struct {
	pid   int
	start uint64
}

func collectorState(err error, count int) contract.CollectorState {
	if err == nil {
		return contract.CollectorOK
	}
	if count > 0 {
		return contract.CollectorPartial
	}
	return contract.CollectorFailed
}

func readBootID() (string, error) {
	data, err := os.ReadFile(procRoot + "/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read host boot id: %w", err)
	}
	bootID := strings.TrimSpace(string(data))
	if bootID == "" || len(bootID) > 128 {
		return "", fmt.Errorf("host boot id is empty or invalid")
	}
	return bootID, nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
