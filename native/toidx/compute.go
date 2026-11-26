package main

// "toidx compute": native ports of sessions.parse_session_turns and
// sessions._parse_session_jsonl, run over the turns/session projections.
//
// Python writes a context file (pricing cards resolved per model string by
// its own pricing code, installed skills, a local-time offset table) and
// sends jobs on stdin:
//
//	turns\t<path>\t<sidechain reason>
//	session\t<path>\t<sidechain reason>\t<window start iso>\t<window end iso>
//
// Each job yields one stdout line: a JSON header, a tab, then the result
// JSON (or nothing). Header status is "ok", "fallback" (Python must compute
// this file itself) or "need_models" (resolve these model strings and rerun).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/valyala/fastjson"
)

const largeBytes = 96 * 1024 * 1024 // transcript_stream.LARGE_BYTES

type priceCard struct {
	Kind        string             `json:"kind"` // claude | flat | none | error
	Priced      bool               `json:"priced"`
	Rates       map[string]float64 `json:"rates"`
	LC          map[string]float64 `json:"lc"`
	LCThreshold float64            `json:"lc_threshold"`
}

type computeCtx struct {
	V      int                   `json:"v"`
	Models map[string]*priceCard `json:"models"`
	Skills struct {
		OK        bool              `json:"ok"`
		Names     []string          `json:"names"`
		NameToDir map[string]string `json:"name_to_dir"`
	} `json:"skills"`
	TZ    [][2]int64 `json:"tz"` // [first second, UTC offset seconds], ascending
	TZMax int64      `json:"tz_max"`

	installed map[string]bool
}

func (c *computeCtx) offsetAt(sec int64) (int64, bool) {
	if len(c.TZ) == 0 || sec < c.TZ[0][0] || sec >= c.TZMax {
		return 0, false
	}
	i := sort.Search(len(c.TZ), func(i int) bool { return c.TZ[i][0] > sec }) - 1
	return c.TZ[i][1], true
}

// resolveSkill is sessions._resolve_installed_skill.
func (c *computeCtx) resolveSkill(name string) (string, bool) {
	if name == "" || !c.Skills.OK {
		return "", false
	}
	if c.installed[name] {
		return name, true
	}
	if d, ok := c.Skills.NameToDir[name]; ok {
		return d, true
	}
	if parent, child, found := strings.Cut(name, ":"); found {
		for _, cand := range []string{parent, child} {
			if c.installed[cand] {
				return cand, true
			}
			if d, ok := c.Skills.NameToDir[cand]; ok {
				return d, true
			}
		}
	}
	return "", false
}

// job state shared by the ports.
type jobState struct {
	ctx     *computeCtx
	missing map[string]bool
}

var noneCard = &priceCard{Kind: "none"}

func (j *jobState) card(model string) *priceCard {
	if m, ok := j.ctx.Models[model]; ok {
		return m
	}
	j.missing[model] = true
	return noneCard // result is discarded when anything is missing
}

// ---- pricing (pricing._get_model_cost, runtime._price_usage_parts) ----------

type buckets struct{ inp, out, cr, cc, cc1h, cc5m int64 }

func (b buckets) zero() bool {
	return b.inp == 0 && b.out == 0 && b.cr == 0 && b.cc == 0 && b.cc1h == 0 && b.cc5m == 0
}

func maxBuckets(a, b buckets) buckets {
	return buckets{max(a.inp, b.inp), max(a.out, b.out), max(a.cr, b.cr),
		max(a.cc, b.cc), max(a.cc1h, b.cc1h), max(a.cc5m, b.cc5m)}
}

type part struct {
	model string
	b     buckets
}

func rate(rates map[string]float64, k string) float64 {
	v, ok := rates[k]
	if !ok {
		fb("rate card without " + k) // Python raises KeyError here
	}
	return v
}

// term is `tokens * rate / 1e6` with every step rounded (no fused ops).
func term(n int64, r float64) float64 {
	p := float64(float64(n) * r)
	return float64(p / 1e6)
}

