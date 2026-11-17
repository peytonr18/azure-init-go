# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Disposable-container lifecycle adapted from Azure/azure-init testinit."""

import os
from pathlib import Path
import re
import subprocess
import uuid


def create_command(image, consumer, cli, artifacts, coverage=None, rust=None, selected=None):
    command = [
        "docker", "create",
        "--name", f"azure-init-go-testinit-{uuid.uuid4().hex}",
        "--network", "none",
        "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges",
        "--read-only",
        "--pids-limit", "64",
        "--memory", "256m",
        "--cpus", "1",
        "--user", f"{os.getuid()}:{os.getgid()}",
        "--env", "GOMAXPROCS=2",
        "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m,mode=1777",
        "--mount", f"type=bind,src={consumer},dst=/test/consumer,readonly",
        "--mount", f"type=bind,src={cli},dst=/test/azure-init-kvp,readonly",
        "--mount", f"type=bind,src={artifacts},dst=/artifacts",
    ]
    arguments = [
        "--consumer", "/test/consumer",
        "--cli", "/test/azure-init-kvp",
        "--artifacts", "/artifacts",
    ]
    if coverage:
        command.extend(["--mount", f"type=bind,src={coverage},dst=/profiles"])
        arguments.extend(["--coverage", "/profiles"])
    if rust:
        command.extend(["--mount", f"type=bind,src={rust},dst=/test/rust-kvp,readonly"])
        arguments.extend(["--rust-kvp", "/test/rust-kvp"])
    for scenario in selected or []:
        arguments.extend(["--scenario", scenario])
    return command + [image] + arguments


def run_container(consumer, cli, artifacts, coverage=None, rust=None, selected=None,
                  base_image="ubuntu:24.04"):
    subprocess.run(
        ["docker", "info", "--format", "{{.ServerVersion}}"],
        check=True, timeout=15,
    )
    artifacts.mkdir(parents=True, exist_ok=True)
    image = f"azure-init-go-testinit:{uuid.uuid4().hex}"
    built = False
    container = None
    context = Path(__file__).resolve().parents[1]
    try:
        subprocess.run(
            ["docker", "build", "--build-arg", f"BASE_IMAGE={base_image}",
             "-f", str(context / "Dockerfile.isolated"), "-t", image, str(context)],
            check=True, timeout=300,
        )
        built = True
        created = subprocess.run(
            create_command(image, consumer, cli, artifacts, coverage, rust, selected),
            check=True, capture_output=True, text=True, timeout=30,
        )
        candidate = created.stdout.strip()
        if not re.fullmatch(r"[a-f0-9]{12,64}", candidate):
            raise RuntimeError(f"unexpected Docker container id: {candidate!r}")
        container = candidate
        subprocess.run(
            ["docker", "start", "--attach", container],
            check=True, timeout=180,
        )
        return True
    finally:
        try:
            if container:
                subprocess.run(
                    ["docker", "rm", "--force", "--volumes", container],
                    check=True, timeout=30,
                )
        finally:
            if built:
                subprocess.run(
                    ["docker", "image", "rm", image], check=True, timeout=30
                )
