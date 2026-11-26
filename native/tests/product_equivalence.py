"""Check transcript_product.scan with the native extraction against upstream.

Runs scan over the window's transcripts with every native path disabled
(TOKEN_OPTIMIZER_NATIVE_INDEX=0, raw files) and with the native extraction
(native/toidx/product.go), for the whole set and for each session alone.

    python native/tests/product_equivalence.py [days]
"""
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "skills" / "token-optimizer" / "scripts"))

import measure  # noqa: E402,F401  (prepares the facade)
from _measure import _compat, native_compute, native_index, transcript_product  # noqa: E402

ENV = "TOKEN_OPTIMIZER_NATIVE_INDEX"


def scan(on, files, now, days):
    os.environ[ENV] = "1" if on else "0"
    return transcript_product.scan(files, budget=None, now=now, days=days)


def main():
    days = int(sys.argv[1]) if len(sys.argv) > 1 else 30
    facade = _compat.facade()
    assert native_index.binary() is not None, "build native/bin/toidx first"
    files = [Path(fp) for fp, _m, _p in facade._find_all_jsonl_files(days)]
    now = datetime.now(timezone.utc)
    print(f"{len(files)} main transcripts from the last {days} days")

    start = time.perf_counter()
    fast = scan(True, files, now, days)
    t_fast = time.perf_counter() - start
    start = time.perf_counter()
    raw = scan(False, files, now, days)
    t_raw = time.perf_counter() - start
    print(f"  whole set: python {t_raw:.2f}s, native {t_fast:.2f}s")
    failures = 0
    if raw != fast:
        # A live session may have grown between the two scans: retry once.
        if scan(False, files, now, days) != scan(True, files, now, days):
            failures += 1
            print("  whole set DIFFERENT\n    raw :", raw, "\n    fast:", fast)

    bad = []
    for f in files:
        for _attempt in range(3):
            if scan(False, [f], now, days) == scan(True, [f], now, days):
                break
        else:
            bad.append(str(f))
    failures += len(bad)
    print(f"  per session: {len(files) - len(bad)}/{len(files)} identical", *bad[:5], sep="\n    ")
    fallbacks = {}
    for jk, reason in native_compute.FALLBACKS.items():
        if jk[0] == "product":
            fallbacks[reason] = fallbacks.get(reason, 0) + 1
    print("  product fallbacks:", fallbacks)
    print("OK" if not failures else f"FAILED ({failures})")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
