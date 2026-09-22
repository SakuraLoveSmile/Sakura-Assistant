#!/usr/bin/env python3
"""Build and publish immutable assistant-agent release assets.

The GitHub Actions workflow uses this module for the deterministic parts of
release handling.  The GitHub client is deliberately tiny and injectable so
the no-overwrite rules can be tested without network access.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol

VERSION_RE = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$")
TAG_RE = re.compile(r"^v([0-9]+\.[0-9]+\.[0-9]+)$")
ASSET_RE = re.compile(r"^assistant-agent_([0-9]+\.[0-9]+\.[0-9]+)_linux_(amd64|arm64)$")
SUMS_NAME = "agent-SHA256SUMS.txt"
MANIFEST_NAME = "agent-manifest.json"
ARCHES = ("amd64", "arm64")
MAX_ASSET_SIZE = 128 * 1024 * 1024
OWNERSHIP_MARKER = "<!-- assistant-agent-release-workflow -->"


class ReleaseError(RuntimeError):
    pass


def release_lookup_decision(status_code: int, *, draft: bool = False,
                            body: str = "") -> tuple[bool, bool]:
    """Return (create, publish) for a release lookup response."""
    if status_code == 404:
        return True, True
    if status_code == 200:
        return False, draft and OWNERSHIP_MARKER in body
    raise ReleaseError(f"release lookup failed with HTTP {status_code}")


def ensure_release(repository: str, tag: str) -> dict[str, Any]:
    """Look up an authenticated release; only an actual 404 permits creation."""
    version_from_tag(tag)
    result = subprocess.run(
        ["gh", "api", "--include", f"repos/{repository}/releases/tags/{tag}"],
        capture_output=True, text=True,
    )
    response = result.stdout.replace("\r\n", "\n")
    headers, separator, body_text = response.partition("\n\n")
    first_line = headers.split("\n", 1)[0]
    status_match = re.fullmatch(r"HTTP/\S+ (\d{3})(?: .*)?", first_line)
    if not status_match or not separator:
        raise ReleaseError("release lookup did not return an HTTP response")
    status = int(status_match.group(1))
    if status == 200 and result.returncode != 0:
        raise ReleaseError("release lookup failed despite HTTP 200")
    data = json.loads(body_text) if status == 200 else {}
    create, publish = release_lookup_decision(
        status, draft=data.get("draft", False), body=data.get("body") or "",
    )
    if create:
        payload = {"tag_name": tag, "name": f"assistant-agent {tag}",
                   "body": OWNERSHIP_MARKER, "draft": True, "prerelease": False}
        created = subprocess.run(
            ["gh", "api", "--method", "POST", f"repos/{repository}/releases", "--input", "-"],
            input=json.dumps(payload), capture_output=True, text=True, check=True,
        )
        data = json.loads(created.stdout)
    if not isinstance(data.get("id"), int) or data["id"] <= 0:
        raise ReleaseError("release response is missing its numeric ID")
    return {"release_id": data["id"], "publish": publish}


def version_from_tag(tag: str) -> str:
    match = TAG_RE.fullmatch(tag)
    if not match:
        raise ReleaseError(f"tag must match vX.Y.Z: {tag!r}")
    return match.group(1)


def _asset_name(version: str, arch: str) -> str:
    if not VERSION_RE.fullmatch(version) or arch not in ARCHES:
        raise ReleaseError("invalid version or architecture")
    return f"assistant-agent_{version}_linux_{arch}"


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def build_manifest(version: str, assets_dir: Path) -> dict[str, Any]:
    if not VERSION_RE.fullmatch(version):
        raise ReleaseError(f"invalid release version: {version!r}")
    assets: list[dict[str, Any]] = []
    for arch in ARCHES:
        path = assets_dir / _asset_name(version, arch)
        if not path.is_file():
            raise ReleaseError(f"missing binary: {path}")
        size = path.stat().st_size
        if size <= 0:
            raise ReleaseError(f"binary is empty: {path}")
        assets.append({
            "os": "linux",
            "arch": arch,
            "name": path.name,
            "size": size,
            "sha256": sha256(path),
        })
    return {"schemaVersion": 1, "version": version, "assets": assets}


def write_manifest(version: str, assets_dir: Path) -> Path:
    manifest = build_manifest(version, assets_dir)
    path = assets_dir / MANIFEST_NAME
    path.write_text(json.dumps(manifest, ensure_ascii=True, separators=(",", ":")) + "\n",
                    encoding="utf-8")
    return path


def write_checksums(version: str, assets_dir: Path) -> Path:
    paths = [assets_dir / _asset_name(version, arch) for arch in ARCHES]
    for path in paths:
        if not path.is_file() or path.stat().st_size <= 0:
            raise ReleaseError(f"missing or empty binary: {path}")
    path = assets_dir / SUMS_NAME
    path.write_text("".join(f"{sha256(item)}  {item.name}\n" for item in paths),
                    encoding="utf-8")
    return path


def validate_manifest(path: Path, version: str) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ReleaseError(f"invalid manifest: {path}: {exc}") from exc
    if not isinstance(data, dict) or type(data.get("schemaVersion")) is not int or data.get("schemaVersion") != 1 or data.get("version") != version:
        raise ReleaseError("manifest schemaVersion/version mismatch")
    assets = data.get("assets")
    if not isinstance(assets, list) or len(assets) != 2:
        raise ReleaseError("manifest must contain exactly two assets")
    expected = {("linux", arch) for arch in ARCHES}
    actual: set[tuple[str, str]] = set()
    for item in assets:
        if not isinstance(item, dict):
            raise ReleaseError("manifest asset must be an object")
        key = (item.get("os"), item.get("arch"))
        if key in actual or key not in expected:
            raise ReleaseError("manifest contains duplicate or unexpected asset")
        actual.add(key)
        name = item.get("name")
        if name != _asset_name(version, item["arch"]):
            raise ReleaseError("manifest asset name mismatch")
        if type(item.get("size")) is not int or not 0 < item["size"] <= MAX_ASSET_SIZE:
            raise ReleaseError("manifest asset size must be positive and at most 128 MiB")
        if not isinstance(item.get("sha256"), str) or not re.fullmatch(r"[0-9a-f]{64}", item["sha256"]):
            raise ReleaseError("manifest asset sha256 must be lowercase hex")
    return data


def validate_local_assets(assets_dir: Path) -> tuple[str, dict[str, Path]]:
    manifest_path = assets_dir / MANIFEST_NAME
    try:
        data = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ReleaseError(f"invalid manifest: {manifest_path}: {exc}") from exc
    version = data.get("version") if isinstance(data, dict) else None
    if not isinstance(version, str):
        raise ReleaseError("manifest version is missing")
    validate_manifest(manifest_path, version)
    expected = {
        _asset_name(version, arch) for arch in ARCHES
    } | {SUMS_NAME, MANIFEST_NAME}
    actual = {item.name for item in assets_dir.iterdir() if item.is_file()}
    if actual != expected:
        raise ReleaseError(f"assets directory must contain exactly {sorted(expected)}; got {sorted(actual)}")
    paths = {name: assets_dir / name for name in expected}
    for name, path in paths.items():
        if path.stat().st_size > MAX_ASSET_SIZE:
            raise ReleaseError(f"asset exceeds 128 MiB: {name}")
    by_name = {item["name"]: item for item in data["assets"]}
    for arch in ARCHES:
        name = _asset_name(version, arch)
        path = paths[name]
        item = by_name[name]
        if path.stat().st_size != item["size"] or sha256(path) != item["sha256"]:
            raise ReleaseError(f"manifest does not match local asset: {name}")
    expected_sums = "".join(
        f"{item['sha256']}  {item['name']}\n" for item in data["assets"]
    )
    if paths[SUMS_NAME].read_text(encoding="utf-8") != expected_sums:
        raise ReleaseError("checksum file does not match manifest")
    return version, paths


@dataclass(frozen=True)
class RemoteAsset:
    asset_id: int
    name: str
    size: int


class ReleaseClient(Protocol):
    def list_assets(self) -> list[RemoteAsset]: ...
    def download_asset(self, asset_id: int, destination: Path) -> None: ...
    def upload_asset(self, path: Path) -> None: ...
    def publish_release(self) -> None: ...


def _same_file(local: Path, remote: Path) -> bool:
    return local.stat().st_size == remote.stat().st_size and sha256(local) == sha256(remote)


def _verify_remote(client: ReleaseClient, remote: RemoteAsset, path: Path) -> None:
    with tempfile.TemporaryDirectory(prefix="assistant-agent-remote-") as directory:
        downloaded = Path(directory) / path.name
        client.download_asset(remote.asset_id, downloaded)
        if not _same_file(path, downloaded):
            raise ReleaseError(f"remote asset differs from local: {path.name}")


def publish_assets(client: ReleaseClient, assets_dir: Path, *, publish: bool) -> None:
    """Upload missing assets, verify every remote asset, then optionally publish.

    The manifest is intentionally uploaded last.  Existing assets are always
    downloaded before they are accepted; a same-name mismatch is fatal.
    """
    version, paths = validate_local_assets(assets_dir)
    files = [paths[_asset_name(version, arch)] for arch in ARCHES] + [
        paths[SUMS_NAME]
    ]
    manifest = paths[MANIFEST_NAME]
    remote = {item.name: item for item in client.list_assets()}
    # Complete the read-only conflict check before uploading anything. This
    # keeps a bad rerun from leaving a partially populated release.
    for path in files:
        if path.name in remote:
            _verify_remote(client, remote[path.name], path)
    if MANIFEST_NAME in remote:
        _verify_remote(client, remote[MANIFEST_NAME], manifest)
    for path in files:
        if path.name in remote:
            continue
        client.upload_asset(path)
        uploaded = {item.name: item for item in client.list_assets()}
        if path.name not in uploaded:
            raise ReleaseError(f"uploaded asset not visible: {path.name}")
        _verify_remote(client, uploaded[path.name], path)
    # The manifest is the completion marker and is always handled last.
    remote = {item.name: item for item in client.list_assets()}
    if MANIFEST_NAME in remote:
        _verify_remote(client, remote[MANIFEST_NAME], manifest)
    else:
        client.upload_asset(manifest)
        uploaded = {item.name: item for item in client.list_assets()}
        if MANIFEST_NAME not in uploaded:
            raise ReleaseError("uploaded manifest not visible")
        _verify_remote(client, uploaded[MANIFEST_NAME], manifest)
    if publish:
        client.publish_release()


class GhReleaseClient:
    def __init__(self, repository: str, release_id: str, tag: str) -> None:
        self.repository = repository
        self.release_id = release_id
        self.tag = tag

    def _api(self, *args: str, output: Path | None = None) -> Any:
        command = ["gh", "api", *args]
        if output is None:
            result = subprocess.run(command, check=True, capture_output=True, text=True)
            return json.loads(result.stdout) if result.stdout else None
        with output.open("wb") as stream:
            subprocess.run(command, check=True, stdout=stream)
        return None

    def list_assets(self) -> list[RemoteAsset]:
        pages = self._api(f"repos/{self.repository}/releases/{self.release_id}/assets"
                          "?per_page=100", "--paginate", "--slurp")
        return [RemoteAsset(int(item["id"]), item["name"], int(item["size"]))
                for page in pages for item in page]

    def download_asset(self, asset_id: int, destination: Path) -> None:
        self._api(f"repos/{self.repository}/releases/assets/{asset_id}",
                  "-H", "Accept: application/octet-stream", output=destination)

    def upload_asset(self, path: Path) -> None:
        subprocess.run([
            "gh", "release", "upload", self.tag, str(path),
            "--repo", self.repository,
        ], check=True)

    def publish_release(self) -> None:
        self._api(f"repos/{self.repository}/releases/{self.release_id}",
                  "--method", "PATCH", "-F", "draft=false")


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    ensure = sub.add_parser("ensure")
    ensure.add_argument("--repo", required=True)
    ensure.add_argument("--tag", required=True)
    make = sub.add_parser("prepare")
    make.add_argument("--version", required=True)
    make.add_argument("--dir", type=Path, required=True)
    publish = sub.add_parser("publish")
    publish.add_argument("--repo", required=True)
    publish.add_argument("--release-id", required=True)
    publish.add_argument("--tag", required=True)
    publish.add_argument("--dir", type=Path, required=True)
    publish.add_argument("--publish", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.command == "ensure":
            print(json.dumps(ensure_release(args.repo, args.tag)))
        elif args.command == "prepare":
            write_checksums(args.version, args.dir)
            write_manifest(args.version, args.dir)
            validate_manifest(args.dir / MANIFEST_NAME, args.version)
        else:
            version, _ = validate_local_assets(args.dir)
            if version_from_tag(args.tag) != version:
                raise ReleaseError("release tag does not match local manifest version")
            publish_assets(GhReleaseClient(args.repo, args.release_id, args.tag), args.dir,
                           publish=args.publish)
    except ReleaseError as exc:
        print(f"release: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
