package deepflow

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"compute-audit/internal/config"
)

const (
	maxSQLBytes      = 64 << 10
	maxResponseBytes = 32 << 20
	maxResultRows    = 100000
)

type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

type QueryResult struct {
	Columns []string
	Rows    [][]any
}

func New(cfg config.DeepFlowConfig) (*Client, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("DeepFlow integration is disabled")
	}
	parsed, err := url.ParseRequestURI(cfg.Endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("DeepFlow endpoint must be an HTTP(S) URL without userinfo, query, or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/v1/query") {
		parsed.Path += "/v1/query"
	}
	parsed.Path += "/"

	timeout := 30 * time.Second
	if cfg.Timeout != "" {
		timeout, err = time.ParseDuration(cfg.Timeout)
		if err != nil || timeout <= 0 {
			return nil, fmt.Errorf("DeepFlow timeout must be a positive duration")
		}
	}
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConnsPerHost: 2,
	}
	if cfg.CAFile != "" {
		caPEM, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read DeepFlow CA file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("DeepFlow CA file contains no certificates")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	var token string
	if cfg.TokenEnv != "" {
		token, _ = os.LookupEnv(cfg.TokenEnv)
		if token == "" {
			return nil, fmt.Errorf("DeepFlow token environment variable %q is empty or unset", cfg.TokenEnv)
		}
	}
	return &Client{
		endpoint: parsed.String(),
		token:    token,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Client) Query(ctx context.Context, database, statement string) (QueryResult, error) {
	if database == "" {
		return QueryResult{}, fmt.Errorf("DeepFlow database is required")
	}
	if statement == "" || len(statement) > maxSQLBytes {
		return QueryResult{}, fmt.Errorf("DeepFlow SQL must be between 1 and %d bytes", maxSQLBytes)
	}
	form := url.Values{}
	form.Set("db", database)
	form.Set("sql", statement)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return QueryResult{}, fmt.Errorf("create DeepFlow query request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return QueryResult{}, fmt.Errorf("send DeepFlow query: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return QueryResult{}, fmt.Errorf("read DeepFlow query response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return QueryResult{}, fmt.Errorf("DeepFlow query response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		return QueryResult{}, fmt.Errorf("DeepFlow query returned HTTP %d", response.StatusCode)
	}
	return decodeResult(body)
}

func decodeResult(body []byte) (QueryResult, error) {
	var envelope struct {
		Status      string `json:"OPT_STATUS"`
		Description string `json:"DESCRIPTION"`
		Result      struct {
			Columns []string            `json:"columns"`
			Values  [][]json.RawMessage `json:"values"`
		} `json:"result"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil {
		return QueryResult{}, fmt.Errorf("decode DeepFlow query response: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return QueryResult{}, fmt.Errorf("DeepFlow response contains multiple JSON values")
		}
		return QueryResult{}, fmt.Errorf("decode DeepFlow response trailer: %w", err)
	}
	if envelope.Status != "SUCCESS" {
		return QueryResult{}, fmt.Errorf("DeepFlow query status is %q", envelope.Status)
	}
	if len(envelope.Result.Values) > maxResultRows {
		return QueryResult{}, fmt.Errorf("DeepFlow result contains more than %d rows", maxResultRows)
	}
	result := QueryResult{Columns: envelope.Result.Columns, Rows: make([][]any, 0, len(envelope.Result.Values))}
	for i, rawRow := range envelope.Result.Values {
		if len(rawRow) != len(result.Columns) {
			return QueryResult{}, fmt.Errorf("DeepFlow result row %d has %d values for %d columns", i, len(rawRow), len(result.Columns))
		}
		row := make([]any, len(rawRow))
		for j, raw := range rawRow {
			cellDecoder := json.NewDecoder(bytes.NewReader(raw))
			cellDecoder.UseNumber()
			if err := cellDecoder.Decode(&row[j]); err != nil {
				return QueryResult{}, fmt.Errorf("decode DeepFlow result cell [%d,%d]: %w", i, j, err)
			}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func (c *Client) DiscoverSchema(ctx context.Context, database, table string) (map[string]QueryResult, error) {
	if !validIdentifier(table) {
		return nil, fmt.Errorf("DeepFlow table name contains unsupported characters")
	}
	tags, err := c.Query(ctx, database, "SHOW TAGS FROM "+table)
	if err != nil {
		return nil, fmt.Errorf("discover DeepFlow tags: %w", err)
	}
	metrics, err := c.Query(ctx, database, "SHOW METRICS FROM "+table)
	if err != nil {
		return nil, fmt.Errorf("discover DeepFlow metrics: %w", err)
	}
	return map[string]QueryResult{"tags": tags, "metrics": metrics}, nil
}

func validIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