// modelCost is _get_model_cost; split passes cache_create_1h/_5m (TTL tiers).
func modelCost(m *priceCard, b buckets, perRequest, split bool) float64 {
	full := b.inp + b.cr + b.cc
	switch m.Kind {
	case "none":
		return 0.0
	case "flat": // OpenAI / Gemini cards
		rates := m.Rates
		if m.LC != nil && float64(full) > m.LCThreshold {
			rates = m.LC
		}
		cw := rates["cache_write"] // rates.get("cache_write", 0)
		a := term(b.inp, rate(rates, "input"))
		a = float64(a + term(b.out, rate(rates, "output")))
		a = float64(a + term(b.cr, rate(rates, "cache_read")))
		return float64(a + term(b.cc, cw))
	case "claude":
		rates := m.Rates
		if perRequest && m.LC != nil && float64(full) > m.LCThreshold {
			rates = m.LC
		}
		var ccw float64
		if split {
			// split_1h * rates.get("cache_write_1h", rates["cache_write"]) / 1e6
			//   + (split_5m + unsplit) * rates["cache_write"] / 1e6
			unsplit := max(0, b.cc-b.cc1h-b.cc5m)
			cw := rate(rates, "cache_write")
			cw1h, ok := rates["cache_write_1h"]
			if !ok {
				cw1h = cw
			}
			ccw = float64(term(b.cc1h, cw1h) + term(b.cc5m+unsplit, cw))
		} else {
			ccw = term(b.cc, rate(rates, "cache_write"))
		}
		a := term(b.inp, rate(rates, "input"))
		a = float64(a + term(b.out, rate(rates, "output")))
		a = float64(a + term(b.cr, rate(rates, "cache_read")))
		return float64(a + ccw)
	}
	fb("price card " + m.Kind)
	return 0
}

// priceParts is _price_usage_parts as measure.py binds it: ttl_split defaults
// to True, so a part with a 1h/5m split prices its cache writes by tier.
func (j *jobState) priceParts(parts []part, perRequest bool) float64 {
	total := 0.0
	for _, p := range parts {
		split := p.b.cc1h != 0 || p.b.cc5m != 0
		total = float64(total + modelCost(j.card(p.model), p.b, perRequest, split))
	}
	return total
}

// ---- usage helpers (runtime.py) ---------------------------------------------
//
// All take the decoded usage (or iteration) object; nil or a non-dict value
// behaves like Python's non-dict branches.

func usageBuckets(u val) buckets {
	if !isObj(u) {
		return buckets{}
	}
	var nested val
	if cc := get(u, "cache_creation"); isObj(cc) {
		nested = cc
	}
	cc1h := safeInt(orVal(get(nested, "ephemeral_1h_input_tokens"), get(u, "ephemeral_1h_input_tokens")))
	cc5m := safeInt(orVal(get(nested, "ephemeral_5m_input_tokens"), get(u, "ephemeral_5m_input_tokens")))
	cc := safeInt(get(u, "cache_creation_input_tokens"))
	if cc == 0 {
		cc = cc1h + cc5m
	}
	return buckets{max(0, safeInt(get(u, "input_tokens"))), max(0, safeInt(get(u, "output_tokens"))),
		max(0, safeInt(get(u, "cache_read_input_tokens"))), max(0, cc), max(0, cc1h), max(0, cc5m)}
}

func (j *jobState) billedUsageParts(usage val, model string) []part {
	agg := usageBuckets(usage)
	iters := get(usage, "iterations")
	if !isArr(iters) {
		return []part{{model, agg}}
	}
	var exSum, advSum buckets
	var advisors []part
	add := func(s *buckets, b buckets) {
		s.inp += b.inp
		s.out += b.out
		s.cr += b.cr
		s.cc += b.cc
		s.cc1h += b.cc1h
		s.cc5m += b.cc5m
	}
	for _, it := range items(iters) {
		if !isObj(it) {
			continue
		}
		switch typ := get(it, "type"); {
		case strEq(typ, "message"):
			add(&exSum, usageBuckets(it))
		case strEq(typ, "advisor_message"):
			b := usageBuckets(it)
			if b.zero() {
				continue
			}
			adv := model
			if am := get(it, "model"); isStr(am) {
				if s := strVal(am); s != "" && j.card(s).Priced {
					adv = s
				}
			}
			advisors = append(advisors, part{adv, b})
			add(&advSum, b)
		}
	}
	if len(advisors) == 0 {
		return []part{{model, agg}}
	}
	ex := agg
	if agg.inp >= exSum.inp+advSum.inp && agg.out >= exSum.out+advSum.out && agg.cr >= exSum.cr+advSum.cr &&
		agg.cc >= exSum.cc+advSum.cc && agg.cc1h >= exSum.cc1h+advSum.cc1h && agg.cc5m >= exSum.cc5m+advSum.cc5m {
		ex = buckets{agg.inp - advSum.inp, agg.out - advSum.out, agg.cr - advSum.cr,
			agg.cc - advSum.cc, agg.cc1h - advSum.cc1h, agg.cc5m - advSum.cc5m}
	}
	return append([]part{{model, ex}}, advisors...)
}

