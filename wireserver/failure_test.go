// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package wireserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peytonr18/azure-init-go/kvp"
)

type closeFailure struct{ io.Reader }

func (closeFailure) Close() error { return io.ErrClosedPipe }

func TestResponseCloseAndInvalidInputs(t *testing.T) {
	config := DefaultConfig()
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: closeFailure{strings.NewReader("")}}, nil
	})}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReportReady(context.Background()); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if err := client.ReportFailure(context.Background(), kvp.ProvisioningReport{}); !errors.Is(err, kvp.Malformed) {
		t.Fatal(err)
	}
	if err := client.ReportInProgress(context.Background(), string([]byte{0xff})); !errors.Is(err, kvp.ErrInvalidUTF8) {
		t.Fatal(err)
	}
	config.HealthEndpoint = "http://host/%invalid"
	if _, err := NewClient(config); err == nil {
		t.Fatal("invalid request URL accepted")
	}
}

func TestDeliveryErrorFormatting(t *testing.T) {
	status := &HTTPStatusError{Endpoint: "http://example.test/health", StatusCode: 403}
	if status.Error() != "WireServer http://example.test/health returned HTTP 403" {
		t.Fatal(status.Error())
	}
	for _, delivery := range []*DeliveryError{
		{Cause: context.Canceled}, {Cause: context.DeadlineExceeded, LastError: status},
	} {
		if !strings.Contains(delivery.Error(), delivery.Cause.Error()) || !errors.Is(delivery, delivery.Cause) {
			t.Fatal(delivery)
		}
		if delivery.LastError != nil && !strings.Contains(delivery.Error(), status.Error()) {
			t.Fatal(delivery)
		}
	}
}

func TestRedirectReplaysExactJSONBody(t *testing.T) {
	var first string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/first" {
			first = string(data)
			w.Header().Set("Location", "/second")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if r.Method != http.MethodPost || string(data) != first || r.Header.Get("x-ms-guest-agent-name") != "redirect-test" {
			t.Errorf("redirect changed request: %s %s", r.Method, data)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	config := DefaultConfig()
	config.HealthEndpoint, config.AgentName = server.URL+"/first", "redirect-test"
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if err := client.ReportInProgress(context.Background(), "vm"); err != nil {
		t.Fatal(err)
	}
}
