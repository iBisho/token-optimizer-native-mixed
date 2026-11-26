"""Fork-only: native parse_session_turns / _parse_session_jsonl ("toidx compute").

`prepare_*` sends a batch of transcripts to the Go binary, which runs exact
ports of both parsers over the native projections (see native/toidx/turns.go
and session.go) and returns every result in one call. The two parsers then
return a prepared result instead of reading the file, when the file is
unchanged since it was prepared.

What stays in Python, by design:
- which rate card a model string maps to: resolved here with the plugin's
  own pricing code (`_card`) and handed to Go as data, self-checked against
  `_get_model_cost` before use;
- the session topic: Go returns candidate user records and
  `_extract_topic(_task_user_text(record))` runs here (credential redaction);
- the local calendar day: Go reads a UTC-offset table built from this
  interpreter's own `time.localtime` (`_tz_table`).

Anything the Go ports do not replicate exactly makes Go report "fallback"
for that file, and the original Python code computes it. Everything here is
fail-open and returns MISS when unsure.
"""
import calendar
import hashlib
import json
import os
import subprocess
import time

from . import native_index

MISS = object()
_COMPUTE_ENV = "TOKEN_OPTIMIZER_NATIVE_COMPUTE"  # "0" disables only the native parsers
_RESULTS = {}    # job key -> (size, mtime_ns, result JSON bytes)
FALLBACKS = {}   # job key -> reason the binary gave (diagnostics)
_ATTEMPTED = {}  # job key -> (size, mtime_ns) sent to the binary
_STATE = {"ctx": None, "ctx_path": None, "models": None, "session_ok": True}
_MODEL_ROUNDS_MAX = 3  # binary runs per prepare while it reports unknown models


def _facade():
    from . import _compat
    return _compat.facade()


# ---- context -------------------------------------------------------------

def _card_cost(card, inp, out, cr, cc, per_request, cc1h=None, cc5m=None):
    """The formula toidx applies to a card (mirrors pricing._get_model_cost)."""
    kind = card["kind"]
    if kind == "none":
        return 0.0
    full = inp + cr + cc
    rates = card["rates"]
    if kind == "flat":
        if card["lc"] is not None and full > card["lc_threshold"]:
            rates = card["lc"]
        return (inp * rates["input"] / 1e6 + out * rates["output"] / 1e6
                + cr * rates["cache_read"] / 1e6 + cc * rates.get("cache_write", 0) / 1e6)
    if per_request and card["lc"] is not None and full > card["lc_threshold"]:
        rates = card["lc"]
    if cc1h is not None and cc5m is not None:
        unsplit = max(0, cc - cc1h - cc5m)
        ccw = cc1h * rates.get("cache_write_1h", rates["cache_write"]) / 1e6 + (cc5m + unsplit) * rates["cache_write"] / 1e6
    else:
        ccw = cc * rates["cache_write"] / 1e6
    return inp * rates["input"] / 1e6 + out * rates["output"] / 1e6 + cr * rates["cache_read"] / 1e6 + ccw


# (input, output, cache read, cache write, 1h write, 5m write); None = no split
_PROBES = ((1234, 567, 89012, 3456, None, None), (150000, 2000, 250000, 120000, None, None),
           (0, 0, 0, 0, None, None), (7, 0, 0, 0, None, None), (1234, 567, 89012, 3456, 1000, 2456),
           (150000, 2000, 250000, 120000, 100000, 0), (10, 3, 0, 50, 0, 70), (10, 3, 5, 900, 300, 200))


def _numeric_card(rates):
    return isinstance(rates, dict) and all(
        isinstance(k, str) and isinstance(v, (int, float)) and not isinstance(v, bool)
        and v == v and abs(v) != float("inf") for k, v in rates.items())


