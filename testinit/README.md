# Isolated module end-to-end tests

This is an adaptation of Azure-init's **isolated** `testinit` suite for the Go
KVP/health module. It runs a real compiled external Go consumer, real pool-file
I/O and file locks, real HTTP sockets, the shipping Go CLI, and optionally the
pinned Rust CLI.

It does **not** run the Azure-init agent or provision a machine. In particular,
it does not create users, change the hostname or SSH configuration, mount
provisioning media, start systemd, or contact real Azure endpoints.

## What was brought over

The source is pinned at
[`82ede83`](https://github.com/Azure/azure-init/tree/82ede83b61e21c34504b4291acc762b5b34db6a3/testinit).
The upstream MIT attribution is retained.

| Upstream concept | Go-module adaptation |
|---|---|
| Real Cargo-built agent subprocess | Real Go-built SDK consumer under `testdata/consumer`, plus the shipping CLI |
| Shared WireServer handler | Strict, loopback-only handler with per-server state |
| Success/failure scenarios | Reporting lifecycle, diagnostics, independent channel failures, retries and deadlines |
| Stored HTTP report and KVP artifacts | Raw HTTP/header capture, byte-exact binary snapshots, stdout/stderr and parsed dumps |
| Isolated Docker lifecycle | Optional nonprivileged, network-disabled, read-only container |
| Rust reads the produced report | Optional Rust parsing of every Go-consumer output pool |

Agent-only IMDS/OVF fallback, user/hostname/SSH setup, provisioning markers, and
cleanup scenarios were not copied: this library does not implement those agent
responsibilities. Copying them would test the Rust agent rather than this Go
module.

The old upstream privileged systemd/Compose setup was deliberately not copied.
Its README warns about changing the host. This adaptation has no host network,
cgroup, D-Bus, `/sys`, `/etc`, or real `/var/lib/hyperv` mounts.

See [upstream.json](upstream.json) for the source mapping and safety differences.

## End-to-end architecture

```text
Python testinit runner
  |
  +-- creates disposable work directory and marked fixture root
  +-- starts strict mock WireServer on 127.0.0.1:<ephemeral port>
  |
  +-- starts compiled Go consumer in another OS process
  |     |
  |     +-- imports the actual kvp and wireserver packages
  |     +-- explicitly selects KVP-only, HTTP-only, both, or HTTP-first
  |     +-- publishes in-progress, diagnostics, and final report
  |     +-- makes real HTTP requests, including retries/deadlines
  |
  +-- snapshots .kvp_pool_1 when HTTP is received
  +-- independently decodes fixed-width binary records and pipe-delimited CSV
  +-- independently reassembles chunks, decodes base64, checks zlib/gzip streams
  +-- verifies the original payload hash, identity, spans and report replacement
  +-- executes the real Go CLI to read/parse the pool
  +-- optionally executes the actual Rust CLI and compares decoded JSON
  +-- saves the complete scenario evidence
```

The consumer is test-only code in Go's conventional `testdata` directory. It is
not a second shipping agent or a new SDK API. Coverage from its calls instruments
the production SDK packages; the test driver itself, like other test code, is
not part of the production-coverage denominator.

Channel orchestration is intentionally in the **consumer**:

- KVP-only cases must generate zero HTTP requests.
- HTTP-only cases must neither create nor change the guest pool.
- HTTP-first must observe the old pool, without an implicitly published report.
- A KVP failure must not prevent an explicitly requested HTTP operation.
- An HTTP failure must not undo the independently successful KVP report.

## Scenarios and assertions

The runner currently executes 19 scenarios:

| Scenarios | What is verified |
|---|---|
| `success`, `fresh-success` | Existing records survive, newly created pools are private, final result is success, HTTP is state-only Ready |
| `failure` | Quoted/multiline reason and ordered duplicate extras survive; HTTP description is the exact stored report |
| `in-progress` | KVP `in_progress` and HTTP NotReady/Provisioning have their distinct expected formats |
| `plain-diagnostics` | Long UTF-8 data is chunked without splitting characters and reassembles correctly |
| `kvp-only` | Publishing through KVP causes no HTTP side effect |
| `http-only`, `http-preserves-kvp` | HTTP neither creates an absent guest pool nor changes an existing one |
| `http-first` | The caller controls ordering; HTTP does not implicitly publish KVP |
| `kvp-io-failure` | Real missing-parent I/O errors are surfaced while independent HTTP delivery still succeeds |
| `http-fatal` | HTTP 403 fails without retries, leaving successful KVP intact |
| `http-deadline`, `request-timeout` | Retry budgets and per-request deadlines are enforced and surfaced |
| `retry-400/404/410/429/500/503` | Every supported retryable status retries, then succeeds with unchanged report context |

Checks include:

- exact method, endpoint, content type and both agent headers;
- exact JSON shape, no silently tolerated extra Ready fields;
- 2,560-byte pool records and strict UTF-8 decoding;
- one replaceable `PROVISIONING_REPORT`, unchanged unrelated records/pools;
- private pool-file permissions;
- correct producer/VM identity, start/finish event correlation and payload hash;
- real multichunk plain/zlib/gzip diagnostics and valid checksums/stream endings;
- unchanged KVP bytes across HTTP/retries;
- surfaced failures through separate KVP/HTTP errors and the consumer exit code.
- measured deadline duration against the configured retry budget, with scheduler
  tolerance, rather than relying only on a generic outer subprocess timeout.

## Commands

Prerequisites for the process suite are Go 1.23+ and Python 3.10+. Python uses
only the standard library. Neither Docker nor Rust is required.

```sh
# Full native suite: vet, race tests, CLI, process testinit, exact 100% gate.
make test

# Only the external-consumer E2E matrix and harness/safety tests.
make test-e2e

# Go compiler override, without changing system Go.
make test-e2e GO=/path/to/go

# Full pinned Rust baseline, differential checks, native and testinit suites.
make test-rust
```

`make test-rust` uses the clean checkout described in the root [README](../README.md).
It also asks the Rust CLI to interpret every testinit pool, in addition to the
normal bidirectional/concurrent-writer differential tests.

For just one scenario, run the underlying entrypoint:

```sh
bash scripts/test-e2e.sh --scenario failure
```

### Optional container isolation

With Docker available and its daemon running:

```sh
make test-e2e-container
make test-e2e-container BASE_IMAGE=debian:12
```

The same scenario code runs inside the image. Each run builds a uniquely named
image and creates a uniquely named disposable container. Only those owned
resources are removed, including on failures/timeouts.

The container has:

- `--network none`: only its own loopback network;
- `--cap-drop ALL`, `no-new-privileges`, and a read-only root filesystem;
- no privileged mode or host namespaces/system mounts;
- read-only mounted consumer/CLI binaries;
- writable mounts only for disposable test artifacts and coverage counters;
- bounded CPU, memory, process counts and execution time.

The consumer additionally requires a marked disposable directory, rejects pool
paths escaping that directory or traversing symlinks, and refuses non-loopback
HTTP destinations. Those guards are tested against the actual executable.

The container target **fails** if Docker is unavailable; it does not pretend a
process run was a container run. CI runs this target on Ubuntu 24.04 and Debian 12.

## Artifacts and coverage

Each invocation creates a new `.coverage/e2e/run-.../` directory. The portable
`.coverage/e2e/latest.json` points to the latest run and records pass/fail plus
the backend, whether Rust re-reading was enabled, and expected/completed scenarios.

Each scenario retains:

- `plan.json`, `result.json`, stdout/stderr and exit-code evidence;
- `requests.json` with raw/decoded bodies, headers, response status and snapshots;
- `http-*.kvp` binary snapshots at request time and `final.kvp`;
- independent pool/report/codec assertions;
- the real Go parsed dump, and the Rust dump when enabled;
- unexpected service-thread errors, which fail the scenario.

`make test` merges coverage from Go test binaries, the real CLI and the external
SDK consumer before checking **every production package** and the exact 100%
executable-statement/block threshold. Reports remain at
`.coverage/coverage.out` and `.coverage/coverage.html`.

## What this does not prove

This is full process/container E2E for the module's public API boundaries. Like
the upstream mock-service `testinit`, it is **not live-Azure E2E**.

It does not prove that `hv_kvp_daemon`/VMBus transported a record to an Azure
host, that the fabric consumed KVP telemetry, or that a real WireServer accepted
the report. Those require a separate disposable Azure VM test environment and
host/fabric observability. The fixtures intentionally cannot be pointed at a
production VM or real Azure health endpoint.
