package main

// Python semantics for values decoded from transcript JSON.
//
// Values come from valyala/fastjson (a reusable, allocation-light parser);
// nil means "key absent". Every line is first checked with encoding/json's
// strict validator, so fastjson only ever sees valid JSON.
//
// The ports in turns.go and session.go only handle the common, well-formed
// shapes. Anything whose Python behavior is not replicated exactly here
// (numbers written as strings, lone surrogates, integers beyond 2^53,
// non-standard timestamps...) calls fb(), which abandons the native result
// for that file so Python computes it instead.

import (
	"bytes"
	"math"
	"strconv"
	"time"

	"github.com/valyala/fastjson"
)

type fallbackErr struct{ reason string }

func fb(reason string) { panic(fallbackErr{reason}) }

type val = *fastjson.Value

// jkind: 0 absent, 'n' null, 't' true, 'f' false, '"' string, '[' array,
// '{' object, '0' number.
func jkind(v val) byte {
	if v == nil {
		return 0
	}
	switch v.Type() {
	case fastjson.TypeNull:
		return 'n'
	case fastjson.TypeTrue:
		return 't'
	case fastjson.TypeFalse:
		return 'f'
	case fastjson.TypeString:
		return '"'
	case fastjson.TypeArray:
		return '['
	case fastjson.TypeObject:
		return '{'
	}
	return '0'
}

func isStr(v val) bool { return jkind(v) == '"' }
func isObj(v val) bool { return jkind(v) == '{' }
func isArr(v val) bool { return jkind(v) == '[' }

// isNone: Python None (key absent, or JSON null).
func isNone(v val) bool { k := jkind(v); return k == 0 || k == 'n' }

// get is dict.get(key) on a decoded object: json.loads keeps the LAST of
// duplicate keys, fastjson's Get the first, so scan them all.
func get(v val, key string) val {
	if v == nil || v.Type() != fastjson.TypeObject {
		return nil
	}
	o, _ := v.Object()
	var found val
	o.Visit(func(k []byte, item *fastjson.Value) {
		if string(k) == key {
			found = item
		}
	})
	return found
}

// getObj is `x.get(key)` where x is known to be a dict or {} (absent).
func items(v val) []*fastjson.Value {
	if v == nil || v.Type() != fastjson.TypeArray {
		return nil
	}
	a, _ := v.Array()
	return a
}

func numFloat(v val) float64 {
	if n, err := v.Int64(); err == nil {
		return float64(n)
	}
	var buf [48]byte
	f, err := strconv.ParseFloat(string(v.MarshalTo(buf[:0])), 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f // ±Inf on overflow, 0 on underflow, like Python's float()
		}
		fb("number")
	}
	return f
}

// truthy is Python's bool() of the decoded value; absent counts as None.
func truthy(v val) bool {
	switch jkind(v) {
	case 0, 'n', 'f':
		return false
	case 't':
		return true
	case '"':
		b, _ := v.StringBytes()
		return len(b) > 0
	case '[':
		a, _ := v.Array()
		return len(a) > 0
	case '{':
		o, _ := v.Object()
		return o.Len() > 0
	}
	return numFloat(v) != 0
}

// orVal is Python's `a or b`.
func orVal(a, b val) val {
	if truthy(a) {
		return a
	}
	return b
}

const maxExactInt = 1 << 53

// safeInt is runtime._safe_int: int(float(value or 0)), 0 on garbage.
func safeInt(v val) int64 {
	if !truthy(v) {
		return 0
	}
	switch jkind(v) {
	case 't':
		return 1
	case '"':
		fb("numeric string") // float("...") parsing rules are not ported
	case '[', '{':
		return 0 // float() raises TypeError -> 0
	}
	if n, err := v.Int64(); err == nil && n >= -maxExactInt && n <= maxExactInt {
		return n
	}
	f := numFloat(v)
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 0 // int(inf) raises OverflowError -> 0
	}
	t := math.Trunc(f)
	if math.Abs(t) > maxExactInt {
		fb("integer beyond 2^53")
	}
	return int64(t)
}

// pyFloatValid is `math.isfinite(float(v)) and float(v) >= 0` with a raised
// TypeError/OverflowError mapped to false (runtime._valid_context_usage).
func pyFloatValid(v val) bool {
	switch jkind(v) {
	case 0, 'n', '[', '{':
		return false
	case 't', 'f':
		return true
	case '"':
		fb("numeric string")
	}
	f := numFloat(v)
	return !math.IsInf(f, 0) && !math.IsNaN(f) && f >= 0
}

