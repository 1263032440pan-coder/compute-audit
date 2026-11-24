package ascend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

const maxMetricsBodyBytes = 16 << 20

type Binding struct {
	DeviceKey   string
	PID         int
	MemoryBytes *uint64
}

type Result struct {
	Bindings        []Binding
	SourceTimeKnown bool
}

type Mapping struct {
	MetricName      string
	PIDLabel        string
	PIDFromValue    bool
	DeviceLabels    []string
	MemoryLabel     string
	MemoryFromValue bool
	MemoryUnit      string
	SourceTimeKnown bool
}

func Collect(ctx context.Context, hostID, metricsURL string, mapping Mapping, timeout time.Duration, client *http.Client) (Result, error) {
	parsedURL, err := url.ParseRequestURI(metricsURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return Result{}, fmt.Errorf("ascend metrics_url must be an HTTP or HTTPS URL")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, metricsURL, nil)
	if err != nil {
		return Result{}, fmt.Errorf("create NPU metrics request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("fetch NPU metrics: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("NPU metrics endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetricsBodyBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read NPU metrics: %w", err)
	}
	if len(body) > maxMetricsBodyBytes {
		return Result{}, fmt.Errorf("NPU metrics response exceeds %d bytes", maxMetricsBodyBytes)
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return Result{}, fmt.Errorf("parse NPU Prometheus metrics: %w", err)
	}
	family := families[mapping.MetricName]
	if family == nil {
		return Result{}, fmt.Errorf("NPU process metric %q was not found", mapping.MetricName)
	}

	var result Result
	result.SourceTimeKnown = mapping.SourceTimeKnown
	var rowErr error
	seen := make(map[string]struct{})
	for i, metric := range family.Metric {
		labels := make(map[string]string, len(metric.Label))
		for _, pair := range metric.Label {
			labels[pair.GetName()] = pair.GetValue()
		}
		pidValue := ""
		var valueErr error
		if mapping.PIDFromValue {
			pidValue, valueErr = metricValueString(family.GetType(), metric)
		} else {
			pidValue = labels[mapping.PIDLabel]
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(pidValue))
		if valueErr != nil || parseErr != nil || pid <= 0 {
			rowErr = fmt.Errorf("NPU metric sample %d has invalid PID mapping", i)
			continue
		}

		deviceParts := make([]string, 0, len(mapping.DeviceLabels))
		validDevice := true
		for _, labelName := range mapping.DeviceLabels {
			value := strings.TrimSpace(labels[labelName])
			if value == "" {
				validDevice = false
				break
			}
			deviceParts = append(deviceParts, labelName+"="+value)
		}
		if !validDevice {
			rowErr = fmt.Errorf("NPU metric sample %d is missing a configured device label", i)
			continue
		}
		deviceKey := "ascend:" + hostID + ":" + strings.Join(deviceParts, ",")
		if len(deviceKey) > 191 {
			digest := sha256.Sum256([]byte(deviceKey))
			deviceKey = "ascend:" + hex.EncodeToString(digest[:])
		}

		var memoryBytes *uint64
		if mapping.MemoryFromValue || mapping.MemoryLabel != "" {
			memoryText := ""
			if mapping.MemoryFromValue {
				memoryText, err = metricValueString(family.GetType(), metric)
			} else {
				memoryText = labels[mapping.MemoryLabel]
			}
			if err != nil {
				rowErr = fmt.Errorf("NPU metric sample %d has invalid memory value", i)
				continue
			}
			if memoryText != "" {
				value, parseErr := bytesFromUnit(memoryText, mapping.MemoryUnit)
				if parseErr != nil {
					rowErr = fmt.Errorf("NPU metric sample %d memory: %w", i, parseErr)
					continue
				}
				memoryBytes = &value
			}
		}
		key := fmt.Sprintf("%s/%d", deviceKey, pid)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result.Bindings = append(result.Bindings, Binding{DeviceKey: deviceKey, PID: pid, MemoryBytes: memoryBytes})
	}
	return result, rowErr
}

func metricValueString(metricType dto.MetricType, metric *dto.Metric) (string, error) {
	var value float64
	switch metricType {
	case dto.MetricType_GAUGE:
		if metric.Gauge == nil {
			return "", fmt.Errorf("gauge value is missing")
		}
		value = metric.Gauge.GetValue()
	case dto.MetricType_COUNTER:
		if metric.Counter == nil {
			return "", fmt.Errorf("counter value is missing")
		}
		value = metric.Counter.GetValue()
	case dto.MetricType_UNTYPED:
		if metric.Untyped == nil {
			return "", fmt.Errorf("untyped value is missing")
		}
		value = metric.Untyped.GetValue()
	default:
		return "", fmt.Errorf("unsupported metric type %s", metricType.String())
	}
	return strconv.FormatFloat(value, 'f', -1, 64), nil
}

func bytesFromUnit(raw, unit string) (uint64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, fmt.Errorf("invalid non-negative number %q", raw)
	}
	multiplier := float64(1)
	switch unit {
	case "B":
	case "KiB":
		multiplier = 1 << 10
	case "MiB":
		multiplier = 1 << 20
	case "GiB":
		multiplier = 1 << 30
	default:
		return 0, fmt.Errorf("unsupported unit %q", unit)
	}
	bytes := value * multiplier
	if bytes >= math.Ldexp(1, 64) {
		return 0, fmt.Errorf("value overflows byte count")
	}
	return uint64(math.Round(bytes)), nil
}
