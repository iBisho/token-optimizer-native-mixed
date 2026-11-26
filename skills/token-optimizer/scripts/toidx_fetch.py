"""Download one toidx release binary and install it only if its hash matches.

    python toidx_fetch.py URL DEST SHA256

Started detached by _measure/native_index.py when the binary pinned in
native/toidx.lock.json is not in the plugin's data folder yet. The expected
SHA-256 comes from that lock file (committed in the repo), never from the
download itself. One download at a time per DEST (an exclusive lock file);
after a failure, a marker keeps further attempts away for a day. Exits 0
whatever happens: the plugin keeps using its Python paths until DEST exists.
"""
import hashlib
import os
import sys
import time
import urllib.request

MAX_BYTES = 64 * 1024 * 1024
LOCK_STALE_SECONDS = 600


def _fetch(url, dest, sha256):
    tmp = dest + ".part"
    digest = hashlib.sha256()
    size = 0
    request = urllib.request.Request(url, headers={"User-Agent": "token-optimizer-native"})
    with urllib.request.urlopen(request, timeout=60) as response, open(tmp, "wb") as out:
        while True:
            chunk = response.read(1 << 16)
            if not chunk:
                break
            size += len(chunk)
            if size > MAX_BYTES:
                raise ValueError("download too large")
            digest.update(chunk)
            out.write(chunk)
    if digest.hexdigest() != sha256.lower():
        os.remove(tmp)
        raise ValueError("hash mismatch")
    if os.name != "nt":
        os.chmod(tmp, 0o755)
    os.replace(tmp, dest)


def main(argv):
    if len(argv) != 4:
        return 0
    url, dest, sha256 = argv[1:]
    if not url.startswith("https://github.com/") or os.path.exists(dest):
        return 0
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    lock, failed = dest + ".lock", dest + ".failed"
    try:
        if time.time() - os.path.getmtime(lock) > LOCK_STALE_SECONDS:
            os.remove(lock)
    except OSError:
        pass
    try:
        fd = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
        os.close(fd)
    except OSError:
        return 0  # another download is running
    try:
        _fetch(url, dest, sha256)
        try:
            os.remove(failed)
        except OSError:
            pass
    except Exception:
        try:
            with open(failed, "w") as marker:
                marker.write(str(int(time.time())))
        except OSError:
            pass
        try:
            os.remove(dest + ".part")
        except OSError:
            pass
    finally:
        try:
            os.remove(lock)
        except OSError:
            pass
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
