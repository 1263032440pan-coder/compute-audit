package deepflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Event struct {
	RawEventID       string
	TenantID         string
	RequestStart     time.Time
	RequestEnd       time.Time
	SourceIP         string
	DestinationIP    string
	ObserverAgentID  string
	ServerPID        *uint32
	Method           string
	Path             string
	StatusCode       string
	DurationMS       *float64
	ObservationPoint string
	NormalizedSHA256 [32]byte
}

func PollWindow(ctx context.Context, client *Client, database, table string, profile FieldProfile, from, to time.Time, pageSize, maxPages int, consume func([]Event) error) (int, error) {
	if client == nil || consume == nil {
		return 0, fmt.Errorf("DeepFlow client and page consumer are required")
	}
	if err := profile.Validate(); err != nil {
		return 0, err
	}
	if !validIdentifier(table) {
		return 0, fmt.Errorf("DeepFlow table name contains unsupported characters")
	}
	if !to.After(from) {
		return 0, fmt.Errorf("DeepFlow query end must be after its start")
	}
	if pageSize < 1 || pageSize > 10000 || maxPages < 1 {
		return 0, fmt.Errorf("DeepFlow page bounds are invalid")
	}
	fromValue, err := timeToUnit(from, profile.TimeUnit)
	if err != nil {
		return 0, fmt.Errorf("convert query start: %w", err)
	}
	toValue, err := timeToUnit(to, profile.TimeUnit)
	if err != nil {
		return 0, fmt.Errorf("convert query end: %w", err)
	}

	fields := profile.selectedFields()
	selectParts := make([]string, 0, len(fields))
	for _, field := range fields {
		selectParts = append(selectParts, quoteField(field.field)+" AS `"+field.alias+"`")
	}
	whereParts := []string{
		quoteField(profile.EndTime) + " >= " + strconv.FormatInt(fromValue, 10),
		quoteField(profile.EndTime) + " < " + strconv.FormatInt(toValue, 10),
	}
	if profile.ServiceSideValue != "" {
		whereParts = append(whereParts, quoteField(profile.ObservationPoint)+" = '"+profile.ServiceSideValue+"'")
	}
	queryBase := "SELECT " + strings.Join(selectParts, ", ") + " FROM `" + table + "` WHERE " + strings.Join(whereParts, " AND ") + " ORDER BY " + quoteField(profile.EndTime) + ", " + quoteField(profile.EventID)

	total := 0
	for page := 0; page < maxPages; page++ {
		offset := page * pageSize
		statement := fmt.Sprintf("%s LIMIT %d OFFSET %d", queryBase, pageSize, offset)
		result, err := client.Query(ctx, database, statement)
		if err != nil {
			return total, fmt.Errorf("query DeepFlow page %d: %w", page, err)
		}
		events, err := normalizePage(result, fields, profile)
		if err != nil {
			return total, fmt.Errorf("normalize DeepFlow page %d: %w", page, err)
		}
		if err := consume(events); err != nil {
			return total, fmt.Errorf("persist DeepFlow page %d: %w", page, err)
		}
		total += len(events)
		if len(result.Rows) < pageSize {
			return total, nil
		}
	}
	return total, fmt.Errorf("DeepFlow page limit %d reached; window is incomplete", maxPages)
}

func normalizePage(result QueryResult, fields []selectedField, profile FieldProfile) ([]Event, error) {
	columns := make(map[string]int, len(result.Columns))
	for i, name := range result.Columns {
		columns[name] = i
	}
	for _, field := range fields {
		if _, ok := columns[field.alias]; !ok {
			return nil, fmt.Errorf("DeepFlow response omitted selected column %q", field.alias)
		}
	}
	events := make([]Event, 0, len(result.Rows))
	for rowIndex, row := range result.Rows {
		value := func(alias string) any {
			column, ok := columns[alias]
			if !ok || column >= len(row) {
				return nil
			}
			return row[column]
		}
		event, err := normalizeEvent(value, profile)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", rowIndex, err)
		}
		events = append(events, event)
	}
	return events, nil
}

