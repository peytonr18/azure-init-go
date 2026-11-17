# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class ConsumerSafetyTests(unittest.TestCase):
    def setUp(self):
        self.consumer = os.environ.get("AZURE_INIT_TESTINIT_CONSUMER")
        if not self.consumer:
            self.skipTest("make test-e2e builds the guarded external consumer fixture")
        self.temporary = tempfile.TemporaryDirectory(prefix="testinit-safety-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root / ".azure-init-go-testinit").write_text("isolated azure-init-go fixture\n")
        self.plan = {
            "pool_dir": str(self.root / "pool"),
            "endpoint": "http://127.0.0.1:1/provisioning/health",
            "state": "success",
            "order": "kvp-first",
            "agent": "safety-test",
            "vm_id": "vm",
            "kvp": True,
            "http": False,
            "request_ms": 100,
            "retry_ms": 10,
            "total_ms": 200,
        }

    def rejected(self, expected):
        environment = dict(os.environ, AZURE_INIT_TEST_ROOT=str(self.root))
        if os.environ.get("AZURE_INIT_TESTINIT_COVERAGE"):
            environment["GOCOVERDIR"] = os.environ["AZURE_INIT_TESTINIT_COVERAGE"]
        result = subprocess.run(
            [self.consumer],
            input=json.dumps(self.plan),
            capture_output=True,
            text=True,
            timeout=3,
            env=environment,
        )
        self.assertEqual(result.returncode, 2, result)
        self.assertEqual(result.stdout, "")
        self.assertIn(expected, result.stderr)
        self.assertFalse((self.root / "pool" / ".kvp_pool_1").exists())

    def test_refuses_real_azure_endpoint(self):
        self.plan.update(http=True, endpoint="http://168.63.129.16/provisioning/health")
        self.rejected("non-loopback")

    def test_refuses_outside_pool_and_symlink_traversal(self):
        with tempfile.TemporaryDirectory(prefix="testinit-outside-") as outside:
            self.plan["pool_dir"] = outside
            self.rejected("inside the disposable fixture")
            (self.root / "pool").symlink_to(outside, target_is_directory=True)
            self.plan["pool_dir"] = str(self.root / "pool")
            self.rejected("symlinks")
            self.assertFalse((Path(outside) / ".kvp_pool_1").exists())

    def test_requires_disposable_work_marker(self):
        (self.root / ".azure-init-go-testinit").unlink()
        self.rejected(".azure-init-go-testinit")
