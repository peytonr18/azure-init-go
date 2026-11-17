// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errInjectedIO = errors.New("injected filesystem failure")

type faultHandle struct {
	poolHandle
	stage    string
	at       int
	counts   map[string]int
	closeErr error
}

func (f *faultHandle) fail(stage string) error {
	f.counts[stage]++
	if f.stage == stage && f.counts[stage] == f.at {
		return errInjectedIO
	}
	return nil
}
func (f *faultHandle) ReadAt(data []byte, offset int64) (int, error) {
	if err := f.fail("read"); err != nil {
		return 0, err
	}
	return f.poolHandle.ReadAt(data, offset)
}
func (f *faultHandle) WriteAt(data []byte, offset int64) (int, error) {
	if err := f.fail("write"); err != nil {
		return 0, err
	}
	return f.poolHandle.WriteAt(data, offset)
}
func (f *faultHandle) Stat() (os.FileInfo, error) {
	if err := f.fail("stat"); err != nil {
		return nil, err
	}
	return f.poolHandle.Stat()
}
func (f *faultHandle) Truncate(size int64) error {
	if err := f.fail("truncate"); err != nil {
		return err
	}
	return f.poolHandle.Truncate(size)
}
func (f *faultHandle) lock(exclusive bool) error {
	if err := f.fail("lock"); err != nil {
		return err
	}
	return f.poolHandle.lock(exclusive)
}
func (f *faultHandle) Close() error {
	return errors.Join(f.poolHandle.Close(), f.closeErr, f.fail("close"))
}

type faultOps struct {
	storeOps
	stage    string
	at       int
	counts   map[string]int
	closeErr error
}

func (o *faultOps) open(path string, flags int) (poolHandle, error) {
	if o.stage == "open" {
		o.counts["open"]++
		return nil, errInjectedIO
	}
	handle, err := o.storeOps.open(path, flags)
	if err != nil {
		return nil, err
	}
	return &faultHandle{handle, o.stage, o.at, o.counts, o.closeErr}, nil
}
func (o *faultOps) stat(path string) (os.FileInfo, error) {
	if o.stage == "path-stat" {
		o.counts["path-stat"]++
		return nil, errInjectedIO
	}
	return o.storeOps.stat(path)
}
func (o *faultOps) bootTime() (int64, error) {
	if o.stage == "boot" {
		o.counts["boot"]++
		return 0, errInjectedIO
	}
	return o.storeOps.bootTime()
}

func TestStorePropagatesEveryFilesystemFailure(t *testing.T) {
	operations := []struct {
		name   string
		call   func(*Store) error
		stages map[string][]int
	}{
		{"append", func(s *Store) error { return s.Append("new", "value") }, map[string][]int{"write": {1}}},
		{"append-batch", func(s *Store) error { return s.AppendMultiple([]Record{{"new", "1"}, {"new2", "2"}}) }, map[string][]int{"write": {1, 2}}},
		{"insert", func(s *Store) error { return s.Insert("a", "updated") }, map[string][]int{"read": {1, 2, 3}, "write": {1, 2}, "truncate": {1}}},
		{"insert-new", func(s *Store) error { return s.Insert("new", "value") }, map[string][]int{"read": {1}, "write": {1}}},
		{"load", func(s *Store) error { return s.Load([]Record{{"new", "1"}, {"new2", "2"}}) }, map[string][]int{"truncate": {1}, "write": {1, 2}}},
		{"dump", func(s *Store) error { _, err := s.Dump(); return err }, map[string][]int{"read": {1}}},
		{"entries", func(s *Store) error { _, err := s.Entries(); return err }, map[string][]int{"read": {1}}},
		{"read", func(s *Store) error { _, _, err := s.Read("a"); return err }, map[string][]int{"read": {1}}},
		{"delete", func(s *Store) error { _, err := s.Delete("a"); return err }, map[string][]int{"read": {1, 2}, "write": {1}, "truncate": {1}}},
		{"delete-batch", func(s *Store) error { _, err := s.DeleteMultiple([]string{"a", "b"}); return err }, map[string][]int{"read": {1, 2}, "write": {1}, "truncate": {1}}},
		{"clear", func(s *Store) error { return s.Clear() }, map[string][]int{"truncate": {1}}},
		{"clear-stale", func(s *Store) error { return s.ClearIfStale() }, map[string][]int{"boot": {1}}},
	}
	for _, operation := range operations {
		stages := map[string][]int{"open": {1}, "lock": {1}, "close": {1}}
		if operation.name != "clear" {
			stages["stat"] = []int{1}
		}
		for stage, positions := range operation.stages {
			stages[stage] = positions
		}
		for stage, positions := range stages {
			for _, at := range positions {
				t.Run(fmt.Sprintf("%s/%s-%d", operation.name, stage, at), func(t *testing.T) {
					store := testStore(t, Safe)
					requireNoError(t, store.Load([]Record{{"a", "1"}, {"a", "2"}, {"b", "3"}, {"c", "4"}}))
					counts := make(map[string]int)
					store.ops = &faultOps{storeOps: osStoreOps{}, stage: stage, at: at, counts: counts}
					if err := operation.call(store); !errors.Is(err, errInjectedIO) {
						t.Fatalf("lost %s failure at call %d: %v", stage, at, err)
					}
					if counts[stage] != at {
						t.Fatalf("expected failure at %s call %d, saw %d", stage, at, counts[stage])
					}
					store.ops = osStoreOps{}
					requireNoError(t, store.Append("after-failure", "locks released"))
				})
			}
		}
	}
}

