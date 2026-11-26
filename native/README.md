# Native fast paths

This edition keeps the original's Python code (alexgreensh/token-optimizer,
"upstream" below) and adds one Go binary, `toidx`
(one dependency: `github.com/valyala/fastjson`), for the three things that made
`measure.py dashboard --user` take 5+ minutes on Windows:

1. **Re-reading gigabytes of transcripts on every regen.** Several readers
   open every raw `.jsonl` in the window, often more than once per regen,
   although each looks at a handful of fields of a few record kinds.
2. **Re-parsing every session in Python on every regen**:
   `_parse_session_jsonl` (collector, trends, savings, the weekly runway
   window over all transcripts) and `parse_session_turns` (per-turn cost).
3. **Starting PowerShell three times** for the process table, which costs
   1-20 s per start on some machines.

## How it works

### 1. Transcript projections (`toidx -cache DIR`)

For each reader there is a *profile*. `toidx` reads a transcript once and
writes a projected copy that keeps exactly the lines and fields that reader
acts on. 3 GB of transcripts become about 100 MB per profile.

Projections live in `<plugin data>/toidx/<profile>/v2/` and are named
`<sha1(profile, path)>.<size>.<mtime_ns>.jsonl`, so a projection is valid
exactly while the source file is unchanged; only changed transcripts are
re-projected, in one parallel native call.

The Python readers run unchanged over the projection, through the same line
reader. Output is identical by construction:

- A line is dropped only when the Python reader would certainly skip it.
- A line is reduced only to fields the reader reads. Values are copied as
  raw JSON bytes, never re-escaped, so a projected line is never longer than
  its source (the readers have per-line length caps).
- Anything Go is not sure about (invalid UTF-8, a stray `\r` that text mode
  would split on, JSON Go rejects but Python accepts such as `NaN`) is copied
  verbatim, so Python decides.
- `turns` and `session` skip files over 96 MB: upstream reads those through
  its own incremental SQLite index (`transcript_stream.records`).

| Profile   | Python reader                    | Reader semantics mirrored |
|-----------|----------------------------------|---------------------------|
| `turns`   | `sessions.parse_session_turns`   | binary `_raw_lines` (16 MB line cap, 8M char cap) |
| `session` | `sessions._parse_session_jsonl`  | binary `_raw_lines`, every dict record kept |
| `product` | `transcript_product.scan`        | text mode, tag pre-filter |
| `nudge`   | `nudge_followthrough._rows`      | text mode, every dict row kept (order matters) |

### 2. Native parsers (`toidx compute`)

`native/toidx/turns.go` and `session.go` are line-for-line ports of
`parse_session_turns` and `_parse_session_jsonl` (with
`_aggregate_request_usage`, `_daily_usage_from_merged`,
`_compute_call_gap_stats`, `_billed_usage_parts`, `_price_usage_parts`,
`_get_model_cost`, streaming dedup, advisor parts, slash-command skills,
OSRC detection and time windows). Python calls the binary once per batch
(`native_compute.prepare_*`) and the two parsers then return the prepared
result for any file that has not changed since.

What stays in Python on purpose, handed to Go as data in a context file:

- **Which rate card a model string maps to.** Python resolves each model
  with the plugin's own pricing code and checks the card against
  `_get_model_cost` on probe vectors before Go may use it. Go only applies the
  formula (same operation order, every product rounded, no fused ops).
- **Installed skills** for `_resolve_installed_skill`.
- **Local time.** A table of UTC offsets built with this interpreter's own
  `time.localtime`, so "local day" matches `datetime.astimezone()` exactly,
  DST changes included.
- **The session topic.** Go returns the first candidate user records and
  Python runs `_extract_topic(_task_user_text(record))` (credential
  redaction stays in one place).

Python details the ports reproduce: `measure.py` re-binds some defaults
(`_price_usage_parts(ttl_split=True)`), `sum()` of floats uses Neumaier
compensation (3.12+), `round(x, 6)` rounds half to even on the exact binary
value, `max(0, x)` returns the int `0`, `astimezone()` rounds the timestamp
to the nearest second, floats are written with `float.__repr__` so they stay
floats, and dict key order is preserved.

