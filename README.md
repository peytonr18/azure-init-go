# azure-init-go

A library-first Go port of Azure's `libazureinit-kvp`, with independent JSON
WireServer health reporting. Intended for Linux provisioning agents and other
consumers that need the functionality without a Rust runtime or the azure-init
agent.

Requires Go 1.23 or newer. There are no third-party Go dependencies and no cgo.
Use a currently supported Go release for production.
The canonical test suite also needs Python 3.10+ (standard library only).
Rust/Cargo and Docker are optional test-layer dependencies, not runtime
dependencies for consumers.

## Status and upstream references

The library includes pool storage, provisioning reports and
health helpers, native/cloud-init diagnostic reading, diagnostic writing, the
KVP CLI, and direct HTTP health reporting.

`make test` enforces **100% coverage of all Linux production Go executable
statements/blocks**, including the actual CLI entrypoint, with no package
exclusions. Go reports statements rather than literal text lines; comments and
type declarations are not executable code. The gate checks exact counter totals,
not rounded percentages.

The pinned Rust baseline has 755 unit cases, 85 CLI integration cases,
39 diagnostic integration cases, and 4 documentation tests. All 883 cases are
inventoried in [testdata/rust-tests.txt](testdata/rust-tests.txt) and mapped to
Go contract-test families in [testdata/parity.json](testdata/parity.json).
`make test-rust` runs that original baseline, verifies the live case inventory,
and runs bidirectional differential tests, including generated duration cases,
captured cloud-init records, CLI argument/help contracts, and mixed Rust/Go
writers.

The mapping is by observable contract, not identical private implementation:
Go uses positioned I/O instead of Rust's cursor iterator, so seek/iterator/mock
mechanics map to Go read/write/removal and fault-injection tests. Coverage alone
is not proof of parity; the independent fixtures and live Rust comparisons are
also required.

The adapted [testinit suite](testinit/README.md) adds 19 process-level E2E
scenarios using a real external SDK consumer, real pool files, a strict mock
WireServer, independent binary/codec checks, the Go CLI and optional Rust
re-reading. An optional hardened Docker backend runs the same suite. These are
hermetic process/container tests, not a claim of live Azure VM/host delivery
validation.

References are pinned, rather than following a moving branch:

