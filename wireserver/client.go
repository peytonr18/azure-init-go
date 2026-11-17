// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package wireserver reports provisioning health directly to Azure using JSON.
// It never opens or changes KVP pool files. Callers publish KVP independently.
package wireserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/peytonr18/azure-init-go/kvp"
)

const DefaultHealthEndpoint = "http://168.63.129.16/provisioning/health"

// Config contains only HTTP health settings, not azure-init agent configuration.
// Start with DefaultConfig. HTTPClient, if supplied, owns connection management;
// request and total timeouts still apply. Configuration is copied by NewClient.
type Config struct {
	HealthEndpoint    string
	AgentName         string
	ConnectionTimeout time.Duration
	RequestTimeout    time.Duration
	RetryInterval     time.Duration
	TotalRetryTimeout time.Duration
	HTTPClient        *http.Client
}

func DefaultConfig() Config {
	return Config{
		HealthEndpoint: DefaultHealthEndpoint, AgentName: "azure-init-go/0.1.0",
		ConnectionTimeout: time.Minute, RequestTimeout: time.Minute,
		RetryInterval: time.Minute, TotalRetryTimeout: 20 * time.Minute,
	}
}

// Client is reusable and safe for concurrent calls, like http.Client.
type Client struct {
	config  Config
	http    *http.Client
	request *http.Request
}

func NewClient(config Config) (*Client, error) {
	request, err := http.NewRequest(http.MethodPost, config.HealthEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid WireServer health endpoint %q: %w", config.HealthEndpoint, err)
	}
	endpoint := request.URL
	if endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Fragment != "" {
		return nil, fmt.Errorf("invalid WireServer health endpoint %q", config.HealthEndpoint)
	}
	if config.AgentName == "" {
		return nil, fmt.Errorf("WireServer agent name must not be empty")
	}
	for _, c := range config.AgentName {
		if c < ' ' || c > '~' {
			return nil, fmt.Errorf("WireServer agent name must contain printable ASCII only")
		}
	}
	if config.ConnectionTimeout <= 0 || config.RequestTimeout <= 0 || config.RetryInterval <= 0 || config.TotalRetryTimeout < 0 {
		return nil, fmt.Errorf("WireServer connection/request/retry intervals must be positive and total retry timeout nonnegative")
	}
	client := config.HTTPClient
	if client == nil {
		transport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout: config.ConnectionTimeout, KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2: true, MaxIdleConns: 100,
			IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
		client = &http.Client{Transport: transport}
	}
	request.Header.Set("User-Agent", config.AgentName)
	request.Header.Set("x-ms-guest-agent-name", config.AgentName)
	request.Header.Set("Content-Type", "application/json")
	return &Client{config: config, http: client, request: request}, nil
}

// HTTPStatusError is a non-retryable response from the health endpoint.
type HTTPStatusError struct {
	Endpoint   string
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("WireServer %s returned HTTP %d", e.Endpoint, e.StatusCode)
}

// DeliveryError preserves the last transport or retryable HTTP failure as well
// as cancellation/deadline classification through errors.Is.
type DeliveryError struct {
	Cause     error
	LastError error
}

func (e *DeliveryError) Error() string {
	if e.LastError != nil {
		return fmt.Sprintf("WireServer health delivery: %v (last failure: %v)", e.Cause, e.LastError)
	}
	return fmt.Sprintf("WireServer health delivery: %v", e.Cause)
}

func (e *DeliveryError) Unwrap() error { return e.Cause }

func (c *Client) ReportReady(ctx context.Context) error {
	return c.report(ctx, "Ready", "", "")
}

// ReportFailure sends the supplied report's encoding without changing its
// timestamp, reason, or context, and without publishing it to KVP.
func (c *Client) ReportFailure(ctx context.Context, report kvp.ProvisioningReport) error {
	value, err := report.Encode()
	if err != nil {
		return err
	}
	return c.report(ctx, "NotReady", "ProvisioningFailed", value)
}

func (c *Client) ReportInProgress(ctx context.Context, vmID string) error {
	if !utf8.ValidString(vmID) {
		return kvp.ErrInvalidUTF8
	}
	return c.report(ctx, "NotReady", "Provisioning", fmt.Sprintf("Provisioning is still in progress for vm_id=%s.", vmID))
}

func (c *Client) report(ctx context.Context, state, subStatus, description string) error {
	type details struct {
		SubStatus   string `json:"subStatus"`
		Description string `json:"description"`
	}
	payload := struct {
		State   string   `json:"state"`
		Details *details `json:"details,omitempty"`
	}{State: state}
	if subStatus != "" {
		payload.Details = &details{subStatus, description}
	}
	// This closed, acyclic type contains only builtin strings, so JSON encoding
	// cannot return an error (there are no custom marshalers or unsupported types).
	body, _ := json.Marshal(payload)
	total, cancel := context.WithTimeout(ctx, c.config.TotalRetryTimeout)
	defer cancel()
	var lastError error
	for {
		if err := total.Err(); err != nil {
			return &DeliveryError{Cause: err, LastError: lastError}
		}
		attempt, cancelAttempt := context.WithTimeout(total, c.config.RequestTimeout)
		request := c.request.Clone(attempt)
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
		request.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		response, requestErr := c.http.Do(request)
		if requestErr != nil {
			lastError = requestErr
		} else {
			status := response.StatusCode
			closeErr := response.Body.Close()
			cancelAttempt()
			if closeErr != nil {
				return fmt.Errorf("close WireServer response: %w", closeErr)
			}
			if status >= 200 && status < 300 {
				return nil
			}
			lastError = &HTTPStatusError{Endpoint: c.config.HealthEndpoint, StatusCode: status}
			if !retryable(status) {
				return lastError
			}
		}
		cancelAttempt()
		timer := time.NewTimer(c.config.RetryInterval)
		select {
		case <-total.Done():
			timer.Stop()
			return &DeliveryError{Cause: total.Err(), LastError: lastError}
		case <-timer.C:
		}
	}
}

func retryable(status int) bool {
	switch status {
	case 400, 404, 410, 429, 500, 503:
		return true
	default:
		return false
	}
}

// CloseIdleConnections releases idle sockets owned by the client's transport.
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }
