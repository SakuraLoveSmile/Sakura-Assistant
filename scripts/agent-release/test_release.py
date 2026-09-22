#!/usr/bin/env python3
import json
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from pathlib import Path

from release import (MANIFEST_NAME, ReleaseError, RemoteAsset, GhReleaseClient, build_manifest,
                     publish_assets, validate_manifest, write_checksums,
                     write_manifest, version_from_tag, release_lookup_decision, ensure_release)


class MockRelease:
    def __init__(self):
        self.files: dict[str, bytes] = {}
        self.uploaded: list[str] = []
        self.published = False
        self.fail_publish_once = False

    def list_assets(self):
        return [RemoteAsset(index + 1, name, len(content))
                for index, (name, content) in enumerate(self.files.items())]

    def download_asset(self, asset_id, destination):
        name = self.list_assets()[asset_id - 1].name
        destination.write_bytes(self.files[name])

    def upload_asset(self, path):
        self.files[path.name] = path.read_bytes()
        self.uploaded.append(path.name)

    def publish_release(self):
        if self.fail_publish_once:
            self.fail_publish_once = False
            raise RuntimeError("transient publish failure")
        self.published = True


class ReleaseTests(unittest.TestCase):
    def make_assets(self, directory: Path, version="1.2.3"):
        for arch, payload in (("amd64", b"amd64 binary"), ("arm64", b"arm64 binary")):
            (directory / f"assistant-agent_{version}_linux_{arch}").write_bytes(payload)

    def test_manifest_and_checksums_are_deterministic(self):
        with tempfile.TemporaryDirectory() as raw:
            directory = Path(raw)
            self.make_assets(directory)
            write_checksums("1.2.3", directory)
            write_manifest("1.2.3", directory)
            manifest = validate_manifest(directory / MANIFEST_NAME, "1.2.3")
            self.assertEqual(manifest["schemaVersion"], 1)
            self.assertEqual([item["arch"] for item in manifest["assets"]], ["amd64", "arm64"])
            self.assertEqual(len((directory / "agent-SHA256SUMS.txt").read_text().splitlines()), 2)

    def test_publish_skips_equal_existing_assets_and_publishes_manifest_last(self):
        with tempfile.TemporaryDirectory() as raw:
            directory = Path(raw)
            self.make_assets(directory)
            write_checksums("1.2.3", directory)
            write_manifest("1.2.3", directory)
            client = MockRelease()
            client.files = {path.name: path.read_bytes() for path in directory.iterdir()
                            if path.name != MANIFEST_NAME}
            publish_assets(client, directory, publish=True)
            self.assertEqual(client.uploaded, [MANIFEST_NAME])
            self.assertTrue(client.published)

    def test_conflicting_existing_asset_fails_without_upload(self):
        with tempfile.TemporaryDirectory() as raw:
            directory = Path(raw)
            self.make_assets(directory)
            write_checksums("1.2.3", directory)
            write_manifest("1.2.3", directory)
            client = MockRelease()
            client.files["assistant-agent_1.2.3_linux_amd64"] = b"tampered"
            with self.assertRaises(ReleaseError):
                publish_assets(client, directory, publish=True)
            self.assertFalse(client.uploaded)
            self.assertFalse(client.published)

    def test_tag_and_manifest_reject_invalid_values(self):
        self.assertEqual(version_from_tag("v1.2.3"), "1.2.3")
        with self.assertRaises(ReleaseError):
            version_from_tag("1.2.3")
        with tempfile.TemporaryDirectory() as raw:
            directory = Path(raw)
            self.make_assets(directory)
            manifest = build_manifest("1.2.3", directory)
            manifest["assets"][0]["sha256"] = "A" * 64
            path = directory / MANIFEST_NAME
            path.write_text(json.dumps(manifest))
            with self.assertRaises(ReleaseError):
                validate_manifest(path, "1.2.3")

    def test_manifest_conflict_is_checked_before_any_upload(self):
        with tempfile.TemporaryDirectory() as raw:
            directory = Path(raw)
            self.make_assets(directory)
            write_checksums("1.2.3", directory)
            write_manifest("1.2.3", directory)
            client = MockRelease()
            client.files[MANIFEST_NAME] = b"wrong manifest"
            with self.assertRaises(ReleaseError):
                publish_assets(client, directory, publish=True)
            self.assertEqual(client.uploaded, [])
            self.assertFalse(client.published)

    def test_extra_asset_is_rejected_before_upload(self):
        with tempfile.TemporaryDirectory() as raw:
            directory = Path(raw)
            self.make_assets(directory)
            write_checksums("1.2.3", directory)
            write_manifest("1.2.3", directory)
            (directory / "unexpected").write_bytes(b"x")
            with self.assertRaises(ReleaseError):
                publish_assets(MockRelease(), directory, publish=True)

    def test_publish_failure_can_be_retried_without_reuploading_assets(self):
        with tempfile.TemporaryDirectory() as raw:
            directory = Path(raw)
            self.make_assets(directory)
            write_checksums("1.2.3", directory)
            write_manifest("1.2.3", directory)
            client = MockRelease()
            client.fail_publish_once = True
            with self.assertRaises(RuntimeError):
                publish_assets(client, directory, publish=True)
            first_uploads = list(client.uploaded)
            publish_assets(client, directory, publish=True)
            self.assertEqual(client.uploaded, first_uploads)
            self.assertTrue(client.published)

    def test_release_lookup_distinguishes_not_found_server_error_and_owned_draft(self):
        self.assertEqual(release_lookup_decision(404), (True, True))
        with self.assertRaises(ReleaseError):
            release_lookup_decision(500)
        self.assertEqual(release_lookup_decision(200, draft=True,
                         body="<!-- assistant-agent-release-workflow -->"), (False, True))
        self.assertEqual(release_lookup_decision(200, draft=True, body="other"), (False, False))
        self.assertEqual(release_lookup_decision(200, draft=False,
                         body="<!-- assistant-agent-release-workflow -->"), (False, False))

    def test_actual_lookup_does_not_create_on_server_error(self):
        result = SimpleNamespace(stdout='HTTP/2.0 500 Internal Server Error\r\n\r\n{}', returncode=1)
        with patch("release.subprocess.run", return_value=result) as run:
            with self.assertRaises(ReleaseError):
                ensure_release("SakuraLoveSmile/Sakura-Assistant", "v1.2.3")
            self.assertEqual(run.call_count, 1)

    def test_actual_lookup_creates_draft_only_on_404(self):
        not_found = SimpleNamespace(stdout='HTTP/2.0 404 Not Found\r\n\r\n{}', returncode=1)
        created = SimpleNamespace(stdout='{"id":42}', returncode=0)
        with patch("release.subprocess.run", side_effect=[not_found, created]) as run:
            self.assertEqual(ensure_release("SakuraLoveSmile/Sakura-Assistant", "v1.2.3"),
                             {"release_id": 42, "publish": True})
            payload = json.loads(run.call_args.kwargs["input"])
            self.assertIs(payload["draft"], True)
            self.assertEqual(payload["tag_name"], "v1.2.3")

    def test_actual_lookup_preserves_existing_and_recovers_owned_draft(self):
        for draft, body, publish in [(False, "existing", False), (True, "someone else's draft", False),
                                      (True, "<!-- assistant-agent-release-workflow -->", True)]:
            response = json.dumps({"id": 42, "draft": draft, "body": body})
            result = SimpleNamespace(stdout='HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n' + response,
                                     returncode=0)
            with patch("release.subprocess.run", return_value=result) as run:
                self.assertEqual(ensure_release("SakuraLoveSmile/Sakura-Assistant", "v1.2.3"),
                                 {"release_id": 42, "publish": publish})
                self.assertEqual(run.call_count, 1)

    def test_workflow_checks_out_immutable_tag_commit(self):
        workflow = (Path(__file__).parents[2] / ".github" / "workflows" /
                    "release-agent.yml").read_text(encoding="utf-8")
        self.assertIn('printf \'tag=%s\\nref=%s\\nversion=%s\\n\' "$tag" "$tag_commit" "$version"', workflow)
        self.assertGreaterEqual(workflow.count('ref: "${{ needs.validate.outputs.ref }}"'), 2)

    def test_cli_adapter_uses_asset_id_download_and_tag_upload(self):
        calls = []

        def run(command, **kwargs):
            calls.append(command)
            if command[1:2] == ["api"] and kwargs.get("stdout") is None:
                class Result:
                    stdout = '[[{"id":7,"name":"agent-manifest.json","size":3}]]'
                return Result()
            if kwargs.get("stdout") is not None:
                kwargs["stdout"].write(b"abc")
            return object()

        with patch("release.subprocess.run", side_effect=run):
            client = GhReleaseClient("SakuraLoveSmile/Sakura-Assistant", "42", "v1.2.3")
            assets = client.list_assets()
            with tempfile.NamedTemporaryFile() as output:
                client.download_asset(7, Path(output.name))
            client.upload_asset(Path(output.name))
            client.publish_release()
        self.assertEqual(assets[0].asset_id, 7)
        list_call = next(item for item in calls if item[0:2] == ["gh", "api"] and "per_page=100" in item[2])
        self.assertIn("--paginate", list_call)
        self.assertIn("--slurp", list_call)
        self.assertTrue(any(any("releases/assets/7" in arg for arg in item) for item in calls))
        upload = next(item for item in calls if item[0:3] == ["gh", "release", "upload"])
        self.assertEqual(upload[3], "v1.2.3")
        self.assertIn("--repo", upload)
        patch_call = next(item for item in calls if "--method" in item)
        self.assertIn("-F", patch_call)


if __name__ == "__main__":
    unittest.main()
