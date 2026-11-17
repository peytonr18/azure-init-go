// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestIndependentCodecFixturesAndEveryTruncation(t *testing.T) {
	var corpus struct {
		Fixtures []struct {
			Name     string   `json:"name"`
			Encoding Encoding `json:"encoding"`
			Encoded  string   `json:"encoded"`
			Decoded  string   `json:"decoded_base64"`
		} `json:"fixtures"`
	}
	data, err := os.ReadFile("../testdata/codecs.json")
	requireNoError(t, err)
	requireNoError(t, json.Unmarshal(data, &corpus))
	for _, fixture := range corpus.Fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			want, err := base64.StdEncoding.DecodeString(fixture.Decoded)
			requireNoError(t, err)
			payload, err := decodePayload(fixture.Encoded, fixture.Encoding)
			requireNoError(t, err)
			if !payload.IsBinary() || !bytes.Equal(payload.Bytes(), want) {
				t.Fatal("independent codec fixture changed")
			}
			for i := 0; i < len(fixture.Encoded); i++ {
				if _, err := decodePayload(fixture.Encoded[:i], fixture.Encoding); !errors.Is(err, Undecodable) {
					t.Fatalf("accepted base64 truncation %d/%d: %v", i, len(fixture.Encoded), err)
				}
			}
			compressed, err := base64.StdEncoding.DecodeString(fixture.Encoded)
			requireNoError(t, err)
			for i := 0; i < len(compressed); i++ {
				if _, err := decompress(compressed[:i], fixture.Encoding); !errors.Is(err, Undecodable) {
					t.Fatalf("accepted compressed truncation %d/%d: %v", i, len(compressed), err)
				}
			}
			for i := len(compressed) - 4; i < len(compressed); i++ {
				corrupt := append([]byte(nil), compressed...)
				corrupt[i] ^= 0x80
				if _, err := decompress(corrupt, fixture.Encoding); !errors.Is(err, Undecodable) {
					t.Fatal("checksum/size corruption accepted", err)
				}
			}
		})
	}
}

func TestCapturedCloudInitPoolBothLayouts(t *testing.T) {
	var fixture struct {
		SHA256  string   `json:"sha256"`
		Size    int      `json:"decoded_bytes"`
		Records []Record `json:"records"`
	}
	data, err := os.ReadFile("../testdata/cloud-init.json")
	requireNoError(t, err)
	requireNoError(t, json.Unmarshal(data, &fixture))
	for _, current := range []bool{false, true} {
		store := testStore(t, Unsafe)
		records := append([]Record(nil), fixture.Records...)
		if current {
			for i := range records {
				fields := strings.Split(records[i].Key, "|")
				records[i].Key = strings.Join(fields[:4], "|") + "|" + testVM + "|" + strings.Join(fields[4:], "|")
			}
		}
		requireNoError(t, store.Load(records))
		before, err := os.ReadFile(store.Path())
		requireNoError(t, err)
		readerStore, err := NewStoreIn(Guest, strings.TrimSuffix(store.Path(), "/.kvp_pool_1"), Safe)
		requireNoError(t, err)
		entries, err := NewDiagnosticReader(readerStore).Entries()
		requireNoError(t, err)
		if len(entries) != 1 || entries[0].Diagnostic == nil {
			t.Fatal(entries)
		}
		d := entries[0].Diagnostic
		hash := sha256.Sum256(d.Payload.Bytes())
		if !d.Payload.IsBinary() || len(d.Payload.Bytes()) != fixture.Size || hex.EncodeToString(hash[:]) != fixture.SHA256 ||
			(d.VMID != nil) != current || d.Encoding != GzipBase64 || d.Name != "cloud-init.log" {
			t.Fatal("captured cloud-init fixture did not decode faithfully")
		}
		after, err := os.ReadFile(store.Path())
		requireNoError(t, err)
		if !bytes.Equal(before, after) {
			t.Fatal("reader rewrote captured records")
		}
	}
}

