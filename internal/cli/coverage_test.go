// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peytonr18/azure-init-go/kvp"
)

const cliVM = "00112233-4455-6677-8899-aabbccddeeff"

func cliSystem() services {
	return services{openStore, func() (string, error) { return cliVM, nil }}
}

func TestCommandSpecificHelp(t *testing.T) {
	for _, topic := range strings.Fields(commandOrder) {
		for _, args := range [][]string{{"help", topic}, {topic, "--help"}} {
			if topic == "help" && args[1] == "--help" {
				continue
			}
			var out, stderr bytes.Buffer
			if code := Run(args, strings.NewReader(""), &out, &stderr); code != 0 {
				t.Fatal(code, stderr.String())
			}
			usage := "Usage: azure-init-kvp " + topic + " [OPTIONS]"
			if topic == "help" {
				usage = "Usage: azure-init-kvp help [COMMAND]..."
			}
			if !strings.Contains(out.String(), usage) ||
				!strings.Contains(out.String(), commands[topic].summary) {
				t.Fatal(args, out.String())
			}
			for _, name := range strings.Fields(commands[topic].flags) {
				if !strings.Contains(out.String(), "--"+name) {
					t.Fatal(topic, name)
				}
			}
		}
	}
	for _, args := range [][]string{{"help"}, {"--help"}} {
		var out, stderr bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, &stderr); code != 0 ||
			!strings.Contains(out.String(), "Commands:\n") {
			t.Fatal(code, out.String(), stderr.String())
		}
	}
	for _, args := range [][]string{{"-x"}, {"--unknown"}, {"--json=true", "info"}, {"--dir"},
		{"info", "--json=true"}, {"--dir", "--json"}, {"--dir", "a", "--dir", "b", "info"},
		{"info", "--append"}, {"help", "--help"}, {"--name", "op", "emit"}} {
		if code, _, stderr := run(t, t.TempDir(), "", args...); code != 2 || stderr == "" {
			t.Fatal(args, code, stderr)
		}
	}
}

func TestConstructorFailuresAndExitClassification(t *testing.T) {
	if _, err := openStore(kvp.Pool(255), t.TempDir(), kvp.Safe); !errors.Is(err, kvp.ErrInvalidPool) {
		t.Fatal(err)
	}
	for _, test := range []struct {
		err  error
		code int
	}{
		{&os.PathError{Op: "read", Path: "pool", Err: io.ErrClosedPipe}, 3},
		{kvp.ErrInvalidFraming, 3}, {usage("invalid"), 2},
		{&kvp.ValidationError{Err: kvp.ErrFieldTooLong, Field: "name"}, 2}, {kvp.Malformed, 2},
		{kvp.ErrEmptyKey, 2}, {kvp.ErrKeyContainsNull, 2}, {kvp.ErrValueContainsNull, 2},
		{kvp.ErrInvalidUTF8, 2}, {kvp.ErrEncoding, 2}, {kvp.ErrPayloadNotUTF8, 2}, {kvp.ErrInvalidDuration, 2},
		{io.ErrUnexpectedEOF, 3},
	} {
		system := cliSystem()
		system.newStore = func(kvp.Pool, string, kvp.PoolMode) (commandStore, error) { return nil, test.err }
		var out, stderr bytes.Buffer
		if code := runWithServices([]string{"info"}, strings.NewReader(""), &out, &stderr, system); code != test.code ||
			stderr.Len() == 0 || out.Len() != 0 {
			t.Fatal(test.err, code, out.String(), stderr.String())
		}
	}
	if code := Run(nil, strings.NewReader(""), io.Discard, brokenWriter{}); code != 3 {
		t.Fatal(code)
	}
}

