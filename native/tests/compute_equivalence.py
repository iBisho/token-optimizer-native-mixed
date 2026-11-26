"""Compare the native parsers (toidx compute) with the untouched Python ones.

    python native/tests/compute_equivalence.py [days]

For every transcript of the last N days and its subagent files: Go's
parse_session_turns and _parse_session_jsonl (whole file, and two time
windows) against the upstream Python code reading the raw file
(TOKEN_OPTIMIZER_NATIVE_INDEX=0). Equality is strict: same types (0 vs 0.0,
True vs 1) and the same dict key order. Exits 1 on any difference.
"""
import os
import sys
import time
from collections import Counter
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "skills" / "token-optimizer" / "scripts"))

import measure  # noqa: E402,F401  (prepares the facade)
from _measure import _compat, native_compute  # noqa: E402


def strict_diff(a, b, path="$"):
    """First difference between a and b (types and key order included), or None."""
    if type(a) is not type(b):
        return f"{path}: type {type(a).__name__} != {type(b).__name__} ({a!r:.80} / {b!r:.80})"
    if isinstance(a, dict):
        if list(a) != list(b):
            return f"{path}: keys {list(a)[:8]} != {list(b)[:8]}"
        for k in a:
            d = strict_diff(a[k], b[k], f"{path}.{k}")
            if d:
                return d
        return None
    if isinstance(a, list):
        if len(a) != len(b):
            return f"{path}: len {len(a)} != {len(b)}"
        for i, (x, y) in enumerate(zip(a, b)):
            d = strict_diff(x, y, f"{path}[{i}]")
            if d:
                return d
        return None
    if a != b:
        return f"{path}: {a!r:.80} != {b!r:.80}"
    return None


def python(fn):
    os.environ["TOKEN_OPTIMIZER_NATIVE_INDEX"] = "0"
    try:
        _compat.facade()._parse_session_jsonl_cache.clear()
        return fn()
    finally:
        os.environ["TOKEN_OPTIMIZER_NATIVE_INDEX"] = "1"


def native(fn):
    _compat.facade()._parse_session_jsonl_cache.clear()
    return fn()


def main():
    days = int(sys.argv[1]) if len(sys.argv) > 1 else 30
    f = _compat.facade()
    mains = [Path(p) for p, _m, _pr in f._find_all_jsonl_files(days)]
    files = mains + [c for p in mains for c in f._find_subagent_jsonl_files(p)]
    now = datetime.now(timezone.utc)
    windows = [(None, None), (now - timedelta(days=7), now),
               (datetime.now(timezone(timedelta(hours=-3))) - timedelta(days=3),
                datetime.now(timezone(timedelta(hours=-3))))]
    print(f"{len(files)} transcripts ({len(mains)} main) from the last {days} days")

    failures = []
    stats = Counter()
    t0 = time.perf_counter()
    native_compute.prepare_turns(files)
    for ws, we in windows:
        native_compute.prepare_session(files, ws, we)
    print(f"  native prepare (all jobs)   {time.perf_counter() - t0:7.2f}s")

    t_py = t_go = 0.0
    for path in files:
        checks = [("turns", lambda p=path: f.parse_session_turns(str(p)))]
        for ws, we in windows:
            checks.append((f"session{'' if ws is None else ' window'}",
                           lambda p=path, ws=ws, we=we: f._parse_session_jsonl(p, ws, we)))
        for label, fn in checks:
            for attempt in range(3):  # a live session may grow between the reads
                s = time.perf_counter(); want = python(fn); t_py += time.perf_counter() - s
                s = time.perf_counter(); got = native(fn); t_go += time.perf_counter() - s
                diff = strict_diff(want, got)
                if diff is None:
                    break
                native_compute.prepare_turns([path])
                for ws2, we2 in windows:
                    native_compute.prepare_session([path], ws2, we2)
            stats[label] += 1
            if diff:
                failures.append(f"{path.name} {label}: {diff}")
    print(f"  python {t_py:7.2f}s   native (load + topic) {t_go:7.2f}s")
    reasons = Counter(r for r in native_compute.FALLBACKS.values())
    print(f"  native results: {len(native_compute._RESULTS)}  fallbacks: {dict(reasons) or 0}")
    print(f"  checks: {dict(stats)}")
    for line in failures[:20]:
        print("  DIFF", line)
    print("OK" if not failures else f"FAILED: {len(failures)} differences")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
