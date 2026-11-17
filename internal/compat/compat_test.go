// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/peytonr18/azure-init-go/internal/cli"
	"github.com/peytonr18/azure-init-go/kvp"
)

func rustBinary(t *testing.T) string {
	t.Helper()
	path := os.Getenv("AZURE_INIT_RUST_KVP")
	if path == "" {
		t.Skip("set AZURE_INIT_RUST_KVP to the pinned Rust libazureinit-kvp executable")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func rust(t *testing.T, binary, dir, input string, args ...string) (int, []byte, []byte) {
	t.Helper()
	command := exec.Command(binary, append([]string{"--dir", dir}, args...)...)
	command.Stdin = strings.NewReader(input)
	var out, stderr bytes.Buffer
	command.Stdout, command.Stderr = &out, &stderr
	err := command.Run()
	if err == nil {
		return 0, out.Bytes(), stderr.Bytes()
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), out.Bytes(), stderr.Bytes()
	}
	t.Fatal(err)
	return 0, nil, nil
}

func compareJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var goValue, rustValue interface{}
	if err := json.Unmarshal(got, &goValue); err != nil {
		t.Fatalf("invalid Go JSON: %v: %s", err, got)
	}
	if err := json.Unmarshal(want, &rustValue); err != nil {
		t.Fatalf("invalid Rust JSON: %v: %s", err, want)
	}
	if !reflect.DeepEqual(goValue, rustValue) {
		t.Fatalf("Go:   %s\nRust: %s", got, want)
	}
}

func compareReaders(t *testing.T, binary string, store *kvp.Store) {
	t.Helper()
	entries, err := kvp.NewDiagnosticReader(store).Entries()
	if err != nil {
		t.Fatal(err)
	}
	goJSON, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	code, rustJSON, stderr := rust(t, binary, strings.TrimSuffix(store.Path(), "/.kvp_pool_1"), "", "dump", "--parse")
	if code != 0 {
		t.Fatalf("Rust read: %s", stderr)
	}
	compareJSON(t, goJSON, rustJSON)
}

