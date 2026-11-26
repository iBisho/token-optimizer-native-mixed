// toidx: native fast paths for the Token Optimizer dashboard.
//
//  1. Projections (default command). Several Python readers re-read every raw
//     transcript on each run although each one only looks at a few fields of a
//     few record kinds. toidx writes, once per (reader profile, path, size,
//     mtime), a projected copy that keeps exactly what that reader looks at.
//     The Python reader then runs unchanged over the projection.
//
//     Exactness rule: a line is dropped or reduced only when the Python reader
//     would certainly treat the projected line (or its absence) the same way.
//     Anything uncertain (invalid UTF-8, a stray "\r", JSON that Go rejects
//     but Python may accept, such as NaN) is copied verbatim so the Python code
//     decides for itself. Raw JSON values are copied byte for byte and never
//     re-escaped, so a projected line is never longer than its source line.
//
//     Cache layout: <cache>/<profile>/v<N>/<sha1(profile NUL path)[:24]>.<size>.<mtime_ns>.jsonl
//     A file with the exact name is complete (written to a temp file, renamed).
//
//  2. "toidx compute": native ports of parse_session_turns and
//     _parse_session_jsonl over the projections (compute.go).
//
// 3. "toidx procs": the Windows process table (procs_windows.go).
package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// v2: projections written without HTML escaping (v1 could grow a line).
	formatVersion = 2
	// transcript_stream.LINE_BYTES: the binary-mode raw reader skips lines
	// longer than this, newline included.
	lineBytes = 16 * 1024 * 1024
	// codex_session.MAX_JSONL_LINE_CHARS: parse_session_turns and
	// _parse_session_jsonl skip decoded lines with more characters than this
	// (newline included).
	maxLineChars = 8 * 1024 * 1024
)

type profile struct {
	name string
	// binaryReader: Python reads with transcript_stream._raw_lines (split on
	// "\n" only, oversized lines skipped). Otherwise Python iterates a text
	// file (universal newlines, no size limit).
	binaryReader bool
	project      func(line []byte) ([]byte, bool)
}

var profiles = map[string]profile{
	"turns":   {"turns", true, projectTurns},
	"product": {"product", false, projectProduct},
	"nudge":   {"nudge", false, projectNudge},
	"session": {"session", true, projectSession},
}

// cacheName must match native_index.py:_projection.
func cacheName(prof, src string, size, mtime int64) string {
	sum := sha1.Sum([]byte(prof + "\x00" + src))
	return fmt.Sprintf("%s.%d.%d.jsonl", hex.EncodeToString(sum[:])[:24], size, mtime)
}

type raw = json.RawMessage

func verbatim(line []byte) ([]byte, bool) {
	return append([]byte(nil), bytes.TrimRight(line, "\n")...), true
}

func str(r raw) (string, bool) {
	var s string
	if r == nil || json.Unmarshal(r, &s) != nil {
		return "", false
	}
	return s, true
}