// strVal decodes a JSON string. Lines with surrogate escapes never get here
// (checkLine), so fastjson's unescape matches Python's.
func strVal(v val) string {
	b, _ := v.StringBytes()
	return string(b)
}

func strEq(v val, s string) bool {
	if !isStr(v) {
		return false
	}
	b, _ := v.StringBytes()
	return string(b) == s
}

// pyStrKey is str(value) for a value used as a dict key or an id: only real
// strings are supported natively.
func pyStrKey(v val) string {
	if !isStr(v) {
		fb("non-string id")
	}
	return strVal(v)
}

// checkLine rejects lines the fastjson path must not interpret: invalid
// JSON or UTF-8 (verbatim copies of uncertain source lines) and surrogate
// escapes (Go and Python decode lone surrogates differently).
func checkLine(b []byte) {
	if !validJSON(b) {
		fb("unparsed projection line")
	}
	if i := bytes.Index(b, []byte(`\u`)); i >= 0 {
		for ; i+5 < len(b); i++ {
			if b[i] != '\\' {
				continue
			}
			if b[i+1] == '\\' {
				i++
				continue
			}
			if b[i+1] == 'u' && (b[i+2] == 'd' || b[i+2] == 'D') {
				switch b[i+3] {
				case '8', '9', 'a', 'b', 'c', 'd', 'e', 'f', 'A', 'B', 'C', 'D', 'E', 'F':
					fb("surrogate escape")
				}
			}
		}
	}
}

// rawJSON re-encodes a value as JSON for output: numbers keep their exact
// literal, strings are re-escaped without HTML escaping, key order and
// duplicates are kept (json.loads resolves them the same way).
func rawJSON(dst []byte, v val) []byte {
	switch jkind(v) {
	case 0, 'n':
		return append(dst, "null"...)
	case 't':
		return append(dst, "true"...)
	case 'f':
		return append(dst, "false"...)
	case '"':
		b, _ := v.StringBytes()
		return appendStr(dst, string(b))
	case '0':
		return v.MarshalTo(dst)
	case '[':
		dst = append(dst, '[')
		for i, it := range items(v) {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = rawJSON(dst, it)
		}
		return append(dst, ']')
	}
	o, _ := v.Object()
	dst = append(dst, '{')
	first := true
	o.Visit(func(k []byte, item *fastjson.Value) {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = appendStr(dst, string(k))
		dst = append(dst, ':')
		dst = rawJSON(dst, item)
	})
	return append(dst, '}')
}

// rawOrNullV copies a value out of the parser (which reuses its memory).
func rawOrNullV(v val) []byte { return rawJSON(nil, v) }

// isPySpace is str.isspace() for one character.
func isPySpace(c rune) bool {
	switch {
	case c >= 0x09 && c <= 0x0d, c >= 0x1c && c <= 0x20, c == 0x85, c == 0xa0,
		c == 0x1680, c >= 0x2000 && c <= 0x200a, c == 0x2028, c == 0x2029,
		c == 0x202f, c == 0x205f, c == 0x3000:
		return true
	}
	return false
}

func pyLstrip(s string) string {
	for i, c := range s {
		if !isPySpace(c) {
			return s[i:]
		}
	}
	return ""
}

// ---- datetimes ------------------------------------------------------------

// pyDT is an aware datetime from datetime.fromisoformat.
type pyDT struct {
	us                     int64 // UTC epoch microseconds
	off                    int   // UTC offset in seconds
	y, mo, d, h, mi, s, fr int   // wall clock fields as written, fr in microseconds
}