func TestGoWritesRustReads(t *testing.T) {
	binary := rustBinary(t)
	store, err := kvp.NewStoreIn(kvp.Guest, t.TempDir(), kvp.Safe)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := kvp.NewDiagnosticWriter(store, "interop/1", "00112233-4455-6677-8899-aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	for _, encoding := range []kvp.Encoding{kvp.Plain, kvp.ZlibBase64, kvp.GzipBase64} {
		if err := writer.EmitEvent("long", kvp.TextPayload(strings.Repeat("Unicode \u754c ", 1000)), encoding, nil, nil); err != nil {
			t.Fatal(err)
		}
		if encoding != kvp.Plain {
			if err := writer.EmitEvent("binary", kvp.BytesPayload([]byte{0xff, 0x00, 0x01}), encoding, nil, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	report := kvp.NewFailureReport("interop/1", "unrestricted-vm", "quotes\"|pipes\nnewlines", kvp.PPSSavable).
		WithExtra("duplicate", "one").WithExtra("duplicate", "two").WithDocumentationURL("https://help/?a=b|c")
	report.Timestamp = "2026-06-17T00:00:00.123400+00:00"
	if err := kvp.WriteReport(store, report); err != nil {
		t.Fatal(err)
	}
	compareReaders(t, binary, store)
}

func TestRustWritesGoReads(t *testing.T) {
	binary := rustBinary(t)
	dir := t.TempDir()
	for _, args := range [][]string{
		{"emit", "--agent", "rust/1", "--vm-id", "00112233-4455-6677-8899-aabbccddeeff", "--name", "event", "--message", strings.Repeat("payload", 400)},
		{"report-in-progress", "--agent", "rust/1", "--vm-id", "vm", "--supporting-data", "build=one"},
		{"report-failure", "--agent", "rust/1", "--vm-id", "vm", "--reason", "failed|reason", "--supporting-data", "extra='a,b'"},
		{"report-success", "--agent", "rust/1", "--vm-id", "vm"},
	} {
		if code, _, stderr := rust(t, binary, dir, "", args...); code != 0 {
			t.Fatalf("%v: %s", args, stderr)
		}
		store, err := kvp.NewStoreIn(kvp.Guest, dir, kvp.Safe)
		if err != nil {
			t.Fatal(err)
		}
		compareReaders(t, binary, store)
		value, found, err := store.Read(kvp.ProvisioningReportKey)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			report, err := kvp.ParseProvisioningReport(value)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := report.Encode()
			if err != nil || encoded != value {
				t.Fatalf("report encoding differs: %v\n%s\n%s", err, encoded, value)
			}
		}
	}
}

func TestCLIStorageParity(t *testing.T) {
	binary := rustBinary(t)
	goDir, rustDir := t.TempDir(), t.TempDir()
	for _, step := range []struct {
		args  []string
		input string
	}{
		{[]string{"read", "absent"}, ""},
		{[]string{"load"}, "a=first\nb=second\na=last\n"},
		{[]string{"dump"}, ""},
		{[]string{"entries", "--json"}, ""},
		{[]string{"read", "a", "--json"}, ""},
		{[]string{"write", "--append", "b", "duplicate"}, ""},
		{[]string{"write", "a", "upsert"}, ""},
		{[]string{"dump", "--text"}, ""},
		{[]string{"append-multiple"}, "c=v\nc=another\n"},
		{[]string{"delete-multiple", "a", "c", "--json"}, ""},
		{[]string{"dump"}, ""},
		{[]string{"delete", "b"}, ""},
		{[]string{"is-stale", "--json"}, ""},
		{[]string{"clear", "--if-stale"}, ""},
		{[]string{"write", "", "invalid"}, ""},
		{[]string{"dump", "--parse", "--kind="}, ""},
		{[]string{"help", "invalid"}, ""},
		{[]string{"invalid", "--help"}, ""},
		{[]string{"load"}, "valid=v\ninvalid-line\n"},
		{[]string{"clear"}, ""},
		{[]string{"dump"}, ""},
	} {
		var goOut, goErr bytes.Buffer
		goCode := cli.Run(append([]string{"--dir", goDir}, step.args...), strings.NewReader(step.input), &goOut, &goErr)
		rustCode, rustOut, rustErr := rust(t, binary, rustDir, step.input, step.args...)
		if goCode != rustCode || !bytes.Equal(goOut.Bytes(), rustOut) || (goErr.Len() == 0) != (len(rustErr) == 0) {
			t.Fatalf("%v: Go=%d %q %q Rust=%d %q %q", step.args, goCode, goOut.String(), goErr.String(), rustCode, rustOut, rustErr)
		}
	}
}

func TestDecoderDifferentialCases(t *testing.T) {
	binary := rustBinary(t)
	base := []string{"DIAG", "agent", "00112233-4455-6677-8899-aabbccddeeff", "event", "operation",
		"arbitrary-reader-event-id", "2026-08-31T12:34:56.789Z", "none", "", "", "0"}
	cases := make([]struct {
		name    string
		records []kvp.Record
	}, 0)
	addField := func(name string, field int, values []string) {
		for i, value := range values {
			fields := append([]string(nil), base...)
			fields[field] = value
			cases = append(cases, struct {
				name    string
				records []kvp.Record
			}{
				fmt.Sprintf("%s-%d", name, i), []kvp.Record{{Key: strings.Join(fields, "|"), Value: "value"}}})
		}
	}
	addField("timestamps", 6, []string{
		"2026-08-31T12:34:56Z", "2026-08-31t12:34:56.789z", "2026-08-31T12:34:56.7891234567Z",
		"2026-08-31T14:34:56.789+02:00", "2026-08-31 12:34:56Z", "2026-08-31T12:34:60Z",
		"2026-08-31T1:34:56Z", "2026-08-31T12:34:56,123Z",
		"2026-08-31T12:34:56+24:00", "2026-08-31T12:34:56+01:60", "invalid",
		"0000-01-01T00:00:60+01:00", "9999-12-31T23:59:59-01:00",
	})
	addField("durations", 9, []string{
		"0", "0.000000001", "18446744073709551615.999999999", "18446744073709551616",
		"1e-10", "0.0000000005", "1.25e1", "+1", "-0", "-1", "NaN", "inf", "0x1p0", "1_0", ".5", "1.",
	})
	addField("results", 8, []string{"ok", "error", "SUCCESS", "warn"})
	addField("uuid", 2, []string{
		"00112233445566778899aabbccddeeff",
		"{00112233-4455-6677-8899-aabbccddeeff}",
		"urn:uuid:00112233-4455-6677-8899-aabbccddeeff",
		"urn:uuid:00112233445566778899aabbccddeeff",
		"URN:UUID:00112233-4455-6677-8899-aabbccddeeff",
	})
	addField("kind", 3, []string{"start", "finish", "event", "other"})
	addField("encoding", 7, []string{"none", "gz+b64", "zlib+b64", "unknown", ""})
	addField("index", 10, []string{"0", "00", "+0", "-1", "1", "18446744073709551615", "18446744073709551616", ""})
	for _, value := range []string{
		`{"name":"op","type":"event","ts":"2026-08-31T12:34:56Z","msg":"text"}`,
		`{"name":"op","type":"event","ts":"2026-08-31T12:34:56Z","msg":"\ud800"}`,
		`{"name":"op","type":"event","ts":"2026-08-31T12:34:56Z","msg":"\ud83d\ude00"}`,
		`{"name":"op","type":"event","ts":"2026-08-31T12:34:56Z","msg":"text","huge":1e9999}`,
	} {
		cases = append(cases, struct {
			name    string
			records []kvp.Record
		}{
			fmt.Sprintf("cloud-%d", len(cases)), []kvp.Record{{Key: "CLOUD_INIT|0|event|op|00112233-4455-6677-8899-aabbccddeeff", Value: value}}})
	}
	for _, numbers := range [][2]string{{"1.0", "1.00"}, {"1", "1.0"}, {"-0", "0.0"}} {
		base := "CLOUD_INIT|0|event|op|00112233-4455-6677-8899-aabbccddeeff"
		first := `{"name":"op","type":"event","ts":"2026-08-31T12:34:56Z","extra":` + numbers[0] + `,"msg_i":0,"msg":"one"}`
		second := `{"name":"op","type":"event","ts":"2026-08-31T12:34:56Z","extra":` + numbers[1] + `,"msg_i":1,"msg":"two"}`
		cases = append(cases, struct {
			name    string
			records []kvp.Record
		}{
			fmt.Sprintf("cloud-metadata-%d", len(cases)), []kvp.Record{{Key: base + "|0", Value: first}, {Key: base + "|1", Value: second}}})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store, err := kvp.NewStoreIn(kvp.Guest, t.TempDir(), kvp.Safe)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Load(test.records); err != nil {
				t.Fatal(err)
			}
			compareReaders(t, binary, store)
		})
	}
}

