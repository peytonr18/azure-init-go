// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package kvp reads and writes Linux Hyper-V KVP pool files, diagnostics, and
// provisioning reports. It does not send HTTP requests. Pool-file writes are
// local operations; the Hyper-V daemon and kernel handle host transport.
//
// Stores select a path without I/O. Their parent directory must already exist.
// Use Guest and Safe for host-readable telemetry. Operations coordinate with
// cloud-init and hv_kvp_daemon using both Linux file-locking mechanisms.
package kvp
