#!/usr/bin/env python3
"""Validate GitHub Actions workflow files for structural hygiene and policy compliance."""

import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent.parent
WORKFLOWS_DIR = ROOT / ".github" / "workflows"

REQUIRED_WORKFLOWS = [
    "ci.yml",
    "validate.yml",
    "code-quality.yml",
    "security.yml",
    "docker.yml",
    "release-please.yml",
    "release.yml",
    "maintenance.yml",
    "pr-policy.yml",
]

USES_PINNED_SHA = re.compile(r"uses:\s+([a-zA-Z0-9_\-\./]+)@([0-9a-f]{40})\b")
USES_ANY = re.compile(r"uses:\s+([^#\s\n]+)")


def check_workflows() -> list[str]:
    errors = []

    if not WORKFLOWS_DIR.is_dir():
        return [f"Workflows directory {WORKFLOWS_DIR} does not exist"]

    for req in REQUIRED_WORKFLOWS:
        wf_path = WORKFLOWS_DIR / req
        if not wf_path.is_file():
            errors.append(f"Missing required workflow: {req}")

    for wf_file in sorted(WORKFLOWS_DIR.glob("*.yml")):
        text = wf_file.read_text(encoding="utf-8")
        lines = text.splitlines()

        if "permissions:" not in text:
            errors.append(f"{wf_file.name}: Missing explicit 'permissions' block")

        open_brackets = 0
        open_braces = 0
        in_block_scalar = False
        block_indent = 0

        for line_no, raw_line in enumerate(lines, 1):
            line = raw_line.strip()
            if not line or line.startswith("#"):
                continue

            current_indent = len(raw_line) - len(raw_line.lstrip())
            if in_block_scalar:
                if current_indent <= block_indent:
                    in_block_scalar = False
                else:
                    continue

            if line.endswith("|") or line.endswith(">"):
                in_block_scalar = True
                block_indent = current_indent
                continue

            # Detect malformed YAML mapping lines with multiple unquoted colons
            if ":" in line and not line.startswith("-") and not line.startswith("#"):
                colons = [idx for idx, ch in enumerate(line) if ch == ":"]
                if len(colons) > 1:
                    first_val = line[colons[0] + 1 :].strip()
                    if ":" in first_val and not (
                        first_val.startswith('"')
                        or first_val.startswith("'")
                        or first_val.startswith("http:")
                        or first_val.startswith("https:")
                        or first_val.startswith("{")
                    ):
                        errors.append(f"{wf_file.name}:{line_no}: Malformed YAML mapping line: {line}")

            in_quote = False
            quote_char = ""
            for ch in raw_line:
                if ch in ('"', "'") and not in_quote:
                    in_quote = True
                    quote_char = ch
                elif ch == quote_char and in_quote:
                    in_quote = False
                elif not in_quote:
                    if ch == "[":
                        open_brackets += 1
                    elif ch == "]":
                        open_brackets -= 1
                    elif ch == "{":
                        open_braces += 1
                    elif ch == "}":
                        open_braces -= 1

            # Match uses: with or without leading dash
            line_clean = line
            if line_clean.startswith("- "):
                line_clean = line_clean[2:].strip()
            if line_clean.startswith("uses:"):
                match_any = USES_ANY.search(line_clean)
                if match_any:
                    action_target = match_any.group(1).strip()
                    if not action_target.startswith("./"):
                        if not USES_PINNED_SHA.search(line_clean):
                            errors.append(
                                f"{wf_file.name}:{line_no}: Action '{action_target}' must be pinned to a 40-character commit SHA"
                            )

        if open_brackets != 0:
            errors.append(f"{wf_file.name}: Malformed YAML - unbalanced square brackets")
        if open_braces != 0:
            errors.append(f"{wf_file.name}: Malformed YAML - unbalanced curly braces")

    # Check ci.yml has ci-required job
    ci_file = WORKFLOWS_DIR / "ci.yml"
    if ci_file.is_file():
        ci_text = ci_file.read_text(encoding="utf-8")
        if "ci-required:" not in ci_text:
            errors.append("ci.yml: Missing required aggregate gate job 'ci-required'")

    # Run actionlint if available
    actionlint_bin = shutil.which("actionlint") or "/tmp/ci-verification-tools/actionlint"
    if os.path.exists(actionlint_bin) and os.access(actionlint_bin, os.X_OK):
        wf_files = [str(f) for f in sorted(WORKFLOWS_DIR.glob("*.yml"))]
        res = subprocess.run([actionlint_bin] + wf_files, capture_output=True, text=True)
        if res.returncode != 0:
            for l in res.stdout.splitlines():
                if l.strip():
                    errors.append(f"actionlint: {l.strip()}")

    return errors


def main():
    print(f"Validating GitHub Actions workflows in {ROOT.name}...")
    errors = check_workflows()
    if errors:
        print("\nWorkflow validation errors found:", file=sys.stderr)
        for err in errors:
            print(f"  {err}", file=sys.stderr)
        sys.exit(1)

    print("All workflows passed structural and security policy validation.")


if __name__ == "__main__":
    main()
