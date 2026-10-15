#!/usr/bin/env python3
"""Check tracked repository files and recent history for accidental secret leakage and vulnerabilities."""

from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent.parent

SECRET_PATTERNS = [
    (re.compile(r"-----BEGIN (RSA|EC|DSA|OPENSSH|PGP) PRIVATE KEY-----"), "Private key header"),
    (re.compile(r"(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{36,}"), "GitHub personal access token"),
    (re.compile(r"github_pat_[A-Za-z0-9_]{82}"), "GitHub fine-grained PAT"),
    (re.compile(r"AKIA[0-9A-Z]{16}"), "AWS Access Key ID"),
    (re.compile(r"AIza[0-9A-Za-z\\-_]{35}"), "Google API Key"),
    (re.compile(r"1//0[a-zA-Z0-9_\-]{40,}"), "Live Google OAuth refresh token"),
]


def check_tracked_files():
    result = subprocess.run(
        ["git", "-C", str(ROOT), "ls-files"],
        capture_output=True,
        text=True,
        check=True,
    )
    tracked = [line.strip() for line in result.stdout.splitlines() if line.strip()]

    findings = []
    for rel_path in tracked:
        file_path = ROOT / rel_path
        if not file_path.is_file():
            continue

        try:
            content = file_path.read_text(encoding="utf-8", errors="replace")
        except Exception:
            continue

        for pattern, desc in SECRET_PATTERNS:
            for match in pattern.finditer(content):
                matched_text = match.group(0)
                if any(k in matched_text.lower() for k in ("dummy", "synthetic", "test", "fake", "example")):
                    continue
                line_no = content[: match.start()].count("\n") + 1
                findings.append(f"{rel_path}:{line_no}: Potential {desc} detected: {matched_text[:8]}...")

    return findings


def check_git_history():
    findings = []
    try:
        res = subprocess.run(
            ["git", "-C", str(ROOT), "log", "-p", "-n", "30"],
            capture_output=True,
            text=True,
        )
        if res.returncode != 0:
            return findings

        for line in res.stdout.splitlines():
            if not line.startswith("+"):
                continue
            added = line[1:].strip()
            for pattern, desc in SECRET_PATTERNS:
                for match in pattern.finditer(added):
                    matched_text = match.group(0)
                    if any(k in matched_text.lower() for k in ("dummy", "synthetic", "test", "fake", "example")):
                        continue
                    findings.append(f"Git history: Potential {desc} detected in recent commit: {matched_text[:8]}...")
    except Exception:
        pass
    return findings


def main():
    print(f"Scanning tracked files and history in {ROOT.name} for secrets...")
    findings = check_tracked_files() + check_git_history()

    if findings:
        print("\nSecurity check findings:", file=sys.stderr)
        for finding in findings:
            print(f"  {finding}", file=sys.stderr)
        sys.exit(1)

    print("Secret scan passed: no unredacted credentials detected.")


if __name__ == "__main__":
    main()
