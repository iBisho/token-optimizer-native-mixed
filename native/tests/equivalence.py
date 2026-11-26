"""Compare every native-projected reader with the raw upstream reader.

Run from the repo root with the plugin's Python (as a file, never via stdin):
    python native/tests/equivalence.py [days]

Exits 1 on any mismatch. Reads the real ~/.claude transcripts; writes only the
projection cache (and nothing when TOKEN_OPTIMIZER_NATIVE_INDEX=0).
"""
import os
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "skills" / "token-optimizer" / "scripts"))

import measure  # noqa: E402,F401  (prepares the facade)
from _measure import _compat, native_index, nudge_followthrough, transcript_product  # noqa: E402

ENV = "TOKEN_OPTIMIZER_NATIVE_INDEX"


def mode(on):
    os.environ[ENV] = "1" if on else "0"


def timed(label, fn):
    start = time.perf_counter()
    value = fn()
    print(f"  {label:<28} {time.perf_counter() - start:7.2f}s")
    return value


def main():
    days = int(sys.argv[1]) if len(sys.argv) > 1 else 30
    facade = _compat.facade()
    assert native_index.binary() is not None, "build native/bin/toidx first"
    files = [Path(fp) for fp, _m, _p in facade._find_all_jsonl_files(days)]
    print(f"{len(files)} transcripts in the last {days} days")
    failures = 0

    # --- warm all profiles (cold cost shows on the first run only)
    mode(True)
    subagents = [f for p in files for f in sorted((p.parent / p.stem / "subagents").glob("*.jsonl"))]
    projects = sorted({p.parent for p in files})
    nudge_files = [f for d in projects for f in d.glob("*.jsonl") if not f.is_symlink()]
    timed("warm turns", lambda: native_index.warm(files, "turns"))
    timed("warm product", lambda: native_index.warm(files + subagents, "product"))
    timed("warm nudge", lambda: native_index.warm(nudge_files, "nudge"))

    # --- turns
    def all_turns():
        return [facade.parse_session_turns(str(f)) for f in files]
    mode(False); timed("turns raw", all_turns)
    mode(True); timed("turns native", all_turns)
    bad = []
    for f in files:
        for _attempt in range(3):  # a live session may grow between the two reads
            native_index.warm([f], "turns")
            mode(False); a = facade.parse_session_turns(str(f))
            mode(True); b = facade.parse_session_turns(str(f))
            if a == b:
                break
        else:
            bad.append(str(f))
    failures += len(bad)
    print(f"  turns: {len(files) - len(bad)}/{len(files)} identical", *bad[:5], sep="\n    ")

    # --- session (_parse_session_jsonl), whole file and a 7-day window slice
    native_index.warm(files + subagents, "session")
    win_end = datetime.now(timezone.utc)
    win_start = win_end - timedelta(days=7)

    def parse(f, window):
        facade._parse_session_jsonl_cache.clear()  # in-process memo, keyed without mode
        return (facade._parse_session_jsonl(str(f), win_start, win_end) if window
                else facade._parse_session_jsonl(str(f)))

    sbad = []
    t_raw = t_fast = 0.0
    for f in files + subagents:
        for _attempt in range(3):
            native_index.warm([f], "session")
            mode(False); s = time.perf_counter(); a = (parse(f, False), parse(f, True)); t_raw += time.perf_counter() - s
            mode(True); s = time.perf_counter(); b = (parse(f, False), parse(f, True)); t_fast += time.perf_counter() - s
            if a == b:
                break
        else:
            sbad.append(str(f))
    failures += len(sbad)
    print(f"  session raw {t_raw:.2f}s, native {t_fast:.2f}s (incl. retries)")
    print(f"  session: {len(files) + len(subagents) - len(sbad)}/{len(files) + len(subagents)} identical", *sbad[:5], sep="\n    ")

    # --- product
    now = datetime.now(timezone.utc)
    mode(False); raw = timed("product raw", lambda: transcript_product.scan(files, budget=None, now=now, days=days))
    mode(True); fast = timed("product native", lambda: transcript_product.scan(files, budget=None, now=now, days=days))
    same = raw == fast
    failures += not same
    print(f"  product: {'identical' if same else 'DIFFERENT'}")
    if not same:
        print("    raw :", raw, "\n    fast:", fast)

    # --- nudge: compare what resolve() derives from the rows
    def summary(path, stamps):
        rows = list(nudge_followthrough._rows(path))
        ts = [nudge_followthrough._epoch(r.get("timestamp")) for r in rows]
        ts = [t for t in ts if t is not None]
        out = [len(rows), min(ts, default=None), max(ts, default=None),
               rows[0].get("timestamp") if rows else None,
               [(nudge_followthrough._context(m), m.get("model"), m.get("id"))
                for m in nudge_followthrough._requests(rows)]]
        for stamp in stamps:
            status, action, reqs = nudge_followthrough.classify(rows, stamp)
            out.append((status, action, [(nudge_followthrough._context(m), m.get("model")) for m in reqs]))
        return out

    nbad = []
    t_raw = t_fast = 0.0
    for f in nudge_files:
        mode(False); s = time.perf_counter(); a = summary(f, [])
        t_raw += time.perf_counter() - s
        # probe classify at a few points inside the session
        lo, hi = a[1], a[2]
        stamps = [] if lo is None else [datetime.fromtimestamp(lo + (hi - lo) * q, timezone.utc).isoformat()
                                        for q in (0, .25, .5, .9)]
        mode(False); a = summary(f, stamps)
        mode(True); s = time.perf_counter(); b = summary(f, stamps)
        t_fast += time.perf_counter() - s
        if a != b:
            nbad.append(str(f))
    failures += len(nbad)
    print(f"  nudge rows raw {t_raw:.2f}s, native {t_fast:.2f}s")
    print(f"  nudge: {len(nudge_files) - len(nbad)}/{len(nudge_files)} identical", *nbad[:5], sep="\n    ")

    print("OK" if not failures else f"FAILED: {failures} mismatches")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