def _card(f, model, tier):
    """Rate card _get_model_cost would use for `model`, as plain data."""
    try:
        tier_data = f.PRICING_TIERS.get(tier, f.PRICING_TIERS["anthropic"])
        card = None
        for norm_fn, pricing, lc_pricing, lc_threshold in (
            (f._normalize_openai_model_name, f.OPENAI_MODEL_PRICING, f.OPENAI_LONG_CONTEXT_PRICING, f.OPENAI_LONG_CONTEXT_INPUT_THRESHOLD),
            (f._normalize_gemini_model_name, f.GEMINI_MODEL_PRICING, f.GEMINI_LONG_CONTEXT_PRICING, f.GEMINI_LONG_CONTEXT_INPUT_THRESHOLD),
        ):
            key = norm_fn(model)
            if key:
                card = {"kind": "flat", "rates": dict(pricing[key]),
                        "lc": dict(lc_pricing[key]) if key in lc_pricing else None,
                        "lc_threshold": lc_threshold}
                break
        if card is None:
            normalized = f._claude_price_key(model, tier_data["claude_models"]) if model else None
            if normalized and normalized in tier_data["claude_models"]:
                rates = tier_data["claude_models"][normalized]
                lc_models = tier_data.get("claude_models_lc") or {}
            else:
                rates = f.PRICING_TIERS["anthropic"]["claude_models"].get(normalized or "", None)
                lc_models = f.PRICING_TIERS["anthropic"].get("claude_models_lc") or {}
            if rates is None:
                card = {"kind": "none"}
            else:
                card = {"kind": "claude", "rates": dict(rates),
                        "lc": dict(lc_models[normalized]) if normalized in lc_models else None,
                        "lc_threshold": f.ANTHROPIC_LONG_CONTEXT_INPUT_THRESHOLD}
        if card["kind"] != "none":
            threshold = card["lc_threshold"]
            if (not _numeric_card(card["rates"]) or (card["lc"] is not None and not _numeric_card(card["lc"]))
                    or isinstance(threshold, bool) or not isinstance(threshold, (int, float))):
                return {"kind": "error", "priced": False}
        # Self-check against the real pricing function before trusting the card.
        for inp, out, cr, cc, cc1h, cc5m in _PROBES:
            for per_request in (True, False):
                want = f._get_model_cost(model, inp, out, cr, cc, tier=tier, cache_create_1h=cc1h,
                                         cache_create_5m=cc5m, per_request=per_request)
                if _card_cost(card, inp, out, cr, cc, per_request, cc1h, cc5m) != want:
                    return {"kind": "error", "priced": False}
        card["priced"] = bool(f._is_priced_model(model, tier))
        return card
    except Exception:
        return {"kind": "error", "priced": False}


def _tz_table():
    """[(first second, UTC offset)] as datetime.astimezone() sees it here."""
    def offset(t):
        return calendar.timegm(time.localtime(t)) - t
    start = calendar.timegm((2015, 1, 1, 0, 0, 0))
    end = int(time.time()) + 400 * 86400
    t, cur = start, offset(start)
    table = [[start, cur]]
    while t < end:
        nxt = t + 86400
        o = offset(nxt)
        if o != cur:
            lo, hi = t, nxt
            while hi - lo > 1:
                mid = (lo + hi) // 2
                if offset(mid) == cur:
                    lo = mid
                else:
                    hi = mid
            # A day can hold only one transition in practice; re-check after it.
            table.append([hi, o])
            cur = o
        t = nxt
    return table, end


def _skills(f):
    """Inputs of sessions._resolve_installed_skill, or None when unsupported."""
    try:
        comp = f._cached_measure_components()
        skills = comp.get("skills", {})
        names = list(set(skills.get("names", [])))
        name_to_dir = skills.get("name_to_dir", {}) or {}
    except Exception:
        return {"ok": False, "names": [], "name_to_dir": {}}
    if not all(isinstance(n, str) for n in names) or not isinstance(name_to_dir, dict) or not all(
            isinstance(k, str) and isinstance(v, str) for k, v in name_to_dir.items()):
        return None
    return {"ok": True, "names": sorted(names), "name_to_dir": name_to_dir}


def _models_file():
    return native_index._cache_root() / "compute-models.json"


def _known_models():
    if _STATE["models"] is None:
        try:
            names = json.loads(_models_file().read_text(encoding="utf-8"))
            _STATE["models"] = {n for n in names if isinstance(n, str)}
        except (OSError, ValueError):
            _STATE["models"] = set()
    return _STATE["models"]


def _write_ctx(f, extra_models=()):
    models = _known_models()
    if extra_models:
        models.update(extra_models)
        try:
            _models_file().parent.mkdir(parents=True, exist_ok=True)
            _models_file().write_text(json.dumps(sorted(models)), encoding="utf-8")
        except OSError:
            pass
    tier = f._load_pricing_tier()
    skills = _skills(f)
    if skills is None:
        _STATE["session_ok"] = False
        skills = {"ok": False, "names": [], "name_to_dir": {}}
    tz, tz_max = _tz_table()
    ctx = {"v": 1, "models": {m: _card(f, m, tier) for m in sorted(models)},
           "skills": skills, "tz": tz, "tz_max": tz_max}
    data = json.dumps(ctx, sort_keys=True).encode("utf-8")
    path = native_index._cache_root() / ("compute-ctx-" + hashlib.sha1(data).hexdigest()[:16] + ".json")
    path.parent.mkdir(parents=True, exist_ok=True)
    if not path.exists():
        tmp = path.with_suffix(".tmp")
        tmp.write_bytes(data)
        os.replace(tmp, path)
    _STATE["ctx"], _STATE["ctx_path"] = ctx, path
    return path


# ---- jobs ------------------------------------------------------------------

def _window_iso(value):
    if value is None:
        return ""
    if getattr(value, "tzinfo", None) is None:
        raise ValueError("naive window")  # Python itself would fail on it
    return value.isoformat()


