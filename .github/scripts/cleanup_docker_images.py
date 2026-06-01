#!/usr/bin/env python3
# Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
# source code is governed by the Apache v2 license that can be found in the
# LICENSE file.

"""Cleans up old Docker images from GitHub Container Registry.

Handles multi-platform manifests: resolves manifest lists to find referenced
platform images and protects them from deletion.

Keeps: latest, edge, semver (v*), and their arch-suffixed variants.
Deletes: everything else older than 7 days.
"""

import base64
import json
import os
import re
import sys
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

# Tags matching these patterns are always kept.
_KEEP_RE = re.compile(r"^(latest(-.+)?|v\d+.*|edge(-.+)?)$")


@dataclass(frozen=True)
class OwnerInfo:
    """GitHub account metadata needed to choose package API endpoints."""

    kind: str


def _request(
    url: str, method: str = "GET", headers: dict[str, str] | None = None
) -> tuple[int, bytes]:
    req = Request(url, method=method)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urlopen(req) as resp:
            return resp.status, resp.read()
    except HTTPError as e:
        return e.code, e.read()
    except URLError as e:
        print(f"Network error: {e.reason}", file=sys.stderr)
        return 0, b""


def github_api(
    url: str, token: str, method: str = "GET"
) -> tuple[int, bytes]:
    """Make an authenticated GitHub API request."""
    return _request(
        url,
        method,
        {
            "Accept": "application/vnd.github.v3+json",
            "Authorization": f"token {token}",
        },
    )


def _fetch_owner_info(owner: str, token: str) -> OwnerInfo:
    """Return GitHub owner metadata."""
    status, body = github_api(f"https://api.github.com/users/{owner}", token)
    if status != 200:
        raise RuntimeError(f"fetching owner {owner}: HTTP {status}")
    data = json.loads(body)
    kind = data.get("type", "")
    if kind not in ("Organization", "User"):
        raise RuntimeError(f"unsupported GitHub owner type for {owner}: {kind!r}")
    return OwnerInfo(kind=kind)


def _package_api_base(owner: str, repo: str, token: str) -> str:
    """Return the GitHub API base URL for a user or organization package."""
    owner_info = _fetch_owner_info(owner, token)
    scope = "orgs" if owner_info.kind == "Organization" else "users"
    return f"https://api.github.com/{scope}/{owner}/packages/container/{repo}"


def _get_ghcr_token(owner: str, repo: str, github_token: str) -> str:
    """Exchange GitHub token for a GHCR registry token."""
    url = f"https://ghcr.io/token?scope=repository:{owner}/{repo}:pull&service=ghcr.io"
    cred = base64.b64encode(f"token:{github_token}".encode()).decode()
    status, body = _request(url, headers={"Authorization": f"Basic {cred}"})
    if status != 200:
        return ""
    return json.loads(body).get("token", "")


def _fetch_manifest_refs(
    owner: str, repo: str, registry_token: str, tag: str
) -> set[str]:
    """Return digests of platform images referenced by a manifest list."""
    url = f"https://ghcr.io/v2/{owner}/{repo}/manifests/{tag}"
    accept = (
        "application/vnd.oci.image.index.v1+json,"
        "application/vnd.docker.distribution.manifest.list.v2+json"
    )
    status, body = _request(
        url,
        headers={"Authorization": f"Bearer {registry_token}", "Accept": accept},
    )
    if status != 200:
        return set()
    data = json.loads(body)
    return {m["digest"] for m in data.get("manifests", [])}


def _fetch_all_versions(package_api_base: str, token: str) -> list[dict]:
    all_versions: list[dict] = []
    page = 1
    while True:
        print(f"Fetching versions page {page}...")
        url = f"{package_api_base}/versions?per_page=100&page={page}"
        status, body = github_api(url, token)
        if status != 200:
            print(f"Error fetching versions: HTTP {status}", file=sys.stderr)
            break
        batch = json.loads(body)
        if not batch:
            break
        all_versions.extend(batch)
        page += 1
    return all_versions


def _get_tags(v: dict) -> list[str]:
    return v.get("metadata", {}).get("container", {}).get("tags", [])


def _should_keep(tags: list[str]) -> bool:
    return any(_KEEP_RE.match(t) for t in tags)


