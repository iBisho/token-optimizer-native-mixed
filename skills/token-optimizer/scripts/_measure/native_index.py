"""Fork-only: native projection cache for the transcript readers of the dashboard.

The native ``toidx`` binary (source in ``native/toidx`` at the repo root)
reads a transcript once and writes, per reader "profile", a projected copy
that keeps only the records and fields that reader looks at. Projections are
keyed by path and named by the source's size and mtime, so a dashboard regen
only re-reads transcripts that changed since the last one. Lines the binary
is not certain about are copied verbatim, so every reader still makes its own
decisions and its output is identical to reading the raw file.

Profiles (see native/toidx/main.go):
  turns    sessions.parse_session_turns       (binary _raw_lines reader)
  product  transcript_product.scan            (text-mode reader)
  nudge    nudge_followthrough._rows          (text-mode reader)
  session  sessions._parse_session_jsonl      (binary _raw_lines reader)

Everything is fail-open: without the binary, with a stale projection or on
any error, readers use the raw transcript exactly as upstream does. Set
TOKEN_OPTIMIZER_NATIVE_INDEX=0 to disable, TOKEN_OPTIMIZER_TOIDX=<path> to
point at a binary elsewhere.
"""
import hashlib
import os
import subprocess
import sys
from pathlib import Path

FORMAT_VERSION = 2  # must match formatVersion in native/toidx/main.go
PROFILES = ("turns", "product", "nudge", "session")
_DISABLE_ENV = "TOKEN_OPTIMIZER_NATIVE_INDEX"
_BINARY_ENV = "TOKEN_OPTIMIZER_TOIDX"
_ATTEMPTED = set()  # (profile, key, size, mtime_ns) already sent to the binary


def enabled():
    return os.environ.get(_DISABLE_ENV, "1") != "0" and binary() is not None


def binary():
    override = os.environ.get(_BINARY_ENV)
    if override:
        return Path(override) if Path(override).is_file() else None
    root = Path(__file__).resolve().parents[4]
    candidate = root / "native" / "bin" / ("toidx.exe" if sys.platform == "win32" else "toidx")
    return candidate if candidate.is_file() else None


def _key(path):
    """Normalized path text; the binary hashes exactly this string."""
    return os.path.normcase(os.path.abspath(str(path)))


def _cache_root():
    from plugin_env import resolve_snapshot_dir
    return resolve_snapshot_dir() / "toidx"


def _projection(profile, key, st):
    digest = hashlib.sha1((profile + "\0" + key).encode("utf-8")).hexdigest()[:24]
    return _cache_root() / profile / f"v{FORMAT_VERSION}" / f"{digest}.{st.st_size}.{st.st_mtime_ns}.jsonl"


def _eligible(profile, key):
    try:
        st = os.stat(key)
    except OSError:
        return None
    if profile in ("turns", "session"):
        # Larger transcripts go through upstream's own SQLite index instead.
        from . import transcript_stream
        if st.st_size > transcript_stream.LARGE_BYTES:
            return None
    return st


def resolve(path, profile):
    """Path of a fresh projection of ``path`` for ``profile``, else None."""
    if os.environ.get(_DISABLE_ENV, "1") == "0":
        return None
    try:
        key = _key(path)
        st = _eligible(profile, key)
        if st is None:
            return None
        candidate = _projection(profile, key, st)
        return candidate if candidate.is_file() else None
    except (OSError, ValueError, UnicodeError):
        return None


def source(path, profile):
    """The file a reader should open: the projection when fresh, else ``path``."""
    found = resolve(path, profile)
    return found if found is not None else Path(path)


def raw_lines(path, profile):
    """Binary-reader profiles: open the projection, else upstream's reader."""
    from . import transcript_stream
    found = resolve(path, profile)
    if found is not None:
        try:
            return transcript_stream._raw_lines(open(found, "rb"))
        except OSError:
            pass
    return transcript_stream.open_transcript(path)


def warm_window(facade, days=30):
    """Project every transcript a dashboard regen reads, in one native call.

    Main transcripts of the window plus their subagent files, for the binary
    reader profiles. Called once at the start of a regen so the many later
    per-file parses all find a fresh projection.
    """
    try:
        if not enabled():
            return None
        files = [Path(fp) for fp, _m, _p in facade._find_all_jsonl_files(days)]
        files += [f for p in list(files) for f in (p.parent / p.stem / "subagents").glob("*.jsonl")]
        # The native parsers build any missing projection themselves.
        from . import native_compute
        native_compute.prepare_session(files)
        native_compute.prepare_turns(files)
        return len(files)
    except Exception:
        return None


def warm(paths, profile, timeout=None):
    """Build every missing projection in one native call (parallel inside).

    Returns how many files were sent to the binary, or None when the native
    path is unavailable. Never raises.
    """
    try:
        if not enabled():
            return None
        todo = []
        for path in paths:
            key = _key(path)
            st = _eligible(profile, key)
            if st is None:
                continue
            ident = (profile, key, st.st_size, st.st_mtime_ns)
            if ident in _ATTEMPTED or _projection(profile, key, st).is_file():
                continue
            _ATTEMPTED.add(ident)
            todo.append(f"{profile}\t{key}")
        if not todo:
            return 0
        payload = "\n".join(todo).encode("utf-8")
        subprocess.run([str(binary()), "-cache", str(_cache_root())], input=payload,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                       timeout=timeout or 300, check=False,
                       creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
        return len(todo)
    except (OSError, ValueError, UnicodeError, subprocess.SubprocessError):
        return None
