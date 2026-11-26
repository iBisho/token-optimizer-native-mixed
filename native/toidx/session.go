package main

// Port of sessions._parse_session_jsonl (Claude transcripts), line for line,
// plus _aggregate_request_usage, _daily_usage_from_merged and
// trends._compute_call_gap_stats. The topic is left to Python: the parser
// returns the first candidate user records and Python runs
// _extract_topic(_task_user_text(record)) on them (credential redaction).

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const topicCandidates = 16

// ordInt is a str->int dict with insertion order.
type ordInt struct {
	keys []string
	vals map[string]int64
}

func newOrdInt() *ordInt { return &ordInt{vals: map[string]int64{}} }

func (o *ordInt) add(k string, d int64) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] += d
}

func (o *ordInt) encode() []byte {
	out := make(obj, len(o.keys))
	for i, k := range o.keys {
		out[i] = kv{k, jsonInt(o.vals[k])}
	}
	return out.encode()
}

type modelBuckets struct{ fresh, cr, cc, cc1h, cc5m, out int64 }

func (m *modelBuckets) addReq(b buckets) {
	m.fresh += b.inp
	m.cr += b.cr
	m.cc += b.cc
	m.cc1h += b.cc1h
	m.cc5m += b.cc5m
	m.out += b.out
}

func (m *modelBuckets) encode() []byte {
	return obj{{"fresh_input", jsonInt(m.fresh)}, {"cache_read", jsonInt(m.cr)}, {"cache_create", jsonInt(m.cc)},
		{"cache_create_1h", jsonInt(m.cc1h)}, {"cache_create_5m", jsonInt(m.cc5m)}, {"output", jsonInt(m.out)}}.encode()
}

// ordBuckets is a str->modelBuckets dict with insertion order.
type ordBuckets struct {
	keys []string
	vals map[string]*modelBuckets
}

func newOrdBuckets() *ordBuckets { return &ordBuckets{vals: map[string]*modelBuckets{}} }

func (o *ordBuckets) get(k string) *modelBuckets {
	b, ok := o.vals[k]
	if !ok {
		b = &modelBuckets{}
		o.vals[k] = b
		o.keys = append(o.keys, k)
	}
	return b
}

func (o *ordBuckets) encode() []byte {
	out := make(obj, len(o.keys))
	for i, k := range o.keys {
		out[i] = kv{k, o.vals[k].encode()}
	}
	return out.encode()
}

// tsInfo keeps what the parser needs from a record timestamp after the line
// is gone: its JSON, and the parsed datetime when Python would parse it.
type tsInfo struct {
	raw    []byte
	truthy bool
	parsed *pyDT // set for a truthy str (else Python skips the parse)
}

func newTsInfo(v val) tsInfo {
	t := tsInfo{raw: rawOrNullV(v), truthy: truthy(v)}
	if t.truthy && isStr(v) {
		p := isoTS(v)
		t.parsed = &p
	}
	return t
}

type reqEntry struct {
	b       buckets
	model   string
	ts      tsInfo
	advisor bool
	day     string // "" = None
}

type dayMetric struct {
	messages                  int64
	skills, subagents, tools  *ordInt
	first, last, durationMins float64
	seen                      bool
}