func contextTokenCount(u val) int64 {
	if !isObj(u) {
		return 0
	}
	creation := safeInt(get(u, "cache_creation_input_tokens"))
	if creation == 0 {
		var nested val
		if cc := get(u, "cache_creation"); isObj(cc) {
			nested = cc
		}
		for _, k := range []string{"ephemeral_1h_input_tokens", "ephemeral_5m_input_tokens"} {
			creation += safeInt(orVal(get(nested, k), get(u, k)))
		}
	}
	return safeInt(get(u, "input_tokens")) + safeInt(get(u, "cache_read_input_tokens")) + creation
}

func validContextUsage(u val) bool {
	var values []val
	for _, k := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if v := get(u, k); v != nil {
			values = append(values, v)
		}
	}
	if safeInt(get(u, "cache_creation_input_tokens")) == 0 {
		nested := get(u, "cache_creation") // usage.get("cache_creation", {})
		if nested != nil && !isObj(nested) {
			return false
		}
		for _, k := range []string{"ephemeral_1h_input_tokens", "ephemeral_5m_input_tokens"} {
			nv := get(nested, k)
			if nv != nil {
				values = append(values, nv)
			}
			if v := get(u, k); v != nil && !truthy(nv) {
				values = append(values, v)
			}
		}
	}
	for _, v := range values {
		if !pyFloatValid(v) {
			return false
		}
	}
	return true
}

// executorUsage is _context_usage (last=true) / _resume_usage (last=false):
// returns the chosen iteration, or nil when the aggregate usage is used.
func executorUsage(usage val, last bool) val {
	its := items(get(usage, "iterations"))
	for n := range its {
		it := its[n]
		if last {
			it = its[len(its)-1-n]
		}
		if isObj(it) && strEq(get(it, "type"), "message") {
			if validContextUsage(it) && contextTokenCount(it) > 0 {
				return it
			}
			return nil
		}
	}
	return nil
}

// recordModel is sessions._record_model.
func recordModel(msg val) string {
	if v := get(msg, "model"); isStr(v) {
		if s := strVal(v); s != "" {
			return s
		}
	}
	return "unknown"
}

// usageRequestKey is sessions._usage_request_key ("" means None).
func usageRequestKey(rec, msg val) string {
	if v := get(rec, "requestId"); truthy(v) {
		return pyStrKey(v)
	}
	if v := get(msg, "id"); truthy(v) {
		return pyStrKey(v)
	}
	return ""
}

func validJSON(b []byte) bool { return utf8.Valid(b) && json.Valid(b) }

// ---- driver -----------------------------------------------------------------

type computeJob struct {
	idx            int
	kind, src      string
	reason, ws, we string
}

func runCompute(args []string) int {
	var cacheDir, ctxPath string
	jobs := runtime.NumCPU()
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-cache":
			i++
			cacheDir = args[i]
		case "-ctx":
			i++
			ctxPath = args[i]
		case "-j":
			i++
			fmt.Sscan(args[i], &jobs)
		case "-cpuprofile":
			i++
			if pf, err := os.Create(args[i]); err == nil {
				pprof.StartCPUProfile(pf)
				defer pprof.StopCPUProfile()
			}
		}
	}
	if cacheDir == "" || ctxPath == "" {
		fmt.Fprintln(os.Stderr, "usage: toidx compute -cache DIR -ctx CTX.json < jobs")
		return 2
	}
	debug.SetGCPercent(400) // short-lived process: trade memory for fewer GC cycles
	data, err := os.ReadFile(ctxPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var ctx computeCtx
	if err := json.Unmarshal(data, &ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ctx:", err)
		return 1
	}
	ctx.installed = map[string]bool{}
	for _, n := range ctx.Skills.Names {
		ctx.installed[n] = true
	}

	var list []computeJob
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		f := strings.Split(strings.TrimRight(sc.Text(), "\r"), "\t")
		if len(f) < 3 {
			continue
		}
		j := computeJob{idx: len(list), kind: f[0], src: f[1], reason: f[2]}
		if len(f) >= 5 {
			j.ws, j.we = f[3], f[4]
		}
		list = append(list, j)
	}
	// Largest files first, so one big transcript does not finish last alone.
	sizes := make(map[int]int64, len(list))
	for _, j := range list {
		if st, err := os.Stat(j.src); err == nil {
			sizes[j.idx] = st.Size()
		}
	}
	order := append([]computeJob(nil), list...)
	sort.SliceStable(order, func(a, b int) bool { return sizes[order[a].idx] > sizes[order[b].idx] })

	out := bufio.NewWriterSize(os.Stdout, 1<<20)
	var mu sync.Mutex
	ch := make(chan computeJob)
	var wg sync.WaitGroup
	for w := 0; w < max(1, jobs); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var p fastjson.Parser // one reusable parser per worker
			for j := range ch {
				line := computeOne(&ctx, cacheDir, j, &p)
				mu.Lock()
				out.Write(line)
				out.WriteByte('\n')
				mu.Unlock()
			}
		}()
	}
	for _, j := range order {
		ch <- j
	}
	close(ch)
	wg.Wait()
	out.Flush()
	return 0
}

