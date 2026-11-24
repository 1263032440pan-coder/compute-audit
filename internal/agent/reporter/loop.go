package reporter

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"compute-audit/internal/agent/spool"
)

const (
	initialRetryDelay = 2 * time.Second
	maxRetryDelay     = 2 * time.Minute
)

func RunLoop(ctx context.Context, queue *spool.Queue, client *Client, notify <-chan struct{}, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	delay := initialRetryDelay
	for {
		if ctx.Err() != nil {
			return
		}
		pending, err := queue.Pending()
		if err != nil {
			logger.Error("could not list pending snapshots", "error", err)
			if !wait(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
			continue
		}
		if len(pending) == 0 {
			delay = initialRetryDelay
			select {
			case <-ctx.Done():
				return
			case <-notify:
				continue
			}
		}

		blocked := false
		for _, path := range pending {
			if ctx.Err() != nil {
				return
			}
			batch, payload, err := queue.Read(path)
			if err != nil {
				logger.Error("spooled snapshot is unreadable; moving it to quarantine", "path", path, "error", err)
				if quarantineErr := queue.Quarantine(path); quarantineErr != nil {
					logger.Error("could not quarantine unreadable snapshot", "path", path, "error", quarantineErr)
					blocked = true
					break
				}
				continue
			}
			if err := client.Send(ctx, batch, payload); err != nil {
				var permanent *PermanentError
				if errors.As(err, &permanent) {
					logger.Error("server permanently rejected snapshot; moving it to quarantine", "host_id", batch.HostID, "batch_id", batch.BatchID, "status", permanent.StatusCode)
					if quarantineErr := queue.Quarantine(path); quarantineErr != nil {
						logger.Error("could not quarantine rejected snapshot", "path", path, "error", quarantineErr)
						blocked = true
						break
					}
					delay = initialRetryDelay
					continue
				}
				if ctx.Err() != nil {
					return
				}
				logger.Warn("snapshot report failed; it remains on disk for retry", "host_id", batch.HostID, "batch_id", batch.BatchID, "retry_in", delay.String(), "error", err)
				blocked = true
				break
			}
			if err := queue.Ack(path); err != nil {
				logger.Error("server acknowledged snapshot but local queue removal failed", "host_id", batch.HostID, "batch_id", batch.BatchID, "error", err)
				blocked = true
				break
			}
			logger.Info("snapshot acknowledged", "host_id", batch.HostID, "batch_id", batch.BatchID, "sequence", batch.Sequence)
			delay = initialRetryDelay
		}
		if blocked {
			if !wait(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
		}
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextDelay(current time.Duration) time.Duration {
	if current >= maxRetryDelay/2 {
		return maxRetryDelay
	}
	return current * 2
}