JSON is read with `valyala/fastjson` (one reusable parser per worker, so
lines are not turned into maps and garbage): with `encoding/json` maps the
parsers spent 70% of their time decoding and collecting garbage (7 s for
30 days of `turns`; 2.3 s with fastjson). Each line is first checked with
the strict `encoding/json` validator, and duplicate keys resolve to the last
one, as `json.loads` does (fastjson's own `Get` returns the first).

`transcript_product.scan` (skill-listing budget and unused connectors) gets
the same treatment in `product.go`: Go does the per-line work (tag filter,
parse, type checks) and returns, per record that matters, only the values
`scan` reads (timestamp, listing length, delta names and lengths, MCP tool
names and call ids); `scan` replays them through its own logic. 24 s of
parsing for 30 days becomes about 1 s.

Anything outside the shapes Go reproduces exactly (timestamps not written as
`YYYY-MM-DDTHH:MM:SS[.fff](Z|±HH:MM)`, numbers stored as strings, lone
surrogate escapes, integers beyond 2^53, non-string ids, a rate card that
fails its self-check) makes Go report `fallback` for that file and the
original Python code computes it.

### 3. Transcripts over 96 MB (Python only)

Upstream reads these through an incremental SQLite index, and every read
re-encodes each indexed record as a JSON line; a regen reads the same large
transcript many times (one 101 MB session cost about 25 s per regen).
`transcript_stream.open_transcript` now keeps those lines in memory once the
index covers the whole file, for as long as the file's size and mtime stay
the same. The lines are the ones upstream would produce.

### 4. Process table (`toidx procs`)

`toidx procs get-process claude | cim claude | names` prints the same CSV
columns (and row order) the PowerShell pipelines printed, read with
Toolhelp32, `QueryFullProcessImageName`, `NtQueryInformationProcess` and
`GetProcessTimes`. 0.15 s instead of 2-20 s. Paths may differ in letter case
from WMI; the callers only `stat` them, which is case-insensitive on Windows.

### 5. Hook front door (`toidx hook`, Windows)

Every hook event used to start a bash launcher that probes for a Python
(on Windows a `pythonw -c ""` liveness probe on every call, plus a
`TOKEN_OPTIMIZER_PYTHON` probe when set), then `run.py` in one Python, which
starts the handler in a second Python. About 0.7 s before any hook work.

Now `hooks/python-launcher.sh` first runs `native/bin/toidx.exe hook <run.py>
<target> [args]` when that file exists:

- The interpreter is the one `run.py` recorded for its own `sys.executable`
  the last time the launcher ran it (`~/.cache/token-optimizer/native-hook/`,
  keyed by `PATH` and `TOKEN_OPTIMIZER_PYTHON`). No record, or a record that
  no longer fits: toidx exits 75 before reading stdin and the launcher
  carries on exactly as before.
- `hook.go` ports `run.py`'s decisions: host disable switch in
  `settings.json`, consent (with the plugin-data identity check), target path
  rules, the prompt-hook opt-out, a stale `CLAUDE_PLUGIN_ROOT`, the bytecode
  prefix, the child environment and the Claude runtime export. It then starts
  `module_runner.py` directly, detached from any console, with the same
  120 s ceiling: one Python per event instead of two.
- Anything it cannot decide exactly like `run.py` (another runtime, an
  unusual `CLAUDE_CONFIG_DIR`, a config `json` would parse differently, the
  consent backfill that rewrites `config.json`) runs `python run.py` instead.
- `toidx.exe` is built for the Windows GUI subsystem (`-H windowsgui`), like
  `pythonw.exe`, so it never opens a console window.
- The release binary reaches `native/bin/` as a copy made by `native_index`
  with a `toidx.exe.digest` marker; toidx and `native_index` ignore the copy
  once the plugin's Go sources differ from the ones it was built from.

`native/tests/hook_equivalence.py` (run by CI on Windows) runs the same
scenarios through `run.py` and `toidx hook` and compares what the handler
received. POSIX keeps the launcher: there `run.py`'s runtime detection scans
the process tree for OpenCode, which is not ported.

### Fallbacks and switches

Everything is fail-open: no binary, a stale projection, a file that changed
since it was prepared or any error means the upstream code path runs
unchanged.

- `TOKEN_OPTIMIZER_NATIVE_INDEX=0` disables every native path.
- `TOKEN_OPTIMIZER_NATIVE_COMPUTE=0` disables only the native parsers.
- `TOKEN_OPTIMIZER_TOIDX=<path>` points at a binary elsewhere.
- `TOKEN_OPTIMIZER_NATIVE_DOWNLOAD=0` never downloads the release binary.
- `TOKEN_OPTIMIZER_NATIVE_HOOK=0` keeps the hook launcher on its own path.

## Distribution

No binary is committed. `native_index.binary()` uses, in order:

1. `TOKEN_OPTIMIZER_TOIDX`, if set.
2. A local build, `native/bin/toidx.exe` or `native/bin/toidx` (git-ignored).
3. The release binary pinned by `native/toidx.lock.json`, from
   `<plugin data>/toidx/bin/<tag>/<asset>`.

When 3 is missing, the plugin starts `scripts/toidx_fetch.py` detached and
keeps running its Python paths. The fetcher downloads
`https://github.com/iBisho/token-optimizer-native-mixed/releases/download/<tag>/<asset>`
and installs it only if its SHA-256 equals the one in the lock file; after a
failure it waits a day before trying again.

The lock also holds a digest of the Go sources (`native_index.source_digest()`:
`native/toidx/*.go`, `go.mod`, `go.sum`, line endings normalized). A
downloaded binary is used only while that digest matches the sources the
plugin ships, so a binary built from other code never runs, even in the
minutes between a Go change and its release.

Releases are automatic: `.github/workflows/native-release.yml` runs when
`native/toidx/**` changes on main. It names the release `toidx-<digest[:12]>`,
cross-compiles six binaries (Windows, Linux and macOS; amd64 and arm64),
attaches them with `SHA256SUMS`, and commits the new lock file to main.

## Build (local)

Requires Go 1.24+ (see `go.mod`); `go build` fetches fastjson, pinned in
`go.sum`. From the repo root:

```powershell
cd native/toidx; go build -trimpath -ldflags="-s -w -H windowsgui" -o ../bin/toidx.exe .
```

`go test ./...` runs the hook front door's unit tests.

On macOS/Linux build `../bin/toidx`. A local build always wins over the
release binary.

## Verify

```powershell
python native/tests/equivalence.py 30
python native/tests/compute_equivalence.py 30
python native/tests/product_equivalence.py 30
python native/tests/compare_dashboard.py
```

- `equivalence.py`: every projected reader against the raw reader.
- `compute_equivalence.py`: the native parsers against the untouched Python
  parsers reading raw files, for whole files and two time windows, with
  strict equality (types such as `0` vs `0.0`, and dict key order).
- `product_equivalence.py`: `transcript_product.scan` native against
  upstream, for the whole window and each session alone.
- `compare_dashboard.py`: the whole dashboard data, native off vs on (values
  that move between two runs, such as timestamps and live sessions, differ).

Transcripts of sessions that are running while a test runs can differ
between the two reads; the tests retry those.

## Changes from upstream

Native layer:

- `native/` (new): Go source, binary, tests, this README.
- `_measure/native_index.py` (new): projection cache client, binary lookup
  and download trigger.
- `scripts/toidx_fetch.py` (new): hash-checked release download.
- `hooks/python-launcher.sh`: tries `toidx hook` first (Windows).
- `hooks/run.py`: records its interpreter for `toidx hook` (Windows).
- `PRIVACY.md`: the one network request (the binary download).
- `_measure/native_compute.py` (new): native parser client (context, jobs, results).
- `_measure/sessions.py`: both parsers return prepared native results; their
  Python paths read the `turns` / `session` projections.
- `_measure/dashboard.py`: one batch prepare at the start of a regen.
- `_measure/cache_policy.py`: `_get_cache_ttl_waste` prepares `turns`.
- `_measure/weekly_activity.py`: `price` prepares its windowed pass.
- `_measure/transcript_product.py`: `scan` replays native events, else reads
  the `product` projection.
- `_measure/transcript_stream.py`: `open_transcript` keeps the lines of a
  fully indexed large transcript.
- `_measure/nudge_followthrough.py`: `_project_index` warms, `_rows` reads `nudge`.
- `_measure/daemon.py`: `_native_procs_csv` replaces the three PowerShell calls.

Packaging (Claude Code only):

- Removed: the Codex, Cowork, OpenCode, OpenClaw, Pi, VS Code, Copilot,
  Cursor, Devin, Hermes and Antigravity adapters and mirrors, the docs site,
  npm packaging, release signing and `install.sh`. The Python code under
  `skills/` that serves other runtimes is untouched (it is inert here).
- `.claude-plugin/`: own marketplace `token-optimizer-native-mixed`.
- `README.md`: this edition's header, install and credits.
- `.github/workflows/`: `tests.yml` (pytest on Linux and Windows),
  `native.yml` (gofmt, vet and build of `toidx` on the three platforms),
  `native-release.yml` (releases the binaries and pins them),
  `refresh-prices.yml` (daily price refresh as a review PR),
  `file-size.yml` (upstream's line limit).
- `scripts/refresh_prices.py`: writes only `prices.json`.
- `scripts/check_measure_split_payload.py`: lists the two new `_measure`
  modules.

## Taking changes from upstream

Upstream is not followed automatically. To bring in a change:

```powershell
git fetch upstream
git merge upstream/main
git rm -rq --ignore-unmatch plugins cowork opencode openclaw pi vscode-extension docs-site packaging
git commit
python native/tests/equivalence.py 30
python native/tests/compute_equivalence.py 30
python native/tests/product_equivalence.py 30
```

Upstream changes to removed folders come back as modify/delete conflicts;
resolve them by removing the files again. When upstream changes a
ported function, port the change to the matching Go file; when it changes
what a reader reads, update its projection profile in `main.go`, bump
`formatVersion` there and `FORMAT_VERSION` in `native_index.py`, rebuild and
rerun the tests. Check `measure.py` for re-bound defaults
(`_measure_compat.bind(..., defaults=...)`) of any function you port.
