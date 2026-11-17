// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"errors"
	"syscall"
	"testing"
)

func TestLockErrorsInterruptsAndRollback(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		flockCalls, fcntlCalls := 0, 0
		err := lockFileWith(42, exclusive, func(fd, op int) error {
			flockCalls++
			if fd != 42 {
				t.Fatal(fd)
			}
			if flockCalls == 1 {
				return syscall.EINTR
			}
			want := syscall.LOCK_SH
			if exclusive {
				want = syscall.LOCK_EX
			}
			if op != want {
				t.Fatal(op, want)
			}
			return nil
		}, func(fd uintptr, cmd int, lock *syscall.Flock_t) error {
			fcntlCalls++
			if fd != 42 || cmd != 38 || lock.Start != 0 || lock.Len != 0 || lock.Whence != 0 {
				t.Fatal(fd, cmd, lock)
			}
			want := int16(syscall.F_RDLCK)
			if exclusive {
				want = syscall.F_WRLCK
			}
			if lock.Type != want {
				t.Fatal(lock.Type, want)
			}
			if fcntlCalls == 1 {
				return syscall.EINTR
			}
			return nil
		})
		if err != nil || flockCalls != 2 || fcntlCalls != 2 {
			t.Fatal(err, flockCalls, fcntlCalls)
		}
	}
	if err := lockFileWith(42, true, func(int, int) error { return syscall.EBADF },
		func(uintptr, int, *syscall.Flock_t) error { t.Fatal("fcntl after flock failure"); return nil }); !errors.Is(err, syscall.EBADF) {
		t.Fatal(err)
	}
	unlocked := false
	err := lockFileWith(42, true, func(fd, op int) error {
		if op == syscall.LOCK_UN {
			unlocked = true
			return syscall.EIO
		}
		return nil
	}, func(uintptr, int, *syscall.Flock_t) error { return syscall.ENOTSUP })
	if !unlocked || !errors.Is(err, syscall.ENOTSUP) || !errors.Is(err, syscall.EIO) {
		t.Fatal("partial lock not rolled back with both failures surfaced", err)
	}
}
