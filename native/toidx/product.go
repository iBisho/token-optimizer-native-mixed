package main

// Extraction for transcript_product.scan. Go does the per-line work (tag
// filter, json.loads, the type checks) and emits, per record that matters,
// only the values scan reads; Python replays them through the same logic
// (transcript_product._replay). Every shape outside what that replay covers
// exactly makes the whole file fall back to the Python reader.
//
// Event: [timestamp-or-null, kind-or-null, data, uses]
//   kind "skill_listing":          data = [len(content), skillCount] when
//                                  isInitial is truthy and content is a str,
//                                  else null
//   kind "deferred_tools_delta":   data = [removed, [[first token, len], ...]]
//   kind "mcp_instructions_delta": data = [removed, [[name, len], ...], mismatch]
//   uses: [[tool name, call id or null], ...] for tool_use items whose name
//         starts with "mcp__" (Python applies _server to them)

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// pyLen is len(str) of a decoded JSON string. Lines with surrogate escapes
// never get here (checkLine), so code points equal runes.
func pyLen(v val) int64 {
	b, _ := v.StringBytes()
	return int64(utf8.RuneCount(b))
}

// listOrEmpty is `x or []` where scan then iterates or len()s the value:
// only falsy values and lists are reproduced.
func listOrEmpty(v val) []val {
	if !truthy(v) {
		return nil
	}
	if !isArr(v) {
		fb("non-list attachment field")
	}
	return items(v)
}

// removedNames: target.pop(name, None) for each name. Targets only hold
// str keys, so other hashable scalars are no-ops; unhashable values raise.
func removedNames(a val) []byte {
	var out [][]byte
	for _, n := range listOrEmpty(get(a, "removedNames")) {
		switch jkind(n) {
		case '"':
			out = append(out, jsonStr(strVal(n)))
		case '[', '{':
			fb("unhashable removed name")
		}
	}
	return encodeArr(out)
}

// firstToken is `value.split(None, 1)[0]`, or "" when value.strip() is empty.
func firstToken(s string) string {
	s = pyLstrip(s)
	for i, c := range s {
		if isPySpace(c) {
			return s[:i]
		}
	}
	return s
}

func productAttachment(kind string, a val) []byte {
	switch kind {
	case "skill_listing":
		c := get(a, "content")
		if !truthy(get(a, "isInitial")) || !isStr(c) {
			return jsonNull
		}
		return encodeArr([][]byte{jsonInt(pyLen(c)), rawOrNullV(get(a, "skillCount"))})
	case "deferred_tools_delta":
		removed := removedNames(a)
		var pairs [][]byte
		for _, v := range listOrEmpty(get(a, "addedLines")) {
			if !isStr(v) {
				continue
			}
			name := firstToken(strVal(v))
			if strings.HasPrefix(name, "mcp__") {
				pairs = append(pairs, encodeArr([][]byte{jsonStr(name), jsonInt(pyLen(v))}))
			}
		}
		return encodeArr([][]byte{removed, encodeArr(pairs)})
	}
	// mcp_instructions_delta
	removed := removedNames(a)
	names := listOrEmpty(get(a, "addedNames"))
	values := listOrEmpty(get(a, "addedBlocks"))
	var pairs [][]byte
	mismatch := len(names) != len(values)
	if !mismatch {
		for i, n := range names {
			if isStr(n) && isStr(values[i]) {
				pairs = append(pairs, encodeArr([][]byte{jsonStr(strVal(n)), jsonInt(pyLen(values[i]))}))
			}
		}
	}
	return encodeArr([][]byte{removed, encodeArr(pairs), jsonBool(mismatch)})
}

func computeProduct(lines func(func([]byte, val) bool)) []byte {
	var events [][]byte
	for line, rec := range lines {
		body := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if bytes.IndexByte(body, '\r') >= 0 {
			fb("carriage return") // text mode would split the line
		}
		if !hasProductTag(line) {
			continue
		}
		var att val
		if a := get(rec, "attachment"); truthy(a) {
			if !isObj(a) {
				continue // scan skips the whole record
			}
			att = a
		}
		kind := ""
		if k := get(att, "type"); isStr(k) {
			switch s := strVal(k); s {
			case "skill_listing", "deferred_tools_delta", "mcp_instructions_delta":
				kind = s
			}
		}
		var data []byte = jsonNull
		if kind != "" {
			data = productAttachment(kind, att)
		}
		var uses [][]byte
		if strEq(get(rec, "type"), "assistant") {
			if msg := get(rec, "message"); isObj(msg) {
				for _, it := range items(get(msg, "content")) {
					if !isObj(it) || !strEq(get(it, "type"), "tool_use") {
						continue
					}
					name := get(it, "name")
					if !isStr(name) || !strings.HasPrefix(strVal(name), "mcp__") {
						continue // _server(name) is None: the item is ignored
					}
					id := get(it, "id")
					idOut := jsonNull
					if truthy(id) {
						if !isStr(id) {
							fb("non-string call id")
						}
						idOut = jsonStr(strVal(id))
					}
					uses = append(uses, encodeArr([][]byte{jsonStr(strVal(name)), idOut}))
				}
			}
		}
		if kind == "" && len(uses) == 0 {
			continue
		}
		ts := jsonNull
		if t := get(rec, "timestamp"); isStr(t) {
			ts = jsonStr(strVal(t))
		}
		k := jsonNull
		if kind != "" {
			k = jsonStr(kind)
		}
		events = append(events, encodeArr([][]byte{ts, k, data, encodeArr(uses)}))
	}
	return encodeArr(events)
}
