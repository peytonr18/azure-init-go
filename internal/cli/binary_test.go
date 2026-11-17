// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRealBinaryCommands(t *testing.T) {
	binary := os.Getenv("AZURE_INIT_GO_KVP")
	if binary == "" {
		t.Skip("make test builds and instruments the real CLI")
	}
	dir := t.TempDir()
	for _, step := range []struct {
		args     []string
		input    string
		code     int
		contains string
	}{
		{[]string{"--help"}, "", 0, "Commands:"},
		{[]string{"help", "write"}, "", 0, "<KEY> <VALUE>"},
		{[]string{"info", "--json"}, "", 0, `"pool":"guest"`},
		{[]string{"read", "missing"}, "", 1, ""},
		{[]string{"load"}, "key=first\nkey=last\n", 0, ""},
		{[]string{"dump"}, "", 0, `"value":"last"`},
		{[]string{"entries", "--text"}, "", 0, "key=last"},
		{[]string{"write", "--append", "other", "value"}, "", 0, ""},
		{[]string{"write", "key", "upsert"}, "", 0, ""},
		{[]string{"append-multiple"}, "last=record\n", 0, ""},
		{[]string{"delete", "other", "--json"}, "", 0, `"removed":true`},
		{[]string{"delete-multiple", "key", "last"}, "", 0, "2"},
		{[]string{"emit", "--vm-id", cliVM, "--name", "op", "--message", "text"}, "", 0, ""},
		{[]string{"report-in-progress", "--vm-id", "vm"}, "", 0, ""},
		{[]string{"report-failure", "--vm-id", "vm", "--reason", "failure", "--supporting-data", "stage='a,b'"}, "", 0, ""},
		{[]string{"report-success", "--vm-id", "vm"}, "", 0, ""},
		{[]string{"dump", "--parse"}, "", 0, `"type":"diagnostic"`},
		{[]string{"dump", "--parse", "--text"}, "", 0, "result=success"},
		{[]string{"is-stale"}, "", 1, "false"},
		{[]string{"clear", "--if-stale"}, "", 0, ""},
		{[]string{"clear"}, "", 0, ""},
		{[]string{"--unsafe", "write", strings.Repeat("k", 512), strings.Repeat("v", 2048)}, "", 0, ""},
		{[]string{"--pool", "auto-external", "info"}, "", 0, "pool=auto-external"},
		{[]string{"unknown-command"}, "", 2, ""},
		{[]string{"load", "--file", dir + "/missing"}, "", 3, ""},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, binary, append([]string{"--dir", dir}, step.args...)...)
		command.Stdin = strings.NewReader(step.input)
		command.Env = os.Environ()
		if coverage := os.Getenv("AZURE_INIT_GO_COVERDIR"); coverage != "" {
			command.Env = append(command.Env, "GOCOVERDIR="+coverage)
		}
		var out, stderr bytes.Buffer
		command.Stdout, command.Stderr = &out, &stderr
		err := command.Run()
		cancel()
		code := 0
		if err != nil {
			exit, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		if code != step.code || !strings.Contains(out.String(), step.contains) {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", step.args, code, out.String(), stderr.String())
		}
		if code == 0 && stderr.Len() != 0 {
			t.Fatal(stderr.String())
		}
		if len(step.args) != 0 && step.args[0] == "dump" && !strings.Contains(strings.Join(step.args, " "), "--text") &&
			!json.Valid(out.Bytes()) {
			t.Fatal("binary did not emit valid JSON", out.String())
		}
	}
}

func TestExactCoverageGate(t *testing.T) {
	for _, test := range []struct {
		profile string
		success bool
	}{
		{"mode: atomic\nfile.go:1.1,2.1 1 1\n", true},
		{"mode: atomic\nfile.go:1.1,2.1 1 0\nfile.go:1.1,2.1 1 1\n", true},
		{"mode: atomic\nfile.go:1.1,2.1 100000 1\nfile.go:3.1,4.1 1 0\n", false},
		{"mode: atomic\nfile.go:1.1,2.1 1000000000000000000000 1\nfile.go:3.1,4.1 1 0\n", false},
		{"mode: atomic\n", false}, {"invalid\n", false},
		{"mode: invalid\nfile.go:1.1,2.1 1 1\n", false},
		{"mode: atomic\ninvalid row\n", false},
		{"mode: atomic\nfile.go:1.1,2.1 1 0\nfile.go:1.1,2.1 2 1\n", false},
	} {
		command := exec.Command("awk", "-f", "../../scripts/check-coverage.awk")
		command.Stdin = strings.NewReader(test.profile)
		output, err := command.CombinedOutput()
		if (err == nil) != test.success {
			t.Fatalf("gate: %v: %s", err, output)
		}
	}
}
