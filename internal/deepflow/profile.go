package deepflow

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

const maxProfileBytes = 1 << 20

var sqlIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
var serviceSideValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

type FieldProfile struct {
	EventID          string `yaml:"event_id"`
	StartTime        string `yaml:"start_time"`
	EndTime          string `yaml:"end_time"`
	SourceIP         string `yaml:"source_ip"`
	DestinationIP    string `yaml:"destination_ip"`
	ServerPID        string `yaml:"server_pid"`
	ObserverAgentID  string `yaml:"observer_agent_id"`
	TenantID         string `yaml:"tenant_id"`
	Method           string `yaml:"method"`
	Path             string `yaml:"path"`
	StatusCode       string `yaml:"status_code"`
	Duration         string `yaml:"duration"`
	ObservationPoint string `yaml:"observation_point"`
	ServiceSideValue string `yaml:"service_side_value"`
	TimeUnit         string `yaml:"time_unit"`
	DurationUnit     string `yaml:"duration_unit"`
}

func LoadProfile(path string) (FieldProfile, error) {
	file, err := os.Open(path)
	if err != nil {
		return FieldProfile{}, fmt.Errorf("open DeepFlow field profile: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxProfileBytes+1))
	if err != nil {
		return FieldProfile{}, fmt.Errorf("read DeepFlow field profile: %w", err)
	}
	if len(data) > maxProfileBytes {
		return FieldProfile{}, fmt.Errorf("DeepFlow field profile exceeds %d bytes", maxProfileBytes)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var profile FieldProfile
	if err := decoder.Decode(&profile); err != nil {
		return FieldProfile{}, fmt.Errorf("decode DeepFlow field profile: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return FieldProfile{}, fmt.Errorf("DeepFlow field profile must contain one YAML document")
		}
		return FieldProfile{}, fmt.Errorf("decode DeepFlow field profile trailer: %w", err)
	}
	if err := profile.Validate(); err != nil {
		return FieldProfile{}, err
	}
	return profile, nil
}

func (p FieldProfile) Validate() error {
	for name, value := range map[string]string{
		"event_id":          p.EventID,
		"start_time":        p.StartTime,
		"end_time":          p.EndTime,
		"source_ip":         p.SourceIP,
		"destination_ip":    p.DestinationIP,
		"server_pid":        p.ServerPID,
		"observer_agent_id": p.ObserverAgentID,
		"tenant_id":         p.TenantID,
		"method":            p.Method,
		"path":              p.Path,
		"status_code":       p.StatusCode,
		"duration":          p.Duration,
		"observation_point": p.ObservationPoint,
	} {
		if value != "" && !sqlIdentifier.MatchString(value) {
			return fmt.Errorf("DeepFlow profile field %s contains unsupported SQL identifier characters", name)
		}
	}
	if p.EventID == "" || p.StartTime == "" || p.EndTime == "" || p.TimeUnit == "" {
		return fmt.Errorf("DeepFlow profile requires event_id, start_time, end_time, and time_unit")
	}
	if p.ServiceSideValue != "" && (p.ObservationPoint == "" || !serviceSideValuePattern.MatchString(p.ServiceSideValue)) {
		return fmt.Errorf("service_side_value requires observation_point and a simple verified enum value")
	}
	if p.TimeUnit != "seconds" && p.TimeUnit != "milliseconds" && p.TimeUnit != "microseconds" && p.TimeUnit != "nanoseconds" {
		return fmt.Errorf("DeepFlow profile time_unit must be seconds, milliseconds, microseconds, or nanoseconds")
	}
	if p.Duration != "" {
		switch p.DurationUnit {
		case "seconds", "milliseconds", "microseconds", "nanoseconds":
		default:
			return fmt.Errorf("DeepFlow profile duration_unit is required and must be seconds, milliseconds, microseconds, or nanoseconds")
		}
	}
	return nil
}

func (p FieldProfile) selectedFields() []selectedField {
	values := []struct {
		alias string
		field string
	}{
		{"raw_event_id", p.EventID},
		{"request_start", p.StartTime},
		{"request_end", p.EndTime},
		{"source_ip", p.SourceIP},
		{"destination_ip", p.DestinationIP},
		{"server_pid", p.ServerPID},
		{"observer_agent_id", p.ObserverAgentID},
		{"tenant_id", p.TenantID},
		{"method", p.Method},
		{"path", p.Path},
		{"status_code", p.StatusCode},
		{"duration", p.Duration},
		{"observation_point", p.ObservationPoint},
	}
	fields := make([]selectedField, 0, len(values))
	for _, value := range values {
		if value.field != "" {
			fields = append(fields, selectedField{alias: value.alias, field: value.field})
		}
	}
	return fields
}

type selectedField struct {
	alias string
	field string
}

func quoteField(field string) string {
	return "`" + field + "`"
}