def main():
    """Delete old untagged Docker images from GHCR."""
    owner = get_env("GITHUB_REPOSITORY_OWNER")
    repo = get_env("PACKAGE_NAME")
    token = get_env("GITHUB_TOKEN")
    print(f"Starting cleanup for image package: {owner}/{repo}")
    cutoff = datetime.now(timezone.utc) - timedelta(days=7)
    print(f"Cutoff date: {cutoff}")

    package_api_base = _package_api_base(owner, repo, token)
    versions = _fetch_all_versions(package_api_base, token)
    print(f"Found {len(versions)} total versions")

    # Phase 1: Identify versions to keep by their tags.
    keep_ids: set[int] = set()
    keep_tags: list[str] = []
    for v in versions:
        tags = _get_tags(v)
        if _should_keep(tags):
            keep_ids.add(v["id"])
            keep_tags.extend(tags)

    # Phase 2: Resolve manifest lists to protect referenced platform images.
    registry_token = _get_ghcr_token(owner, repo, token)
    referenced_digests: set[str] = set()
    if registry_token:
        seen: set[str] = set()
        for tag in keep_tags:
            if tag in seen:
                continue
            seen.add(tag)
            refs = _fetch_manifest_refs(owner, repo, registry_token, tag)
            referenced_digests.update(refs)
        if referenced_digests:
            print(
                f"Protecting {len(referenced_digests)} platform digests"
                " referenced by kept manifests"
            )
    else:
        print("Warning: could not get GHCR token, skipping manifest resolution")
    for v in versions:
        # v["name"] is the version digest (sha256:...).
        if v["id"] not in keep_ids and v["name"] in referenced_digests:
            keep_ids.add(v["id"])

    # Phase 3: Delete old, non-kept versions.
    deleted_count = 0
    failed_count = 0
    kept_count = 0
    processed: list[tuple[str, str, str]] = []
    for v in versions:
        tags = _get_tags(v)
        label = ", ".join(tags) if tags else v["name"][:20]
        if v["id"] in keep_ids:
            kept_count += 1
            continue
        created = datetime.fromisoformat(v["created_at"])
        if created >= cutoff:
            kept_count += 1
            continue
        created_fmt = str(created)
        print(f"Deleting: {label} (created: {created_fmt})")
        del_url = f"{package_api_base}/versions/{v['id']}"
        del_status, _ = github_api(del_url, token, method="DELETE")
        if del_status == 204:
            deleted_count += 1
            processed.append((label, created_fmt, "Deleted"))
        else:
            print(f"Failed to delete {label} (HTTP {del_status})")
            failed_count += 1
            processed.append((label, created_fmt, f"Failed ({del_status})"))

    print(
        f"Cleanup complete. Deleted: {deleted_count},"
        f" Failed: {failed_count}, Kept/Skipped: {kept_count}"
    )

    summary_path = os.environ.get("GITHUB_STEP_SUMMARY", "")
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as f:
            f.write("# Docker Image Cleanup Report\n\n")
            f.write(f"**Repository:** {owner}/{repo}\n")
            f.write(f"**Date:** {datetime.now(timezone.utc)}\n")
            f.write(f"**Cutoff Date:** {cutoff}\n\n")
            f.write("## Summary\n")
            f.write("| Status | Count |\n")
            f.write("| :--- | :--- |\n")
            f.write(f"| Deleted | {deleted_count} |\n")
            f.write(f"| Failed | {failed_count} |\n")
            f.write(f"| Kept/Skipped | {kept_count} |\n\n")
            if processed:
                f.write("## Processed Images\n")
                f.write("| Tag | Created At | Status |\n")
                f.write("| :--- | :--- | :--- |\n")
                for tag, created_fmt, status in processed:
                    f.write(f"| `{tag}` | {created_fmt} | {status} |\n")
            else:
                f.write("_No images were old enough to be deleted._\n")
    return 0


def get_env(name: str) -> str:
    """Return the value of an environment variable or exit."""
    val = os.environ.get(name, "")
    if not val:
        print(f"Error: {name} is not set.", file=sys.stderr)
        sys.exit(1)
    return val


if __name__ == "__main__":
    sys.exit(main())
