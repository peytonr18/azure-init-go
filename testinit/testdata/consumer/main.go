// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// This is an external SDK consumer fixture, not a provisioning agent.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peytonr18/azure-init-go/kvp"
	"github.com/peytonr18/azure-init-go/wireserver"
)

type plan struct {
	PoolDir     string `json:"pool_dir"`
	Endpoint    string `json:"endpoint"`
	State       string `json:"state"`
	Order       string `json:"order"`
	Agent       string `json:"agent"`
	VMID        string `json:"vm_id"`
	Reason      string `json:"reason"`
	KVP         bool   `json:"kvp"`
	HTTP        bool   `json:"http"`
	Diagnostics bool   `json:"diagnostics"`
	Encoding    string `json:"encoding"`
	RequestMS   int    `json:"request_ms"`
	RetryMS     int    `json:"retry_ms"`
	TotalMS     int    `json:"total_ms"`
}

type result struct {
	EncodedReport string `json:"encoded_report"`
	ReportAfter   string `json:"report_after"`
	KVPError      string `json:"kvp_error"`
	HTTPError     string `json:"http_error"`
	HTTPMillis    int64  `json:"http_duration_ms"`
	PayloadSHA256 string `json:"payload_sha256"`
}

func validate(p plan) error {
	root := os.Getenv("AZURE_INIT_TEST_ROOT")
	if root == "" || root == "/" {
		return fmt.Errorf("AZURE_INIT_TEST_ROOT must name a disposable fixture directory")
	}
	marker, err := os.ReadFile(filepath.Join(root, ".azure-init-go-testinit"))
	if err != nil {
		return err
	}
	if string(marker) != "isolated azure-init-go fixture\n" {
		return fmt.Errorf("invalid testinit work-directory marker")
	}
	relative, err := filepath.Rel(root, p.PoolDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || !filepath.IsAbs(p.PoolDir) {
		return fmt.Errorf("pool directory must be inside the disposable fixture")
	}
	current := root
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture pool paths must not traverse symlinks")
		}
	}
	if !p.KVP && !p.HTTP {
		return fmt.Errorf("at least one explicit reporting channel is required")
	}
	if p.Order != "kvp-first" && p.Order != "http-first" {
		return fmt.Errorf("invalid reporting order %q", p.Order)
	}
	if p.State != "success" && p.State != "failure" && p.State != "in-progress" {
		return fmt.Errorf("invalid provisioning state %q", p.State)
	}
	if p.HTTP {
		endpoint, err := url.Parse(p.Endpoint)
		if err != nil || endpoint.Scheme != "http" || endpoint.Path != "/provisioning/health" {
			return fmt.Errorf("fixture requires a loopback mock health endpoint")
		}
		address := net.ParseIP(endpoint.Hostname())
		if address == nil || !address.IsLoopback() || endpoint.Port() == "" {
			return fmt.Errorf("fixture refuses non-loopback HTTP destinations")
		}
	}
	return nil
}

func execute(p plan) (result, error) {
	var output result
	report := kvp.NewInProgressReport(p.Agent, p.VMID, kvp.PPSNone)
	switch p.State {
	case "success":
		report = kvp.NewSuccessReport(p.Agent, p.VMID, kvp.PPSNone)
	case "failure":
		report = kvp.NewFailureReport(p.Agent, p.VMID, p.Reason, kvp.PPSNone).
			WithDocumentationURL("https://example.test/testinit")
	}
	report = report.WithExtra("stage", "testinit").WithExtra("detail", "one").WithExtra("detail", "two")
	var err error
	output.EncodedReport, err = report.Encode()
	if err != nil {
		return output, err
	}
	publish := func() error {
		if !p.KVP {
			return nil
		}
		store, err := kvp.NewStoreIn(kvp.Guest, p.PoolDir, kvp.Safe)
		if err != nil {
			return err
		}
		err = kvp.ReportInProgress(store, p.Agent, p.VMID, kvp.PPSNone)
		if p.Diagnostics {
			writer, createErr := kvp.NewDiagnosticWriter(store, p.Agent, p.VMID)
			if createErr != nil {
				err = errors.Join(err, createErr)
			} else {
				const eventID = "11111111-2222-4333-8444-555555555555"
				err = errors.Join(err, writer.EmitStart(eventID, "testinit:provision", kvp.TextPayload("starting"), kvp.Plain))
				payload := kvp.TextPayload(strings.Repeat("provisioning log \u754c\n", 300))
				if p.Encoding != "none" {
					data := make([]byte, 4096)
					if _, randomErr := rand.New(rand.NewSource(370)).Read(data); randomErr != nil {
						return errors.Join(err, randomErr)
					}
					payload = kvp.BytesPayload(data)
				}
				digest := sha256.Sum256(payload.Bytes())
				output.PayloadSHA256 = hex.EncodeToString(digest[:])
				err = errors.Join(err, writer.EmitEvent("testinit:log", payload, kvp.Encoding(p.Encoding), nil, nil))
				outcome := kvp.OK
				if p.State == "failure" {
					outcome = kvp.Error
				}
				err = errors.Join(err, writer.EmitFinish(eventID, "testinit:provision", kvp.TextPayload("finished"),
					kvp.Plain, outcome, kvp.Duration{Nanoseconds: 25000000}))
			}
		}
		return errors.Join(err, kvp.WriteReport(store, report))
	}
	deliver := func() error {
		if !p.HTTP {
			return nil
		}
		config := wireserver.DefaultConfig()
		config.AgentName, config.HealthEndpoint = p.Agent, p.Endpoint
		config.ConnectionTimeout = 100 * time.Millisecond
		config.RequestTimeout = time.Duration(p.RequestMS) * time.Millisecond
		config.RetryInterval = time.Duration(p.RetryMS) * time.Millisecond
		config.TotalRetryTimeout = time.Duration(p.TotalMS) * time.Millisecond
		client, err := wireserver.NewClient(config)
		if err != nil {
			return err
		}
		defer client.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), config.TotalRetryTimeout+time.Second)
		defer cancel()
		switch p.State {
		case "success":
			return client.ReportReady(ctx)
		case "failure":
			return client.ReportFailure(ctx, report)
		default:
			return client.ReportInProgress(ctx, p.VMID)
		}
	}
	var kvpErr, httpErr error
	timedDeliver := func() error {
		started := time.Now()
		err := deliver()
		output.HTTPMillis = time.Since(started).Milliseconds()
		return err
	}
	// This ordering belongs to the consuming application, never to the SDK.
	if p.Order == "http-first" {
		httpErr = timedDeliver()
		kvpErr = publish()
	} else {
		kvpErr = publish()
		httpErr = timedDeliver()
	}
	if kvpErr != nil {
		output.KVPError = kvpErr.Error()
	}
	if httpErr != nil {
		output.HTTPError = httpErr.Error()
	}
	output.ReportAfter, err = report.Encode()
	return output, errors.Join(kvpErr, httpErr, err)
}

func main() {
	var p plan
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "expected exactly one fixture plan")
		os.Exit(2)
	}
	if err := validate(p); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	output, err := execute(p)
	if encodeErr := json.NewEncoder(os.Stdout).Encode(output); encodeErr != nil {
		fmt.Fprintln(os.Stderr, encodeErr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
