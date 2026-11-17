// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/peytonr18/azure-init-go/internal/cli"
	"github.com/peytonr18/azure-init-go/kvp"
)

func TestGeneratedDurationParity(t *testing.T) {
	binary := rustBinary(t)
	store, err := kvp.NewStoreIn(kvp.Guest, t.TempDir(), kvp.Unsafe)
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(370))
	values := []string{"0.0000000015", "0.0000000025", "18446744073709551615.000000001",
		"0.9999999995", "-0.0000000001", "1e-9999", "1e9999"}
	for i := 0; i < 512; i++ {
		values = append(values, strconv.FormatFloat(math.Float64frombits(random.Uint64()), 'g', -1, 64))
	}
	for i, value := range values {
		t.Run(fmt.Sprintf("%d-%s", i, value), func(t *testing.T) {
			key := "DIAG|agent|00112233-4455-6677-8899-aabbccddeeff|event|op|event|2026-01-01T00:00:00Z|none||" + value + "|0"
			if err := store.Load([]kvp.Record{{Key: key, Value: "text"}}); err != nil {
				t.Fatal(err)
			}
			compareReaders(t, binary, store)
		})
	}
}

func TestCapturedCloudInitDifferential(t *testing.T) {
	binary := rustBinary(t)
	var fixture struct {
		Records []kvp.Record `json:"records"`
	}
	data, err := os.ReadFile("../../testdata/cloud-init.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	store, err := kvp.NewStoreIn(kvp.Guest, t.TempDir(), kvp.Unsafe)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Load(fixture.Records); err != nil {
		t.Fatal(err)
	}
	compareReaders(t, binary, store)
}

func TestCLIArgumentAndHelpParity(t *testing.T) {
	binary := rustBinary(t)
	cases := [][]string{
		{"--help"}, {"--help", "unknown"}, {"unknown", "--help"}, {"help", "--help"}, {"help", "help"},
		{"--pool", "invalid", "--help"}, {"--pool", "guest", "--help"}, {"dump", "--name", "op", "--help"},
		{"--name", "op", "emit", "--message", "text"}, {"info", "--append"}, {"info", "--json=true"},
		{"--version"}, {"write", "--", "-key", "-value"},
		{"emit", "--message", "--help"}, {"emit", "--name", "op", "--message", "-", "--vm-id", "00112233-4455-6677-8899-aabbccddeeff"},
		{"report-failure", "--reason=-value", "--vm-id", "vm"}, {"dump", "--parse", "--kind=invalid", "--help"},
		{"dump", "--name", "op"}, {"dump", "--kind", "event"}, {"info", "--json", "--text"},
	}
	for _, topic := range strings.Fields("info dump entries read write emit load append-multiple delete delete-multiple clear is-stale report-in-progress report-success report-failure") {
		cases = append(cases, []string{"help", topic}, []string{topic, "--help"})
	}
	optionPattern := regexp.MustCompile(`(?m)^\s+(?:-[a-z],\s*)?--([a-z][a-z-]*)`)
	options := func(data []byte) []string {
		var result []string
		seen := make(map[string]bool)
		for _, match := range optionPattern.FindAllSubmatch(data, -1) {
			name := string(match[1])
			if !seen[name] {
				result = append(result, name)
				seen[name] = true
			}
		}
		sort.Strings(result)
		return result
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := cli.Run(append([]string{"--dir", t.TempDir()}, args...), strings.NewReader(""), &out, &stderr)
			rustCode, rustOut, rustErr := rust(t, binary, t.TempDir(), "", args...)
			if code != rustCode || (stderr.Len() == 0) != (len(rustErr) == 0) {
				t.Fatalf("Go=%d %s; Rust=%d %s", code, stderr.String(), rustCode, rustErr)
			}
			if code == 0 && bytes.Contains(rustOut, []byte("Usage:")) {
				if !reflect.DeepEqual(options(out.Bytes()), options(rustOut)) {
					t.Fatalf("help options differ\nGo: %s\nRust: %s", out.String(), rustOut)
				}
			}
		})
	}
}
