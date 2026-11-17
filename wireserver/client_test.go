// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package wireserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peytonr18/azure-init-go/kvp"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *int32) {
	t.Helper()
	var count int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	config := DefaultConfig()
	config.HealthEndpoint = server.URL + "/provisioning/health"
	config.ConnectionTimeout = 2 * time.Second
	config.RequestTimeout = 2 * time.Second
	config.RetryInterval = time.Millisecond
	config.TotalRetryTimeout = 5 * time.Second
	config.AgentName = "consumer/1.2.3"
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client, &count
}

func TestJSONRequestContractsAndIndependence(t *testing.T) {
	store, err := kvp.NewStoreIn(kvp.Guest, t.TempDir(), kvp.Safe)
	if err != nil {
		t.Fatal(err)
	}
	report := kvp.NewFailureReport("consumer", "vm", "reason|details", kvp.PPSNone).WithExtra("build", "1")
	report.Timestamp = "2026-06-17T00:00:00+00:00"
	encoded, err := report.Encode()
	if err != nil {
		t.Fatal(err)
	}
	wants := []map[string]interface{}{
		{"state": "Ready"},
		{"state": "NotReady", "details": map[string]interface{}{"subStatus": "ProvisioningFailed", "description": encoded}},
		{"state": "NotReady", "details": map[string]interface{}{"subStatus": "Provisioning", "description": "Provisioning is still in progress for vm_id=vm."}},
	}
	var next int
	client, count := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/provisioning/health" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		for header, want := range map[string]string{
			"User-Agent": "consumer/1.2.3", "x-ms-guest-agent-name": "consumer/1.2.3", "Content-Type": "application/json",
		} {
			if r.Header.Get(header) != want {
				t.Errorf("%s = %q", header, r.Header.Get(header))
			}
		}
		var got map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if next >= len(wants) || !reflect.DeepEqual(got, wants[next]) {
			t.Errorf("body %v, index %d", got, next)
		}
		next++
		w.WriteHeader(http.StatusCreated)
	})
	for _, send := range []func() error{
		func() error { return client.ReportReady(context.Background()) },
		func() error { return client.ReportFailure(context.Background(), report) },
		func() error { return client.ReportInProgress(context.Background(), "vm") },
	} {
		if err := send(); err != nil {
			t.Fatal(err)
		}
	}
	if atomic.LoadInt32(count) != 3 {
		t.Fatal("unexpected retries")
	}
	if report.Timestamp != "2026-06-17T00:00:00+00:00" {
		t.Fatal("failure report mutated")
	}
	records, err := store.Dump()
	if err != nil || len(records) != 0 {
		t.Fatalf("HTTP reporting changed KVP: %v %v", records, err)
	}
	if err := kvp.WriteReport(store, report); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(count) != 3 {
		t.Fatal("KVP publication sent HTTP")
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	for _, test := range []struct {
		status int
		retry  bool
	}{
		{200, false}, {201, false}, {202, false}, {204, false},
		{400, true}, {404, true}, {410, true}, {429, true}, {500, true}, {503, true},
		{301, false}, {401, false}, {403, false}, {405, false}, {418, false}, {501, false},
	} {
		status := test.status
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var responses int32
			client, count := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				attempt := atomic.AddInt32(&responses, 1)
				if test.retry && attempt > 1 {
					w.WriteHeader(http.StatusNoContent)
				} else {
					w.WriteHeader(status)
				}
			})
			err := client.ReportReady(context.Background())
			attempts := atomic.LoadInt32(count)
			switch {
			case status >= 200 && status < 300:
				if err != nil || attempts != 1 {
					t.Fatalf("success: %v attempts=%d", err, attempts)
				}
			case test.retry:
				if err != nil || attempts != 2 {
					t.Fatalf("retry: %v attempts=%d", err, attempts)
				}
			default:
				var statusError *HTTPStatusError
				if !errors.As(err, &statusError) || statusError.StatusCode != status || attempts != 1 {
					t.Fatalf("fatal: %v attempts=%d", err, attempts)
				}
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportFailureRetainsLastError(t *testing.T) {
	sentinel := errors.New("connection failed")
	var count int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := DefaultConfig()
	config.RetryInterval = time.Millisecond
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if atomic.AddInt32(&count, 1) == 2 {
			cancel()
		}
		return nil, sentinel
	})}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	err = client.ReportReady(ctx)
	var delivery *DeliveryError
	if !errors.As(err, &delivery) || !errors.Is(err, context.Canceled) ||
		!errors.Is(delivery.LastError, sentinel) || atomic.LoadInt32(&count) != 2 {
		t.Fatalf("transport retry lost its cause: %v", err)
	}
}

func TestCancellationInterruptsRetryWait(t *testing.T) {
	called := make(chan struct{}, 1)
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		called <- struct{}{}
	})
	client.config.RetryInterval, client.config.TotalRetryTimeout = time.Minute, time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- client.ReportReady(ctx) }()
	<-called
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt retry wait")
	}
}

func TestRetryThenSuccessReusesBody(t *testing.T) {
	var calls int
	var first string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		calls++
		if calls == 1 {
			first = string(data)
			w.WriteHeader(503)
		} else {
			if first != string(data) {
				t.Error("retry changed body")
			}
			w.WriteHeader(204)
		}
	})
	if err := client.ReportInProgress(context.Background(), "vm"); err != nil || calls != 2 {
		t.Fatalf("%v calls=%d", err, calls)
	}
}

func TestCancellationAndRequestTimeout(t *testing.T) {
	firstTimedOut := make(chan struct{})
	secondArrived := make(chan struct{})
	var received int32
	client, count := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&received, 1)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
		}
		if attempt == 2 {
			close(secondArrived)
		}
		<-r.Context().Done()
		if attempt == 1 {
			close(firstTimedOut)
		}
	})
	client.config.RequestTimeout = 100 * time.Millisecond
	client.config.TotalRetryTimeout = 15 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- client.ReportReady(ctx) }()
	for _, observed := range []<-chan struct{}{firstTimedOut, secondArrived} {
		select {
		case <-observed:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("per-request timeout did not cancel and retry the request")
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || atomic.LoadInt32(count) < 2 {
			t.Fatalf("request timeouts did not retry before cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller cancellation did not stop delivery")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	before := atomic.LoadInt32(count)
	if err := client.ReportReady(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if atomic.LoadInt32(count) != before {
		t.Fatal("canceled request made a network call")
	}
}

func TestConfigValidationAndZeroBudget(t *testing.T) {
	for _, modify := range []func(*Config){
		func(c *Config) { c.HealthEndpoint = "invalid" },
		func(c *Config) { c.HealthEndpoint = "ftp://host" },
		func(c *Config) { c.AgentName = "bad\r\nheader" },
		func(c *Config) { c.AgentName = "" },
		func(c *Config) { c.RequestTimeout = -1 },
		func(c *Config) { c.RetryInterval = 0 },
		func(c *Config) { c.TotalRetryTimeout = -1 },
	} {
		config := DefaultConfig()
		modify(&config)
		if _, err := NewClient(config); err == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
	client, count := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected request") })
	client.config.TotalRetryTimeout = 0
	if err := client.ReportReady(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if atomic.LoadInt32(count) != 0 {
		t.Fatal("zero budget sent a request")
	}
}