_CODEX = {}  # path key -> _use_codex_session_adapter(path); it resolves the path each call


def _job(f, kind, path, window_start=None, window_end=None):
    """(job key, stat) for an eligible transcript, else None."""
    key = native_index._key(path)
    codex = _CODEX.get(key)
    if codex is None:
        codex = _CODEX[key] = bool(f._use_codex_session_adapter(path))
    if codex:
        return None
    try:
        st = os.stat(key)
    except OSError:
        return None
    from . import transcript_stream
    if st.st_size > transcript_stream.LARGE_BYTES:
        return None
    if kind == "turns":
        return ("turns", key, "", "", ""), st
    if not _STATE["session_ok"]:
        return None
    try:
        ws, we = _window_iso(window_start), _window_iso(window_end)
    except ValueError:
        return None
    reason = f._sidechain_path_reason(path) or ""
    return ("session", key, reason, ws, we), st


def _run(f, jobs):
    """Send jobs to the binary; store results. Returns models it asked for."""
    ctx_path = _STATE["ctx_path"] or _write_ctx(f)
    lines = []
    for jk in jobs:
        kind, key, reason, ws, we = jk
        lines.append("\t".join((kind, key, reason, ws, we)) if kind == "session" else "\t".join((kind, key, reason)))
    proc = subprocess.run(
        [str(native_index.binary()), "compute", "-cache", str(native_index._cache_root()), "-ctx", str(ctx_path)],
        input="\n".join(lines).encode("utf-8"), capture_output=True, timeout=900, check=False,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    if proc.returncode != 0:
        return set(), []
    missing, retry = set(), []
    for line in proc.stdout.split(b"\n"):
        if not line:
            continue
        head, _, body = line.partition(b"\t")
        try:
            h = json.loads(head)
            jk = jobs[h["i"]]
        except (ValueError, KeyError, IndexError, TypeError):
            continue
        if h.get("status") == "ok":
            _RESULTS[jk] = (h["size"], h["mtime_ns"], body)
        elif h.get("status") == "need_models":
            missing.update(m for m in h.get("models", []) if isinstance(m, str))
            retry.append(jk)
        else:
            FALLBACKS[jk] = h.get("reason")
    return missing, retry


def _prepare(kind, paths, window_start=None, window_end=None):
    try:
        if not native_index.enabled() or os.environ.get(_COMPUTE_ENV, "1") == "0":
            return None
        f = _facade()
        jobs = []
        for path in paths:
            found = _job(f, kind, path, window_start, window_end)
            if found is None:
                continue
            jk, st = found
            stamp = (st.st_size, st.st_mtime_ns)
            if _ATTEMPTED.get(jk) == stamp:
                continue
            _ATTEMPTED[jk] = stamp
            jobs.append(jk)
        jobs = list(dict.fromkeys(jobs))
        rounds = 0
        while jobs and rounds < _MODEL_ROUNDS_MAX:
            rounds += 1
            missing, jobs = _run(f, jobs)
            if missing:
                _write_ctx(f, missing)
        return len(_RESULTS)
    except Exception:
        return None


def prepare_turns(paths):
    return _prepare("turns", paths)


def prepare_session(paths, window_start=None, window_end=None):
    return _prepare("session", paths, window_start, window_end)


# ---- results -----------------------------------------------------------------

def _result(f, kind, path, window_start=None, window_end=None):
    if (os.environ.get("TOKEN_OPTIMIZER_NATIVE_INDEX", "1") == "0"
            or os.environ.get(_COMPUTE_ENV, "1") == "0" or not _RESULTS):
        return MISS
    try:
        found = _job(f, kind, path, window_start, window_end)
    except Exception:
        return MISS
    if found is None:
        return MISS
    jk, st = found
    entry = _RESULTS.get(jk)
    if entry is None or entry[:2] != (st.st_size, st.st_mtime_ns):
        return MISS
    return json.loads(entry[2])


def turns(filepath):
    """parse_session_turns(filepath) when prepared, else MISS."""
    try:
        return _result(_facade(), "turns", filepath)
    except Exception:
        return MISS


def session(filepath, window_start=None, window_end=None):
    """_parse_session_jsonl(filepath, ...) when prepared, else MISS."""
    try:
        f = _facade()
        parsed = _result(f, "session", filepath, window_start, window_end)
        if parsed is MISS or parsed is None:
            return parsed
        candidates = parsed.pop("__topic_candidates")
        more = parsed.pop("__topic_more")
        topic = None
        for record in candidates:
            topic = f._extract_topic(f._task_user_text(record))
            if topic is not None:
                break
        else:
            if more:
                return MISS  # the topic may sit past the candidates Go kept
        parsed["topic"] = topic
        return parsed
    except Exception:
        return MISS
