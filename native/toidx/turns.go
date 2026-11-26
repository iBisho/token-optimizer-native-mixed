package main

// Port of sessions.parse_session_turns (Claude transcripts), line for line.

import "math"

type turn struct {
	index                              int64
	inp, out, cr, cc, cc1h, cc5m       int64
	model                              string
	ts                                 []byte // raw record timestamp (JSON)
	gap                                *int64
	tools                              []string
	cost                               float64
	reqKey                             string // "" = None
	ctxCache, resumeRead, resumeCreate int64
	opt                                []string // optional keys in insertion order
}

func (t *turn) has(k string) bool {
	for _, o := range t.opt {
		if o == k {
			return true
		}
	}
	return false
}

func (t *turn) setOpt(k string, v int64) {
	switch k {
	case "ctx_cache":
		t.ctxCache = v
	case "resume_cache_read":
		t.resumeRead = v
	case "resume_cache_creation":
		t.resumeCreate = v
	}
	if !t.has(k) {
		t.opt = append(t.opt, k)
	}
}

func (t *turn) opt64(k string) int64 {
	switch k {
	case "ctx_cache":
		return t.ctxCache
	case "resume_cache_read":
		return t.resumeRead
	}
	return t.resumeCreate
}

func (t *turn) encode() []byte {
	o := obj{
		{"turn_index", jsonInt(t.index)},
		{"role", jsonStr("assistant")},
		{"input_tokens", jsonInt(t.inp)},
		{"output_tokens", jsonInt(t.out)},
		{"cache_read", jsonInt(t.cr)},
		{"cache_creation", jsonInt(t.cc)},
		{"cache_creation_1h", jsonInt(t.cc1h)},
		{"cache_creation_5m", jsonInt(t.cc5m)},
		{"model", jsonStr(t.model)},
		{"timestamp", t.ts},
	}
	if t.gap == nil {
		o = append(o, kv{"gap_since_prev_seconds", jsonNull})
	} else {
		o = append(o, kv{"gap_since_prev_seconds", jsonInt(*t.gap)})
	}
	tools := make([][]byte, len(t.tools))
	for i, n := range t.tools {
		tools[i] = jsonStr(n)
	}
	o = append(o, kv{"tools_used", encodeArr(tools)}, kv{"cost_usd", jsonFloat(t.cost)})
	if t.reqKey == "" {
		o = append(o, kv{"request_id", jsonNull})
	} else {
		o = append(o, kv{"request_id", jsonStr(t.reqKey)})
	}
	for _, k := range t.opt {
		o = append(o, kv{k, jsonInt(t.opt64(k))})
	}
	return o.encode()
}

func computeTurns(j *jobState, lines func(func(val) bool)) []byte {
	var turns []*turn
	byKey := map[string]*turn{}
	partsByKey := map[string][]part{}
	var turnIndex int64
	var prevCall *pyDT

	for rec := range lines {
		if !strEq(get(rec, "type"), "assistant") {
			continue
		}
		msg := get(rec, "message")
		if !isObj(msg) {
			continue
		}
		usage := get(msg, "usage")
		if !truthy(usage) || !isObj(usage) {
			continue
		}
		inp := safeInt(get(usage, "input_tokens"))
		out := safeInt(get(usage, "output_tokens"))
		cr := safeInt(get(usage, "cache_read_input_tokens"))
		var nested val
		if c := get(usage, "cache_creation"); isObj(c) {
			nested = c
		}
		cc1h := safeInt(orVal(get(nested, "ephemeral_1h_input_tokens"), get(usage, "ephemeral_1h_input_tokens")))
		cc5m := safeInt(orVal(get(nested, "ephemeral_5m_input_tokens"), get(usage, "ephemeral_5m_input_tokens")))
		cc := safeInt(get(usage, "cache_creation_input_tokens"))
		if cc == 0 {
			cc = cc1h + cc5m
		}
		model := recordModel(msg)

		var tools []string
		for _, b := range items(get(msg, "content")) {
			if !isObj(b) || !strEq(get(b, "type"), "tool_use") {
				continue
			}
			if name := get(b, "name"); name == nil {
				tools = append(tools, "")
			} else {
				tools = append(tools, pyStrKey(name))
			}
		}

		reqKey := usageRequestKey(rec, msg)
		var prior *turn
		if reqKey != "" {
			prior = byKey[reqKey]
		}

		tsV := get(rec, "timestamp")
		var gap *int64
		if prior == nil && truthy(tsV) && isStr(tsV) {
			t := isoTS(tsV)
			if prevCall != nil {
				d := secondsBetween(t, *prevCall)
				var g int64
				if d > 0 {
					g = int64(math.RoundToEven(d))
				}
				gap = &g
			}
			prevCall = &t
		}
		parts := j.billedUsageParts(usage, model)
		cost := j.priceParts(parts, true)

		t := &turn{index: turnIndex, inp: inp, out: out, cr: cr, cc: cc, cc1h: cc1h, cc5m: cc5m,
			model: model, gap: gap, tools: tools, cost: pyRound6(cost), reqKey: reqKey}
		if it := executorUsage(usage, true); it != nil {
			t.setOpt("ctx_cache", contextTokenCount(it)-safeInt(get(it, "input_tokens")))
		}
		if it := executorUsage(usage, false); it != nil {
			read := safeInt(get(it, "cache_read_input_tokens"))
			t.setOpt("resume_cache_read", read)
			t.setOpt("resume_cache_creation", contextTokenCount(it)-read-safeInt(get(it, "input_tokens")))
		}

		if prior != nil {
			latest := t.out >= prior.out
			prior.inp = max(prior.inp, t.inp)
			prior.out = max(prior.out, t.out)
			prior.cr = max(prior.cr, t.cr)
			prior.cc = max(prior.cc, t.cc)
			prior.cc1h = max(prior.cc1h, t.cc1h)
			prior.cc5m = max(prior.cc5m, t.cc5m)
			before := len(prior.tools)
			for _, name := range t.tools {
				found := false
				for _, p := range prior.tools[:before] {
					if p == name {
						found = true
						break
					}
				}
				if !found {
					prior.tools = append(prior.tools, name)
				}
			}
			if model != "" && model != "unknown" {
				prior.model = model
			}
			for _, k := range []string{"ctx_cache", "resume_cache_read", "resume_cache_creation"} {
				if t.has(k) && (latest || !prior.has(k)) {
					prior.setOpt(k, t.opt64(k))
				}
			}
			merged := partsByKey[reqKey]
			for i, p := range parts {
				if i == len(merged) {
					merged = append(merged, p)
					continue
				}
				merged[i].b = maxBuckets(merged[i].b, p.b)
				if p.model != "" && p.model != "unknown" {
					merged[i].model = p.model
				}
			}
			partsByKey[reqKey] = merged
			prior.cost = pyRound6(j.priceParts(merged, true))
			continue
		}
		t.ts = rawOrNullV(tsV) // copy: the parser reuses its memory
		turns = append(turns, t)
		if reqKey != "" {
			byKey[reqKey] = t
			partsByKey[reqKey] = append([]part(nil), parts...)
		}
		turnIndex++
	}
	out := make([][]byte, len(turns))
	for i, t := range turns {
		out[i] = t.encode()
	}
	return encodeArr(out)
}
