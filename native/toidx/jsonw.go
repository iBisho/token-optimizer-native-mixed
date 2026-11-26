package main

// Ordered JSON output that never rewrites raw values.
//
// encoding/json HTML-escapes <, > and & (also inside RawMessage values), which
// can make a projected line LONGER than the original and push it over the
// Python reader's per-line character cap. Everything here copies raw values
// byte for byte and encodes only our own strings, without HTML escaping.

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type kv struct {
	k string
	v []byte
}

// obj is a JSON object with a fixed key order (Python dicts keep insertion order).
type obj []kv

func (o obj) encode() []byte {
	b := make([]byte, 0, 64)
	b = append(b, '{')
	for i, p := range o {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendStr(b, p.k)
		b = append(b, ':')
		b = append(b, p.v...)
	}
	return append(b, '}')
}

func encodeArr(items [][]byte) []byte {
	b := []byte{'['}
	for i, it := range items {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, it...)
	}
	return append(b, ']')
}

// appendStr encodes a valid UTF-8 Go string as a JSON string, escaping only
// what JSON requires.
func appendStr(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				b = append(b, '\\', c)
			case c == '\n':
				b = append(b, '\\', 'n')
			case c == '\r':
				b = append(b, '\\', 'r')
			case c == '\t':
				b = append(b, '\\', 't')
			case c < 0x20:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			default:
				b = append(b, c)
			}
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		b = append(b, s[i:i+size]...)
		i += size
	}
	return append(b, '"')
}

func jsonStr(s string) []byte { return appendStr(nil, s) }

func jsonInt(i int64) []byte { return strconv.AppendInt(nil, i, 10) }

func jsonBool(v bool) []byte {
	if v {
		return []byte("true")
	}
	return []byte("false")
}

var jsonNull = []byte("null")

// jsonFloat writes a float the way Python's json.dumps (float.__repr__) does,
// so the value keeps its Python type (float, never int) when read back.
func jsonFloat(f float64) []byte {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		fb("non-finite float")
	}
	return []byte(pyRepr(f))
}

// pyRepr reproduces float.__repr__: shortest round-trip digits, fixed
// notation when -4 < decimal point <= 16, otherwise d.ddde+XX.
func pyRepr(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	neg := s[0] == '-'
	if neg {
		s = s[1:]
	}
	mant, expS, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expS)
	digits := strings.Replace(mant, ".", "", 1)
	decpt := exp + 1
	var out string
	if decpt > -4 && decpt <= 16 {
		switch {
		case decpt <= 0:
			out = "0." + strings.Repeat("0", -decpt) + digits
		case decpt >= len(digits):
			out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
		default:
			out = digits[:decpt] + "." + digits[decpt:]
		}
	} else {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		sign := "+"
		if exp < 0 {
			sign, exp = "-", -exp
		}
		e := strconv.Itoa(exp)
		if len(e) < 2 {
			e = "0" + e
		}
		out = m + "e" + sign + e
	}
	if neg {
		out = "-" + out
	}
	return out
}

// pyRound6 is Python's round(x, 6): correctly rounded to 6 decimals with
// ties to even on the exact binary value, then read back as a float.
func pyRound6(x float64) float64 {
	if math.IsInf(x, 0) || math.IsNaN(x) {
		return x
	}
	v, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 6, 64), 64)
	if err != nil {
		fb("round")
	}
	return v
}