func TestStoreJoinsOperationAndCloseFailures(t *testing.T) {
	store := testStore(t, Safe)
	requireNoError(t, store.Append("key", "value"))
	closeError := errors.New("injected close failure")
	store.ops = &faultOps{storeOps: osStoreOps{}, stage: "read", at: 1, counts: make(map[string]int), closeErr: closeError}
	_, err := store.Dump()
	if !errors.Is(err, errInjectedIO) || !errors.Is(err, closeError) {
		t.Fatalf("operation/cleanup failures must both remain inspectable: %v", err)
	}
}

func TestStoreMetadataAndBootFailures(t *testing.T) {
	store := testStore(t, Safe)
	requireNoError(t, store.Append("key", "value"))
	for _, operation := range []func() error{
		func() error { _, err := store.Len(); return err },
		func() error { _, err := store.IsStale(); return err },
	} {
		store.ops = &faultOps{storeOps: osStoreOps{}, stage: "path-stat", counts: make(map[string]int)}
		if err := operation(); !errors.Is(err, errInjectedIO) {
			t.Fatal(err)
		}
	}
	store.ops = &faultOps{storeOps: osStoreOps{}, stage: "boot", counts: make(map[string]int)}
	if _, err := store.IsStale(); !errors.Is(err, errInjectedIO) {
		t.Fatal(err)
	}
	if _, err := readBootTime(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "stat")
	requireNoError(t, os.WriteFile(path, []byte("btime 42\n"), 0600))
	got, err := readBootTime(path)
	requireNoError(t, err)
	if got != 42 {
		t.Fatal(got)
	}
}

type shortWriterAt struct{}

func (shortWriterAt) WriteAt(data []byte, offset int64) (int, error) { return len(data) - 1, nil }

func TestIncompleteRecordsAndShortWrites(t *testing.T) {
	if err := writeAt(shortWriterAt{}, []byte("record"), 0); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	for _, size := range []int64{-1, 1, RecordSize - 1, RecordSize + 1} {
		if _, err := recordCount(size); !errors.Is(err, ErrInvalidFraming) {
			t.Fatal(size, err)
		}
	}
	store := testStore(t, Safe)
	data := make([]byte, RecordSize)
	copy(data, "valid-key")
	data[WireMaxKeyBytes] = 0xff
	requireNoError(t, os.WriteFile(store.Path(), data, 0600))
	if _, err := store.Dump(); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatal(err)
	}
	if _, err := NewDiagnosticReader(store).Entries(); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatal(err)
	}
	if _, _, err := store.Read(""); !errors.Is(err, ErrEmptyKey) {
		t.Fatal(err)
	}
}

func TestPoolNamesAndUnnormalizedPaths(t *testing.T) {
	for pool, want := range map[Pool]string{External: "external", Guest: "guest", Auto: "auto", AutoExternal: "auto-external", AutoInternal: "auto-internal", 255: "Pool(255)"} {
		if pool.String() != want {
			t.Fatal(pool.String(), want)
		}
	}
	if PoolMode(255).String() != "PoolMode(255)" {
		t.Fatal(PoolMode(255).String())
	}
	for _, dir := range []string{"", "/", "/tmp/", "/tmp/a/../b", "/tmp//a"} {
		store, err := NewStoreIn(Guest, dir, Safe)
		requireNoError(t, err)
		if dir != "" && !strings.HasSuffix(dir, "/") {
			dir += "/"
		}
		if store.Path() != dir+".kvp_pool_1" {
			t.Fatal(store.Path())
		}
	}
	base := t.TempDir()
	target := filepath.Join(base, "target")
	requireNoError(t, os.MkdirAll(filepath.Join(target, "nested"), 0700))
	requireNoError(t, os.Mkdir(filepath.Join(target, "pool"), 0700))
	requireNoError(t, os.Symlink(filepath.Join(target, "nested"), filepath.Join(base, "alias")))
	store, err := NewStoreIn(Guest, base+"/alias/../pool", Safe)
	requireNoError(t, err)
	requireNoError(t, store.Append("key", "value"))
	if _, err := os.Stat(filepath.Join(target, "pool", ".kvp_pool_1")); err != nil {
		t.Fatal("constructor changed filesystem symlink traversal", err)
	}
}
