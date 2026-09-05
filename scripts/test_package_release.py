#!/usr/bin/env python3
"""Regression tests for companion and opt-in embedded-runtime packages."""

from __future__ import annotations

import argparse
import json
import tempfile
import unittest
from pathlib import Path, PurePosixPath

import package_release
from verify_package import read_archive


class PackageReleaseTests(unittest.TestCase):
    def make_fixture(self, temporary: Path) -> tuple[Path, dict[str, str]]:
        root = temporary / "repo"
        (root / "packaging").mkdir(parents=True)
        (root / "docs").mkdir()
        (root / "README.md").write_text("companion documentation\n", encoding="utf-8")
        (root / "docs" / "release.md").write_text("release documentation\n", encoding="utf-8")
        controller = root / "codexmarathon.bin"
        runtime = root / "codexmarathon-runtime.bin"
        controller.write_bytes(b"controller\n")
        runtime.write_bytes(b"embedded runtime\n")
        manifest = {
            "schema_version": 1,
            "product": "codexmarathon",
            "distribution": "companion",
            "artifact_root": "codexmarathon-{version}-{platform}",
            "entrypoints": {"controller": "codexmarathon"},
            "optional_entrypoints": {"runtime": "codexmarathon-runtime"},
            "include": ["README.md", "docs/release.md"],
            "provenance": {},
        }
        (root / "packaging" / "release-manifest.json").write_text(
            json.dumps(manifest) + "\n", encoding="utf-8"
        )
        return root, {"controller": str(controller.relative_to(root)), "runtime": str(runtime.relative_to(root))}

    def build_args(self, root: Path, output: Path, binaries: dict[str, str], *, include_runtime: bool) -> argparse.Namespace:
        return argparse.Namespace(
            root=root,
            manifest=None,
            output=output,
            controller_binary=binaries["controller"],
            runtime_binary=binaries["runtime"] if include_runtime else None,
            include_embedded_runtime=include_runtime,
            platform="linux-aarch64",
            version="test",
            format="tar.gz",
            windows_exe=False,
        )

    @staticmethod
    def suffixes(artifact: Path) -> set[str]:
        files = read_archive(artifact)
        return {"/".join(PurePosixPath(name).parts[1:]) for name in files}

    @staticmethod
    def generated_manifest(artifact: Path) -> dict[str, object]:
        files = read_archive(artifact)
        manifest_name = next(name for name in files if PurePosixPath(name).name == "release-manifest.json")
        return json.loads(files[manifest_name].decode("utf-8"))

    def test_default_package_contains_only_companion(self) -> None:
        with tempfile.TemporaryDirectory(prefix="codexmarathon-package-test-") as directory:
            root, binaries = self.make_fixture(Path(directory))
            artifact = package_release.build(
                self.build_args(root, Path(directory) / "out", binaries, include_runtime=False)
            )
            files = self.suffixes(artifact)
            self.assertIn("codexmarathon", files)
            self.assertNotIn("codexmarathon-runtime", files)
            self.assertEqual(self.generated_manifest(artifact)["distribution"], "companion")

    def test_embedded_runtime_requires_explicit_opt_in(self) -> None:
        with tempfile.TemporaryDirectory(prefix="codexmarathon-package-test-") as directory:
            root, binaries = self.make_fixture(Path(directory))
            with self.assertRaisesRegex(RuntimeError, "explicit --include-embedded-runtime"):
                bad_args = self.build_args(root, Path(directory) / "out", binaries, include_runtime=False)
                bad_args.runtime_binary = binaries["runtime"]
                package_release.build(bad_args)

    def test_opt_in_package_contains_runtime_and_marks_distribution(self) -> None:
        with tempfile.TemporaryDirectory(prefix="codexmarathon-package-test-") as directory:
            root, binaries = self.make_fixture(Path(directory))
            artifact = package_release.build(
                self.build_args(root, Path(directory) / "out", binaries, include_runtime=True)
            )
            files = self.suffixes(artifact)
            self.assertIn("codexmarathon", files)
            self.assertIn("codexmarathon-runtime", files)
            self.assertEqual(
                self.generated_manifest(artifact)["distribution"],
                "companion-with-embedded-runtime",
            )


if __name__ == "__main__":
    unittest.main()
