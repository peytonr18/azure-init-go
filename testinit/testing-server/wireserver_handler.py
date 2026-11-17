# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Adapted from Azure/azure-init testinit's WireServer handler.

Unlike the privileged systemd fixture, this handler is loopback-only, snapshots
binary KVP bytes, validates requests, and keeps state local to each server.
"""

from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
from pathlib import Path
import threading
import time
import traceback
import xml.etree.ElementTree as ET


@dataclass(frozen=True)
class Response:
    status: int = 201
    delay: float = 0.0

    def __post_init__(self):
        if not 200 <= self.status <= 599:
            raise ValueError("fixture response status must be between 200 and 599")
        if not math.isfinite(self.delay) or not 0 <= self.delay <= 5:
            raise ValueError("fixture delay must be finite and between 0 and 5 seconds")


def load_responses(path):
    """Keep compatibility with upstream testinit's WireServer XML fixtures."""
    root = ET.parse(path).getroot()
    responses = []
    for element in root.findall("response"):
        responses.append(
            Response(
                int(element.findtext("status_code")),
                float(element.findtext("delay", "0")),
            )
        )
    if not responses:
        raise ValueError("response fixture must not be empty")
    return responses


class State:
    def __init__(self, pool, artifacts, agent, responses):
        if not responses:
            raise ValueError("at least one response is required")
        self.pool = Path(pool)
        self.artifacts = Path(artifacts)
        self.agent = agent
        self.responses = tuple(responses)
        self.requests = []
        self.errors = []
        self.disconnects = 0
        self.lock = threading.Lock()


class WireServerHandler(BaseHTTPRequestHandler):
    server_version = "azure-init-go-testinit"

    def log_message(self, *_args):
        pass

    def do_GET(self):
        if self.path != "/healthz":
            raise ValueError(f"unexpected GET endpoint: {self.path}")
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        state = self.server.state
        if self.path != "/provisioning/health":
            raise ValueError(f"unexpected health endpoint: {self.path}")
        for header, expected in (
            ("Content-Type", "application/json"),
            ("User-Agent", state.agent),
            ("x-ms-guest-agent-name", state.agent),
        ):
            if self.headers.get(header) != expected:
                raise ValueError(f"incorrect {header}: {self.headers.get(header)!r}")
        length = int(self.headers["Content-Length"])
        if not 0 < length <= 16384:
            raise ValueError("unexpected health payload length")
        raw = self.rfile.read(length)
        body = json.loads(raw.decode("utf-8"))
        if body == {"state": "Ready"}:
            pass
        elif (
            isinstance(body, dict)
            and set(body) == {"state", "details"}
            and body["state"] == "NotReady"
            and isinstance(body["details"], dict)
            and set(body["details"]) == {"subStatus", "description"}
            and body["details"]["subStatus"] in ("Provisioning", "ProvisioningFailed")
            and isinstance(body["details"]["description"], str)
        ):
            pass
        else:
            raise ValueError(f"invalid health payload: {body!r}")
        with state.lock:
            index = len(state.requests)
            response = state.responses[min(index, len(state.responses) - 1)]
            snapshot = None
            if state.pool.exists():
                snapshot = f"http-{index + 1:03d}.kvp"
                (state.artifacts / snapshot).write_bytes(state.pool.read_bytes())
            state.requests.append(
                {
                    "path": self.path,
                    "headers": dict(self.headers),
                    "body": body,
                    "raw": raw.decode("utf-8"),
                    "status": response.status,
                    "kvp_snapshot": snapshot,
                }
            )
        if response.delay:
            time.sleep(response.delay)
        try:
            self.send_response(response.status)
            self.send_header("Content-Length", "0")
            self.end_headers()
        except (BrokenPipeError, ConnectionResetError):
            # Expected when exercising client cancellation/timeouts.
            with state.lock:
                state.disconnects += 1


class WireServer(ThreadingHTTPServer):
    daemon_threads = False

    def __init__(self, state):
        self.state = state
        super().__init__(("127.0.0.1", 0), WireServerHandler)

    def handle_error(self, _request, _client_address):
        with self.state.lock:
            self.state.errors.append(traceback.format_exc())
