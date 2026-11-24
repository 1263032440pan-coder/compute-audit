package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"compute-audit/internal/config"
	"compute-audit/internal/deepflow"
	"compute-audit/internal/ingest"
	mysqlstore "compute-audit/internal/store/mysql"
)

func main() {
	if err := run(); err != nil {
		slog.Error("audit-server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "configs/server.example.yaml", "path to server YAML config")
	mode := flag.String("mode", "serve", "mode: serve, migrate, register-host, register-observer, close-observer, deepflow-schema, or deepflow-poll")
	hostID := flag.String("host-id", "", "host ID for register-host mode")
	hostname := flag.String("hostname", "", "host name for register-host mode")
	arch := flag.String("arch", "", "host architecture for register-host mode")
	vendor := flag.String("vendor", "", "accelerator vendor for register-host mode")
	queryFrom := flag.String("from", "", "RFC3339 query window start for deepflow-poll mode")
	queryTo := flag.String("to", "", "RFC3339 query window end for deepflow-poll mode")
	deepflowInstanceID := flag.String("deepflow-instance-id", "", "DeepFlow instance ID for register-observer mode")
	observerAgentID := flag.String("observer-agent-id", "", "DeepFlow observer agent ID for register-observer mode")
	validFrom := flag.String("valid-from", "", "RFC3339 start of observer mapping validity")
	validTo := flag.String("valid-to", "", "RFC3339 exclusive end of observer mapping validity; empty means no end")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *mode == "deepflow-schema" {
		if !cfg.DeepFlow.Enabled {
			return fmt.Errorf("deepflow.enabled must be true for deepflow-schema mode")
		}
		client, err := deepflow.New(cfg.DeepFlow)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		schema, err := client.DiscoverSchema(ctx, cfg.DeepFlow.Database, cfg.DeepFlow.Table)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(schema)
	}
	if *mode != "serve" && *mode != "migrate" && *mode != "register-host" && *mode != "register-observer" && *mode != "close-observer" && *mode != "deepflow-poll" {
		return fmt.Errorf("unsupported mode %q", *mode)
	}
	password, ok := os.LookupEnv(cfg.Database.PasswordEnv)
	if !ok || password == "" {
		return fmt.Errorf("required database password environment variable %q is not set", cfg.Database.PasswordEnv)
	}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer startupCancel()
	db, err := mysqlstore.Open(startupCtx, cfg.Database, password)
	if err != nil {
		return err
	}
	defer db.Close()

	switch *mode {
	case "migrate":
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := db.Migrate(ctx); err != nil {
			return err
		}
		logger.Info("database migrations applied")
		return nil
	case "register-host":
		if *hostID == "" || *arch == "" || *vendor == "" {
			return fmt.Errorf("register-host requires --host-id, --arch, and --vendor")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.RegisterHost(ctx, *hostID, *hostname, *arch, *vendor); err != nil {
			return err
		}
		logger.Info("host registered", "host_id", *hostID, "arch", *arch, "vendor", *vendor)
		return nil
	case "register-observer":
		if *deepflowInstanceID == "" || *observerAgentID == "" || *hostID == "" || *validFrom == "" {
			return fmt.Errorf("register-observer requires --deepflow-instance-id, --observer-agent-id, --host-id, and --valid-from")
		}
		from, err := time.Parse(time.RFC3339Nano, *validFrom)
		if err != nil {
			return fmt.Errorf("--valid-from must be RFC3339: %w", err)
		}
		var to *time.Time
		if *validTo != "" {
			parsed, err := time.Parse(time.RFC3339Nano, *validTo)
			if err != nil {
				return fmt.Errorf("--valid-to must be RFC3339: %w", err)
			}
			to = &parsed
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.RegisterObserverBinding(ctx, *deepflowInstanceID, *observerAgentID, *hostID, from, to); err != nil {
			return err
		}
		logger.Info("DeepFlow observer mapped to host", "deepflow_instance_id", *deepflowInstanceID, "observer_agent_id", *observerAgentID, "host_id", *hostID, "valid_from", from.UTC(), "valid_to", to)
		return nil
	case "close-observer":
		if *deepflowInstanceID == "" || *observerAgentID == "" || *validTo == "" {
			return fmt.Errorf("close-observer requires --deepflow-instance-id, --observer-agent-id, and --valid-to")
		}
		to, err := time.Parse(time.RFC3339Nano, *validTo)
		if err != nil {
			return fmt.Errorf("--valid-to must be RFC3339: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := db.CloseObserverBinding(ctx, *deepflowInstanceID, *observerAgentID, to); err != nil {
			return err
		}
		logger.Info("DeepFlow observer mapping closed", "deepflow_instance_id", *deepflowInstanceID, "observer_agent_id", *observerAgentID, "valid_to", to.UTC())
		return nil
	case "serve":
		return serve(cfg, db, logger)
	case "deepflow-poll":
		if !cfg.DeepFlow.Enabled || cfg.DeepFlow.Profile == "" {
			return fmt.Errorf("deepflow.enabled and deepflow.profile_file are required for deepflow-poll mode")
		}
		from, err := time.Parse(time.RFC3339Nano, *queryFrom)
		if err != nil {
			return fmt.Errorf("--from must be an RFC3339 timestamp: %w", err)
		}
		to, err := time.Parse(time.RFC3339Nano, *queryTo)
		if err != nil {
			return fmt.Errorf("--to must be an RFC3339 timestamp: %w", err)
		}
		profile, err := deepflow.LoadProfile(cfg.DeepFlow.Profile)
		if err != nil {
			return err
		}
		client, err := deepflow.New(cfg.DeepFlow)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		maxSnapshotAge, err := time.ParseDuration(cfg.DeepFlow.SnapshotMaxAge)
		if err != nil || maxSnapshotAge <= 0 {
			return fmt.Errorf("deepflow.snapshot_max_age must be a positive duration")
		}
		count, err := deepflow.PollWindow(ctx, client, cfg.DeepFlow.Database, cfg.DeepFlow.Table, profile, from, to, cfg.DeepFlow.PageSize, cfg.DeepFlow.MaxPages,
			func(events []deepflow.Event) error {
				return db.PersistDeepFlowEvents(ctx, cfg.DeepFlow.InstanceID, maxSnapshotAge, events)
			},
		)
		if err != nil {
			logger.Error("DeepFlow window poll incomplete", "rows_persisted_or_replayed", count, "error", err)
			return err
		}
		logger.Info("DeepFlow window poll completed", "rows_persisted_or_replayed", count, "from", from.UTC(), "to", to.UTC())
		return nil
	default:
		return fmt.Errorf("unsupported mode %q", *mode)
	}
}

func serve(cfg config.Config, repository *mysqlstore.Store, logger *slog.Logger) error {
	certificate, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return fmt.Errorf("load server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.TLS.ClientCAFile)
	if err != nil {
		return fmt.Errorf("read client CA file: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("client CA file contains no valid certificates")
	}

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           ingest.NewHandler(repository, logger).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{certificate},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    clientCAs,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("audit-server listening", "address", cfg.Listen)
		serveErr <- server.ListenAndServeTLS("", "")
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTPS: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTPS: %w", err)
		}
		return nil
	}
}
