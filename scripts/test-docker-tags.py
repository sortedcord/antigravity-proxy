#!/usr/bin/env python3
"""Test Docker container tag rules and version tag validation."""

import re
import unittest

SEMVER_TAG_REGEX = re.compile(
    r"^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$"
)


def is_valid_version_tag(tag: str) -> bool:
    return bool(SEMVER_TAG_REGEX.match(tag))


def generate_tags(
    event: str,
    ref: str,
    ref_name: str,
    commit_sha: str = "abcdef0123456789abcdef0123456789abcdef01",
    is_default_branch: bool = False,
    is_prerelease: bool = False,
) -> list[str]:
    """Simulate docker/metadata-action rules in .github/workflows/docker.yml."""
    tags = []
    if event == "pull_request":
        tags.append(f"pr-{ref_name}")
        return tags

    if ref.startswith("refs/heads/"):
        tags.append(ref_name)
        tags.append(f"sha-{commit_sha}")
        # Note: latest is NOT generated for branch pushes.
        return tags

    if ref.startswith("refs/tags/"):
        match = SEMVER_TAG_REGEX.match(ref_name)
        if not match:
            raise ValueError(f"Invalid semver tag: {ref_name}")
        major, minor, patch = match.group(1), match.group(2), match.group(3)
        prerelease = match.group(4)

        # type=semver,pattern={{version}}
        version_tag = f"{major}.{minor}.{patch}"
        if prerelease:
            version_tag += f"-{prerelease}"
        tags.append(version_tag)

        # major.minor and major are skipped for prereleases
        if not prerelease and not is_prerelease:
            tags.append(f"{major}.{minor}")
            tags.append(f"{major}")
            # flavor: latest=auto generates latest only for non-prerelease
            tags.append("latest")

        tags.append(f"sha-{commit_sha}")
        return tags

    return tags


class TestDockerTagRules(unittest.TestCase):
    def test_semver_tag_validation(self):
        valid = ["v0.1.0", "v0.1.1", "v0.2.0-rc.1", "v1.0.0", "v1.2.3+build42"]
        for tag in valid:
            self.assertTrue(is_valid_version_tag(tag), f"expected valid: {tag}")

        invalid = ["0.1.0", "v0.1", "master", "latest", "v1.0.0.0", "v01.0.0", "v01.2.3", "v1.2.3-01", "v1.2.3-rc..1"]
        for tag in invalid:
            self.assertFalse(is_valid_version_tag(tag), f"expected invalid: {tag}")

    def test_default_branch_never_publishes_latest(self):
        tags = generate_tags(
            event="push",
            ref="refs/heads/master",
            ref_name="master",
            is_default_branch=True,
        )
        self.assertIn("master", tags)
        self.assertIn("sha-abcdef0123456789abcdef0123456789abcdef01", tags)
        self.assertNotIn("latest", tags)
        self.assertNotIn("0.1.0", tags)

    def test_stable_release_publishes_version_and_latest(self):
        tags = generate_tags(
            event="push",
            ref="refs/tags/v0.1.0",
            ref_name="v0.1.0",
        )
        self.assertIn("0.1.0", tags)
        self.assertIn("0.1", tags)
        self.assertIn("0", tags)
        self.assertIn("latest", tags)
        self.assertNotIn("master", tags)

    def test_prerelease_never_publishes_latest_or_minor_alias(self):
        tags = generate_tags(
            event="push",
            ref="refs/tags/v0.2.0-rc.1",
            ref_name="v0.2.0-rc.1",
            is_prerelease=True,
        )
        self.assertIn("0.2.0-rc.1", tags)
        self.assertNotIn("latest", tags)
        self.assertNotIn("0.2", tags)
        self.assertNotIn("0", tags)
        self.assertNotIn("master", tags)

    def test_pull_request_build_tags(self):
        tags = generate_tags(
            event="pull_request",
            ref="refs/pull/42/merge",
            ref_name="42",
        )
        self.assertEqual(tags, ["pr-42"])


if __name__ == "__main__":
    unittest.main()
