# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from container_runner import create_command, run_container


class ContainerTests(unittest.TestCase):
    def test_isolation_policy_and_read_only_binaries(self):
        command = create_command(
            "image", Path("/consumer"), Path("/cli"), Path("/artifacts"),
            Path("/coverage"), Path("/rust"), ["success"],
        )
        for flag, expected in (
            ("--network", "none"), ("--cap-drop", "ALL"),
            ("--security-opt", "no-new-privileges"), ("--pids-limit", "64"),
        ):
            self.assertEqual(command[command.index(flag) + 1], expected)
        self.assertIn("--read-only", command)
        self.assertNotIn("--privileged", command)
        self.assertNotIn("--cap-add", command)
        self.assertNotIn("--pid", command)
        self.assertNotIn("--cgroupns", command)
        for mount in (arg for arg in command if arg.startswith("type=bind")):
            if "dst=/test/" in mount:
                self.assertTrue(mount.endswith(",readonly"), mount)
            self.assertNotIn("/sys", mount)
            self.assertNotIn("/etc", mount)
            self.assertNotIn("/var/lib/hyperv", mount)
        self.assertEqual(command[-2:], ["--scenario", "success"])

    def test_failures_clean_only_owned_container_and_image(self):
        container = "a" * 64
        for fail in (False, True):
            calls = []

            def docker(command, **_kwargs):
                calls.append(command)
                if command[1] == "create":
                    return subprocess.CompletedProcess(command, 0, container + "\n", "")
                if command[1] == "start" and fail:
                    raise subprocess.CalledProcessError(1, command)
                return subprocess.CompletedProcess(command, 0, "", "")

            with tempfile.TemporaryDirectory() as temporary, patch(
                "container_runner.subprocess.run", side_effect=docker
            ):
                if fail:
                    with self.assertRaises(subprocess.CalledProcessError):
                        run_container(Path("/consumer"), Path("/cli"), Path(temporary))
                else:
                    self.assertTrue(run_container(Path("/consumer"), Path("/cli"), Path(temporary)))
            self.assertEqual(
                [command[1] for command in calls],
                ["info", "build", "create", "start", "rm", "image"],
            )
            self.assertEqual(calls[-2][-1], container)
            self.assertEqual(calls[-1][-2], "rm")
            self.assertTrue(calls[-1][-1].startswith("azure-init-go-testinit:"))

    def test_failed_build_does_not_remove_other_images(self):
        def docker(command, **_kwargs):
            if command[1] == "build":
                raise subprocess.CalledProcessError(1, command)
            return subprocess.CompletedProcess(command, 0, "", "")

        with tempfile.TemporaryDirectory() as temporary, patch(
            "container_runner.subprocess.run", side_effect=docker
        ) as process:
            with self.assertRaises(subprocess.CalledProcessError):
                run_container(Path("/consumer"), Path("/cli"), Path(temporary))
            self.assertEqual([call.args[0][1] for call in process.call_args_list], ["info", "build"])
