# Native fast paths (fork of alexgreensh/token-optimizer)

This fork keeps upstream's Python code and adds one Go binary, `toidx`
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

### Fallbacks and switches

Everything is fail-open: no binary, a stale projection, a file that changed
since it was prepared or any error means the upstream code path runs
unchanged.

- `TOKEN_OPTIMIZER_NATIVE_INDEX=0` disables every native path.
- `TOKEN_OPTIMIZER_NATIVE_COMPUTE=0` disables only the native parsers.
- `TOKEN_OPTIMIZER_TOIDX=<path>` points at a binary elsewhere.

## Build

Requires Go 1.23+ (range-over-func); `go build` fetches fastjson, pinned in
`go.sum`. From the repo root:

```powershell
cd native/toidx; go build -trimpath -ldflags="-s -w" -o ../bin/toidx.exe .
```

On macOS/Linux build `../bin/toidx` instead. The Python side looks for
`native/bin/toidx(.exe)` relative to the plugin root. `native/bin/` is
committed for Windows so the installed plugin works without a Go toolchain;
rebuild it after changing the Go code.

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

## Fork changes (keep this list short; it is the merge surface)

- `native/` (new): Go source, binary, tests, this README.
- `_measure/native_index.py` (new): projection cache client.
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

Upstream mirrors `skills/` into `plugins/token-optimizer/` and
`cowork/token-optimizer/`; this fork only edits `skills/` (the copy the
installed plugin runs).

## Syncing with upstream

```powershell
git fetch upstream
git merge upstream/main
python native/tests/equivalence.py 30
python native/tests/compute_equivalence.py 30
python native/tests/product_equivalence.py 30
```

Conflicts can only happen in the files listed above. When upstream changes a
ported function, port the change to the matching Go file; when it changes
what a reader reads, update its projection profile in `main.go`, bump
`formatVersion` there and `FORMAT_VERSION` in `native_index.py`, rebuild and
rerun the tests. Check `measure.py` for re-bound defaults
(`_measure_compat.bind(..., defaults=...)`) of any function you port.
