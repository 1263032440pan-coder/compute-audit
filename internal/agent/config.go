package agent

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maxConfigBytes = 1 << 20

type Config struct {
	HostID   string       `yaml:"host_id"`
	Vendor   string       `yaml:"vendor"`
	Interval string       `yaml:"interval"`
	Report   ReportConfig `yaml:"report"`
	Spool    SpoolConfig  `yaml:"spool"`
	NVIDIA   NVIDIAConfig `yaml:"nvidia"`
	Ascend   AscendConfig `yaml:"ascend"`
}

type ReportConfig struct {
	URL      string `yaml:"url"`
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type SpoolConfig struct {
	Directory string `yaml:"directory"`
	MaxBytes  int64  `yaml:"max_bytes"`
}

type NVIDIAConfig struct {
	SmiPath string `yaml:"smi_path"`
	Timeout string `yaml:"timeout"`
}

type AscendConfig struct {
	MetricsURL      string   `yaml:"metrics_url"`
	MetricName      string   `yaml:"metric_name"`
	PIDLabel        string   `yaml:"pid_label"`
	PIDFromValue    bool     `yaml:"pid_from_value"`
	DeviceLabels    []string `yaml:"device_labels"`
	MemoryLabel     string   `yaml:"memory_label"`
	MemoryFromValue bool     `yaml:"memory_from_value"`
	MemoryUnit      string   `yaml:"memory_unit"`
	SourceTimeKnown bool     `yaml:"source_time_known"`
	Timeout         string   `yaml:"timeout"`
}

func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open agent config: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read agent config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("agent config exceeds %d bytes", maxConfigBytes)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode agent config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("agent config must contain one YAML document")
		}
		return Config{}, fmt.Errorf("decode agent config trailer: %w", err)
	}

	if cfg.HostID == "" || len(cfg.HostID) > 128 || strings.TrimSpace(cfg.HostID) != cfg.HostID {
		return Config{}, fmt.Errorf("host_id is required and must be at most 128 characters")
	}
	if cfg.Vendor != "nvidia" && cfg.Vendor != "ascend" && cfg.Vendor != "mixed" {
		return Config{}, fmt.Errorf("vendor must be nvidia, ascend, or mixed")
	}
	if cfg.Interval == "" {
		cfg.Interval = "10s"
	}
	if duration, err := time.ParseDuration(cfg.Interval); err != nil || duration <= 0 {
		return Config{}, fmt.Errorf("interval must be a positive duration")
	}
	if cfg.Report.URL == "" || cfg.Report.CAFile == "" || cfg.Report.CertFile == "" || cfg.Report.KeyFile == "" {
		return Config{}, fmt.Errorf("report.url, ca_file, cert_file, and key_file are required")
	}
	reportURL, err := url.ParseRequestURI(cfg.Report.URL)
	if err != nil || reportURL.Scheme != "https" || reportURL.Host == "" {
		return Config{}, fmt.Errorf("report.url must be a valid HTTPS URL")
	}
	if cfg.Spool.Directory == "" {
		cfg.Spool.Directory = "/var/lib/compute-audit/spool"
	}
	if cfg.Spool.MaxBytes == 0 {
		cfg.Spool.MaxBytes = 512 << 20
	}
	if cfg.Spool.MaxBytes < 1<<20 {
		return Config{}, fmt.Errorf("spool.max_bytes must be at least 1 MiB")
	}
	if cfg.NVIDIA.SmiPath == "" {
		cfg.NVIDIA.SmiPath = "nvidia-smi"
	}
	if cfg.NVIDIA.Timeout == "" {
		cfg.NVIDIA.Timeout = "5s"
	}
	if duration, err := time.ParseDuration(cfg.NVIDIA.Timeout); err != nil || duration <= 0 {
		return Config{}, fmt.Errorf("nvidia.timeout must be a positive duration")
	}
	if cfg.Ascend.Timeout == "" {
		cfg.Ascend.Timeout = "5s"
	}
	if duration, err := time.ParseDuration(cfg.Ascend.Timeout); err != nil || duration <= 0 {
		return Config{}, fmt.Errorf("ascend.timeout must be a positive duration")
	}
	if cfg.Vendor == "ascend" || cfg.Vendor == "mixed" {
		if cfg.Ascend.MetricsURL == "" || cfg.Ascend.MetricName == "" {
			return Config{}, fmt.Errorf("ascend.metrics_url and ascend.metric_name are required for this vendor")
		}
		if !cfg.Ascend.PIDFromValue && cfg.Ascend.PIDLabel == "" {
			return Config{}, fmt.Errorf("ascend.pid_label is required unless pid_from_value is true")
		}
		if cfg.Ascend.PIDFromValue && cfg.Ascend.MemoryFromValue {
			return Config{}, fmt.Errorf("ascend.pid_from_value and memory_from_value cannot both be true for the same metric")
		}
		if len(cfg.Ascend.DeviceLabels) == 0 {
			return Config{}, fmt.Errorf("ascend.device_labels must map at least one device identity label")
		}
		if cfg.Ascend.MemoryFromValue || cfg.Ascend.MemoryLabel != "" {
			switch cfg.Ascend.MemoryUnit {
			case "B", "KiB", "MiB", "GiB":
			default:
				return Config{}, fmt.Errorf("ascend.memory_unit must be B, KiB, MiB, or GiB when memory is configured")
			}
		}
	}
	return cfg, nil
}
