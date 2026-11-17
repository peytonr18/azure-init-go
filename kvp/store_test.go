// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T, mode PoolMode) *Store {
	t.Helper()
	store, err := NewStoreIn(Guest, t.TempDir(), mode)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func recordsEqual(t *testing.T, store *Store, want []Record) {
	t.Helper()
	got, err := store.Dump()
	requireNoError(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %#v, want %#v", got, want)
	}
}

func TestStoreSelectionAndMissingFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	for pool := External; pool <= AutoInternal; pool++ {
		store, err := NewStoreIn(pool, dir, Safe)
		requireNoError(t, err)
		if store.Path() != filepath.Join(dir, fmt.Sprintf(".kvp_pool_%d", pool)) ||
			store.Pool() != pool || store.Mode() != Safe {
			t.Fatalf("incorrect selection: %#v", store)
		}
		recordsEqual(t, store, []Record{})
		entries, err := store.Entries()
		requireNoError(t, err)
		if len(entries) != 0 {
			t.Fatal(entries)
		}
		_, found, err := store.Read("absent")
		requireNoError(t, err)
		if found {
			t.Fatal("found missing key")
		}
		n, err := store.Len()
		requireNoError(t, err)
		if n != 0 {
			t.Fatal(n)
		}
		empty, err := store.IsEmpty()
		requireNoError(t, err)
		if !empty {
			t.Fatal("missing pool must be empty")
		}
		stale, err := store.IsStale()
		requireNoError(t, err)
		if stale {
			t.Fatal("missing pool must not be stale")
		}
		removed, err := store.Delete("absent")
		requireNoError(t, err)
		if removed {
			t.Fatal("deleted missing key")
		}
		requireNoError(t, store.Clear())
		requireNoError(t, store.ClearIfStale())
		requireNoError(t, store.AppendMultiple(nil))
		nRemoved, err := store.DeleteMultiple(nil)
		requireNoError(t, err)
		if nRemoved != 0 {
			t.Fatal(nRemoved)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("constructors or no-ops created directory: %v", err)
	}
	if _, err := NewStoreIn(255, dir, Safe); !errors.Is(err, ErrInvalidPool) {
		t.Fatal(err)
	}
	if _, err := NewStoreIn(Guest, dir, 255); !errors.Is(err, ErrInvalidMode) {
		t.Fatal(err)
	}
	store, err := NewStore(Guest, Safe)
	requireNoError(t, err)
	if store.Path() != "/var/lib/hyperv/.kvp_pool_1" {
		t.Fatal(store.Path())
	}
}

func TestBinaryRecordsAndSizePolicies(t *testing.T) {
	for _, mode := range []PoolMode{Safe, Unsafe} {
		t.Run(mode.String(), func(t *testing.T) {
			store := testStore(t, mode)
			wantKey, wantValue := 254, 1022
			if mode == Unsafe {
				wantKey, wantValue = 512, 2048
			}
			if store.MaxKeySize() != wantKey || store.MaxValueSize() != wantValue {
				t.Fatal("write policy does not match the Hyper-V contract")
			}
			key, value := strings.Repeat("k", store.MaxKeySize()), strings.Repeat("v", store.MaxValueSize())
			requireNoError(t, store.Append(key, value))
			recordsEqual(t, store, []Record{{Key: key, Value: value}})
			data, err := os.ReadFile(store.Path())
			requireNoError(t, err)
			want := make([]byte, RecordSize)
			copy(want[:WireMaxKeyBytes], key)
			copy(want[WireMaxKeyBytes:], value)
			if !bytes.Equal(data, want) {
				t.Fatal("incorrect fixed-width UTF-8 pool record")
			}
			info, err := os.Stat(store.Path())
			requireNoError(t, err)
			if info.Mode().Perm() != 0600 {
				t.Fatalf("mode = %o", info.Mode().Perm())
			}
			for _, test := range []struct {
				key, value string
				want       error
			}{
				{"", "", ErrEmptyKey},
				{"a\x00b", "", ErrKeyContainsNull},
				{"a", "b\x00c", ErrValueContainsNull},
				{key + "x", "", ErrKeyTooLarge},
				{"a", value + "x", ErrValueTooLarge},
				{string([]byte{0xff}), "", ErrInvalidUTF8},
				{"a", string([]byte{0xff}), ErrInvalidUTF8},
				{strings.Repeat("\u00e9", store.MaxKeySize()/2+1), "", ErrKeyTooLarge},
				{"a", strings.Repeat("\u00e9", store.MaxValueSize()/2+1), ErrValueTooLarge},
			} {
				if err := store.Append(test.key, test.value); !errors.Is(err, test.want) {
					t.Fatalf("Append(%q, ...): %v, want %v", test.key, err, test.want)
				}
			}
			recordsEqual(t, store, []Record{{Key: key, Value: value}})
		})
	}
}

