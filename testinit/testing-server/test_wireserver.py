# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from contextlib import contextmanager
from http.client import HTTPConnection, RemoteDisconnected
from pathlib import Path
import tempfile
import threading
import unittest

from wireserver_handler import Response, State, WireServer, load_responses


@contextmanager
def serving(state):
    with WireServer(state) as server:
        thread = threading.Thread(
            target=server.serve_forever, kwargs={"poll_interval": 0.01}
        )
        thread.start()
        connection = HTTPConnection(*server.server_address, timeout=2)
        try:
            yield server, connection
        finally:
            connection.close()
            server.shutdown()
            thread.join(timeout=3)
            if thread.is_alive():
                raise RuntimeError("mock WireServer did not stop")


class WireServerTests(unittest.TestCase):
    def test_binary_snapshots_and_sequential_responses(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            pool = root / ".kvp_pool_1"
            binary = bytes(range(256)) * 10
            pool.write_bytes(binary)
            state = State(pool, root, "test-agent", [Response(500), Response(201)])
            with serving(state) as (_, connection):
                for expected in (500, 201, 201):
                    connection.request(
                        "POST",
                        "/provisioning/health",
                        '{"state":"Ready"}',
                        {
                            "Content-Type": "application/json",
                            "User-Agent": "test-agent",
                            "x-ms-guest-agent-name": "test-agent",
                        },
                    )
                    response = connection.getresponse()
                    self.assertEqual(response.status, expected)
                    self.assertEqual(response.read(), b"")
            self.assertEqual(state.errors, [])
            self.assertEqual(len(state.requests), 3)
            for record in state.requests:
                self.assertEqual((root / record["kvp_snapshot"]).read_bytes(), binary)

    def test_invalid_requests_are_failures_not_swallowed_thread_errors(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for path, body in (
                ("/unexpected", '{"state":"Ready"}'),
                ("/provisioning/health", "not-json"),
            ):
                with self.subTest(path=path):
                    state = State(root / "absent", root, "test-agent", [Response()])
                    with serving(state) as (_, connection):
                        connection.request(
                            "POST",
                            path,
                            body,
                            {
                                "Content-Type": "application/json",
                                "User-Agent": "test-agent",
                                "x-ms-guest-agent-name": "test-agent",
                            },
                        )
                        with self.assertRaises(RemoteDisconnected):
                            connection.getresponse()
                    self.assertEqual(len(state.errors), 1)
                    self.assertEqual(state.requests, [])

    def test_health_readiness_and_per_server_state(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for _ in range(2):
                state = State(root / "absent", root, "agent", [Response()])
                with serving(state) as (_, connection):
                    connection.request("GET", "/healthz")
                    response = connection.getresponse()
                    self.assertEqual(response.status, 200)
                    response.read()
                self.assertEqual(state.requests, [])
                self.assertEqual(state.errors, [])

    def test_xml_and_invalid_response_configuration(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "responses.xml"
            path.write_text(
                "<responses><response><status_code>500</status_code>"
                "<delay>0.01</delay></response><response>"
                "<status_code>201</status_code></response></responses>"
            )
            self.assertEqual(load_responses(path), [Response(500, 0.01), Response(201)])
            path.write_text("<responses/>")
            with self.assertRaises(ValueError):
                load_responses(path)
        for status, delay in ((0, 0), (600, 0), (200, -1), (200, float("inf"))):
            with self.assertRaises(ValueError):
                Response(status, delay)