- Main: [`5bf373c`](https://github.com/Azure/azure-init/tree/5bf373c219f223ddf7839229784d95fab1db4254).
- KVP plus the health helpers from
  [Azure/azure-init#370](https://github.com/Azure/azure-init/pull/370):
  [`82ede83`](https://github.com/Azure/azure-init/tree/82ede83b61e21c34504b4291acc762b5b34db6a3/libazureinit-kvp).
  This revision contains the pinned main plus the PR's changes.
- JSON health transport:
  [`wireserver.rs`](https://github.com/Azure/azure-init/blob/5bf373c219f223ddf7839229784d95fab1db4254/libazureinit/src/wireserver.rs)
  and its HTTP/configuration dependencies.

See [testdata/upstream.json](testdata/upstream.json) for machine-readable provenance.
The upstream MIT license and attribution are preserved in [LICENSE](LICENSE).

## Packages

| Go surface | Responsibility | Rust source |
|---|---|---|
| [`kvp.Store`](kvp/store.go) | Pool files, modes, validation, bulk operations, locking | `store.rs` |
| [`kvp.ProvisioningReport`](kvp/report.go) | Report codecs and KVP-only health helpers | `report.rs`, including Azure/azure-init#370 |
| [`kvp.DiagnosticWriter`](kvp/writer.go), [`kvp.DiagnosticReader`](kvp/reader.go) | Native diagnostics, compression/chunks, cloud-init decoding, raw-record preservation | `diagnostics/` |
| [`wireserver.Client`](wireserver/client.go) | HTTP-only ready, failure, and in-progress delivery | `wireserver.rs`, relevant `http.rs` logic |
| [`cmd/azure-init-kvp`](cmd/azure-init-kvp) | Thin command interface over the library | `cli.rs`, VM identity lookup |

Storage, report, and diagnostic APIs never make HTTP requests. HTTP reporting
never opens or modifies a pool file. Callers explicitly choose either operation
or both and handle each outcome independently; there is no automatic fallback.

## KVP publication

```go
import "github.com/peytonr18/azure-init-go/kvp"

func publishReady(vmID string) error {
    store, err := kvp.NewStore(kvp.Guest, kvp.Safe)
    if err != nil {
        return err
    }
    return kvp.ReportSuccess(store, "my-agent/1.0", vmID, kvp.PPSNone)
}
```

`ReportInProgress` and `ReportFailure` provide the corresponding one-call KVP
operations. For supporting data or for sharing a failure report with the HTTP
client, construct a report explicitly:

```go
report := kvp.NewFailureReport("my-agent/1.0", vmID, "provisioning failed", kvp.PPSNone).
    WithExtra("stage", "configure-network").
    WithDocumentationURL("https://example.com/troubleshooting")
err := kvp.WriteReport(store, report)
```

Reports replace the exact `PROVISIONING_REPORT` key, leaving diagnostics intact.
They must fit in one value. Publishing in-progress early reserves that key
before diagnostics exhaust the distinct-key budget. Parsing preserves timestamp
spellings and ordered, possibly duplicate supporting-data fields.

## Independent HTTP health reporting

```go
import (
    "context"

    "github.com/peytonr18/azure-init-go/wireserver"
)

func reportAzureReady(ctx context.Context) error {
    config := wireserver.DefaultConfig()
    config.AgentName = "my-agent/1.0"
    client, err := wireserver.NewClient(config)
    if err != nil {
        return err
    }
    defer client.CloseIdleConnections()
    return client.ReportReady(ctx)
}
```

For ongoing work, call `client.ReportInProgress(ctx, vmID)`. For failure, call
`client.ReportFailure(ctx, report)` with a `kvp.ProvisioningReport`. Failure
delivery uses that report's encoding without changing its timestamp or context.
The client is reusable and safe for concurrent calls.

Defaults match the upstream timing policy: 60-second connection/request/retry
intervals and a 20-minute overall retry budget. The endpoint is
`http://168.63.129.16/provisioning/health`. All 2xx statuses succeed; 400, 404,
410, 429, 500, and 503 retry, as do transport failures. Other HTTP statuses fail
immediately. Caller cancellation and deadlines bound both requests and waits.
`errors.Is` recognizes cancellation/deadlines; `HTTPStatusError` exposes fatal
statuses and `DeliveryError` retains the last retry failure.

## Diagnostics

```go
writer, err := kvp.NewDiagnosticWriter(store, "my-agent/1.0", vmID)
if err != nil {
    return err
}
err = writer.EmitEvent("provision:log", kvp.TextPayload("configuration started"),
    kvp.ZlibBase64, nil, nil)
```

- Start/finish APIs take a caller-supplied event UUID; standalone events generate
  one using `crypto/rand`.
- Timestamp precision defaults to milliseconds; duration precision defaults to
  microseconds. Writer options configure them independently.
- `kvp.Duration` preserves Rust's full unsigned-second range. Use
  `DurationFromTime`/`TimeDuration` for checked conversion to Go durations.
- `kvp.Timestamp` preserves RFC 3339 leap seconds in addition to an embedded
  `time.Time`; `TimestampString` returns the canonical UTC representation.
- `Plain`, `ZlibBase64`, and `GzipBase64` are distinct native encodings.
  Compressed payloads decode as bytes, not implicitly as text.
- The reader handles current and legacy cloud-init records, including zlib data
  labeled `gz+b64` and JSON escapes split across chunks.
- Invalid groups remain raw records with decoding errors in their original
  positions. Unknown records are preserved. Reads do not rewrite the pool.

## Storage contract

- Linux pool records are 2,560 bytes: a 512-byte UTF-8 key field followed by a
  2,048-byte UTF-8 value field, zero padded.
- `Safe` writes allow 254 key bytes and 1,022 value bytes. `Unsafe` permits full
  field widths, which may be truncated during host transport. Limits count
  **bytes**, not runes.
- `Append`/`AppendMultiple` retain duplicates without a record-count cap.
  `Insert` collapses matching duplicates; adding a new distinct key is limited
  to 1,024 keys. `Load` replaces the pool, preserving duplicates and input order,
  with the same distinct-key limit.
- `Dump` retains physical order/duplicates; `Entries` and `Read` use the last
  value. Deletion uses upstream's tail-swap behavior and may reorder records.
- `NewStoreIn` selects a test/custom directory without I/O. Parent directories
  must already exist. Newly created files use mode `0600`, subject to umask.
- Each operation opens its own descriptor and holds both `flock` and Linux OFD
  `fcntl` locks, coordinating with cloud-init and `hv_kvp_daemon`. Unsupported
  locks produce errors rather than falling back to an incompatible lock. A
  second-lock failure rolls back the first lock; cleanup errors are preserved.
- Inputs are validated before batch writes. Storage errors can leave partial
  writes. `Load` rejects malformed existing framing; `Clear` can repair it.
- Writing a file is not an Azure acknowledgement. The existing Hyper-V
  daemon/kernel move KVP data to the host on the host's schedule.
- Directory selection preserves symlink/`..` traversal rather than lexically
  cleaning the caller's path.

Go-specific choices are explicit: invalid enum values/UTF-8 return errors;
optional reads distinguish absence from an empty value; DMI read/Gen1 conversion
failures return errors rather than logging and returning an optional or invalid
identity. The CLI identifies itself as `azure-init-go/0.1.0`; its
generated help/error wording is not byte-identical to clap's output.

## CLI

```sh
go build ./cmd/azure-init-kvp
./azure-init-kvp --help
./azure-init-kvp --dir /existing/test-directory write example value
./azure-init-kvp --dir /existing/test-directory dump --parse
```

The storage, diagnostic, and report commands, global flags, output shapes,
supporting-data quoting, and exit-code meanings follow the Rust CLI:
0 success, 1 not found/not stale, 2 usage/validation, 3 I/O failure.
There are deliberately no implicit HTTP operations.

## Validation and Rust interoperability

```sh
make test
make test-e2e
make test-e2e-container
make fuzz
```

`make test` runs vet, race-enabled native/functional tests, an instrumented real
CLI command matrix, process-level testinit E2E, coverage-data merging,
package-inclusion verification, and
the exact 100% gate. Reports are written to `.coverage/coverage.out` and
`.coverage/coverage.html`. Temporary counter directories are isolated per run
and cleaned afterward, so previous results cannot hide a new coverage gap.
Individual test-binary percentages printed by `go test` can be lower: the
authoritative total is the final merged result across all test binaries and
the separately instrumented CLI and SDK consumer.

An alternate supported toolchain can be selected without changing system Go:

```sh
make test GO=/path/to/go
```

Plain `go test -race ./...` is useful for focused development, but is not the
complete coverage gate: it cannot merge coverage from the separately executed
CLI by itself.

Ordinary Go tests do not require Rust. The optional interoperability suite uses
the **actual pinned Rust library through its CLI**, not a reimplementation:

```sh
git clone https://github.com/Azure/azure-init.git .rust-reference
git -C .rust-reference fetch origin 82ede83b61e21c34504b4291acc762b5b34db6a3
git -C .rust-reference checkout --detach 82ede83b61e21c34504b4291acc762b5b34db6a3
make test-rust
```

For an existing checkout, use `RUST_SOURCE=/path/to/azure-init make test-rust`.
The command refuses an incorrect revision or dirty source tree, uses the locked
Rust dependencies, and reuses an ignored `.coverage/rust-target` build cache.
The checkout must include the pinned commit (fetch it explicitly if using a
shallow clone).

For a prebuilt reference binary, the focused differential checks can also run
with `AZURE_INIT_RUST_KVP=/path/to/libazureinit-kvp go test -race ./internal/compat`.
Without the reference environment, only the live-Rust tests skip; the native
100% gate and checked-in fixtures do not depend on Rust.
The Rust-enabled full command also re-reads testinit's Go-consumer pools using
the actual Rust CLI.

These tests compare Go-written data read by Rust, Rust-written data read by Go,
CLI behavior, malformed/edge-case decoding, and simultaneous writers sharing
one pool. Contractual binary/report formats are compared exactly; decoded JSON
is compared semantically, allowing legitimate compressed-byte differences.

[CI](.github/workflows/test.yml) enforces the full gate on the minimum and stable
Go versions and in the pinned Rust job, and publishes coverage reports.
It additionally runs the unprivileged testinit container suite on Ubuntu 24.04
and Debian 12. See [testinit/README.md](testinit/README.md) for the full execution
model, safety boundaries, scenario matrix and artifact layout.