func object(r raw) (map[string]raw, bool) {
	var m map[string]raw
	if r == nil || json.Unmarshal(r, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// pick keeps the listed keys that are present, in the listed order.
func pick(src map[string]raw, keys ...string) obj {
	var o obj
	for _, k := range keys {
		if v, ok := src[k]; ok {
			o = append(o, kv{k, v})
		}
	}
	return o
}

func hasKey(o obj, k string) bool {
	for _, p := range o {
		if p.k == k {
			return true
		}
	}
	return false
}

// textLineUnsafe reports whether Python's text-mode iteration could split or
// decode this physical line differently from a "\n" split: invalid UTF-8 or
// a "\r" that is not part of the final "\r\n".
func textLineUnsafe(line []byte) bool {
	body := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	return bytes.IndexByte(body, '\r') >= 0 || !utf8.Valid(line)
}

// toolUseItems keeps the content items with type "tool_use", reduced to keys.
func toolUseItems(content raw, keys ...string) (raw, bool) {
	var items []raw
	if json.Unmarshal(content, &items) != nil {
		return nil, false
	}
	kept := [][]byte{}
	for _, it := range items {
		block, ok := object(it)
		if !ok {
			continue
		}
		if t, ok := str(block["type"]); !ok || t != "tool_use" {
			continue
		}
		kept = append(kept, pick(block, keys...).encode())
	}
	return encodeArr(kept), true
}

// parseLine decodes one physical line for a binary-reader profile. keep=false
// with out=nil drops the line; a non-nil out is a verbatim copy.
func parseBinaryLine(line []byte) (rec map[string]raw, out []byte, keep bool) {
	body := bytes.TrimRight(line, "\n")
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil, false // json.loads fails on blank lines
	}
	if !utf8.Valid(line) {
		out, _ = verbatim(line)
		return nil, out, true
	}
	if utf8.RuneCount(line) > maxLineChars {
		return nil, nil, false
	}
	rec, ok := object(body)
	if !ok {
		var any interface{}
		if json.Unmarshal(body, &any) == nil {
			return nil, nil, false // valid JSON, not an object: skipped
		}
		out, _ = verbatim(line) // Go rejects it, Python might not (NaN...)
		return nil, out, true
	}
	return rec, nil, true
}

// ---- profile "turns": sessions.parse_session_turns -------------------------

var (
	tagAssistant = []byte("assistant")
	tagEscape    = []byte(`\u`)
)

func projectTurns(line []byte) ([]byte, bool) {
	// type == "assistant" must be spelled out, possibly with \u escapes.
	if !bytes.Contains(line, tagAssistant) && !bytes.Contains(line, tagEscape) {
		return nil, false
	}
	rec, out, keep := parseBinaryLine(line)
	if rec == nil {
		return out, keep
	}
	if t, ok := str(rec["type"]); !ok || t != "assistant" {
		return nil, false
	}
	msg, ok := object(rec["message"])
	if !ok {
		return nil, false
	}
	if usage, ok := object(msg["usage"]); !ok || len(usage) == 0 {
		return nil, false // not a dict, or empty (falsy)
	}
	o := pick(rec, "type", "timestamp", "requestId")
	slim := pick(msg, "id", "model", "usage")
	if c, ok := msg["content"]; ok {
		if items, ok := toolUseItems(c, "type", "name"); ok {
			slim = append(slim, kv{"content", items})
		} // non-list content is ignored by Python: omitting it is equivalent
	}
	return append(o, kv{"message", slim.encode()}).encode(), true
}

// ---- profile "product": transcript_product.scan ----------------------------

var productTags = [][]byte{[]byte(`"tool_use"`), []byte(`"skill_listing"`),
	[]byte(`"deferred_tools_delta"`), []byte(`"mcp_instructions_delta"`)}

func hasProductTag(b []byte) bool {
	for _, t := range productTags {
		if bytes.Contains(b, t) {
			return true
		}
	}
	return false
}

func projectProduct(line []byte) ([]byte, bool) {
	if !hasProductTag(line) {
		return nil, false // Python skips the line before parsing it
	}
	if textLineUnsafe(line) {
		return verbatim(line)
	}
	body := bytes.TrimRight(line, "\r\n")
	rec, ok := object(body)
	if !ok {
		var any interface{}
		if json.Unmarshal(body, &any) == nil {
			return nil, false // not a dict: skipped
		}
		return verbatim(line) // parse error: Python marks the scan incomplete
	}
	o := pick(rec, "type", "timestamp", "attachment")
	if msg, ok := object(rec["message"]); ok {
		var slim obj
		if c, ok := msg["content"]; ok {
			if items, ok := toolUseItems(c, "type", "name", "id"); ok {
				slim = append(slim, kv{"content", items})
			}
		}
		o = append(o, kv{"message", slim.encode()})
	}
	// A non-dict message (or none) yields no content in Python either.
	b := o.encode()
	if !hasProductTag(b) {
		return verbatim(line) // keep it visible to Python's tag filter
	}
	return b, true
}

// ---- profile "nudge": nudge_followthrough._rows ----------------------------

func projectNudge(line []byte) ([]byte, bool) {
	if textLineUnsafe(line) {
		return verbatim(line)
	}
	body := bytes.TrimRight(line, "\r\n")
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	rec, ok := object(body)
	if !ok {
		var any interface{}
		if json.Unmarshal(body, &any) == nil {
			return nil, false
		}
		return verbatim(line)
	}
	o := pick(rec, "type", "subtype", "timestamp", "uuid", "isSidechain")
	if msg, ok := object(rec["message"]); ok {
		slim := pick(msg, "id", "model", "usage")
		if c, ok := msg["content"]; ok {
			// Only `isinstance(content, str) and content.strip() == "/clear"`
			// is read. Keep any string that mentions /clear verbatim, replace
			// other strings with "" (still a str, never "/clear").
			if s, ok := str(c); ok {
				if strings.Contains(s, "/clear") {
					slim = append(slim, kv{"content", c})
				} else {
					slim = append(slim, kv{"content", raw(`""`)})
				}
			}
		}
		o = append(o, kv{"message", slim.encode()})
	}
	return o.encode(), true
}

// ---- profile "session": sessions._parse_session_jsonl ----------------------
//
// Every dict record counts (timestamps, version, slug, per-day metrics), so
// none is dropped; each is reduced to the fields the parser and its helpers
// (_osrc_prompt_text, _task_user_text, _leading_user_text) read.

var sessionBaseKeys = []string{"type", "timestamp", "version", "slug", "isSidechain"}

func projectSession(line []byte) ([]byte, bool) {
	rec, out, keep := parseBinaryLine(line)
	if rec == nil {
		return out, keep
	}
	typ, _ := str(rec["type"])
	var o obj
	switch typ {
	case "user":
		o = pick(rec, append(append([]string(nil), sessionBaseKeys...), "isMeta", "isCompactSummary")...)
		m, present := rec["message"]
		if !present {
			break
		}
		msg, isObj := object(m)
		if !isObj {
			o = append(o, kv{"message", m}) // a non-dict message is read as the content itself
			break
		}
		var slim obj
		if c, ok := msg["content"]; ok {
			var blocks []raw
			if json.Unmarshal(c, &blocks) == nil {
				kept := make([][]byte, 0, len(blocks))
				for _, b := range blocks {
					block, ok := object(b)
					if !ok {
						kept = append(kept, b) // strings and scalars as they are
						continue
					}
					if t, ok := str(block["type"]); ok && t == "text" {
						kept = append(kept, b)
					} else {
						kept = append(kept, pick(block, "type").encode())
					}
				}
				slim = append(slim, kv{"content", encodeArr(kept)})
			} else {
				slim = append(slim, kv{"content", c}) // strings (prompts) verbatim
			}
		}
		o = append(o, kv{"message", slim.encode()})
	case "assistant":
		o = pick(rec, append(append([]string(nil), sessionBaseKeys...), "requestId")...)
		msg, isObj := object(rec["message"])
		if !isObj {
			break // non-dict assistant messages are read as {}
		}
		slim := pick(msg, "id", "model", "usage")
		if c, ok := msg["content"]; ok {
			var blocks []raw
			if json.Unmarshal(c, &blocks) == nil {
				kept := [][]byte{}
				for _, b := range blocks {
					block, ok := object(b)
					if !ok {
						continue
					}
					if t, ok := str(block["type"]); !ok || t != "tool_use" {
						continue
					}
					s := pick(block, "type", "name")
					if in, ok := object(block["input"]); ok {
						s = append(s, kv{"input", pick(in, "skill", "subagent_type").encode()})
					}
					kept = append(kept, s.encode())
				}
				slim = append(slim, kv{"content", encodeArr(kept)})
			}
		}
		o = append(o, kv{"message", slim.encode()})
	default:
		o = pick(rec, sessionBaseKeys...)
	}
	return o.encode(), true
}

// ---- driver -----------------------------------------------------------------

func buildOne(p profile, src, dir string) (string, error) {
	st, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	size, mtime := st.Size(), st.ModTime().UnixNano()
	name := cacheName(p.name, src, size, mtime)
	dst := filepath.Join(dir, name)
	if _, err := os.Stat(dst); err == nil {
		return "fresh", nil
	}
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tmp, err := os.CreateTemp(dir, ".toidx-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	published := false
	defer func() {
		if !published {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	w := bufio.NewWriterSize(tmp, 1<<20)
	// Read exactly the bytes the stat described, so a concurrent append is
	// never half-included.
	r := bufio.NewReaderSize(io.LimitReader(f, size), 4<<20)
	var acc []byte
	total := 0 // physical line length in bytes, newline included
	for {
		chunk, rerr := r.ReadSlice('\n')
		total += len(chunk)
		if !p.binaryReader || total <= lineBytes {
			acc = append(acc, chunk...)
		}
		if rerr == bufio.ErrBufferFull {
			continue
		}
		if rerr != nil && rerr != io.EOF {
			return "", rerr
		}
		if total > 0 && (!p.binaryReader || total <= lineBytes) {
			if out, keep := p.project(acc); keep {
				w.Write(out)
				w.WriteByte('\n')
			}
		}
		acc, total = acc[:0], 0
		if rerr == io.EOF {
			break
		}
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// Source changed while reading: do not publish a projection of it.
	if st2, err := os.Stat(src); err != nil || st2.Size() != size || st2.ModTime().UnixNano() != mtime {
		return "changed", nil
	}
	if err := os.Rename(tmpName, dst); err != nil {
		if _, serr := os.Stat(dst); serr == nil {
			return "fresh", nil // a concurrent run published the same projection
		}
		return "", err
	}
	published = true
	// Drop projections of older versions of this file.
	prefix := name[:24] + "."
	if old, err := filepath.Glob(filepath.Join(dir, prefix+"*.jsonl")); err == nil {
		for _, o := range old {
			if filepath.Base(o) != name {
				os.Remove(o)
			}
		}
	}
	return "ok", nil
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "procs":
			os.Exit(runProcs(os.Args[2:]))
		case "compute":
			os.Exit(runCompute(os.Args[2:]))
		}
	}
	cacheDir := flag.String("cache", "", "cache root directory (required)")
	jobs := flag.Int("j", runtime.NumCPU(), "parallel workers")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: toidx -cache DIR [-j N] < jobs.txt   (one \"<profile>\\t<absolute path>\" per line)")
		fmt.Fprintln(os.Stderr, "       toidx compute -cache DIR -ctx CTX.json < jobs")
		fmt.Fprintln(os.Stderr, "       toidx procs get-process PREFIX | cim PREFIX | names")
		fmt.Fprintln(os.Stderr, "       toidx version")
		fmt.Fprintln(os.Stderr, "profiles: turns, product, nudge, session")
	}
	flag.Parse()
	if flag.NArg() == 1 && flag.Arg(0) == "version" {
		fmt.Println(formatVersion)
		return
	}
	if *cacheDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	type job struct {
		p   profile
		src string
		dir string
	}
	jobsCh := make(chan job)
	var mu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	report := func(format string, a ...interface{}) {
		mu.Lock()
		fmt.Fprintf(out, format, a...)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for i := 0; i < max(1, *jobs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobsCh {
				status, err := buildOne(j.p, j.src, j.dir)
				if err != nil {
					report("error\t%s\t%s\t%s\n", j.p.name, j.src, strings.ReplaceAll(err.Error(), "\n", " "))
				} else {
					report("%s\t%s\t%s\n", status, j.p.name, j.src)
				}
			}
		}()
	}
	dirs := map[string]string{}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		name, src, ok := strings.Cut(strings.TrimRight(sc.Text(), "\r"), "\t")
		p, known := profiles[name]
		if !ok || !known || src == "" {
			report("error\t%s\t%s\tbad job line\n", name, src)
			continue
		}
		dir, seen := dirs[name]
		if !seen {
			dir = filepath.Join(*cacheDir, name, fmt.Sprintf("v%d", formatVersion))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				report("error\t%s\t%s\t%s\n", name, src, err)
				continue
			}
			dirs[name] = dir
		}
		jobsCh <- job{p, src, dir}
	}
	close(jobsCh)
	wg.Wait()
	out.Flush()
}

var _ = hasKey
