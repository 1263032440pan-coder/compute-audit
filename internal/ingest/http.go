package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"compute-audit/internal/contract"
	"compute-audit/internal/store"
)

const snapshotPath = "/api/v1/agent/snapshots"

const maxFutureSnapshotSkew = 2 * time.Second

type Repository interface {
	store.SnapshotStore
	store.ReadyStore
}

type Handler struct {
	repository Repository
	logger     *slog.Logger
	accepted   atomic.Uint64
	duplicate  atomic.Uint64
	rejected   atomic.Uint64
	failed     atomic.Uint64
	clockSkew  atomic.Uint64
}

func NewHandler(repository Repository, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{repository: repository, logger: logger}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /readyz", h.ready)
	mux.HandleFunc("GET /metrics", h.metrics)
	mux.HandleFunc(snapshotPath, h.receiveSnapshot)
	return securityHeaders(mux)
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.repository.Ping(ctx); err != nil {
		h.logger.Error("readiness check failed", "error", err)
		h.writeError(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ready\n")
}

func (h *Handler) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP compute_audit_ingest_requests_total Snapshot ingest requests by result.\n")
	_, _ = fmt.Fprintf(w, "# TYPE compute_audit_ingest_requests_total counter\n")
	_, _ = fmt.Fprintf(w, "compute_audit_ingest_requests_total{result=\"accepted\"} %d\n", h.accepted.Load())
	_, _ = fmt.Fprintf(w, "compute_audit_ingest_requests_total{result=\"duplicate\"} %d\n", h.duplicate.Load())
	_, _ = fmt.Fprintf(w, "compute_audit_ingest_requests_total{result=\"rejected\"} %d\n", h.rejected.Load())
	_, _ = fmt.Fprintf(w, "compute_audit_ingest_requests_total{result=\"failed\"} %d\n", h.failed.Load())
	_, _ = fmt.Fprintf(w, "# HELP compute_audit_snapshot_future_skew_warnings_total Snapshots observed more than two seconds in the future.\n")
	_, _ = fmt.Fprintf(w, "# TYPE compute_audit_snapshot_future_skew_warnings_total counter\n")
	_, _ = fmt.Fprintf(w, "compute_audit_snapshot_future_skew_warnings_total %d\n", h.clockSkew.Load())
}

func (h *Handler) receiveSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.reject(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	certHostID, ok := verifiedClientHostID(r)
	if !ok {
		h.reject(w, http.StatusUnauthorized, "verified client certificate required")
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" || len(idempotencyKey) > 191 || strings.TrimSpace(idempotencyKey) != idempotencyKey {
		h.reject(w, http.StatusBadRequest, "valid Idempotency-Key required")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.reject(w, http.StatusUnsupportedMediaType, "application/json required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, contract.MaxSnapshotBodyBytes+1))
	if err != nil {
		h.reject(w, http.StatusBadRequest, "unable to read snapshot")
		return
	}
	batch, err := contract.DecodeSnapshotBytes(body)
	if err != nil {
		h.reject(w, http.StatusBadRequest, "invalid snapshot")
		return
	}
	if batch.BatchID != idempotencyKey {
		h.reject(w, http.StatusBadRequest, "Idempotency-Key must match batch_id")
		return
	}
	if batch.HostID != certHostID {
		h.reject(w, http.StatusForbidden, "certificate identity does not match host_id")
		return
	}
	if skew := time.Until(batch.ObservedAt); skew > maxFutureSnapshotSkew {
		h.clockSkew.Add(1)
		h.logger.Warn("snapshot observed_at is ahead of server clock", "host_id", batch.HostID, "future_skew_seconds", skew.Seconds())
	}

	hash := sha256.Sum256(body)
	result, err := h.repository.PersistSnapshot(r.Context(), batch, hash)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrUnknownHost), errors.Is(err, store.ErrInactiveHost):
			h.reject(w, http.StatusForbidden, "host is not registered or active")
		case errors.Is(err, store.ErrIdempotencyConflict):
			h.reject(w, http.StatusConflict, "batch identity conflicts with previously accepted data")
		default:
			h.failed.Add(1)
			h.logger.Error("snapshot persistence failed", "host_id", batch.HostID, "batch_id", batch.BatchID, "error", err)
			h.writeError(w, http.StatusServiceUnavailable, "snapshot not accepted")
		}
		return
	}
	if result.Duplicate {
		h.duplicate.Add(1)
	} else {
		h.accepted.Add(1)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		BatchID  string `json:"batch_id"`
		Accepted bool   `json:"accepted"`
	}{BatchID: batch.BatchID, Accepted: true})
}

func verifiedClientHostID(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
		return "", false
	}
	hostID := r.TLS.PeerCertificates[0].Subject.CommonName
	if hostID == "" || strings.TrimSpace(hostID) != hostID {
		return "", false
	}
	return hostID, true
}

func (h *Handler) reject(w http.ResponseWriter, status int, message string) {
	h.rejected.Add(1)
	h.writeError(w, status, message)
}

func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	http.Error(w, message, status)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
