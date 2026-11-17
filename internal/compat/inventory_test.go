// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package compat

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func rustTestNames(data []byte) []string {
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasSuffix(line, ": test") {
			names = append(names, strings.TrimSuffix(line, ": test"))
		}
	}
	sort.Strings(names)
	return names
}

func TestParityContractInventory(t *testing.T) {
	var mapping struct {
		Reference string `json:"reference"`
		Inventory string `json:"inventory"`
		Rules     []struct {
			Prefix string   `json:"rust_prefix"`
			Cases  int      `json:"cases"`
			Tests  []string `json:"go_tests"`
		} `json:"rules"`
		Additional []string `json:"additional_go_contracts"`
	}
	data, err := os.ReadFile("../../testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &mapping); err != nil {
		t.Fatal(err)
	}
	var upstream struct {
		Revision string `json:"kvp_revision"`
	}
	data, err = os.ReadFile("../../testdata/upstream.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &upstream); err != nil {
		t.Fatal(err)
	}
	if mapping.Reference != upstream.Revision {
		t.Fatal("parity inventory does not match pinned source")
	}
	data, err = os.ReadFile(filepath.Join("../../testdata", mapping.Inventory))
	if err != nil {
		t.Fatal(err)
	}
	names := rustTestNames(data)
	if len(names) != 883 {
		t.Fatalf("expected 883 pinned Rust cases; got %d", len(names))
	}
	counts := make([]int, len(mapping.Rules))
	targets := append([]string(nil), mapping.Additional...)
	for _, name := range names {
		found := false
		for i, rule := range mapping.Rules {
			if strings.HasPrefix(name, rule.Prefix) {
				counts[i]++
				found = true
				break
			}
		}
		if !found {
			t.Fatal("unmapped Rust test", name)
		}
	}
	for i, rule := range mapping.Rules {
		if counts[i] != rule.Cases || len(rule.Tests) == 0 {
			t.Fatal("incorrect contract mapping", rule.Prefix, counts[i], rule.Cases)
		}
		targets = append(targets, rule.Tests...)
	}
	known := make(map[string]bool)
	files, err := filepath.Glob("../../*/*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	internal, err := filepath.Glob("../../internal/*/*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, internal...)
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && strings.HasPrefix(fn.Name.Name, "Test") {
				dir, err := filepath.Rel("../..", filepath.Dir(path))
				if err != nil {
					t.Fatal(err)
				}
				known[filepath.ToSlash(dir)+"/"+fn.Name.Name] = true
			}
		}
	}
	for _, target := range targets {
		if !known[target] {
			t.Fatal("missing mapped Go test", target)
		}
	}
}

func TestPinnedRustTestInventory(t *testing.T) {
	source := os.Getenv("AZURE_INIT_RUST_SOURCE")
	if source == "" {
		t.Skip("make test-rust verifies the live pinned Rust test inventory")
	}
	args := []string{"test", "--manifest-path", filepath.Join(source, "Cargo.toml"), "-p", "libazureinit-kvp", "--locked", "--quiet"}
	if target := os.Getenv("AZURE_INIT_RUST_TARGET"); target != "" {
		args = append(args, "--target-dir", target)
	}
	args = append(args, "--", "--list")
	command := exec.Command("cargo", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	expected, err := os.ReadFile("../../testdata/rust-tests.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rustTestNames(output), rustTestNames(expected)) {
		t.Fatal("pinned Rust test cases changed; review and update the contract mapping")
	}
}
