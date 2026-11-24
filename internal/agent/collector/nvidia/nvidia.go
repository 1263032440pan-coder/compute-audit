package nvidia

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Binding struct {
	DeviceKey   string
	PID         int
	MemoryBytes *uint64
}

type Collector struct {
	Command string
	Timeout time.Duration
}

func (c Collector) Collect(ctx context.Context) ([]Binding, error) {
	command := c.Command
	if command == "" {
		command = "nvidia-smi"
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, command,
		"--query-compute-apps=gpu_uuid,pid,used_gpu_memory",
		"--format=csv,noheader,nounits",
	)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("run nvidia-smi compute-app query: %w", err)
	}
	return ParseComputeApps(strings.NewReader(string(output)))
}

func ParseComputeApps(input io.Reader) ([]Binding, error) {
	reader := csv.NewReader(input)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	reader.ReuseRecord = false
	var bindings []Binding
	var rowErr error
	seen := make(map[string]struct{})
	for row := 1; ; row++ {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			rowErr = fmt.Errorf("parse nvidia-smi CSV row %d: %w", row, err)
			continue
		}
		if len(record) != 3 {
			rowErr = fmt.Errorf("nvidia-smi CSV row %d has %d fields, expected 3", row, len(record))
			continue
		}
		uuid := strings.TrimSpace(record[0])
		pid, err := strconv.Atoi(strings.TrimSpace(record[1]))
		if uuid == "" || pid <= 0 || err != nil {
			rowErr = fmt.Errorf("nvidia-smi CSV row %d has invalid UUID or PID", row)
			continue
		}
		var memoryBytes *uint64
		memoryText := strings.TrimSpace(record[2])
		if memoryText != "" && !strings.EqualFold(memoryText, "N/A") && !strings.EqualFold(memoryText, "[N/A]") {
			memoryMiB, err := strconv.ParseUint(memoryText, 10, 64)
			if err != nil || memoryMiB > ^uint64(0)/(1024*1024) {
				rowErr = fmt.Errorf("nvidia-smi CSV row %d has invalid GPU memory", row)
				continue
			}
			value := memoryMiB * 1024 * 1024
			memoryBytes = &value
		}
		deviceKey := "nvidia:" + uuid
		key := fmt.Sprintf("%s/%d", deviceKey, pid)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		bindings = append(bindings, Binding{DeviceKey: deviceKey, PID: pid, MemoryBytes: memoryBytes})
	}
	return bindings, rowErr
}
