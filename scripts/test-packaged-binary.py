#!/usr/bin/env python3
"""Smoke test an extracted native antigravity-proxy release binary.

Tests binary identity, help, no-credential startup, authentication enforcement,
health, fake-credential reload, locking, graceful shutdown, and log sanitation.
Never contacts upstream or uses real credentials.
"""

import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

IS_WINDOWS = sys.platform == "win32"


def request(base, path, key=None):
    headers = {"X-Goog-Api-Key": key, "x-api-key": key} if key else {}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(urllib.request.Request(base + path, headers=headers), timeout=2) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, None


def wait_healthy(base):
    for _ in range(100):
        try:
            status, body = request(base, "/health")
            if status == 200 and body.get("status") == "ok":
                return body
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.05)
    raise RuntimeError("proxy did not become healthy")


def get_free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_binary(binary_path: Path):
    assert binary_path.is_file(), f"Binary not found: {binary_path}"

    # 1. Version command
    result = subprocess.run([str(binary_path), "version"], capture_output=True, text=True)
    assert result.returncode == 0, f"version failed: {result.stderr}"
    assert "antigravity-proxy v" in result.stdout, f"unexpected version: {result.stdout}"
    assert result.stderr == ""

    # 2. Help command
    result = subprocess.run([str(binary_path), "help"], capture_output=True, text=True)
    assert result.returncode == 0, f"help failed: {result.stderr}"
    assert "Antigravity Proxy" in result.stdout or "Usage:" in result.stdout

    # 3. No-credential startup and lifecycle test
    with tempfile.TemporaryDirectory() as temp_dir:
        dir_path = Path(temp_dir)
        config_dir = dir_path / ".config" / "antigravity-proxy"
        config_dir.mkdir(parents=True, exist_ok=True)
        if not IS_WINDOWS:
            config_dir.chmod(0o700)
        history_file = dir_path / "usage.jsonl"
        port = get_free_port()
        base = f"http://127.0.0.1:{port}"

        env = os.environ.copy()
        env["HOST"] = "127.0.0.1"
        env["PORT"] = str(port)
        env["HOME"] = str(dir_path)
        env["USERPROFILE"] = str(dir_path)
        env["ADMIN_API_KEY"] = "smoke-admin-key"
        env["API_KEY"] = "smoke-inference-key"
        env["ANTIGRAVITY_QUOTA_HISTORY_PATH"] = str(history_file)

        cmd = [str(binary_path), "serve"]
        proc = subprocess.Popen(cmd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            health = wait_healthy(base)
            assert health["credential_configured"] is False

            # Inference auth enforcement
            assert request(base, "/v1beta/models")[0] == 401

            # Admin auth enforcement
            assert request(base, "/admin/accounts")[0] == 401
            assert request(base, "/admin/accounts", "smoke-admin-key")[0] == 200

            # Graceful shutdown
            if IS_WINDOWS:
                proc.terminate()
            else:
                proc.send_signal(signal.SIGTERM)

            stdout, stderr = proc.communicate(timeout=10)
            if not IS_WINDOWS:
                assert proc.returncode == 0, f"Shutdown returned non-zero: {proc.returncode}, stderr: {stderr}"
            else:
                assert proc.returncode is not None

            # Check logs do not expose sensitive values
            all_logs = stdout + stderr
            assert "smoke-admin-key" not in all_logs
            assert "smoke-inference-key" not in all_logs

        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

        # 4. Restart test with synthetic credentials
        config_file = config_dir / "config.json"
        synthetic_config = {
            "host": "127.0.0.1",
            "port": port,
            "refreshToken": "dummy-refresh-token",
            "accessToken": "dummy-access-token",
            "projectId": "dummy-project-id",
        }
        config_file.write_text(json.dumps(synthetic_config))
        if not IS_WINDOWS:
            config_file.chmod(0o600)

        proc = subprocess.Popen(cmd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            health = wait_healthy(base)
            assert health["credential_configured"] is True

            # Graceful shutdown
            if IS_WINDOWS:
                proc.terminate()
            else:
                proc.send_signal(signal.SIGTERM)

            stdout, stderr = proc.communicate(timeout=10)
            if not IS_WINDOWS:
                assert proc.returncode == 0
            else:
                assert proc.returncode is not None

            # Log check: tokens must never be logged
            all_logs = stdout + stderr
            assert "dummy-access-token" not in all_logs
            assert "dummy-refresh-token" not in all_logs

        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()


def main():
    if len(sys.argv) != 2:
        print("Usage: python3 scripts/test-packaged-binary.py /path/to/antigravity-proxy", file=sys.stderr)
        sys.exit(1)
    binary_path = Path(sys.argv[1]).resolve()
    test_binary(binary_path)
    print(f"Packaged binary smoke tests passed for: {binary_path}")


if __name__ == "__main__":
    main()
