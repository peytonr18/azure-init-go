// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peytonr18/azure-init-go/kvp"
)

func run(t *testing.T, dir, input string, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Run(append([]string{"--dir", dir}, args...), strings.NewReader(input), &out, &stderr)
	return code, out.String(), stderr.String()
}

func TestStorageCommandsAndExitCodes(t *testing.T) {
	dir := t.TempDir()
	for _, step := range []struct {
		args          []string
		input, output string
		code          int
	}{
		{[]string{"read", "absent"}, "", "", 1},
		{[]string{"load"}, "b=one\n\nb=two\r\na=three=more\n", "", 0},
		{[]string{"dump"}, "", "[{\"key\":\"b\",\"value\":\"one\"},{\"key\":\"b\",\"value\":\"two\"},{\"key\":\"a\",\"value\":\"three=more\"}]\n", 0},
		{[]string{"entries"}, "", "a=three=more\nb=two\n", 0},
		{[]string{"--json", "read", "b"}, "", "{\"key\":\"b\",\"value\":\"two\"}\n", 0},
		{[]string{"write", "--append", "b", ""}, "", "", 0},
		{[]string{"read", "b"}, "", "\n", 0},
		{[]string{"write", "b", "replacement"}, "", "", 0},
		{[]string{"delete-multiple", "b", "absent", "--json"}, "", "{\"removed\":1}\n", 0},
		{[]string{"delete", "a"}, "", "true\n", 0},
		{[]string{"is-stale"}, "", "false\n", 1},
		{[]string{"append-multiple"}, "last=value\n", "", 0},
		{[]string{"clear", "--if-stale"}, "", "", 0},
		{[]string{"dump", "--text"}, "", "last=value\n", 0},
		{[]string{"clear"}, "", "", 0},
		{[]string{"dump"}, "", "[]\n", 0},
	} {
		code, out, stderr := run(t, dir, step.input, step.args...)
		if code != step.code || out != step.output || stderr != "" {
			t.Fatalf("%v: code=%d out=%q stderr=%q; want code=%d out=%q", step.args, code, out, stderr, step.code, step.output)
		}
	}
}

func TestCLIValidationDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{}, {"unknown"}, {"--pool", "invalid", "info"}, {"--json", "--text", "info"},
		{"write", "key"}, {"write", "", "v"}, {"write", "k", "nul\x00value"},
		{"dump", "--name", "filter"}, {"dump", "--parse", "--kind", "invalid"},
		{"dump", "--parse", "--kind="}, {"help", "invalid"}, {"invalid", "--help"},
		{"emit", "--name", "name"}, {"emit", "--message", "message"},
		{"report-failure", "--vm-id", "id"}, {"report-success", "--vm-id", "id", "--supporting-data", "result=success"},
		{"report-failure", "--vm-id", "id", "--reason", "r", "--supporting-data", "reason=again"},
		{"report-failure", "--vm-id", "id", "--reason", "r", "--documentation-url", "url", "--supporting-data", "documentation_url=again"},
		{"report-success", "--vm-id", "id", "--supporting-data", "k='unterminated"},
		{"report-success", "--vm-id", "id", "--supporting-data", "k='closed'junk"},
		{"info", "--reason", "invalid"}, {"--json", "--json", "info"},
	} {
		code, out, stderr := run(t, dir, "", args...)
		if code != 2 || out != "" || stderr == "" {
			t.Fatalf("%v: code=%d out=%q stderr=%q", args, code, out, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".kvp_pool_1")); !os.IsNotExist(err) {
		t.Fatal("invalid command created a pool")
	}
	if code, _, _ := run(t, dir, "valid=v\nnot-a-pair\n", "load"); code != 2 {
		t.Fatal(code)
	}
	if code, _, _ := run(t, filepath.Join(dir, "absent"), "", "write", "k", "v"); code != 3 {
		t.Fatal("I/O errors must exit 3")
	}
}

func TestReportAndParsedDiagnosticCommands(t *testing.T) {
	dir := t.TempDir()
	vm := "00112233-4455-6677-8899-aabbccddeeff"
	for _, command := range []string{"report-in-progress", "report-success", "report-failure"} {
		args := []string{command, "--vm-id", vm, "--agent", "producer", "--supporting-data", "k='a,b',k=again"}
		if command == "report-failure" {
			args = append(args, "--reason", "failed|reason", "--documentation-url", "https://help")
		}
		if code, _, stderr := run(t, dir, "", args...); code != 0 {
			t.Fatal(code, stderr)
		}
	}
	if code, _, stderr := run(t, dir, "", "emit", "--name", "operation", "--message", "hello", "--vm-id", vm, "--agent", "producer"); code != 0 {
		t.Fatal(code, stderr)
	}
	if code, out, stderr := run(t, dir, "", "dump", "--parse", "--text", "--name", "operation", "--kind", "event"); code != 0 ||
		!strings.Contains(out, "PROVISIONING_REPORT=") || !strings.Contains(out, "diagnostic kind=event") || stderr != "" {
		t.Fatal(code, out, stderr)
	}
	if code, out, stderr := run(t, dir, "", "dump", "--parse", "--text", "--name", "not-present"); code != 0 ||
		strings.Contains(out, "diagnostic") || !strings.Contains(out, "PROVISIONING_REPORT=") || stderr != "" {
		t.Fatal(code, out, stderr)
	}
}

func TestSupportingDataQuoting(t *testing.T) {
	for _, test := range []struct {
		input string
		want  [][2]string
	}{
		{"", [][2]string{}},
		{",,,", [][2]string{}},
		{"k=v,", [][2]string{{"k", "v"}}},
		{`k='a,b',other="c,d",key=a'b,empty=,equal=a=b`, [][2]string{{"k", "a,b"}, {"other", "c,d"}, {"key", "a'b"}, {"empty", ""}, {"equal", "a=b"}}},
	} {
		got, err := supportingData(test.input)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%q: %v %v", test.input, got, err)
		}
	}
	for _, invalid := range []string{"no-value", "=value", "k='open", `k="closed"x`, "agent=reserved"} {
		if _, err := supportingData(invalid); err == nil {
			t.Fatal("accepted", invalid)
		}
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCLIOutputAndFramingErrors(t *testing.T) {
	if code := Run([]string{"--help"}, strings.NewReader(""), brokenWriter{}, io.Discard); code != 3 {
		t.Fatal(code)
	}
	dir := t.TempDir()
	store, err := kvp.NewStoreIn(kvp.Guest, dir, kvp.Safe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, dir, "", "dump"); code != 3 || !strings.Contains(stderr, kvp.ErrInvalidFraming.Error()) {
		t.Fatal(code, stderr)
	}
	if !errors.Is(kvp.Malformed, kvp.Malformed) {
		t.Fatal("decode errors must be classifiable")
	}
}