func header(j computeJob, status string, extra obj) []byte {
	o := obj{{"i", jsonInt(int64(j.idx))}, {"status", jsonStr(status)}}
	return append(o, extra...).encode()
}

func computeOne(ctx *computeCtx, cacheDir string, j computeJob, p *fastjson.Parser) (line []byte) {
	profName := map[string]string{"turns": "turns", "session": "session"}[j.kind]
	if profName == "" {
		return header(j, "fallback", obj{{"reason", jsonStr("unknown kind")}})
	}
	st, err := os.Stat(j.src)
	if err != nil {
		return header(j, "fallback", obj{{"reason", jsonStr("stat")}})
	}
	if st.Size() > largeBytes {
		return header(j, "fallback", obj{{"reason", jsonStr("large transcript")}})
	}
	dir := filepath.Join(cacheDir, profName, fmt.Sprintf("v%d", formatVersion))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return header(j, "fallback", obj{{"reason", jsonStr("cache dir")}})
	}
	status, err := buildOne(profiles[profName], j.src, dir)
	if err != nil || status == "changed" {
		return header(j, "fallback", obj{{"reason", jsonStr("projection")}})
	}
	size, mtime := st.Size(), st.ModTime().UnixNano()
	proj := filepath.Join(dir, cacheName(profName, j.src, size, mtime))
	f, err := os.Open(proj)
	if err != nil {
		return header(j, "fallback", obj{{"reason", jsonStr("projection open")}})
	}
	defer f.Close()

	js := &jobState{ctx: ctx, missing: map[string]bool{}}
	var result []byte
	defer func() {
		if r := recover(); r != nil {
			reason := fmt.Sprint(r)
			if fe, ok := r.(fallbackErr); ok {
				reason = fe.reason
			}
			line = header(j, "fallback", obj{{"reason", jsonStr(reason)}})
		}
	}()
	lines := projectionLines(f, p)
	if j.kind == "turns" {
		result = computeTurns(js, lines)
	} else {
		result = computeSession(js, lines, j)
	}
	if len(js.missing) > 0 {
		names := make([]string, 0, len(js.missing))
		for m := range js.missing {
			names = append(names, m)
		}
		sort.Strings(names)
		items := make([][]byte, len(names))
		for i, n := range names {
			items[i] = jsonStr(n)
		}
		return header(j, "need_models", obj{{"models", encodeArr(items)}})
	}
	h := header(j, "ok", obj{{"size", jsonInt(size)}, {"mtime_ns", jsonInt(mtime)}})
	return append(append(h, '\t'), result...)
}

// projectionLines yields each projection line as a decoded object. The value
// is only valid until the next line (the parser reuses its memory): copy
// anything kept (rawOrNullV). A line that is not a valid JSON object is a
// verbatim copy of something uncertain: Python must handle the whole file.
func projectionLines(f *os.File, p *fastjson.Parser) func(yield func(val) bool) {
	return func(yield func(val) bool) {
		r := bufio.NewReaderSize(f, 1<<20)
		for {
			b, err := r.ReadSlice('\n')
			if err == bufio.ErrBufferFull {
				big := append([]byte(nil), b...)
				for err == bufio.ErrBufferFull {
					b, err = r.ReadSlice('\n')
					big = append(big, b...)
				}
				b = big
			}
			if len(b) > 0 {
				checkLine(b)
				v, perr := p.ParseBytes(b)
				if perr != nil || v.Type() != fastjson.TypeObject {
					fb("unparsed projection line")
				}
				if !yield(v) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
}
