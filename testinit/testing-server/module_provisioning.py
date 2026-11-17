# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Isolated testinit adaptation: a real Go SDK consumer and mock WireServer.

Derived from Azure/azure-init testinit's container_provisioning.py scenario and
artifact model, without agent provisioning, privileged containers, or host edits.
"""

import argparse
import hashlib
from http.client import HTTPConnection
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest

import kvp_decode
from wireserver_handler import Response, State, WireServer


AGENT = "testinit-go/1"
VM_ID = "00000000-0000-0000-0000-000000000000"
REASON = 'fixture failure | quoted "reason"\nwith a second line'
SCENARIOS = {
    "success": {},
    "fresh-success": {"seed": False},
    "failure": {"state": "failure", "encoding": "gz+b64"},
    "in-progress": {"state": "in-progress", "diagnostics": False},
    "plain-diagnostics": {"encoding": "none"},
    "kvp-only": {"http": False},
    "http-only": {"kvp": False, "diagnostics": False, "seed": False},
    "http-preserves-kvp": {"kvp": False, "diagnostics": False},
    "http-first": {"order": "http-first"},
    "kvp-io-failure": {"bad_dir": True, "state": "failure", "diagnostics": False},
    "http-fatal": {"statuses": [403]},
    "http-deadline": {"statuses": [503], "total_ms": 400},
    "request-timeout": {"delay": 0.4, "request_ms": 80, "total_ms": 600},
}
for status in (400, 404, 410, 429, 500, 503):
    SCENARIOS[f"retry-{status}"] = {"statuses": [status, 201], "state": "failure"}


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def encode_record(key, value):
    key, value = key.encode(), value.encode()
    if len(key) > 512 or len(value) > 2048:
        raise ValueError("oversized seed record")
    return key.ljust(512, b"\0") + value.ljust(2048, b"\0")


def child_environment(coverage):
    environment = dict(os.environ)
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        environment.pop(name, None)
    environment["NO_PROXY"] = environment["no_proxy"] = "127.0.0.1,localhost"
    environment["GOMAXPROCS"] = "2"
    if coverage:
        environment["GOCOVERDIR"] = str(coverage)
    return environment


def execute(command, artifacts, stem, environment, input_data=None):
    with (artifacts / f"{stem}.stdout").open("wb") as out, (
        artifacts / f"{stem}.stderr"
    ).open("wb") as error:
        completed = subprocess.run(
            [str(part) for part in command],
            input=input_data,
            stdout=out,
            stderr=error,
            env=environment,
            timeout=10,
            check=False,
        )
    return (
        completed.returncode,
        (artifacts / f"{stem}.stdout").read_text(),
        (artifacts / f"{stem}.stderr").read_text(),
    )


def check_case(name, options, consumer, cli, artifacts, coverage, rust=None):
    artifacts.mkdir()
    work = artifacts / "work"
    work.mkdir()
    (work / ".azure-init-go-testinit").write_text("isolated azure-init-go fixture\n")
    pool_dir = work / ("missing/pool" if options.get("bad_dir") else "pool")
    seed = None
    if not options.get("bad_dir"):
        pool_dir.mkdir()
        (pool_dir / ".kvp_pool_0").write_bytes(b"untouched external pool")
        if options.get("seed", True):
            seed = encode_record("existing-sentinel", "preserve this record")
            (pool_dir / ".kvp_pool_1").write_bytes(seed)
            (pool_dir / ".kvp_pool_1").chmod(0o600)
    pool = pool_dir / ".kvp_pool_1"
    statuses = options.get("statuses", [201])
    state = State(
        pool,
        artifacts,
        AGENT,
        [Response(status, options.get("delay", 0)) for status in statuses],
    )
    plan = {
        "pool_dir": str(pool_dir),
        "state": options.get("state", "success"),
        "order": options.get("order", "kvp-first"),
        "agent": AGENT,
        "vm_id": VM_ID,
        "reason": REASON,
        "kvp": options.get("kvp", True),
        "http": options.get("http", True),
        "diagnostics": options.get("diagnostics", True),
        "encoding": options.get("encoding", "zlib+b64"),
        "request_ms": options.get("request_ms", 300),
        "retry_ms": 10,
        "total_ms": options.get("total_ms", 1500),
    }
    environment = child_environment(coverage)
    environment["AZURE_INIT_TEST_ROOT"] = str(work)
    with WireServer(state) as server:
        plan["endpoint"] = f"http://127.0.0.1:{server.server_port}/provisioning/health"
        thread = threading.Thread(
            target=server.serve_forever, kwargs={"poll_interval": 0.01}
        )
        thread.start()
        try:
            connection = HTTPConnection("127.0.0.1", server.server_port, timeout=2)
            try:
                connection.request("GET", "/healthz")
                response = connection.getresponse()
                require(response.status == 200, "mock WireServer did not become ready")
                response.read()
            finally:
                connection.close()
            (artifacts / "plan.json").write_text(json.dumps(plan, indent=2))
            code, out, error = execute(
                [consumer], artifacts, "consumer", environment, json.dumps(plan).encode()
            )
        finally:
            server.shutdown()
            thread.join(timeout=3)
            require(not thread.is_alive(), "mock WireServer did not stop")
    (artifacts / "requests.json").write_text(json.dumps(state.requests, indent=2))
    (artifacts / "service-errors.json").write_text(json.dumps(state.errors, indent=2))
    require(not state.errors, f"{name}: mock service errors: {state.errors}")
    result = json.loads(out)
    (artifacts / "result.json").write_text(json.dumps(result, indent=2))
    kvp_failed = options.get("bad_dir", False)
    http_failed = name in ("http-fatal", "http-deadline", "request-timeout")
    require(code == int(kvp_failed or http_failed), f"{name}: unexpected exit {code}: {error}")
    require(bool(result["kvp_error"]) == kvp_failed, f"{name}: incorrect KVP outcome: {result}")
    require(bool(result["http_error"]) == http_failed, f"{name}: incorrect HTTP outcome: {result}")
    require(bool(error) == bool(code), f"{name}: failed operations were not surfaced on stderr")
    require(result["encoded_report"] == result["report_after"], "HTTP changed the supplied report")
    if plan["http"]:
        require(state.requests, f"{name}: no health request")
        if name in ("http-deadline", "request-timeout"):
            require(len(state.requests) >= 2, f"{name}: no retries")
            require("deadline exceeded" in result["http_error"], "deadline was not classified")
            require(
                plan["total_ms"] * 0.75 <= result["http_duration_ms"] <= plan["total_ms"] + 500,
                f"{name}: delivery did not honor its configured retry budget: {result['http_duration_ms']}ms",
            )
        else:
            require(len(state.requests) == len(statuses), f"{name}: unexpected retry count")
        for request in state.requests:
            expected = {"state": "Ready"}
            if plan["state"] == "failure":
                expected = {
                    "state": "NotReady",
                    "details": {
                        "subStatus": "ProvisioningFailed",
                        "description": result["encoded_report"],
                    },
                }
            elif plan["state"] == "in-progress":
                expected = {
                    "state": "NotReady",
                    "details": {
                        "subStatus": "Provisioning",
                        "description": f"Provisioning is still in progress for vm_id={VM_ID}.",
                    },
                }
            require(request["body"] == expected, f"{name}: incorrect health JSON")
    else:
        require(state.requests == [], "KVP-only publication sent HTTP")

    final = pool.read_bytes() if pool.exists() else None
    if final is not None:
        (artifacts / "final.kvp").write_bytes(final)
    rows = kvp_decode.records(final) if final is not None else []
    if plan["kvp"] and not kvp_failed:
        require(pool.stat().st_mode & 0o777 == 0o600, "KVP mode is not private")
        if seed:
            require(("existing-sentinel", "preserve this record") in rows, "publication cleared existing KVP")
        report = kvp_decode.report(rows)
        expected_result = {"success": "success", "failure": "error", "in-progress": "in_progress"}[plan["state"]]
        require(report is not None and report["result"] == expected_result, "incorrect stored provisioning result")
        require(report["agent"] == AGENT and report["vm_id"] == VM_ID, "incorrect report identity")
        require(report["pps_type"] == "None" and report["timestamp"], "missing report metadata")
        require(report["encoded"] == result["encoded_report"], "stored and HTTP report encodings diverged")
        require([value for key, value in report["pairs"] if key == "detail"] == ["one", "two"], "ordered extras changed")
        if plan["state"] == "failure":
            require(("reason", REASON) in report["pairs"], "quoted failure context changed")
        decoded = kvp_decode.diagnostics(rows)
        if plan["diagnostics"]:
            require(len(decoded) == 3, "missing span or log diagnostics")
            log = next(entry for entry in decoded if entry["name"] == "testinit:log")
            require(log["sha256"] == result["payload_sha256"], "decoded payload changed")
            require(log["chunks"] > 1, "long payload was not split across real pool records")
            spans = [entry for entry in decoded if entry["name"] == "testinit:provision"]
            require({entry["kind"] for entry in spans} == {"start", "finish"}, "span endpoints missing")
            require(len({entry["event_id"] for entry in spans}) == 1, "span correlation changed")
            require(all(entry["agent"] == AGENT and entry["vm_id"] == VM_ID for entry in decoded), "diagnostic identity changed")
        for request in state.requests:
            snapshot = request["kvp_snapshot"]
            if plan["order"] == "http-first":
                old = kvp_decode.records((artifacts / snapshot).read_bytes()) if snapshot else []
                require(kvp_decode.report(old) is None, "HTTP implicitly published KVP")
            else:
                require(snapshot is not None and (artifacts / snapshot).read_bytes() == final,
                        "retry or HTTP delivery changed KVP bytes")
    elif not plan["kvp"]:
        require(final == seed, "HTTP-only reporting created or changed the KVP pool")
    else:
        require(final is None, "failed KVP publication created a success-shaped pool")
    if not options.get("bad_dir"):
        require((pool_dir / ".kvp_pool_0").read_bytes() == b"untouched external pool", "wrong pool modified")
    code, parsed, error = execute(
        [cli, "--dir", pool_dir, "dump", "--parse"], artifacts, "go-dump", environment
    )
    require(code == 0 and not error, f"real Go CLI could not read consumer output: {error}")
    entries = json.loads(parsed)
    if plan["kvp"] and not kvp_failed:
        require(sum(entry["type"] == "PROVISIONING_REPORT" for entry in entries) == 1,
                "Go CLI did not expose one typed report")
        if plan["diagnostics"]:
            log = next(entry for entry in entries if entry.get("name") == "testinit:log")
            payload = log["payload"]
            if isinstance(payload, dict):
                import base64
                payload = base64.b64decode(payload["data"], validate=True)
            else:
                payload = payload.encode()
            require(hashlib.sha256(payload).hexdigest() == result["payload_sha256"], "CLI payload changed")
    if rust:
        code, parsed_rust, error = execute(
            [rust, "--dir", pool_dir, "dump", "--parse"], artifacts, "rust-dump", environment
        )
        require(code == 0 and not error, f"Rust could not read Go consumer output: {error}")
        require(entries == json.loads(parsed_rust), "Go/Rust E2E interpretation differs")
    return {"scenario": name, "http_attempts": len(state.requests), "exit_code": int(kvp_failed or http_failed)}


def run_scenarios(consumer, cli, artifacts, coverage=None, rust=None, selected=None, backend="process"):
    artifacts.mkdir(parents=True, exist_ok=True)
    run = Path(tempfile.mkdtemp(prefix="run-", dir=artifacts))
    results = []

    class ModuleProvisioning(unittest.TestCase):
        def test_reporting_lifecycle(self):
            for name in selected or SCENARIOS:
                with self.subTest(scenario=name):
                    record = check_case(name, SCENARIOS[name], consumer, cli, run / name, coverage, rust)
                    results.append(record)
                    print(f"Verified {name}: exit={record['exit_code']}, HTTP attempts={record['http_attempts']}", flush=True)

    run_name = run.name
    (artifacts / "latest.json").write_text(json.dumps({"directory": run_name, "status": "running", "backend": backend}))
    outcome = unittest.TextTestRunner(verbosity=2).run(
        unittest.defaultTestLoader.loadTestsFromTestCase(ModuleProvisioning)
    )
    summary = {
        "directory": run_name,
        "backend": backend,
        "rust_verified": bool(rust),
        "passed": outcome.wasSuccessful(),
        "expected_scenarios": len(selected or SCENARIOS),
        "scenarios": results,
    }
    (run / "summary.json").write_text(json.dumps(summary, indent=2))
    (artifacts / "latest.json").write_text(json.dumps(summary, indent=2))
    print(f"Testinit artifacts: {run}", flush=True)
    return outcome.wasSuccessful()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--consumer", type=Path, required=True)
    parser.add_argument("--cli", type=Path, required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--coverage", type=Path)
    parser.add_argument("--rust-kvp", type=Path)
    parser.add_argument("--scenario", action="append", choices=SCENARIOS)
    parser.add_argument("--inside", action="store_true")
    parser.add_argument("--container", action="store_true")
    parser.add_argument("--base-image", default="ubuntu:24.04")
    args = parser.parse_args()
    if args.inside and not Path("/test/isolated-kvp-fixture").is_file():
        parser.error("--inside requires the dedicated unprivileged testinit image")
    if args.inside and args.container:
        parser.error("--inside and --container are mutually exclusive")
    if args.container:
        from container_runner import run_container
        return 0 if run_container(
            args.consumer.resolve(strict=True),
            args.cli.resolve(strict=True),
            args.artifacts.resolve(),
            args.coverage.resolve(strict=True) if args.coverage else None,
            args.rust_kvp.resolve(strict=True) if args.rust_kvp else None,
            args.scenario,
            args.base_image,
        ) else 1
    return 0 if run_scenarios(
        args.consumer.resolve(strict=True),
        args.cli.resolve(strict=True),
        args.artifacts.resolve(),
        args.coverage.resolve(strict=True) if args.coverage else None,
        args.rust_kvp.resolve(strict=True) if args.rust_kvp else None,
        args.scenario,
        "container" if args.inside else "process",
    ) else 1


if __name__ == "__main__":
    raise SystemExit(main())