func TestMixedRustGoWriters(t *testing.T) {
	binary := rustBinary(t)
	dir := t.TempDir()
	store, err := kvp.NewStoreIn(kvp.Guest, dir, kvp.Safe)
	if err != nil {
		t.Fatal(err)
	}
	const workers, recordsPerWriter = 3, 25
	failures := make(chan error, workers*2)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(2)
		go func(i int) {
			defer wait.Done()
			for j := 0; j < recordsPerWriter; j++ {
				if err := store.Insert(fmt.Sprintf("go-%d-%d", i, j), "value"); err != nil {
					failures <- err
					return
				}
			}
		}(i)
		go func(i int) {
			defer wait.Done()
			var input strings.Builder
			for j := 0; j < recordsPerWriter; j++ {
				fmt.Fprintf(&input, "rust-%d-%d=value\n", i, j)
			}
			command := exec.Command(binary, "--dir", dir, "append-multiple")
			command.Stdin = strings.NewReader(input.String())
			if output, err := command.CombinedOutput(); err != nil {
				failures <- fmt.Errorf("Rust writer: %w: %s", err, output)
			}
		}(i)
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	entries, err := store.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != workers*recordsPerWriter*2 {
		t.Fatalf("mixed-language writers lost updates: %d records", len(entries))
	}
	compareReaders(t, binary, store)
}
