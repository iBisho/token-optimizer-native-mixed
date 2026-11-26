"""Check `toidx hook` against hooks/run.py (Windows; run by CI).

Builds a throwaway plugin root (the real hooks/, skills/ scripts and
manifests, plus a probe runner that records its argv and environment), then
runs each scenario twice, through `python hooks/run.py` and through
`toidx hook`, and compares what the probe saw (or that it did not run).

    python native/tests/hook_equivalence.py path/to/toidx.exe
"""
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
PROBE = '''import json, os, sys
out = os.environ["PROBE_OUT"]
keys = ("PYTHONUTF8", "PYTHONIOENCODING", "TOKEN_OPTIMIZER_RUNTIME", "CLAUDE_PLUGIN_ROOT",
        "PYTHONPYCACHEPREFIX", "TOKEN_OPTIMIZER_HOOK_PARENT_PID")
with open(out, "w", encoding="utf-8") as f:
    json.dump({"argv": sys.argv[1:], "stdin": sys.stdin.read(),
               "env": {k: os.environ.get(k) for k in keys}}, f)
print("probe stdout")
'''


def build_root(base):
    root = base / "plugin"
    shutil.copytree(REPO / "hooks", root / "hooks")
    shutil.copytree(REPO / ".claude-plugin", root / ".claude-plugin")
    shutil.copytree(REPO / "skills" / "token-optimizer" / "scripts", root / "skills" / "token-optimizer" / "scripts",
                    ignore=shutil.ignore_patterns("__pycache__"))
    (root / "hooks" / "probe_runner.py").write_text(PROBE, encoding="utf-8")
    return root


def record(home, env):
    key = hashlib.sha256(("v1\0" + env.get("PATH", "") + "\0" + env.get("TOKEN_OPTIMIZER_PYTHON", "")).encode()).hexdigest()[:24]
    path = home / ".cache" / "token-optimizer" / "native-hook" / f"python-{key}.txt"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(("v1\n" + sys.executable + "\n").encode())


def run(cmd, env, out):
    if out.exists():
        out.unlink()
    proc = subprocess.run(cmd, input="payload", capture_output=True, text=True, env=env, timeout=120)
    seen = json.loads(out.read_text(encoding="utf-8")) if out.exists() else None
    if seen and seen["env"].get("TOKEN_OPTIMIZER_HOOK_PARENT_PID"):
        seen["env"]["TOKEN_OPTIMIZER_HOOK_PARENT_PID"] = "<pid>"  # differs by construction
    return proc.returncode, proc.stdout, seen


def main():
    toidx = Path(sys.argv[1]).resolve()
    base = Path(tempfile.mkdtemp())
    root = build_root(base)
    home = base / "home"
    (home / ".claude" / "token-optimizer").mkdir(parents=True)
    cfg = home / ".claude" / "token-optimizer" / "config.json"
    settings = home / ".claude" / "settings.json"
    out = base / "probe.json"
    clean = {k: v for k, v in os.environ.items()
             if not k.startswith(("TOKEN_OPTIMIZER", "CLAUDE", "PYTHON", "CODEX"))}
    clean.update(USERPROFILE=str(home), HOME=str(home), CLAUDE_PLUGIN_ROOT=str(root), PROBE_OUT=str(out),
                 TOKEN_OPTIMIZER_NO_PROC_SCAN="1")
    record(home, clean)

    scenarios = [
        ("no config", lambda: None, ["hooks/probe_runner.py", "a", "b"]),
        ("consent given", lambda: cfg.write_text('{"enterprise_consent_shown": true}'), ["hooks/probe_runner.py"]),
        ("opted out", lambda: cfg.write_text('{"enterprise_consent_shown": false, "v5_welcome_shown": false}'),
         ["hooks/probe_runner.py"]),
        ("opted out, exempt arg", None, ["hooks/probe_runner.py", "consent"]),
        ("backfill", lambda: cfg.write_text('{"v5_welcome_shown": true}'), ["hooks/probe_runner.py"]),
        ("disabled", lambda: settings.write_text(
            '{"enabledPlugins": {"token-optimizer@token-optimizer-native-mixed": false}}'), ["hooks/probe_runner.py"]),
        ("enabled again", lambda: settings.write_text('{"enabledPlugins": {}}'), ["hooks/probe_runner.py"]),
        ("session-end-flush", None, ["hooks/probe_runner.py", "session-end-flush"]),
        ("missing target", None, ["hooks/nope.py"]),
        ("traversal", None, ["../x.py"]),
        ("git work tree", lambda: (base / ".git").mkdir(), ["hooks/probe_runner.py"]),
    ]
    failures = 0
    for name, setup, args in scenarios:
        if setup:
            setup()
        if name == "backfill":
            # run.py rewrites the config; restore it between the two runs.
            a = run([sys.executable, str(root / "hooks" / "run.py"), *args], clean, out)
            cfg.write_text('{"v5_welcome_shown": true}')
        else:
            a = run([sys.executable, str(root / "hooks" / "run.py"), *args], clean, out)
        b = run([str(toidx), "hook", str(root / "hooks" / "run.py"), *args], clean, out)
        same = a == b
        failures += not same
        print(f"{'ok  ' if same else 'DIFF'} {name}: ran={a[2] is not None}")
        if not same:
            print("   run.py:", a)
            print("   toidx :", b)
    # stale CLAUDE_PLUGIN_ROOT
    stale = dict(clean, CLAUDE_PLUGIN_ROOT=str(root / "gone"))
    a = run([sys.executable, str(root / "hooks" / "run.py"), "hooks/probe_runner.py"], stale, out)
    b = run([str(toidx), "hook", str(root / "hooks" / "run.py"), "hooks/probe_runner.py"], stale, out)
    print(f"{'ok  ' if a == b else 'DIFF'} stale plugin root")
    failures += a != b
    # no interpreter record -> 75, nothing run
    norec = dict(clean, PATH=clean.get("PATH", "") + ";C:\\no-record")
    code, _, seen = run([str(toidx), "hook", str(root / "hooks" / "run.py"), "hooks/probe_runner.py"], norec, out)
    print(f"{'ok  ' if code == 75 and seen is None else 'DIFF'} no record -> 75")
    failures += not (code == 75 and seen is None)
    print("OK" if not failures else f"FAILED ({failures})")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
