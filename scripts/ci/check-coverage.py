#!/usr/bin/env python3
"""Validate test coverage and emit package-level breakdown."""

import os
from pathlib import Path
import sys


def parse_coverage(file_path: Path):
    if not file_path.is_file():
        raise FileNotFoundError(f"Coverage file not found: {file_path}")

    packages = {}
    total_stmts = 0
    covered_stmts = 0

    with file_path.open("r", encoding="utf-8") as stream:
        header = stream.readline()
        if not header.startswith("mode:"):
            raise ValueError(f"Invalid coverage file format: {header}")

        for line_no, line in enumerate(stream, 2):
            line = line.strip()
            if not line:
                continue
            # Format: path/file.go:startline.col,endline.col numstmt count
            parts = line.split(" ")
            if len(parts) != 3:
                raise ValueError(f"Malformed coverage record at line {line_no}: {line}")
            path_range, num_stmt_str, count_str = parts
            try:
                num_stmt = int(num_stmt_str)
                count = int(count_str)
            except ValueError:
                raise ValueError(f"Invalid statement/count integers at line {line_no}: {line}")

            file_path_str = path_range.split(":")[0]
            pkg = os.path.dirname(file_path_str)

            if pkg not in packages:
                packages[pkg] = {"stmts": 0, "covered": 0}

            packages[pkg]["stmts"] += num_stmt
            total_stmts += num_stmt
            if count > 0:
                packages[pkg]["covered"] += num_stmt
                covered_stmts += num_stmt

    return packages, total_stmts, covered_stmts


def main():
    coverage_path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("coverage.out")
    min_coverage = float(os.environ.get("MIN_COVERAGE_PERCENT", "70.0"))

    try:
        packages, total_stmts, covered_stmts = parse_coverage(coverage_path)
    except Exception as exc:
        print(f"Coverage check failed: {exc}", file=sys.stderr)
        sys.exit(1)

    total_pct = (covered_stmts / total_stmts * 100.0) if total_stmts > 0 else 0.0

    print("Coverage summary by package:")
    print(f"{'Package':<50} {'Coverage':>10} {'Statements':>12}")
    print("-" * 74)

    rows = []
    for pkg in sorted(packages.keys()):
        stats = packages[pkg]
        pkg_pct = (stats["covered"] / stats["stmts"] * 100.0) if stats["stmts"] > 0 else 0.0
        print(f"{pkg:<50} {pkg_pct:>9.1f}% {stats['covered']:>5}/{stats['stmts']:<5}")
        rows.append((pkg, pkg_pct, stats["covered"], stats["stmts"]))

    print("-" * 74)
    print(f"{'TOTAL':<50} {total_pct:>9.1f}% {covered_stmts:>5}/{total_stmts:<5}")

    summary_file = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary_file:
        try:
            with open(summary_file, "a", encoding="utf-8") as sf:
                sf.write("### Test Coverage Summary\n\n")
                sf.write(f"**Total Statement Coverage:** `{total_pct:.1f}%` (minimum required: `{min_coverage:.1f}%`)\n\n")
                sf.write("| Package | Coverage | Statements |\n")
                sf.write("| :--- | :---: | :---: |\n")
                for pkg, pct, cov, stmts in rows:
                    sf.write(f"| `{pkg}` | {pct:.1f}% | {cov} / {stmts} |\n")
                sf.write(f"| **Total** | **{total_pct:.1f}%** | **{covered_stmts} / {total_stmts}** |\n\n")
        except Exception as exc:
            print(f"Warning: could not write GITHUB_STEP_SUMMARY: {exc}", file=sys.stderr)

    if total_pct < min_coverage:
        print(f"\nFAILURE: Total coverage {total_pct:.1f}% is below threshold {min_coverage:.1f}%", file=sys.stderr)
        sys.exit(1)

    print(f"\nSUCCESS: Total coverage {total_pct:.1f}% satisfies threshold ({min_coverage:.1f}%).")


if __name__ == "__main__":
    main()
