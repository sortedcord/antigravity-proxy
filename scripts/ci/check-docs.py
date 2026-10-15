#!/usr/bin/env python3
"""Validate markdown documentation internal links, anchors, and code fences."""

import os
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parent.parent.parent
LINK_PATTERN = re.compile(r"\[([^\]]+)\]\(([^)]+)\)")


def extract_heading_slugs(path: Path) -> set[str]:
    slugs = set()
    try:
        text = path.read_text(encoding="utf-8")
        for line in text.splitlines():
            if line.startswith("#"):
                heading_text = line.lstrip("#").strip()
                cleaned = re.sub(r"[^a-zA-Z0-9_\- ]", "", heading_text).strip().lower()
                slugs.add(cleaned.replace(" ", "-"))
                slugs.add(re.sub(r"\s+", "-", cleaned))
    except Exception:
        pass
    return slugs


def check_file(path: Path) -> list[str]:
    errors = []
    try:
        text = path.read_text(encoding="utf-8")
    except Exception as exc:
        return [f"{path}: Cannot read file: {exc}"]

    lines = text.splitlines()

    # Check balanced code fences
    fence_count = sum(1 for line in lines if line.lstrip().startswith("```"))
    if fence_count % 2 != 0:
        errors.append(f"{path}: Unbalanced markdown code fences ({fence_count} found)")

    # Collect headings for same-file anchor checking
    own_slugs = extract_heading_slugs(path)

    # Check local relative links and anchors
    for line_no, line in enumerate(lines, 1):
        for match in LINK_PATTERN.finditer(line):
            link = match.group(2).strip()

            # Skip external links and mailto
            if (
                link.startswith("http://")
                or link.startswith("https://")
                or link.startswith("mailto:")
            ):
                continue

            if link.startswith("#"):
                anchor = link[1:].strip()
                if anchor and anchor not in own_slugs:
                    errors.append(
                        f"{path}:{line_no}: Broken internal anchor '#{anchor}' (heading not found in {path.name})"
                    )
                continue

            parts = link.split("#", 1)
            target_rel = parts[0]
            anchor = parts[1] if len(parts) > 1 else None

            if not target_rel:
                continue

            target_path = (path.parent / target_rel).resolve()
            if not target_path.exists():
                errors.append(
                    f"{path}:{line_no}: Broken relative link '{link}' (target '{target_path}' does not exist)"
                )
            elif anchor and target_path.suffix.lower() == ".md":
                target_slugs = extract_heading_slugs(target_path)
                if anchor not in target_slugs:
                    errors.append(
                        f"{path}:{line_no}: Broken anchor '#{anchor}' in '{target_rel}' (heading not found)"
                    )

    return errors


def main():
    all_errors = []
    md_files = list(ROOT.glob("*.md")) + list(ROOT.glob("docs/**/*.md"))

    print(f"Checking {len(md_files)} markdown files in {ROOT.name}...")
    for md_file in md_files:
        errs = check_file(md_file)
        if errs:
            all_errors.extend(errs)

    if all_errors:
        print("\nDocumentation check errors found:", file=sys.stderr)
        for err in all_errors:
            print(f"  {err}", file=sys.stderr)
        sys.exit(1)

    print("All markdown files passed documentation and anchor checks.")


if __name__ == "__main__":
    main()
