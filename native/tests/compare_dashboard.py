"""Generate the dashboard with native paths off, then on, and diff the data.

    python native/tests/compare_dashboard.py

Prints every differing JSON path. Some values legitimately move between two
runs minutes apart (timestamps, process ages, a live session's transcript);
read the paths, do not expect zero.
"""
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCRIPTS = ROOT / "skills" / "token-optimizer" / "scripts"
OUT = Path(os.environ.get("TEMP", ".")) / "toidx-compare"


def run(native):
    env = dict(os.environ, TOKEN_OPTIMIZER_NATIVE_INDEX="1" if native else "0")
    start = time.perf_counter()
    proc = subprocess.run([sys.executable, str(SCRIPTS / "measure.py"), "dashboard", "--user"],
                          env=env, capture_output=True, text=True, errors="replace")
    took = time.perf_counter() - start
    path = next((line.split("Dashboard:", 1)[1].strip() for line in proc.stdout.splitlines()
                 if "Dashboard:" in line), None)
    if not path:
        sys.exit("dashboard path not printed:\n" + proc.stdout + proc.stderr)
    OUT.mkdir(exist_ok=True)
    copy = OUT / ("native.html" if native else "raw.html")
    shutil.copyfile(path, copy)
    print(f"native={native}: {took:.1f}s")
    html = copy.read_text(encoding="utf-8")
    start = html.index("window.__TOKEN_DATA__ = ") + len("window.__TOKEN_DATA__ = ")
    data, _ = json.JSONDecoder().raw_decode(html.replace("<\\/", "</")[start:])
    return data


def diff(a, b, path="", out=None):
    out = [] if out is None else out
    if type(a) is not type(b):
        out.append(f"{path}: {str(a)[:80]!r} != {str(b)[:80]!r}")
    elif isinstance(a, dict):
        for k in sorted(set(a) | set(b), key=str):
            if k not in a or k not in b:
                out.append(f"{path}.{k}: only in {'native' if k in b else 'raw'}")
            else:
                diff(a[k], b[k], f"{path}.{k}", out)
    elif isinstance(a, list):
        if len(a) != len(b):
            out.append(f"{path}: len {len(a)} != {len(b)}")
        for i, (x, y) in enumerate(zip(a, b)):
            diff(x, y, f"{path}[{i}]", out)
    elif a != b:
        out.append(f"{path}: {str(a)[:80]!r} != {str(b)[:80]!r}")
    return out


if __name__ == "__main__":
    raw, fast = run(False), run(True)
    lines = diff(raw, fast)
    print(f"{len(lines)} differing paths")
    print("\n".join(lines[:200]))
