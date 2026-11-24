package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maxConfigBytes = 1 << 20

type Config struct {
	Listen   string         `yaml:"listen"`
	TLS      TLSConfig      `yaml:"tls"`
	Database DatabaseConfig `yaml:"database"`
	DeepFlow DeepFlowConfig `yaml:"deepflow"`
}

type DeepFlowConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Endpoint       string `yaml:"endpoint"`
	Database       string `yaml:"database"`
	Table          string `yaml:"table"`
	InstanceID     string `yaml:"instance_id"`
	Profile        string `yaml:"profile_file"`
	TokenEnv       string `yaml:"token_env"`
	CAFile         string `yaml:"ca_file"`
	Timeout        string `yaml:"timeout"`
	PageSize       int    `yaml:"page_size"`
	MaxPages       int    `yaml:"max_pages_per_window"`
	SnapshotMaxAge string `yaml:"snapshot_max_age"`
}

type TLSConfig struct {
	CertFile          string `yaml:"cert_file"`
	KeyFile           string `yaml:"key_file"`
	ClientCAFile      string `yaml:"client_ca_file"`
	RequireClientCert bool   `yaml:"require_client_cert"`
}

type DatabaseConfig struct {
	Host            string `yaml:"host"`
	Name            string `yaml:"name"`
	User            string `yaml:"user"`
	PasswordEnv     string `yaml:"password_env"`
	MaxOpenConns    int    `yaml:"max_open_conns"`
	MaxIdleConns    int    `yaml:"max_idle_conns"`
	ConnMaxLifetime string `yaml:"conn_max_lifetime"`
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("config exceeds %d bytes", maxConfigBytes)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("config must contain a single YAML document")
		}
		return Config{}, fmt.Errorf("decode config trailer: %w", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:18080"
	}
	if cfg.Database.MaxOpenConns == 0 {
		cfg.Database.MaxOpenConns = 20
	}
	if cfg.Database.MaxIdleConns == 0 {
		cfg.Database.MaxIdleConns = min(5, cfg.Database.MaxOpenConns)
	}
	if cfg.Database.ConnMaxLifetime == "" {
		cfg.Database.ConnMaxLifetime = "5m"
	}
	if strings.TrimSpace(cfg.Listen) != cfg.Listen {
		return Config{}, fmt.Errorf("listen must be a valid host:port address")
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return Config{}, fmt.Errorf("listen must be a valid host:port address: %w", err)
	}
	if cfg.Database.Host == "" || cfg.Database.Name == "" || cfg.Database.User == "" {
		return Config{}, fmt.Errorf("database.host, database.name, and database.user are required")
	}
	if cfg.Database.PasswordEnv == "" {
		return Config{}, fmt.Errorf("database.password_env is required")
	}
	if cfg.Database.MaxOpenConns < 1 || cfg.Database.MaxIdleConns < 0 || cfg.Database.MaxIdleConns > cfg.Database.MaxOpenConns {
		return Config{}, fmt.Errorf("database connection limits are invalid")
	}
	if _, err := time.ParseDuration(cfg.Database.ConnMaxLifetime); err != nil {
		return Config{}, fmt.Errorf("database.conn_max_lifetime: %w", err)
	}
	if !cfg.TLS.RequireClientCert {
		return Config{}, fmt.Errorf("tls.require_client_cert must be true")
	}
	if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" || cfg.TLS.ClientCAFile == "" {
		return Config{}, fmt.Errorf("tls.cert_file, tls.key_file, and tls.client_ca_file are required")
	}
	if cfg.DeepFlow.Enabled {
		endpoint, err := url.ParseRequestURI(cfg.DeepFlow.Endpoint)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return Config{}, fmt.Errorf("deepflow.endpoint must be an HTTP(S) server URL without userinfo, query, or fragment")
		}
		if cfg.DeepFlow.Database == "" {
			cfg.DeepFlow.Database = "flow_log"
		}
		if cfg.DeepFlow.Table == "" {
			cfg.DeepFlow.Table = "l7_flow_log"
		}
		if cfg.DeepFlow.InstanceID == "" {
			cfg.DeepFlow.InstanceID = "deepflow-default"
		}
		if cfg.DeepFlow.Timeout == "" {
			cfg.DeepFlow.Timeout = "30s"
		}
		if cfg.DeepFlow.PageSize == 0 {
			cfg.DeepFlow.PageSize = 1000
		}
		if cfg.DeepFlow.MaxPages == 0 {
			cfg.DeepFlow.MaxPages = 100
		}
		if cfg.DeepFlow.SnapshotMaxAge == "" {
			cfg.DeepFlow.SnapshotMaxAge = "30s"
		}
		if cfg.DeepFlow.PageSize < 1 || cfg.DeepFlow.PageSize > 10000 || cfg.DeepFlow.MaxPages < 1 || cfg.DeepFlow.MaxPages > 1000 {
			return Config{}, fmt.Errorf("deepflow page_size or max_pages_per_window is out of range")
		}
		if duration, err := time.ParseDuration(cfg.DeepFlow.Timeout); err != nil || duration <= 0 {
			return Config{}, fmt.Errorf("deepflow.timeout must be a positive duration")
		}
		if duration, err := time.ParseDuration(cfg.DeepFlow.SnapshotMaxAge); err != nil || duration <= 0 {
			return Config{}, fmt.Errorf("deepflow.snapshot_max_age must be a positive duration")
		}
	}
	return cfg, nil
}
