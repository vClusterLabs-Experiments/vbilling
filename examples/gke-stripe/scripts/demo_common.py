"""Small, stdlib-only safety helpers for GPU example administration (not vBilling)."""
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile

class GuardError(Exception):
    pass

def require(ok, message):
    if not ok:
        raise GuardError(message)

def outside_git(path):
    path = Path(path).expanduser().resolve()
    for parent in (path, *path.parents):
        require(not (parent / '.git').exists(), 'State and credential files must be outside every Git checkout')
    return path

def private_read(path):
    path = outside_git(path)
    require(path.is_file(), 'Required private file is missing')
    mode = path.stat()
    require(mode.st_uid == os.getuid() and stat.S_IMODE(mode.st_mode) & 0o077 == 0,
            'Private file must be owned by you and have mode 0600 (or 0400)')
    return path.read_text().strip()

def save_json(path, value, exclusive=False):
    path = outside_git(path)
    require(path.parent.is_dir(), 'Create the private state directory first')
    require(not exclusive or not path.exists(), 'State already exists; preserve it and use the same run')
    fd, temp = tempfile.mkstemp(prefix='.' + path.name + '.', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as f:
            json.dump(value, f, indent=2)
            f.write('\n')
            f.flush()
            os.fsync(f.fileno())
        os.replace(temp, path)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)

def run_id(value):
    require(re.fullmatch(r'vbdemo-[0-9]{8}-[0-9]{6}', value) is not None,
            'Run ID must be vbdemo-YYYYMMDD-HHMMSS')
    return value

def provider_id(value, prefix):
    require(isinstance(value, str) and re.fullmatch(re.escape(prefix) + r'[A-Za-z0-9_]+', value),
            'Provider returned an invalid object ID')
    return value

def command(argv):
    result = subprocess.run(argv, capture_output=True, text=True)
    require(result.returncode == 0, 'Command failed; output suppressed to protect credentials: ' + argv[0])
    return result.stdout