func TestFixedContractLimits(t *testing.T) {
	if WireMaxKeyBytes != 512 || WireMaxValueBytes != 2048 || RecordSize != 2560 ||
		SafeMaxKeyBytes != 254 || SafeMaxValueBytes != 1022 || MaxUniqueKeys != 1024 ||
		MaxChunkBytes != 1022 || MaxChunks != 1023 {
		t.Fatal("Hyper-V/diagnostic protocol constants changed")
	}
}

func TestDuplicatesAndSwapRemoval(t *testing.T) {
	store := testStore(t, Safe)
	requireNoError(t, store.Load([]Record{{"a", "1"}, {"b", "2"}, {"a", "3"}, {"c", "4"}, {"a", "5"}}))
	value, found, err := store.Read("a")
	requireNoError(t, err)
	if !found || value != "5" {
		t.Fatalf("read = %q, %t", value, found)
	}
	entries, err := store.Entries()
	requireNoError(t, err)
	if !reflect.DeepEqual(entries, map[string]string{"a": "5", "b": "2", "c": "4"}) {
		t.Fatal(entries)
	}
	requireNoError(t, store.Insert("a", "short"))
	recordsEqual(t, store, []Record{{"a", "short"}, {"b", "2"}, {"c", "4"}})
	removed, err := store.Delete("b")
	requireNoError(t, err)
	if !removed {
		t.Fatal("missing deletion")
	}
	recordsEqual(t, store, []Record{{"a", "short"}, {"c", "4"}})
	requireNoError(t, store.AppendMultiple([]Record{{"a", ""}, {"c", ""}, {"a", ""}}))
	count, err := store.DeleteMultiple([]string{"a", "a", "missing"})
	requireNoError(t, err)
	if count != 3 {
		t.Fatal(count)
	}
	recordsEqual(t, store, []Record{{"c", ""}, {"c", "4"}})
	requireNoError(t, store.Insert("c", ""))
	value, found, err = store.Read("c")
	requireNoError(t, err)
	if !found || value != "" {
		t.Fatal("empty value must differ from absence")
	}
}

func TestBulkValidationAndUniqueKeyBudget(t *testing.T) {
	store := testStore(t, Safe)
	requireNoError(t, store.Append("original", "keep"))
	for _, write := range []func([]Record) error{store.Load, store.AppendMultiple} {
		if err := write([]Record{{"valid", "v"}, {"", "invalid"}}); !errors.Is(err, ErrEmptyKey) {
			t.Fatal(err)
		}
		recordsEqual(t, store, []Record{{"original", "keep"}})
	}
	if _, err := store.DeleteMultiple([]string{"original", ""}); !errors.Is(err, ErrEmptyKey) {
		t.Fatal(err)
	}
	recordsEqual(t, store, []Record{{"original", "keep"}})
	records := make([]Record, MaxUniqueKeys)
	for i := range records {
		records[i] = Record{fmt.Sprintf("key-%d", i), "v"}
	}
	requireNoError(t, store.Load(records))
	if err := store.Insert("overflow", "v"); !errors.Is(err, ErrTooManyKeys) {
		t.Fatal(err)
	}
	requireNoError(t, store.Insert("key-0", "updated"))
	tooMany := append(append([]Record{}, records...), Record{"overflow", ""})
	if err := store.Load(tooMany); !errors.Is(err, ErrTooManyKeys) {
		t.Fatal(err)
	}
	requireNoError(t, store.AppendMultiple([]Record{{"overflow", ""}, {"overflow", ""}}))
	requireNoError(t, store.Insert("overflow", "collapsed"))
	count, err := store.Len()
	requireNoError(t, err)
	if count != MaxUniqueKeys+1 {
		t.Fatal(count)
	}
	requireNoError(t, store.Load(nil))
	recordsEqual(t, store, []Record{})
}