// pyWS is the character class of Python's \s for str patterns (str.isspace).
const pyWS = `[\t\n\v\f\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

// slashCommandRE is measure._SLASH_COMMAND_RE.
var slashCommandRE = regexp.MustCompile(
	`<command-name>` + pyWS + `*/?` + pyWS + `*([A-Za-z0-9:_+.-]+)` + pyWS + `*</command-name>`)

// textOf is `block.get("text") or ""` for a text block; a truthy non-str
// would make Python raise, so that falls back.
func textOf(block val) string {
	t := get(block, "text")
	if !truthy(t) {
		return ""
	}
	if !isStr(t) {
		fb("non-string text block")
	}
	return strVal(t)
}

// osrcPromptText is sessions._osrc_prompt_text for a user record.
func osrcPromptText(rec val) string {
	message := get(rec, "message")
	if !truthy(message) {
		return "" // `or {}`: content None
	}
	content := message
	if isObj(message) {
		content = get(message, "content")
	}
	switch {
	case isStr(content):
		return strVal(content)
	case isArr(content):
		its := items(content)
		parts := make([]string, len(its))
		for i, b := range its {
			switch {
			case isObj(b) && strEq(get(b, "type"), "text"):
				parts[i] = textOf(b)
			case isStr(b):
				parts[i] = strVal(b)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// userContent is `message.get("content") if isinstance(message, dict) else message`
// for record.get("message", {}); present reports whether message was present.
func userContent(rec val) (content val, present bool) {
	message := get(rec, "message")
	if message == nil {
		return nil, false
	}
	if isObj(message) {
		return get(message, "content"), true
	}
	return message, true
}

// leadingUserText is sessions._leading_user_text(record.get("message", {})).
func leadingUserText(rec val) string {
	content, _ := userContent(rec)
	switch {
	case isStr(content):
		return strVal(content)
	case isArr(content):
		its := items(content)
		if len(its) == 0 || !isObj(its[0]) || !strEq(get(its[0], "type"), "text") {
			return ""
		}
		return textOf(its[0])
	}
	return ""
}

// taskTextCertainlyEmpty: _task_user_text(record) returns "" for sure.
func taskTextCertainlyEmpty(rec val) bool {
	if truthy(get(rec, "isMeta")) || truthy(get(rec, "isCompactSummary")) {
		return true
	}
	content, present := userContent(rec)
	if !present {
		return true // {} -> content None
	}
	if isStr(content) {
		return false
	}
	if !isArr(content) {
		return true
	}
	for _, b := range items(content) {
		if isStr(b) || (isObj(b) && strEq(get(b, "type"), "text")) {
			return false
		}
	}
	return true
}

func computeSession(j *jobState, lines func(func(val) bool), job computeJob) []byte {
	var ws, we *pyDT
	if job.ws != "" {
		t, ok := parseISO(job.ws)
		if !ok {
			fb("window start")
		}
		ws = &t
	}
	if job.we != "" {
		t, ok := parseISO(job.we)
		if !ok {
			fb("window end")
		}
		we = &t
	}
	windowed := ws != nil || we != nil

	isSidechain := job.reason != ""
	sidechainReason := job.reason
	firstUserSeen := false
	var version, slug []byte
	var candidates [][]byte
	moreCandidates := false
	var firstTs, lastTs *pyDT
	var messageCount int64
	counted := map[string]bool{}
	var convRecords int64
	skills, subagents, toolCalls := newOrdInt(), newOrdInt(), newOrdInt()
	var reqKeys []string
	reqs := map[string]*reqEntry{}
	var reportedIn, reportedOut int64
	reportedModel := newOrdInt()
	var dayKeys []string
	days := map[string]*dayMetric{}
	delta := map[string]map[string]int64{"skills": {}, "subagents": {}, "tool_calls": {}}

	for rec := range lines {
		if jkind(get(rec, "isSidechain")) == 't' {
			isSidechain = true
			sidechainReason = "isSidechain"
		}
		typ := get(rec, "type")
		recUser := strEq(typ, "user")
		recAssistant := strEq(typ, "assistant")

		if recUser && !firstUserSeen {
			if prompt := osrcPromptText(rec); prompt != "" {
				firstUserSeen = true
				if !isSidechain && (strings.Contains(prompt, "OSRC::PROGRESS") || strings.Contains(prompt, "OSRC::DONE")) {
					isSidechain = true
					sidechainReason = "osrc_prompt"
				}
			}
		}
		if version == nil {
			if v := get(rec, "version"); truthy(v) {
				version = rawOrNullV(v)
			}
		}
		if slug == nil {
			if v := get(rec, "slug"); truthy(v) {
				slug = rawOrNullV(v)
			}
		}

		tsV := get(rec, "timestamp")
		if windowed {
			switch jkind(tsV) {
			case 0, 'n':
				continue // str(None) does not parse: record skipped
			case '"':
				t, ok := parseISO(strVal(tsV))
				if !ok {
					fb("window timestamp shape")
				}
				if ws != nil && t.us < ws.us {
					continue
				}
				if we != nil && t.us >= we.us {
					continue
				}
			default:
				fb("non-string timestamp in window")
			}
		}

		var ts *pyDT
		if truthy(tsV) && isStr(tsV) {
			t := isoTS(tsV)
			ts = &t
			if firstTs == nil {
				firstTs = ts
			}
			lastTs = ts
		}
		recordDay := ""
		if ts != nil {
			recordDay = j.ctx.localDay(*ts)
		}
		priorMessages := messageCount
		for _, d := range delta {
			clear(d)
		}

		if recUser && !taskTextCertainlyEmpty(rec) {
			// `if rec_type == "user" and topic is None`: Python takes the first hit.
			if len(candidates) < topicCandidates {
				candidates = append(candidates, rawJSON(nil, rec))
			} else {
				moreCandidates = true
			}
		}
		if recUser || recAssistant {
			convRecords++
		}

		if recAssistant {
			var mid val
			if m := get(rec, "message"); isObj(m) {
				mid = orVal(get(m, "id"), get(rec, "requestId"))
			}
			mid = orVal(mid, get(rec, "requestId"))
			if isNone(mid) {
				messageCount++
			} else if k := pyStrKey(mid); !counted[k] {
				counted[k] = true
				messageCount++
			}
		} else if recUser {
			c, _ := userContent(rec)
			allToolResults := false
			if its := items(c); len(its) > 0 {
				allToolResults = true
				for _, b := range its {
					if !isObj(b) || !strEq(get(b, "type"), "tool_result") {
						allToolResults = false
						break
					}
				}
			}
			if !allToolResults && !truthy(get(rec, "isMeta")) && !truthy(get(rec, "isCompactSummary")) {
				messageCount++
			}
		}

		if recUser {
			lead := pyLstrip(leadingUserText(rec))
			if strings.HasPrefix(lead, "<command-name>") && strings.Contains(lead, "<command-args>") {
				for _, m := range slashCommandRE.FindAllStringSubmatch(lead, -1) {
					if sk, ok := j.ctx.resolveSkill(m[1]); ok {
						skills.add(sk, 1)
						delta["skills"][sk]++
					}
				}
			}
		}

		if recAssistant {
			msg := get(rec, "message")
			if !isObj(msg) {
				msg = nil // record.get("message", {}) that is not a dict -> {}
			}
			for _, b := range items(get(msg, "content")) {
				if !isObj(b) || !strEq(get(b, "type"), "tool_use") {
					continue
				}
				name := ""
				if v := get(b, "name"); v != nil {
					name = pyStrKey(v)
				}
				toolCalls.add(name, 1)
				delta["tool_calls"][name]++
				inp := get(b, "input")
				if !isObj(inp) {
					inp = nil
				}
				if name == "Skill" {
					sk := "unknown"
					if v := get(inp, "skill"); v != nil {
						sk = pyStrKey(v)
					}
					skills.add(sk, 1)
					delta["skills"][sk]++
				} else if name == "Task" || name == "Agent" {
					at := "unknown"
					if v := get(inp, "subagent_type"); v != nil {
						at = pyStrKey(v)
					}
					subagents.add(at, 1)
					delta["subagents"][at]++
				}
			}

			reqID := usageRequestKey(rec, msg)
			if usage := get(msg, "usage"); truthy(usage) && isObj(usage) {
				inTok := safeInt(get(usage, "input_tokens"))
				outTok := safeInt(get(usage, "output_tokens"))
				model := recordModel(msg)
				reportedIn += inTok
				reportedOut += outTok
				reportedModel.add(model, inTok+outTok)
				key := reqID
				if key == "" {
					key = "__noreq__" + strconv.Itoa(len(reqKeys))
				}
				var tsi *tsInfo
				for i, p := range j.billedUsageParts(usage, model) {
					pk := key
					if i > 0 {
						pk = key + "#advisor" + strconv.Itoa(i)
					}
					if tsi == nil {
						t := newTsInfo(tsV)
						tsi = &t
					}
					prev := reqs[pk]
					if prev == nil {
						reqs[pk] = &reqEntry{b: p.b, model: p.model, ts: *tsi, advisor: i > 0}
						reqKeys = append(reqKeys, pk)
						continue
					}
					prev.b = maxBuckets(prev.b, p.b)
					if p.model != "" && p.model != "unknown" {
						prev.model = p.model
					}
					if tsi.truthy {
						prev.ts = *tsi
					}
				}
			}
		}

		if recordDay != "" {
			dm := days[recordDay]
			if dm == nil {
				dm = &dayMetric{skills: newOrdInt(), subagents: newOrdInt(), tools: newOrdInt()}
				days[recordDay] = dm
				dayKeys = append(dayKeys, recordDay)
			}
			dm.messages += messageCount - priorMessages
			for _, g := range []struct {
				global *ordInt
				d      map[string]int64
				target *ordInt
			}{{skills, delta["skills"], dm.skills}, {subagents, delta["subagents"], dm.subagents}, {toolCalls, delta["tool_calls"], dm.tools}} {
				if len(g.d) == 0 {
					continue
				}
				// Python walks the running totals in their insertion order.
				for _, k := range g.global.keys {
					if d := g.d[k]; d != 0 {
						g.target.add(k, d)
					}
				}
			}
			epoch := ts.timestamp()
			if !dm.seen {
				dm.first, dm.last, dm.seen = epoch, epoch, true
			} else {
				dm.first = math.Min(dm.first, epoch)
				dm.last = math.Max(dm.last, epoch)
			}
			dm.durationMins = float64(float64(dm.last-dm.first) / 60)
		}
	}

	if convRecords == 0 {
		return jsonNull
	}

	// _aggregate_request_usage + _daily_usage_from_merged
	var totIn, totOut, totCR, totCC, totCC1h, totCC5m, apiCalls int64
	var callTimes []pyDT
	modelUsage := newOrdInt()
	breakdown := newOrdBuckets()
	daily := map[string]*ordBuckets{}
	var dailyKeys []string
	for _, k := range reqKeys {
		u := reqs[k]
		if u.ts.parsed != nil {
			u.day = j.ctx.localDay(*u.ts.parsed)
		}
		totIn += u.b.inp
		totOut += u.b.out
		totCR += u.b.cr
		totCC += u.b.cc
		totCC1h += u.b.cc1h
		totCC5m += u.b.cc5m
		if !u.advisor {
			apiCalls++
			if u.ts.parsed != nil {
				callTimes = append(callTimes, *u.ts.parsed)
			}
		}
		modelUsage.add(u.model, u.b.inp+u.b.cc+u.b.out)
		breakdown.get(u.model).addReq(u.b)
		day := u.day
		if day == "" {
			day = "undated" // UNDATED_USAGE_KEY
		}
		dd := daily[day]
		if dd == nil {
			dd = newOrdBuckets()
			daily[day] = dd
			dailyKeys = append(dailyKeys, day)
		}
		model := u.model
		if model == "" {
			model = "unknown"
		}
		dd.get(model).addReq(u.b)
	}

	// duration: max(0, delta / 60) keeps an int 0 unless positive
	duration := jsonInt(0)
	if firstTs != nil && lastTs != nil {
		if d := float64(secondsBetween(*lastTs, *firstTs) / 60); d > 0 {
			duration = jsonFloat(d)
		}
	}
	fullInput := totIn + totCR + totCC
	hitRate := 0.0
	if fullInput > 0 {
		hitRate = float64(totCR) / float64(fullInput)
	}
	avg, mx, p95 := gapStats(callTimes)

	dailyUsage := make(obj, len(dailyKeys))
	for i, d := range dailyKeys {
		dailyUsage[i] = kv{d, daily[d].encode()}
	}
	dailyMetrics := make(obj, len(dayKeys))
	for i, d := range dayKeys {
		m := days[d]
		dailyMetrics[i] = kv{d, obj{
			{"message_count", jsonInt(m.messages)},
			{"skills", m.skills.encode()},
			{"subagents", m.subagents.encode()},
			{"tool_calls", m.tools.encode()},
			{"duration_minutes", jsonFloat(m.durationMins)},
			{"first", jsonFloat(m.first)},
			{"last", jsonFloat(m.last)},
		}.encode()}
	}
	requestUsage := make(obj, len(reqKeys))
	for i, k := range reqKeys {
		u := reqs[k]
		e := obj{{"inp", jsonInt(u.b.inp)}, {"out", jsonInt(u.b.out)}, {"cr", jsonInt(u.b.cr)},
			{"cc", jsonInt(u.b.cc)}, {"cc_1h", jsonInt(u.b.cc1h)}, {"cc_5m", jsonInt(u.b.cc5m)},
			{"model", jsonStr(u.model)}, {"ts", u.ts.raw}}
		if u.advisor {
			e = append(e, kv{"advisor", jsonBool(true)})
		}
		if u.day == "" {
			e = append(e, kv{"day", jsonNull})
		} else {
			e = append(e, kv{"day", jsonStr(u.day)})
		}
		requestUsage[i] = kv{k, e.encode()}
	}
	orNull := func(b []byte) []byte {
		if b == nil {
			return jsonNull
		}
		return b
	}
	firstISO := jsonNull
	if firstTs != nil {
		firstISO = jsonStr(firstTs.isoformat())
	}
	reason := jsonNull
	if sidechainReason != "" {
		reason = jsonStr(sidechainReason)
	}
	return obj{
		{"version", orNull(version)},
		{"slug", orNull(slug)},
		{"topic", jsonNull}, // filled in by Python from __topic_candidates
		{"duration_minutes", duration},
		{"total_input_tokens", jsonInt(fullInput)},
		{"total_output_tokens", jsonInt(totOut)},
		{"total_cache_read", jsonInt(totCR)},
		{"total_cache_create", jsonInt(totCC)},
		{"total_cache_create_1h", jsonInt(totCC1h)},
		{"total_cache_create_5m", jsonInt(totCC5m)},
		{"cache_hit_rate", jsonFloat(hitRate)},
		{"avg_call_gap_seconds", avg},
		{"max_call_gap_seconds", mx},
		{"p95_call_gap_seconds", p95},
		{"model_usage", modelUsage.encode()},
		{"model_usage_breakdown", breakdown.encode()},
		{"daily_usage", dailyUsage.encode()},
		{"daily_metrics", dailyMetrics.encode()},
		{"request_usage", requestUsage.encode()},
		{"reported_input_tokens", jsonInt(reportedIn)},
		{"reported_output_tokens", jsonInt(reportedOut)},
		{"reported_model_usage", reportedModel.encode()},
		{"skills_used", skills.encode()},
		{"subagents_used", subagents.encode()},
		{"tool_calls", toolCalls.encode()},
		{"message_count", jsonInt(messageCount)},
		{"api_calls", jsonInt(apiCalls)},
		{"first_ts", firstISO},
		{"is_sidechain", jsonBool(isSidechain)},
		{"sidechain_reason", reason},
		{"__topic_candidates", encodeArr(candidates)},
		{"__topic_more", jsonBool(moreCandidates)},
	}.encode()
}

// pySum is CPython 3.12+ sum() of floats from start 0 (Neumaier compensation).
func pySum(xs []float64) float64 {
	f, c := 0.0, 0.0
	for _, x := range xs {
		t := float64(f + x)
		if math.Abs(f) >= math.Abs(x) {
			c = float64(c + float64(float64(f-t)+x))
		} else {
			c = float64(c + float64(float64(x-t)+f))
		}
		f = t
	}
	if c != 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
		f = float64(f + c)
	}
	return f
}

// gapStats is trends._compute_call_gap_stats.
func gapStats(times []pyDT) (avg, mx, p95 []byte) {
	if len(times) < 2 {
		return jsonNull, jsonNull, jsonNull
	}
	var gaps []float64
	for i := 1; i < len(times); i++ {
		if d := secondsBetween(times[i], times[i-1]); d >= 0 {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) == 0 {
		return jsonNull, jsonNull, jsonNull
	}
	sorted := append([]float64(nil), gaps...)
	sort.Float64s(sorted)
	n := len(sorted)
	idx := int(math.Ceil(float64(float64(n)*0.95))) - 1
	idx = max(0, min(n-1, idx))
	m := gaps[0]
	for _, g := range gaps[1:] {
		if g > m {
			m = g
		}
	}
	return jsonFloat(float64(pySum(gaps) / float64(len(gaps)))), jsonFloat(m), jsonFloat(sorted[idx])
}
