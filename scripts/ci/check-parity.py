#!/usr/bin/env python3
"""Verify CI/CD workflow and tooling parity between proxy repositories."""

import os
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parent.parent.parent
PARENT = ROOT.parent

COMMON_WORKFLOWS = [
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

EXPECTED_PINS = {
    "actions/checkout": "fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
    "actions/setup-go": "924ae3a1cded613372ab5595356fb5720e22ba16",
    "docker/setup-qemu-action": "c7c53464625b32c7a7e944ae62b3e17d2b600130",
    "docker/setup-buildx-action": "8d2750c68a42422c14e847fe6c8ac0403b4cbd6f",
    "docker/metadata-action": "c299e40c65443455700f0fdfc63efafe5b349051",
    "docker/login-action": "c94ce9fb468520275223c153574b00df6fe4bcc9",
    "docker/build-push-action": "10e90e3645eae34f1e60eeb005ba3a3d33f178e8",
    "goreleaser/goreleaser-action": "90a305fe32ea55e12d9302e171b3127814b745ee",
    "googleapis/release-please-action": "a02a34c4d625f9be7cb89156071d8567266a2445",
    "anchore/sbom-action": "df80a981bc6edbc4e220a492d3cbe9f5547a6e75",
    "actions/attest-build-provenance": "7668571508540a607bdfd90a87a560489fe372eb",
}


def verify_repo_pins(repo_dir: Path) -> list[str]:
    errors = []
    wf_dir = repo_dir / ".github" / "workflows"
    if not wf_dir.is_dir():
        return [f"{repo_dir.name}: workflows directory missing"]

    for wf in wf_dir.glob("*.yml"):
        content = wf.read_text(encoding="utf-8")
        for action, expected_sha in EXPECTED_PINS.items():
            if action in content:
                pattern = re.compile(rf"{re.escape(action)}@([0-9a-f]{{40}})")
                match = pattern.search(content)
                if not match:
                    errors.append(f"{wf.name}: action {action} is not pinned to a 40-character SHA")
                elif match.group(1) != expected_sha:
                    errors.append(f"{wf.name}: action {action} uses SHA {match.group(1)}, expected {expected_sha}")
    return errors


def verify_cross_repo_parity() -> list[str]:
    errors = []
    other_name = "codex-proxy" if ROOT.name == "antigravity-proxy" else "antigravity-proxy"
    other_dir = PARENT / other_name

    if not other_dir.is_dir():
        return []  # Running in isolated single-repo CI run; skip cross-repo diff

    # Verify identical workflows
    for wf in ["code-quality.yml", "security.yml", "maintenance.yml", "pr-policy.yml"]:
        f1 = ROOT / ".github" / "workflows" / wf
        f2 = other_dir / ".github" / "workflows" / wf
        if f1.is_file() and f2.is_file():
            if f1.read_bytes() != f2.read_bytes():
                errors.append(f"Parity mismatch in common workflow: {wf}")

    return errors


def main():
    print(f"Checking workflow pin and parity contract for {ROOT.name}...")
    errors = verify_repo_pins(ROOT) + verify_cross_repo_parity()
    if errors:
        print("\nParity contract errors found:", file=sys.stderr)
        for err in errors:
            print(f"  {err}", file=sys.stderr)
        sys.exit(1)

    print("Parity contract passed: all action pins and common workflows strictly match.")


if __name__ == "__main__":
    main()
