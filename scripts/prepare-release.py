#!/usr/bin/env python3
"""Bind a packaged Helm chart to the image built from its exact source commit."""

import argparse
import json
import os
from pathlib import Path
import re


def prepare(chart, metadata, env):
    tag = env["RELEASE_TAG"]
    commit = env["SOURCE_COMMIT"]
    repository = env["IMAGE_REPOSITORY"]
    digest = env["IMAGE_DIGEST"]
    if not re.fullmatch(r"v\d+\.\d+\.\d+", tag):
        raise ValueError("RELEASE_TAG must be vX.Y.Z")
    if not re.fullmatch(r"[a-f0-9]{40}", commit):
        raise ValueError("SOURCE_COMMIT must be a full commit SHA")
    if not re.fullmatch(r"ghcr.io/[a-z0-9._-]+/[a-z0-9._-]+", repository):
        raise ValueError("IMAGE_REPOSITORY must be a lowercase GHCR repository")
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
        raise ValueError("IMAGE_DIGEST must be a sha256 digest")
    path = chart / "Chart.yaml"
    source = path.read_text()
    for key in ("version", "appVersion"):
        match = re.search(rf'^{key}:\s*[\"\']?([^\s\"\']+)', source, re.MULTILINE)
        if not match or match[1] != tag[1:]:
            raise ValueError(f"Chart {key} must match {tag}")
    if re.search(r"^annotations:", source, re.MULTILINE):
        raise ValueError("Prepare the source chart, which must not already have release annotations")
    annotations = {
        "vbilling.vcluster.com/source-commit": commit,
        "vbilling.vcluster.com/image-repository": repository,
        "vbilling.vcluster.com/image-tag": tag,
        "vbilling.vcluster.com/image-digest": digest,
    }
    block = "\nannotations:\n" + "".join(
        f"  {key}: {json.dumps(value)}\n" for key, value in annotations.items()
    )
    path.write_text(source.rstrip() + block)
    metadata.parent.mkdir(parents=True, exist_ok=True)
    metadata.write_text(json.dumps({
        "tag": tag,
        "source_commit": commit,
        "image": f"{repository}@{digest}",
        "image_digest": digest,
        "platforms": ["linux/amd64", "linux/arm64"],
        "chart": f"vbilling-{tag[1:]}.tgz",
    }, indent=2) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chart", type=Path, required=True)
    parser.add_argument("--metadata", type=Path, required=True)
    args = parser.parse_args()
    try:
        prepare(args.chart, args.metadata, os.environ)
    except (KeyError, ValueError) as exc:
        parser.exit(1, f"Release preparation refused: {exc}\n")
