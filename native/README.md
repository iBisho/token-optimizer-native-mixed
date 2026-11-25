# Native fast paths (fork of alexgreensh/token-optimizer)

This fork keeps upstream's Python code and adds one small Go binary,
`toidx`, that removes the two things that made `measure.py dashboard --user`
take 5+ minutes on Windows:

1. **Re-reading gigabytes of transcripts on every regen.** Several readers
   (`parse_session_turns`, `transcript_product.scan`,
   `nudge_followthrough._rows`) open every raw `.jsonl` in the window, often
   more than once per regen, although each looks at a handful of fields of a
   few record kinds.
2. **Starting PowerShell three times** for the process table
   (`Get-Process`, two `Get-CimInstance Win32_Process` calls), which costs
   1-20 s per start on some machines.

## How it works

### Transcript projections (`toidx -cache DIR`)

For each reader there is a *profile*. `toidx` reads a transcript once and
writes a projected copy that keeps exactly the lines and fields that reader
acts on, for example only assistant records with usage, reduced to
`type/timestamp/requestId/message.{id,model,usage,content[tool_use].name}`
for `turns`. 3 GB of transcripts become about 100 MB.

Projections live in `<plugin data>/toidx/<profile>/v1/` and are named
`<sha1(profile, path)>.<size>.<mtime_ns>.jsonl`, so a projection is valid
exactly while the source file is unchanged. A regen only re-projects
transcripts that changed since the previous one, in one parallel native call
(`native_index.warm`).

The Python readers are unchanged: they read the projection instead of the
raw file, through the same line reader. Output is identical by construction:

- A line is dropped only when the Python reader would certainly skip it
  (wrong record type, oversized, not an object, no tag).
- A line is reduced only to fields the reader reads; values are copied as raw
  JSON, so numbers and strings round-trip byte for byte.
- Anything Go is not sure about (invalid UTF-8, a stray `\r` that text mode
  would split on, JSON Go rejects but Python accepts such as `NaN`) is copied
  verbatim, so Python decides.
- `turns` skips files over 96 MB: upstream already reads those through its
  own incremental SQLite index (`transcript_stream.records`), and its partial
  pass semantics must not change.

| Profile   | Python reader                         | Reader semantics mirrored            |
|-----------|---------------------------------------|--------------------------------------|
| `turns`   | `sessions.parse_session_turns`        | binary `_raw_lines` (16 MB line cap, 8M char cap) |
| `product` | `transcript_product.scan`             | text mode, tag pre-filter            |
| `nudge`   | `nudge_followthrough._rows`           | text mode, every dict row kept (order matters) |

### Process table (`toidx procs`)

`toidx procs get-process claude | cim claude | names` prints the same CSV
columns the PowerShell pipelines printed, read with Toolhelp32,
`QueryFullProcessImageName`, `NtQueryInformationProcess` and
`GetProcessTimes` (stdlib `syscall` only, no dependencies). 0.15 s instead of
2-20 s. Paths may differ in letter case from WMI; the callers only `stat`
them, which is case-insensitive on Windows.

### Fallbacks

Everything is fail-open (`skills/token-optimizer/scripts/_measure/native_index.py`):
no binary, a stale projection or any error means the upstream code path runs
unchanged. `TOKEN_OPTIMIZER_NATIVE_INDEX=0` disables all native paths;
`TOKEN_OPTIMIZER_TOIDX=<path>` points at a binary elsewhere.

## Build

Requires Go 1.22+. From the repo root:

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
```

Runs every projected reader against the raw reader on your real
transcripts (last N days) and exits 1 on any difference. Transcripts of
sessions that are running while the test runs can differ between the two
reads; the test retries those.

## Fork changes (keep this list short; it is the merge surface)

- `native/` (new): Go source, binary, tests, this README.
- `_measure/native_index.py` (new): projection cache client.
- `_measure/sessions.py`: `parse_session_turns` reads the `turns` projection.
- `_measure/cache_policy.py`: `_get_cache_ttl_waste` warms `turns` first.
- `_measure/transcript_product.py`: `scan` warms and reads `product`.
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
```

Conflicts can only happen in the files listed above. When upstream changes
one of the three readers, check whether it now reads a field the profile
drops (see the profile functions in `native/toidx/main.go`); if so, add the
field, bump `formatVersion` in `main.go` and `FORMAT_VERSION` in
`native_index.py` (old projections are then ignored), rebuild, and rerun the
equivalence test.
