package contract

import "time"

const SnapshotSchemaVersion = 1

// SnapshotBatch is the host fact batch sent by audit-agent. It intentionally
// contains no request bodies, credentials, command lines, or environment data.
type SnapshotBatch struct {
	SchemaVersion int             `json:"schema_version"`
	BatchID       string          `json:"batch_id"`
	HostID        string          `json:"host_id"`
	BootID        string          `json:"boot_id"`
	ObservedAt    time.Time       `json:"observed_at"`
	Sequence      uint64          `json:"sequence"`
	Status        CollectorStatus `json:"status"`
	Processes     []Process       `json:"processes"`
	Services      []Service       `json:"services"`
	Bindings      []DeviceBinding `json:"bindings"`
}

type CollectorStatus struct {
	NVIDIA  CollectorState `json:"nvidia"`
	Ascend  CollectorState `json:"ascend"`
	Process CollectorState `json:"process"`
}

type CollectorState string

const (
	CollectorOK                CollectorState = "ok"
	CollectorPartial           CollectorState = "partial"
	CollectorFailed            CollectorState = "failed"
	CollectorNotApplicable     CollectorState = "not_applicable"
	CollectorStale             CollectorState = "stale"
	CollectorSourceTimeUnknown CollectorState = "source_time_unknown"
)

type Process struct {
	PID               int    `json:"pid"`
	StartTicks        uint64 `json:"start_ticks"`
	ServiceInstanceID string `json:"service_instance_id,omitempty"`
	NetNS             string `json:"netns,omitempty"`
}

type Service struct {
	ID     string   `json:"id"`
	Type   string   `json:"type"`
	Name   string   `json:"name"`
	Listen []Socket `json:"listen"`
}

type Socket struct {
	PID   int    `json:"pid"`
	IP    string `json:"ip"`
	Port  uint16 `json:"port"`
	NetNS string `json:"netns,omitempty"`
}

type DeviceBinding struct {
	DeviceKey   string  `json:"device_key"`
	PID         int     `json:"pid"`
	StartTicks  uint64  `json:"start_ticks"`
	MemoryBytes *uint64 `json:"memory_bytes,omitempty"`
}