func TestSafeStoreReadsAndDeletesUnsafeKeys(t *testing.T) {
	unsafe := testStore(t, Unsafe)
	key := strings.Repeat("x", WireMaxKeyBytes)
	requireNoError(t, unsafe.Append(key, strings.Repeat("v", WireMaxValueBytes)))
	safe, err := NewStoreIn(Guest, filepath.Dir(unsafe.Path()), Safe)
	requireNoError(t, err)
	_, found, err := safe.Read(key)
	requireNoError(t, err)
	if !found {
		t.Fatal("safe reads must not apply write-size limits")
	}
	removed, err := safe.Delete(key)
	requireNoError(t, err)
	if !removed {
		t.Fatal("safe deletes must not apply write-size limits")
	}
}

func TestFramingAndUTF8Failures(t *testing.T) {
	store := testStore(t, Safe)
	requireNoError(t, os.WriteFile(store.Path(), []byte("broken"), 0600))
	for _, operation := range []func() error{
		func() error { _, err := store.Dump(); return err },
		func() error { _, err := store.Len(); return err },
		func() error { return store.Load(nil) },
		func() error { return store.Append("a", "b") },
		func() error { return store.Insert("a", "b") },
		func() error { _, err := store.Delete("a"); return err },
	} {
		if err := operation(); !errors.Is(err, ErrInvalidFraming) {
			t.Fatal(err)
		}
	}
	requireNoError(t, store.Clear())
	data := make([]byte, RecordSize)
	data[0] = 0xff
	requireNoError(t, os.WriteFile(store.Path(), data, 0600))
	if _, err := store.Dump(); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatal(err)
	}
	// Framing-only operations do not interpret existing records.
	requireNoError(t, store.Append("a", "b"))
	requireNoError(t, store.Load([]Record{{"repaired", "yes"}}))
	recordsEqual(t, store, []Record{{"repaired", "yes"}})
}

func TestStaleBoundary(t *testing.T) {
	store := testStore(t, Safe)
	requireNoError(t, store.Append("a", "v"))
	boot, err := bootTime()
	requireNoError(t, err)
	for _, offset := range []int64{-1, 0, 1} {
		requireNoError(t, store.Load([]Record{{"a", "v"}}))
		stamp := time.Unix(boot+offset, 500000000)
		requireNoError(t, os.Chtimes(store.Path(), stamp, stamp))
		stale, err := store.IsStale()
		requireNoError(t, err)
		if stale != (offset <= 0) {
			t.Fatalf("offset=%d stale=%t", offset, stale)
		}
		requireNoError(t, store.ClearIfStale())
		n, err := store.Len()
		requireNoError(t, err)
		if (n == 0) != stale {
			t.Fatalf("offset=%d count=%d", offset, n)
		}
	}
}

func TestConcurrentBatchesDoNotInterleave(t *testing.T) {
	store := testStore(t, Safe)
	const workers, batch = 24, 5
	var wg sync.WaitGroup
	failures := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			records := make([]Record, batch)
			for j := range records {
				records[j] = Record{fmt.Sprintf("writer-%d", i), fmt.Sprint(j)}
			}
			failures <- store.AppendMultiple(records)
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		requireNoError(t, err)
	}
	records, err := store.Dump()
	requireNoError(t, err)
	if len(records) != workers*batch {
		t.Fatalf("lost records: %d", len(records))
	}
	for i := 0; i < len(records); i += batch {
		for j := 0; j < batch; j++ {
			if records[i+j].Key != records[i].Key || records[i+j].Value != fmt.Sprint(j) {
				t.Fatalf("interleaved batch: %v", records[i:i+batch])
			}
		}
	}
}

func TestStoreIOErrors(t *testing.T) {
	store, err := NewStoreIn(Guest, filepath.Join(t.TempDir(), "missing"), Safe)
	requireNoError(t, err)
	if err := store.Append("k", "v"); !os.IsNotExist(err) {
		t.Fatal(err)
	}

	requireNoError(t, os.MkdirAll(store.Path(), 0700))
	if _, err := store.Dump(); err == nil {
		t.Fatal("reading a directory must fail")
	}
}

func TestBootTimeParsing(t *testing.T) {
	got, err := parseBootTime("cpu 1 2 3\nbtime 12345 \nother 1")
	requireNoError(t, err)
	if got != 12345 {
		t.Fatal(got)
	}
	for _, input := range []string{"cpu 1", "btime invalid", "btime 18446744073709551616"} {
		if _, err := parseBootTime(input); err == nil {
			t.Fatal("accepted", input)
		}
	}
}