func TestMaximumChunksAndUnboundedReader(t *testing.T) {
	writer, store := testWriter(t)
	message := strings.Repeat("x", MaxChunks*MaxChunkBytes)
	requireNoError(t, writer.EmitStart(testEvent, "maximum", TextPayload(message), Plain))
	count, err := store.Len()
	requireNoError(t, err)
	if count != 1023 {
		t.Fatal(count)
	}
	entries, err := NewDiagnosticReader(store).Entries()
	requireNoError(t, err)
	if len(entries) != 1 || string(entries[0].Diagnostic.Payload.Bytes()) != message {
		t.Fatal("maximum writer payload did not round trip")
	}
	requireNoError(t, writer.EmitEvent("compressed", BytesPayload(make([]byte, MaxChunks*MaxChunkBytes+1)), ZlibBase64, nil, nil))
	requireNoError(t, store.Clear())
	records := make([]Record, 1100)
	for i := range records {
		records[i] = Record{nativeKey("event", "none", "", "", fmt.Sprint(i)), "x"}
	}
	requireNoError(t, store.AppendMultiple(records))
	entries, err = NewDiagnosticReader(store).Entries()
	requireNoError(t, err)
	if len(entries) != 1 || entries[0].Diagnostic == nil || len(entries[0].Diagnostic.Payload.Bytes()) != 1100 {
		t.Fatal("reader incorrectly applied writer chunk limits")
	}
}

func TestReservedReportAtAndAboveKeyLimit(t *testing.T) {
	store := testStore(t, Safe)
	requireNoError(t, ReportInProgress(store, "agent", "vm", PPSNone))
	records := make([]Record, MaxUniqueKeys-1)
	for i := range records {
		records[i] = Record{fmt.Sprintf("diagnostic-%d", i), "keep"}
	}
	requireNoError(t, store.AppendMultiple(records))
	requireNoError(t, ReportFailure(store, "agent", "vm", "failed", PPSRunning))
	requireNoError(t, store.Append("above-cap", "keep"))
	requireNoError(t, ReportSuccess(store, "agent", "vm", PPSNone))
	requireNoError(t, ReportInProgress(store, "agent", "vm", PPSNone))
	value, found, err := store.Read(ProvisioningReportKey)
	requireNoError(t, err)
	if !found {
		t.Fatal("reserved report disappeared")
	}
	report, err := ParseProvisioningReport(value)
	requireNoError(t, err)
	if report.Result != ResultInProgress || report.Reason != nil || report.DocumentationURL != nil || len(report.Extra) != 0 {
		t.Fatal("new attempt retained failure context")
	}
	removed, err := store.Delete(ProvisioningReportKey)
	requireNoError(t, err)
	if !removed {
		t.Fatal("report was not removed")
	}
	before, err := os.ReadFile(store.Path())
	requireNoError(t, err)
	if err := ReportSuccess(store, "agent", "vm", PPSNone); !errors.Is(err, ErrTooManyKeys) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(store.Path())
	requireNoError(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("failed report publication changed a full pool")
	}
}

func TestConcurrentUniqueKeyBudgetAndReaders(t *testing.T) {
	store := testStore(t, Safe)
	records := make([]Record, 1023)
	for i := range records {
		records[i] = Record{fmt.Sprintf("key-%d", i), "value"}
	}
	requireNoError(t, store.Load(records))
	var wait sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func(i int) { defer wait.Done(); results <- store.Insert(fmt.Sprintf("contender-%d", i), "value") }(i)
	}
	wait.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrTooManyKeys) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("%d concurrent writers passed the last-key budget", success)
	}
	requireNoError(t, store.Load([]Record{{"shared", "initial"}}))
	failures := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			for j := 0; j < 30; j++ {
				if i < 4 {
					if err := store.Insert("shared", fmt.Sprintf("writer-%d-%d", i, j)); err != nil {
						failures <- err
						return
					}
				} else {
					value, found, err := store.Read("shared")
					if err != nil {
						failures <- err
						return
					}
					if !found || (value != "initial" && !strings.HasPrefix(value, "writer-")) {
						failures <- fmt.Errorf("torn snapshot %q", value)
						return
					}
				}
			}
		}(i)
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	count, err := store.Len()
	requireNoError(t, err)
	if count != 1 {
		t.Fatal("concurrent upserts produced duplicates", count)
	}
}
