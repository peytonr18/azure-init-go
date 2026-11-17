// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"io"
	"os"
)

type poolHandle interface {
	io.ReaderAt
	io.WriterAt
	Stat() (os.FileInfo, error)
	Truncate(int64) error
	Close() error
	Name() string
	lock(bool) error
}

type storeOps interface {
	open(string, int) (poolHandle, error)
	stat(string) (os.FileInfo, error)
	bootTime() (int64, error)
}

type osStoreOps struct{}

func (osStoreOps) open(path string, flags int) (poolHandle, error) {
	file, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	return systemFile{file}, nil
}

func (osStoreOps) stat(path string) (os.FileInfo, error) { return os.Stat(path) }
func (osStoreOps) bootTime() (int64, error)              { return bootTime() }

type systemFile struct{ *os.File }

func (f systemFile) lock(exclusive bool) error { return lockFile(f.File, exclusive) }

func writeAt(file io.WriterAt, data []byte, offset int64) error {
	n, err := file.WriteAt(data, offset)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}