func normalizeEvent(value func(string) any, profile FieldProfile) (Event, error) {
	event := Event{}
	var err error
	event.RawEventID, err = scalarString(value("raw_event_id"))
	if err != nil || event.RawEventID == "" || len(event.RawEventID) > 255 {
		return Event{}, fmt.Errorf("raw event ID is missing or exceeds 255 bytes")
	}
	event.RequestStart, err = parseTimestamp(value("request_start"), profile.TimeUnit)
	if err != nil {
		return Event{}, fmt.Errorf("invalid request start time: %w", err)
	}
	event.RequestEnd, err = parseTimestamp(value("request_end"), profile.TimeUnit)
	if err != nil || event.RequestEnd.Before(event.RequestStart) {
		return Event{}, fmt.Errorf("invalid request end time")
	}
	event.SourceIP, err = ipString(value("source_ip"))
	if err != nil {
		return Event{}, fmt.Errorf("invalid source IP: %w", err)
	}
	event.DestinationIP, err = ipString(value("destination_ip"))
	if err != nil {
		return Event{}, fmt.Errorf("invalid destination IP: %w", err)
	}
	if pidValue := value("server_pid"); pidValue != nil {
		pidText, parseErr := scalarString(pidValue)
		if parseErr != nil {
			return Event{}, fmt.Errorf("invalid server PID")
		}
		if strings.TrimSpace(pidText) != "" {
			pid, parseErr := strconv.ParseUint(strings.TrimSpace(pidText), 10, 32)
			if parseErr != nil {
				return Event{}, fmt.Errorf("invalid server PID")
			}
			if pid > 0 {
				pid32 := uint32(pid)
				event.ServerPID = &pid32
			}
		}
	}
	event.ObserverAgentID, err = optionalScalarString(value("observer_agent_id"), 191)
	if err != nil {
		return Event{}, fmt.Errorf("invalid observer agent ID: %w", err)
	}
	event.TenantID, err = optionalScalarString(value("tenant_id"), 128)
	if err != nil {
		return Event{}, fmt.Errorf("invalid tenant ID: %w", err)
	}
	event.Method, err = optionalScalarString(value("method"), 32)
	if err != nil {
		return Event{}, fmt.Errorf("invalid HTTP method: %w", err)
	}
	event.Path, err = normalizePath(value("path"))
	if err != nil {
		return Event{}, fmt.Errorf("invalid request path: %w", err)
	}
	event.StatusCode, err = optionalScalarString(value("status_code"), 32)
	if err != nil {
		return Event{}, fmt.Errorf("invalid response status: %w", err)
	}
	event.ObservationPoint, err = optionalScalarString(value("observation_point"), 64)
	if err != nil {
		return Event{}, fmt.Errorf("invalid observation point: %w", err)
	}
	if durationValue := value("duration"); durationValue != nil {
		durationText, parseErr := scalarString(durationValue)
		if parseErr != nil {
			return Event{}, fmt.Errorf("invalid response duration: %w", parseErr)
		}
		if strings.TrimSpace(durationText) != "" {
			duration, parseErr := durationMilliseconds(durationValue, profile.DurationUnit)
			if parseErr != nil {
				return Event{}, fmt.Errorf("invalid response duration: %w", parseErr)
			}
			event.DurationMS = &duration
		}
	}
	canonical, err := json.Marshal(struct {
		RawEventID       string
		TenantID         string
		RequestStart     time.Time
		RequestEnd       time.Time
		SourceIP         string
		DestinationIP    string
		ObserverAgentID  string
		ServerPID        *uint32
		Method           string
		Path             string
		StatusCode       string
		DurationMS       *float64
		ObservationPoint string
	}{event.RawEventID, event.TenantID, event.RequestStart, event.RequestEnd, event.SourceIP, event.DestinationIP, event.ObserverAgentID, event.ServerPID, event.Method, event.Path, event.StatusCode, event.DurationMS, event.ObservationPoint})
	if err != nil {
		return Event{}, fmt.Errorf("encode normalized event: %w", err)
	}
	event.NormalizedSHA256 = sha256.Sum256(canonical)
	return event, nil
}

