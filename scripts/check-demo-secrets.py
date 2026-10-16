#!/usr/bin/env python3
"""Read-only, redacting pre-commit check. Never prints matching credential values.

Run from a Git checkout:
  python3 scripts/check-demo-secrets.py --tracked
  python3 scripts/check-demo-secrets.py --staged
  python3 scripts/check-demo-secrets.py --directory examples/gke-stripe

Staged mode scans full index blobs, not just added lines. --tracked scans the
working copies of all tracked files. No files or Git state are changed.
This focused check supplements review; it is not a complete secret detector.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

PATTERNS = (
    ("Stripe key", re.compile(rb"\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{20,}")),
    ("private key", re.compile(rb"-----BEGIN (?:RSA |EC |OPENSSH |DSA |ENCRYPTED )?PRIVATE KEY-----")),
    ("GitHub token", re.compile(rb"\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})")),
    ("Google API key", re.compile(rb"\bAIza[0-9A-Za-z_-]{30,}")),
    ("AWS access key", re.compile(rb"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b")),
    ("JWT token", re.compile(rb"\beyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{16,}")),
    ("kubeconfig private key", re.compile(rb"client-key-data:\s*[A-Za-z0-9+/=]{30,}")),
    ("join token in URL", re.compile(rb"/node/join\?token=[A-Za-z0-9._~%+/-]{16,}")),
    ("literal bearer token", re.compile(rb"(?i)bearer\s+[A-Za-z0-9._~+/-]{32,}")),
    ("literal credential assignment", re.compile(
        rb"(?i)(?:[\"']?)(?:STRIPE_API_KEY|VCLUSTER_PLATFORM_KEY|VCLUSTER_ACCESS_KEY|LOFT_ACCESS_KEY|API_TOKEN|INGEST_TOKEN|api[-_]?key|access[-_]?key|client[-_]?secret|refresh[-_]?token|access[-_]?token|token|password)(?:[\"']?)\s*[:=]\s*[\"']([A-Za-z0-9._~+/=-]{32,})[\"']"
    )),
)
# These are generated runtime/credential files, not reusable source manifests.
SENSITIVE_NAMES = re.compile(
    r"(?:^|/)(?:\.env(?:\..+)?|join\.txt|startup\.sh|stripe(?:-test|-live)?-key|"
    r"api-token|private-tenants\.json|platform-cli\.json|vcluster-cli\.json|private-join\.txt|private-startup\.sh|credentials\.json|application_default_credentials\.json|"
    r"[^/]*\.kubeconfig|kubeconfig(?:\.[^/]*)?)$", re.I
)
SKIP_DIRS = {".git", "__pycache__", "node_modules", ".venv", "vendor"}


def git(*args):
    return subprocess.check_output(["git", *args], stderr=subprocess.PIPE)


def findings_for(path, data):
    found = []
    if SENSITIVE_NAMES.search(path) and not path.endswith((".example", ".sample", ".template")):
        found.append({"path": path, "line": 1, "type": "runtime credential filename"})
    for kind, pattern in PATTERNS:
        for match in pattern.finditer(data):
            found.append({"path": path, "line": data.count(b"\n", 0, match.start()) + 1, "type": kind})
    return found


def self_test():
    samples = [
        b"sk_" + b"test_" + b"1AbCdEfGhJkLmn234567890pQrSTuvwxyz",
        b"-----BEGIN " + b"PRIVATE KEY-----",
        b"client-key-data: " + b"M" * 40,
        b"https://demo.example/node/join?token=" + b"Xy123" * 10,
        b'API_TOKEN="' + b"aB12" * 12 + b'"',
    ]
    for data in samples:
        assert findings_for("sample.txt", data), "synthetic credential was missed"
    safe = b'export STRIPE_API_KEY="$(cat "$STRIPE_KEY_FILE")"\nsk_test_...\nsecretName: vbilling-stripe\n'
    assert not findings_for("README.md", safe), "placeholder or file reference was flagged"
    assert findings_for("host.kubeconfig", b"apiVersion: v1"), "sensitive filename was missed"
    assert not findings_for("host.kubeconfig.example", b"apiVersion: v1"), "template filename was flagged"
    print("Self-test passed (synthetic data; credential values not displayed).")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--staged", action="store_true")
    mode.add_argument("--tracked", action="store_true")
    mode.add_argument("--directory", type=Path)
    mode.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return 0
    files = []
    if args.directory:
        root = args.directory.resolve(strict=True)
        for current, dirs, names in os.walk(root):
            dirs[:] = sorted(d for d in dirs if d not in SKIP_DIRS)
            for name in sorted(names):
                path = Path(current) / name
                if path.is_symlink():
                    raise ValueError("Refusing symlink in scan directory: " + str(path.relative_to(root)))
                files.append((str(path.relative_to(root)), path.read_bytes()))
    else:
        root = Path(os.fsdecode(git("rev-parse", "--show-toplevel").strip()))
        os.chdir(root)
        if args.staged:
            names = git("diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z").split(b"\0")
            for raw in filter(None, names):
                path = os.fsdecode(raw)
                files.append((path, git("show", ":" + path)))
        else:
            for raw in filter(None, git("ls-files", "-z").split(b"\0")):
                path = os.fsdecode(raw)
                target = root / path
                if target.exists() and target.is_file():
                    if target.is_symlink():
                        raise ValueError("Refusing tracked symlink: " + path)
                    files.append((path, target.read_bytes()))
    findings = [finding for path, data in files for finding in findings_for(path, data)]
    print(json.dumps({"files_checked": len(files), "findings": findings}, indent=2))
    if findings:
        print("BLOCKED: review the paths above. Matching values were not printed.", file=sys.stderr)
        return 1
    print("PASS: no supported credential pattern or runtime credential filename found.")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        # Do not print subprocess stdout/stderr: it could contain credential material.
        print("Scan failed: " + type(exc).__name__ + ". Fix the input and rerun; this is not a pass.", file=sys.stderr)
        sys.exit(2)
