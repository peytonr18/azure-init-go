// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestStoreHoldsBothLockDomains(t *testing.T) {
	store := testStore(t, Safe)
	if err := store.Append("key", "value"); err != nil {
		t.Fatal(err)
	}

	for _, exclusive := range []bool{false, true} {
		err := store.withFile(os.O_RDWR, exclusive, true, func(_ *poolFile) error {
			probe, err := os.OpenFile(store.Path(), os.O_RDWR, 0)
			if err != nil {
				return err
			}
			defer probe.Close()
			err = syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				t.Fatalf("flock domain was not locked: %v", err)
			}
			lock := syscall.Flock_t{Type: syscall.F_WRLCK}
			err = syscall.FcntlFlock(probe.Fd(), syscall.F_SETLK, &lock)
			if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EACCES) {
				t.Fatalf("classic POSIX fcntl domain was not locked: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
