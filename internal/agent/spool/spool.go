package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"compute-audit/internal/contract"
)

var ErrFull = errors.New("snapshot spool is full")

type Queue struct {
	directory string
	maxBytes  int64
}

type sequenceState struct {
	BootID   string `json:"boot_id"`
	Sequence uint64 `json:"sequence"`
}

func Open(directory string, maxBytes int64) (*Queue, error) {
	if directory == "" || maxBytes <= 0 {
		return nil, fmt.Errorf("spool directory and positive maxBytes are required")
	}
	rejectedDir := filepath.Join(directory, "rejected")
	if err := os.MkdirAll(rejectedDir, 0700); err != nil {
		return nil, fmt.Errorf("create spool directory: %w", err)
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, fmt.Errorf("secure spool directory: %w", err)
	}
	if err := os.Chmod(rejectedDir, 0700); err != nil {
		return nil, fmt.Errorf("secure rejected spool directory: %w", err)
	}
	return &Queue{directory: directory, maxBytes: maxBytes}, nil
}

func (q *Queue) NextSequence(bootID string) (uint64, error) {
	if bootID == "" {
		return 0, fmt.Errorf("boot_id is required")
	}
	statePath := filepath.Join(q.directory, "sequence.json")
	var state sequenceState
	data, err := os.ReadFile(statePath)
	if err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return 0, fmt.Errorf("decode spool sequence state: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("read spool sequence state: %w", err)
	}
	if state.BootID != bootID {
		state = sequenceState{BootID: bootID}
	}
	if state.Sequence == ^uint64(0) {
		return 0, fmt.Errorf("snapshot sequence exhausted")
	}
	state.Sequence++
	encoded, err := json.Marshal(state)
	if err != nil {
		return 0, fmt.Errorf("encode spool sequence state: %w", err)
	}
	if err := atomicWrite(statePath, encoded, 0600); err != nil {
		return 0, fmt.Errorf("persist spool sequence state: %w", err)
	}
	return state.Sequence, nil
}

func (q *Queue) Enqueue(batch contract.SnapshotBatch) (string, error) {
	payload, err := json.Marshal(batch)
	if err != nil {
		return "", fmt.Errorf("encode snapshot batch: %w", err)
	}
	if len(payload) > contract.MaxSnapshotBodyBytes {
		return "", fmt.Errorf("snapshot batch exceeds server request limit")
	}
	used, err := q.usedBytes()
	if err != nil {
		return "", err
	}
	if int64(len(payload))+used > q.maxBytes {
		return "", ErrFull
	}
	name := fmt.Sprintf("batch-%020d-%s-%020d.json", batch.ObservedAt.UnixNano(), batch.BootID, batch.Sequence)
	path := filepath.Join(q.directory, name)
	if err := atomicWrite(path, payload, 0600); err != nil {
		return "", fmt.Errorf("persist snapshot batch: %w", err)
	}
	return path, nil
}

func (q *Queue) Pending() ([]string, error) {
	entries, err := os.ReadDir(q.directory)
	if err != nil {
		return nil, fmt.Errorf("list spool: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "batch-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(q.directory, entry.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

func (q *Queue) Read(path string) (contract.SnapshotBatch, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return contract.SnapshotBatch{}, nil, fmt.Errorf("read spooled batch: %w", err)
	}
	batch, err := contract.DecodeSnapshotBytes(data)
	if err != nil {
		return contract.SnapshotBatch{}, nil, fmt.Errorf("decode spooled batch: %w", err)
	}
	return batch, data, nil
}

func (q *Queue) Ack(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove acknowledged snapshot: %w", err)
	}
	return nil
}

func (q *Queue) Quarantine(path string) error {
	destination := filepath.Join(q.directory, "rejected", filepath.Base(path))
	if err := os.Rename(path, destination); err != nil {
		return fmt.Errorf("quarantine rejected snapshot: %w", err)
	}
	return nil
}

func (q *Queue) usedBytes() (int64, error) {
	var total int64
	err := filepath.WalkDir(q.directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure spool size: %w", err)
	}
	return total, nil
}

func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(directory, ".snapshot-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}
