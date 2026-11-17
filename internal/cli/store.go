// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import "github.com/peytonr18/azure-init-go/kvp"

type commandStore interface {
	Path() string
	MaxKeySize() int
	MaxValueSize() int
	Len() (int64, error)
	IsStale() (bool, error)
	Dump() ([]kvp.Record, error)
	Entries() (map[string]string, error)
	Read(string) (string, bool, error)
	Insert(string, string) error
	Append(string, string) error
	Load([]kvp.Record) error
	AppendMultiple([]kvp.Record) error
	Delete(string) (bool, error)
	DeleteMultiple([]string) (int, error)
	Clear() error
	ClearIfStale() error
	emit(string, string, string, string) error
	publish(kvp.ProvisioningReport) error
}

type fileStore struct{ *kvp.Store }

func openStore(pool kvp.Pool, dir string, mode kvp.PoolMode) (commandStore, error) {
	store, err := kvp.NewStoreIn(pool, dir, mode)
	if err != nil {
		return nil, err
	}
	return fileStore{store}, nil
}

func (s fileStore) emit(agent, vmID, name, message string) error {
	writer, err := kvp.NewDiagnosticWriter(s.Store, agent, vmID)
	if err != nil {
		return err
	}
	return writer.EmitEvent(name, kvp.TextPayload(message), kvp.Plain, nil, nil)
}

func (s fileStore) publish(report kvp.ProvisioningReport) error {
	return kvp.WriteReport(s.Store, report)
}