func parseTimestamp(value any, unit string) (time.Time, error) {
	if value == nil {
		return time.Time{}, fmt.Errorf("timestamp is null")
	}
	if text, ok := value.(string); ok {
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err == nil {
			return parsed.UTC(), nil
		}
		number, numberErr := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if numberErr != nil {
			return time.Time{}, err
		}
		return timestampFromInt64(number, unit)
	}
	number, err := scalarInt64(value)
	if err != nil {
		return time.Time{}, err
	}
	return timestampFromInt64(number, unit)
}

func timestampFromInt64(number int64, unit string) (time.Time, error) {
	var seconds, nanoseconds int64
	switch unit {
	case "seconds":
		seconds = number
	case "milliseconds":
		seconds = number / 1000
		nanoseconds = number % 1000 * int64(time.Millisecond)
	case "microseconds":
		seconds = number / 1_000_000
		nanoseconds = number % 1_000_000 * int64(time.Microsecond)
	case "nanoseconds":
		seconds = number / 1_000_000_000
		nanoseconds = number % 1_000_000_000
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp unit %q", unit)
	}
	return time.Unix(seconds, nanoseconds).UTC(), nil
}

func timeToUnit(value time.Time, unit string) (int64, error) {
	value = value.UTC()
	switch unit {
	case "seconds":
		return value.Unix(), nil
	case "milliseconds":
		return value.UnixMilli(), nil
	case "microseconds":
		return value.UnixMicro(), nil
	case "nanoseconds":
		return value.UnixNano(), nil
	default:
		return 0, fmt.Errorf("unsupported timestamp unit %q", unit)
	}
}

func scalarString(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case json.Number:
		return typed.String(), nil
	case bool:
		return strconv.FormatBool(typed), nil
	case nil:
		return "", fmt.Errorf("value is null")
	default:
		return "", fmt.Errorf("unsupported scalar type %T", value)
	}
}

func optionalScalarString(value any, limit int) (string, error) {
	if value == nil {
		return "", nil
	}
	text, err := scalarString(value)
	if err != nil {
		return "", err
	}
	if len(text) > limit {
		return "", fmt.Errorf("value exceeds %d bytes", limit)
	}
	return text, nil
}

func scalarInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case json.Number:
		return typed.Int64()
	case string:
		return strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	default:
		return 0, fmt.Errorf("expected an integer, got %T", value)
	}
}

func ipString(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		if text == "" {
			return "", nil
		}
		ip := net.ParseIP(strings.TrimSpace(text))
		if ip == nil {
			return "", fmt.Errorf("%q is not an IP address", text)
		}
		return ip.String(), nil
	}
	if number, ok := value.(json.Number); ok {
		numeric, err := strconv.ParseUint(number.String(), 10, 32)
		if err != nil {
			return "", fmt.Errorf("numeric IPv4 value is out of range")
		}
		ip := net.IPv4(byte(numeric>>24), byte(numeric>>16), byte(numeric>>8), byte(numeric))
		return ip.String(), nil
	}
	return "", fmt.Errorf("unsupported IP value type %T", value)
}

func normalizePath(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	pathValue, err := scalarString(value)
	if err != nil {
		return "", err
	}
	if parsed, parseErr := url.Parse(pathValue); parseErr == nil && parsed.IsAbs() {
		pathValue = parsed.EscapedPath()
	} else {
		if index := strings.IndexAny(pathValue, "?#"); index >= 0 {
			pathValue = pathValue[:index]
		}
	}
	if len(pathValue) > 2048 {
		pathValue = pathValue[:2048]
		for !utf8.ValidString(pathValue) {
			pathValue = pathValue[:len(pathValue)-1]
		}
	}
	return pathValue, nil
}

func durationMilliseconds(value any, unit string) (float64, error) {
	text, err := scalarString(value)
	if err != nil {
		return 0, err
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, fmt.Errorf("invalid non-negative duration")
	}
	switch unit {
	case "seconds":
		number *= 1000
	case "milliseconds":
	case "microseconds":
		number /= 1000
	case "nanoseconds":
		number /= 1_000_000
	default:
		return 0, fmt.Errorf("unsupported duration unit %q", unit)
	}
	if math.IsInf(number, 0) {
		return 0, fmt.Errorf("duration overflows")
	}
	return number, nil
}
