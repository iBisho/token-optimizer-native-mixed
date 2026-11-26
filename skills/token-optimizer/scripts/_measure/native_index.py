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


_DOWNLOAD_ENV = "TOKEN_OPTIMIZER_NATIVE_DOWNLOAD"
_FOUND = {}  # memo: "binary" -> Path once found in this process
_FETCH_STARTED = set()


def _repo_root():
    return Path(__file__).resolve().parents[4]


def _asset():
    """Release asset name for this OS and CPU, e.g. toidx-linux-arm64."""
    if sys.platform == "win32":
        machine = os.environ.get("PROCESSOR_ARCHITEW6432") or os.environ.get("PROCESSOR_ARCHITECTURE", "")
        osname, ext = "windows", ".exe"
    else:
        machine = os.uname().machine  # platform.machine() can be slow; this is the same value
        osname = "darwin" if sys.platform == "darwin" else "linux" if sys.platform.startswith("linux") else None
        ext = ""
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(machine.lower())
    return f"toidx-{osname}-{arch}{ext}" if osname and arch else None


def _executable(path):
    return path.is_file() and (sys.platform == "win32" or os.access(path, os.X_OK))


def source_digest(root=None):
    """SHA-256 over the Go sources (native/toidx: *.go, go.mod, go.sum),
    line endings normalized. The release workflow records it in the lock
    file; a downloaded binary is used only when it matches the installed
    sources, so a binary built from other code never runs."""
    src = Path(root or _repo_root()) / "native" / "toidx"
    files = sorted([p for p in src.glob("*.go")] + [src / "go.mod", src / "go.sum"], key=lambda p: p.name)
    digest = hashlib.sha256()
    for path in files:
        digest.update(path.name.encode("utf-8") + b"\0")
        digest.update(path.read_bytes().replace(b"\r\n", b"\n") + b"\0")
    return digest.hexdigest()


def _locked():
    """(release tag, sha256, asset) of this platform's binary from
    native/toidx.lock.json, when the lock matches the installed sources."""
    import json
    try:
        lock = json.loads((_repo_root() / "native" / "toidx.lock.json").read_text(encoding="utf-8"))
        asset = _asset()
        if lock["source"] != source_digest():
            return None
        return lock["tag"], lock["assets"][asset], asset
    except (OSError, ValueError, KeyError, TypeError):
        return None


def _downloaded():
    """The pinned release binary from the plugin data folder; starts a
    background download (hash-checked) when it is not there yet."""
    pinned = _locked()
    if pinned is None:
        return None
    tag, sha256, asset = pinned
    try:
        dest = _cache_root() / "bin" / tag / asset
    except Exception:
        return None
    if _executable(dest):
        _install_hook_copy(dest, pinned_source=source_digest())
        return dest
    if os.environ.get(_DOWNLOAD_ENV, "1") == "0" or dest in _FETCH_STARTED:
        return None
    _FETCH_STARTED.add(dest)
    try:
        import time
        failed = Path(str(dest) + ".failed")
        if failed.is_file() and time.time() - failed.stat().st_mtime < 86400:
            return None  # retried once a day at most
        from spawn_utils import spawn_detached
        url = f"https://github.com/{_RELEASE_REPO}/releases/download/{tag}/{asset}"
        script = Path(__file__).resolve().parents[1] / "toidx_fetch.py"
        spawn_detached([sys.executable, str(script), url, str(dest), sha256],
                       stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except Exception:
        pass
    return None


_RELEASE_REPO = "iBisho/token-optimizer-native-mixed"


def _local_binary():
    return _repo_root() / "native" / "bin" / ("toidx.exe" if sys.platform == "win32" else "toidx")


def _install_hook_copy(dest, pinned_source):
    """Windows: copy the verified release binary to native/bin/toidx.exe, where
    hooks/python-launcher.sh looks for the hook front door, with a
    "<exe>.digest" marker naming the sources it was built from (both toidx
    and _usable_local() refuse a copy whose sources have changed since)."""
    if sys.platform != "win32":
        return
    target = _local_binary()
    marker = Path(str(target) + ".digest")
    try:
        if target.is_file() and marker.is_file() and marker.read_text(encoding="utf-8").strip() == pinned_source:
            return
        if target.is_file() and not marker.is_file():
            return  # a local build: never replace it
        import shutil
        target.parent.mkdir(parents=True, exist_ok=True)
        tmp = target.with_name(f"toidx.{os.getpid()}.tmp")
        shutil.copyfile(dest, tmp)
        os.replace(tmp, target)
        marker.write_text(pinned_source + "\n", encoding="utf-8")
    except OSError:
        pass


def _usable_local():
    """A local build (no marker), or a release copy whose marker still names
    the installed Go sources."""
    local = _local_binary()
    if not _executable(local):
        return None
    marker = Path(str(local) + ".digest")
    if not marker.exists():
        return local
    try:
        return local if marker.read_text(encoding="utf-8").strip() == source_digest() else None
    except OSError:
        return None


def binary():
    """The toidx binary to run, or None (the Python paths are used).

    In order: TOKEN_OPTIMIZER_TOIDX, a local build in native/bin
    (toidx.exe / toidx, git-ignored), then the release binary pinned by
    native/toidx.lock.json, downloaded once into the plugin data folder.
    """
    override = os.environ.get(_BINARY_ENV)
    if override:
        return Path(override) if Path(override).is_file() else None
    found = _FOUND.get("binary")
    if found is not None:
        return found
    found = _usable_local() or _downloaded()
    if found is not None:
        _FOUND["binary"] = found
    return found


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