func digits(s string, i, n int) (int, bool) {
	if i+n > len(s) {
		return 0, false
	}
	v := 0
	for _, c := range s[i : i+n] {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	return v, true
}

// parseISO accepts exactly YYYY-MM-DDTHH:MM:SS[.fff|.ffffff](Z|±HH:MM),
// the shape Claude Code writes. ok=false means "not this shape": the caller
// must fall back, because Python's fromisoformat accepts many more forms.
func parseISO(s string) (pyDT, bool) {
	var t pyDT
	var ok bool
	if len(s) < 20 || s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return t, false
	}
	if t.y, ok = digits(s, 0, 4); !ok {
		return t, false
	}
	if t.mo, ok = digits(s, 5, 2); !ok {
		return t, false
	}
	if t.d, ok = digits(s, 8, 2); !ok {
		return t, false
	}
	if t.h, ok = digits(s, 11, 2); !ok {
		return t, false
	}
	if t.mi, ok = digits(s, 14, 2); !ok {
		return t, false
	}
	if t.s, ok = digits(s, 17, 2); !ok {
		return t, false
	}
	i := 19
	if s[i] == '.' {
		n := 0
		for i+1+n < len(s) && s[i+1+n] >= '0' && s[i+1+n] <= '9' {
			n++
		}
		if n != 3 && n != 6 {
			return t, false
		}
		v, _ := digits(s, i+1, n)
		if n == 3 {
			v *= 1000
		}
		t.fr = v
		i += 1 + n
	}
	rest := s[i:]
	switch {
	case rest == "Z":
		t.off = 0
	case len(rest) == 6 && (rest[0] == '+' || rest[0] == '-') && rest[3] == ':':
		hh, ok1 := digits(rest, 1, 2)
		mm, ok2 := digits(rest, 4, 2)
		if !ok1 || !ok2 || hh > 23 || mm > 59 {
			return t, false
		}
		t.off = hh*3600 + mm*60
		if rest[0] == '-' {
			t.off = -t.off
		}
	default:
		return t, false
	}
	if t.y < 1 || t.mo < 1 || t.mo > 12 || t.d < 1 || t.h > 23 || t.mi > 59 || t.s > 59 {
		return t, false
	}
	wall := time.Date(t.y, time.Month(t.mo), t.d, t.h, t.mi, t.s, t.fr*1000, time.UTC)
	if wall.Day() != t.d || wall.Month() != time.Month(t.mo) {
		return t, false // e.g. Feb 30
	}
	t.us = wall.UnixMicro() - int64(t.off)*1_000_000
	return t, true
}

// isoTS parses a timestamp value that Python would try to parse
// (truthy str); a str outside the supported shape falls back.
func isoTS(v val) pyDT {
	t, ok := parseISO(strVal(v))
	if !ok {
		fb("timestamp shape")
	}
	return t
}

// isoformat is datetime.isoformat() of a parsed aware datetime.
func (t pyDT) isoformat() string {
	b := make([]byte, 0, 32)
	pad := func(v, n int) {
		s := strconv.Itoa(v)
		for len(s) < n {
			s = "0" + s
		}
		b = append(b, s...)
	}
	pad(t.y, 4)
	b = append(b, '-')
	pad(t.mo, 2)
	b = append(b, '-')
	pad(t.d, 2)
	b = append(b, 'T')
	pad(t.h, 2)
	b = append(b, ':')
	pad(t.mi, 2)
	b = append(b, ':')
	pad(t.s, 2)
	if t.fr != 0 {
		b = append(b, '.')
		pad(t.fr, 6)
	}
	off := t.off
	if off < 0 {
		b = append(b, '-')
		off = -off
	} else {
		b = append(b, '+')
	}
	pad(off/3600, 2)
	b = append(b, ':')
	pad(off%3600/60, 2)
	return string(b)
}

// timestamp is datetime.timestamp(): microseconds / 10**6, correctly rounded.
func (t pyDT) timestamp() float64 { return float64(t.us) / 1e6 }

// secondsBetween is (a - b).total_seconds().
func secondsBetween(a, b pyDT) float64 { return float64(a.us-b.us) / 1e6 }

// divideNearest is CPython's divide_nearest (round half to even).
func divideNearest(m, n int64) int64 {
	q, r := m/n, m%n
	if r < 0 {
		q--
		r += n
	}
	twice := 2 * r
	if twice > n || (twice == n && q%2 != 0) {
		q++
	}
	return q
}

// localDay is _request_local_day: dt.astimezone().strftime("%Y-%m-%d"),
// with the local offset taken from the table Python built with its own
// time.localtime (see native_compute._tz_table).
func (c *computeCtx) localDay(t pyDT) string {
	sec := divideNearest(t.us, 1_000_000)
	off, ok := c.offsetAt(sec)
	if !ok {
		fb("timezone table range")
	}
	local := time.UnixMicro(t.us + off*1_000_000).UTC()
	if local.Year() < 1000 {
		fb("year < 1000")
	}
	return local.Format("2006-01-02")
}
