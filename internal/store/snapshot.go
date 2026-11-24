package store

import (
	"context"
	"errors"

	"compute-audit/internal/contract"
)

var ErrIdempotencyConflict = errors.New("batch_id was already accepted with different content")

type SnapshotResult struct {
	Duplicate bool
}

// SnapshotStore must persist the batch and all derived host relationships in
// one transaction. It must return only after commit and treat an exact replay
// as a successful duplicate.
type SnapshotStore interface {
	PersistSnapshot(ctx context.Context, batch contract.SnapshotBatch, payloadSHA256 [32]byte) (SnapshotResult, error)
}
