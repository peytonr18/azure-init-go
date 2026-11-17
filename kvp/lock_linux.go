// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"errors"
	"os"
	"syscall"
)

func lockFile(file *os.File, exclusive bool) error {
	return lockFileWith(file.Fd(), exclusive, syscall.Flock, syscall.FcntlFlock)
}

func lockFileWith(fd uintptr, exclusive bool, flockCall func(int, int) error, fcntlCall func(uintptr, int, *syscall.Flock_t) error) error {
	flock, record := syscall.LOCK_SH, int16(syscall.F_RDLCK)
	if exclusive {
		flock, record = syscall.LOCK_EX, syscall.F_WRLCK
	}
	for {
		err := flockCall(int(fd), flock)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	// Linux UAPI F_OFD_SETLKW (38) is absent from the frozen syscall package.
	// OFD locks share the daemon's POSIX lock domain without process-wide unlocks.
	const fOFDSetLKW = 38
	lock := syscall.Flock_t{Type: record, Whence: int16(0), Start: 0, Len: 0}
	for {
		err := fcntlCall(fd, fOFDSetLKW, &lock)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			// Do not retain half of the two-domain lock if the second fails.
			return errors.Join(err, flockCall(int(fd), syscall.LOCK_UN))
		}
		return nil
	}
}
