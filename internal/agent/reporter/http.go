package reporter

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
	"os"
	"time"

	"compute-audit/internal/contract"
)

const maxAckBytes = 64 << 10

type PermanentError struct {
	StatusCode int
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("snapshot rejected with HTTP %d", e.StatusCode)
}

type Client struct {
	url  string
	http *http.Client
}

func New(url, caFile, certFile, keyFile string) (*Client, error) {
	clientCertificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load agent client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read audit-server CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("audit-server CA file contains no certificates")
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      roots,
			Certificates: []tls.Certificate{clientCertificate},
		},
		MaxIdleConnsPerHost: 1,
	}
	return &Client{
		url: url,
		http: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Client) Send(ctx context.Context, batch contract.SnapshotBatch, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create snapshot request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", batch.BatchID)
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("send snapshot: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxAckBytes+1))
	if err != nil {
		return fmt.Errorf("read snapshot response: %w", err)
	}
	if len(responseBody) > maxAckBytes {
		return fmt.Errorf("snapshot response exceeds %d bytes", maxAckBytes)
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return fmt.Errorf("temporary snapshot endpoint response: HTTP %d", response.StatusCode)
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 {
		return &PermanentError{StatusCode: response.StatusCode}
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected snapshot endpoint response: HTTP %d", response.StatusCode)
	}
	var ack struct {
		BatchID  string `json:"batch_id"`
		Accepted bool   `json:"accepted"`
	}
	if err := json.Unmarshal(responseBody, &ack); err != nil {
		return fmt.Errorf("decode snapshot ACK: %w", err)
	}
	if !ack.Accepted || ack.BatchID != batch.BatchID {
		return errors.New("snapshot ACK does not match submitted batch")
	}
	return nil
}