func TestVMIDDefaultsAndLookupErrors(t *testing.T) {
	for _, command := range []string{"emit", "report-in-progress", "report-success", "report-failure"} {
		args := []string{"--dir", t.TempDir(), command}
		if command == "emit" {
			args = append(args, "--name", "op", "--message", "-")
		}
		if command == "report-failure" {
			args = append(args, "--reason", "failure")
		}
		system := cliSystem()
		var out, stderr bytes.Buffer
		if code := runWithServices(args, strings.NewReader(""), &out, &stderr, system); code != 0 {
			t.Fatal(code, stderr.String())
		}
		system.vmID = func() (string, error) { return "", io.ErrUnexpectedEOF }
		stderr.Reset()
		if code := runWithServices(args, strings.NewReader(""), &out, &stderr, system); code != 2 ||
			!strings.Contains(stderr.String(), "--vm-id") {
			t.Fatal(code, stderr.String())
		}
	}
	a, err := parse([]string{"report-success", "--vm-id", "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := resolveVM(a, func() (string, error) { t.Fatal("explicit identity used lookup"); return "", nil })
	if err != nil || value != "explicit" {
		t.Fatal(value, err)
	}
	if code, _, _ := run(t, t.TempDir(), "", "emit", "--vm-id", cliVM, "--agent", "", "--name", "op", "--message", "text"); code != 2 {
		t.Fatal(code)
	}
}

type failingStore struct {
	commandStore
	operation string
}

func (f failingStore) Len() (int64, error) {
	if f.operation == "len" {
		return 0, io.ErrClosedPipe
	}
	return f.commandStore.Len()
}
func (f failingStore) IsStale() (bool, error) {
	if f.operation == "stale" {
		return false, io.ErrClosedPipe
	}
	return f.commandStore.IsStale()
}
func (f failingStore) Dump() ([]kvp.Record, error) {
	if f.operation == "dump" {
		return nil, io.ErrClosedPipe
	}
	return f.commandStore.Dump()
}
func (f failingStore) Entries() (map[string]string, error) {
	if f.operation == "entries" {
		return nil, io.ErrClosedPipe
	}
	return f.commandStore.Entries()
}
func (f failingStore) Read(key string) (string, bool, error) {
	if f.operation == "read" {
		return "", false, io.ErrClosedPipe
	}
	return f.commandStore.Read(key)
}
func (f failingStore) Delete(key string) (bool, error) {
	if f.operation == "delete" {
		return false, io.ErrClosedPipe
	}
	return f.commandStore.Delete(key)
}
func (f failingStore) DeleteMultiple(keys []string) (int, error) {
	if f.operation == "delete-multiple" {
		return 0, io.ErrClosedPipe
	}
	return f.commandStore.DeleteMultiple(keys)
}

func TestCommandStoreFailures(t *testing.T) {
	for _, test := range []struct {
		args      []string
		operation string
	}{
		{[]string{"info"}, "len"}, {[]string{"info"}, "stale"},
		{[]string{"dump"}, "dump"}, {[]string{"dump", "--parse"}, "dump"},
		{[]string{"entries"}, "entries"}, {[]string{"read", "key"}, "read"},
		{[]string{"delete", "key"}, "delete"}, {[]string{"delete-multiple", "key"}, "delete-multiple"},
		{[]string{"is-stale"}, "stale"},
	} {
		store, err := openStore(kvp.Guest, t.TempDir(), kvp.Safe)
		if err != nil {
			t.Fatal(err)
		}
		system := cliSystem()
		system.newStore = func(kvp.Pool, string, kvp.PoolMode) (commandStore, error) {
			return failingStore{store, test.operation}, nil
		}
		var out, stderr bytes.Buffer
		if code := runWithServices(test.args, strings.NewReader(""), &out, &stderr, system); code != 3 ||
			!strings.Contains(stderr.String(), io.ErrClosedPipe.Error()) {
			t.Fatal(test.args, code, stderr.String())
		}
	}
}

func TestAllCommandOutputModesAndFailures(t *testing.T) {
	for _, args := range [][]string{
		{"info"}, {"info", "--json"}, {"dump", "--text"}, {"dump", "--parse"}, {"dump", "--parse", "--text"},
		{"entries"}, {"entries", "--json"}, {"read", "key"}, {"read", "key", "--json"},
		{"delete", "key"}, {"delete", "key", "--json"}, {"delete-multiple", "key"}, {"delete-multiple", "key", "--json"},
		{"is-stale"}, {"is-stale", "--json"},
	} {
		for _, fail := range []bool{false, true} {
			dir := t.TempDir()
			store, err := kvp.NewStoreIn(kvp.Guest, dir, kvp.Safe)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Append("key", "value"); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(store.Path(), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			var output io.Writer = &out
			if fail {
				output = brokenWriter{}
			}
			code := Run(append([]string{"--dir", dir}, args...), strings.NewReader(""), output, &stderr)
			if fail && (code != 3 || stderr.Len() == 0) {
				t.Fatal(args, code, stderr.String())
			}
			if !fail && (code != 0 || out.Len() == 0) {
				t.Fatal(args, code, out.String(), stderr.String())
			}
		}
	}
}

type failingInput struct{}

func (failingInput) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestInputFilesAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input")
	if err := os.WriteFile(path, []byte("key=value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"load", "append-multiple"} {
		if code, _, stderr := run(t, dir, "", command, "--file", path); code != 0 {
			t.Fatal(code, stderr)
		}
		if code, _, _ := run(t, dir, "", command, "--file", path+"-missing"); code != 3 {
			t.Fatal(code)
		}
		var out, stderr bytes.Buffer
		if code := Run([]string{"--dir", dir, command}, failingInput{}, &out, &stderr); code != 3 {
			t.Fatal(code)
		}
	}
	if err := os.WriteFile(path, []byte{0xff}, 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(t, dir, "", "load", "--file", path); code != 3 {
		t.Fatal(code)
	}
	if code, _, _ := run(t, dir, "", "write", "--", "-key", "-value"); code != 0 {
		t.Fatal(code)
	}
	a := arguments{command: "not-a-command", values: map[string][]string{"dir": {dir}}}
	if _, err := dispatch(a, strings.NewReader(""), io.Discard, cliSystem()); err == nil {
		t.Fatal("unknown dispatch succeeded")
	}
	a.command = "info"
	a.values["pool"] = []string{"invalid"}
	if _, err := dispatch(a, strings.NewReader(""), io.Discard, cliSystem()); err == nil {
		t.Fatal("invalid pool dispatched")
	}
}

func TestEntryTextVariantsAndErrors(t *testing.T) {
	stamp := kvp.Timestamp{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	outcome := kvp.OK
	duration := kvp.Duration{Seconds: 12, Nanoseconds: 500000000}
	d := kvp.Diagnostic{Kind: kvp.Event, DiagnosticKey: kvp.DiagnosticKey{Agent: "agent", Name: "name", EventID: "id", Timestamp: stamp, Encoding: kvp.Plain},
		Payload: kvp.BytesPayload([]byte{0xff}), Result: &outcome, Duration: &duration}
	line, err := entryText(kvp.Entry{Diagnostic: &d})
	if err != nil || !strings.Contains(line, "duration=12.5s") || !strings.Contains(line, "payload_b64=/w==") {
		t.Fatal(line, err)
	}
	line, err = entryText(kvp.Entry{Raw: &kvp.RawKeyValue{Key: "key", Value: "value", Error: kvp.Malformed}})
	if err != nil || !strings.Contains(line, "error=malformed diagnostic") {
		t.Fatal(line, err)
	}
	report := kvp.NewSuccessReport("agent", "vm", kvp.PPSNone)
	if _, err := entryText(kvp.Entry{Report: &report}); err != nil {
		t.Fatal(err)
	}
	report.PPSType = "invalid"
	d.Duration = &kvp.Duration{Nanoseconds: 1e9}
	for _, entry := range []kvp.Entry{{}, {Report: &report}, {Diagnostic: &d}, {Report: &report, Raw: &kvp.RawKeyValue{}}} {
		if _, err := entryText(entry); err == nil {
			t.Fatal("invalid model formatted successfully")
		}
		if err := writeParsedEntries(io.Discard, []kvp.Entry{entry}, true); err == nil {
			t.Fatal("invalid model printed successfully")
		}
	}
}
