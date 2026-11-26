package main

// toidx hook: native front door for hooks/python-launcher.sh + hooks/run.py
// (Windows only for now; see native/README.md, "Hook front door").
//
// The launcher runs `toidx hook <run.py> <script_rel> [args...]` before its own
// interpreter discovery. This file decides, the way run.py does, what the hook
// should do, and starts ONE Python for it instead of run.py's two:
//
//   - exit:   run.py would exit 0 without running anything (plugin disabled,
//             consent opted out, a bad target path, the prompt hook switched
//             off): exit 0.
//   - fast:   run.py would start module_runner.py: start it directly, with the
//             environment run.py builds.
//   - medium: anything this port is not certain to decide exactly like run.py
//             (another runtime, an unusual config, a consent backfill that
//             writes): start `python run.py ...` and let it decide.
//
// The interpreter comes from a record run.py writes for its own sys.executable
// (the launcher's validated choice), keyed by PATH and TOKEN_OPTIMIZER_PYTHON.
// Without a valid record toidx exits 75 before touching stdin, and the
// launcher carries on exactly as before.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const hookNotHandled = 75

type hookPlanKind int

const (
	planMedium hookPlanKind = iota
	planExit
	planFast
)

type hookPlan struct {
	kind hookPlanKind
	args []string // fast: module_runner argv after the interpreter
	env  []string // fast: child environment
}

func pyStrip(s string) string {
	s = pyLstrip(s)
	for len(s) > 0 {
		r, n := utf8.DecodeLastRuneInString(s)
		if !isPySpace(r) {
			break
		}
		s = s[:len(s)-n]
	}
	return s
}

// ---- interpreter record (written by run.py) ---------------------------------

func hookRecordPath() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte("v1\x00" + os.Getenv("PATH") + "\x00" + os.Getenv("TOKEN_OPTIMIZER_PYTHON")))
	name := "python-" + hex.EncodeToString(sum[:])[:24] + ".txt"
	return filepath.Join(home, ".cache", "token-optimizer", "native-hook", name), true
}

// hookInterpreter returns the interpreter run.py recorded for this PATH and
// TOKEN_OPTIMIZER_PYTHON, if it is still a plausible one.
func hookInterpreter() (string, bool) {
	p, ok := hookRecordPath()
	if !ok {
		return "", false
	}
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4096 || !privateFile(p, st) {
		return "", false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "v1" {
		return "", false
	}
	interp := lines[1]
	if !filepath.IsAbs(interp) {
		return "", false
	}
	ist, err := os.Stat(interp)
	if err != nil || !ist.Mode().IsRegular() || ist.Size() == 0 {
		return "", false
	}
	// The launcher keeps python.exe when stdout is a console (pythonw would
	// lose it); never run a recorded GUI twin there.
	base := strings.ToLower(filepath.Base(interp))
	if (base == "pythonw.exe" || base == "pyw.exe") && stdoutIsConsole() {
		return "", false
	}
	return interp, true
}

// ---- small Python-semantics helpers ------------------------------------------

func isDirFollow(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isFileFollow(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// resolveStrict is Path.resolve(strict=True): absolute, symlinks resolved,
// error when missing.
func resolveStrict(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// resolvePlain is Path.resolve(strict=False) for paths without links: the
// absolute cleaned path, with changed=true when resolving links would move it
// (then the caller cannot be sure it matches Python and defers to run.py).
func resolvePlain(p string) (string, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", true
	}
	// The deepest existing ancestor decides: Python resolves links there and
	// keeps the missing tail as written.
	dir := abs
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return abs, !samePath(real, dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, false
		}
		dir = parent
	}
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// isRelativeTo is PurePath.is_relative_to (case-insensitive on Windows).
func isRelativeTo(child, base string) bool {
	rel, err := filepath.Rel(base, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// pyTruthy is bool() of a json.loads value (decoded with UseNumber).
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case json.Number:
		// int(...) / float(...) of the literal: overflow is inf (truthy),
		// underflow is 0.0 (falsy), exactly what ParseFloat reports.
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return f != 0 || math.IsInf(f, 0)
			}
			return true
		}
		return f != 0
	}
	return true
}

// loadJSONObject reads a JSON object the way json.loads would, or reports that
// this port cannot be sure (invalid UTF-8, a BOM, NaN, anything Go rejects).
func loadJSONObject(p string, maxBytes int64) (map[string]any, bool) {
	st, err := os.Stat(p)
	if err != nil || st.Size() > maxBytes {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil || !utf8.Valid(data) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// claudeConfigDir is the CLAUDE_CONFIG_DIR rule shared by run.py and
// runtime_env.claude_home(): an absolute, existing, non-symlink directory
// ("" when unset). ok=false when it is set but not plainly usable: Python then
// expands ~ or warns and falls back, so this port defers to run.py.
func claudeConfigDir() (dir string, ok bool) {
	raw := pyStrip(os.Getenv("CLAUDE_CONFIG_DIR"))
	if raw == "" {
		return "", true
	}
	if strings.HasPrefix(raw, "~") || !filepath.IsAbs(raw) || !isDirFollow(raw) || isSymlink(raw) {
		return "", false
	}
	res, changed := resolvePlain(raw)
	if changed {
		return "", false
	}
	return res, true
}

// ---- run.py checks -------------------------------------------------------------

// pluginDisabledByHost is run.py's _plugin_disabled_by_host.
func pluginDisabledByHost(pluginRoot string) (disabled, sure bool) {
	if pluginRoot == "" {
		return false, true
	}
	meta := filepath.Join(pluginRoot, ".claude-plugin")
	pj, mj := filepath.Join(meta, "plugin.json"), filepath.Join(meta, "marketplace.json")
	if !isFileFollow(pj) || !isFileFollow(mj) {
		return false, true
	}
	const capBytes = 4_000_000
	for _, p := range []string{pj, mj} {
		if st, err := os.Stat(p); err != nil || st.Size() > capBytes {
			return false, true // a stat error or an oversized file reads as "enabled"
		}
	}
	plugin, ok1 := loadJSONObject(pj, capBytes)
	market, ok2 := loadJSONObject(mj, capBytes)
	if !ok1 || !ok2 {
		return false, false
	}
	pn, ok1 := plugin["name"].(string)
	mn, ok2 := market["name"].(string)
	if _, present := plugin["name"]; !present {
		pn, ok1 = "", true
	}
	if _, present := market["name"]; !present {
		mn, ok2 = "", true
	}
	if !ok1 || !ok2 {
		return false, true // .strip() on a non-str raises -> False
	}
	pn, mn = pyStrip(pn), pyStrip(mn)
	if pn == "" || mn == "" {
		return false, true
	}
	settingsPath, sure := claudeSettingsPath()
	if !sure {
		return false, false
	}
	if settingsPath == "" {
		return false, true
	}
	if st, err := os.Stat(settingsPath); err != nil || st.Size() > capBytes {
		return false, true
	}
	settings, ok := loadJSONObject(settingsPath, capBytes)
	if !ok {
		return false, false
	}
	ep, ok := settings["enabledPlugins"].(map[string]any)
	if !ok {
		return false, true
	}
	v, present := ep[pn+"@"+mn]
	b, isBool := v.(bool)
	return present && isBool && !b, true
}

// claudeSettingsPath is run.py's _claude_settings_path ("" = none).
func claudeSettingsPath() (string, bool) {
	dir, ok := claudeConfigDir()
	if !ok {
		return "", false
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		dir = filepath.Join(home, ".claude")
	}
	p := filepath.Join(dir, "settings.json")
	if !isFileFollow(p) || isSymlink(p) {
		return "", true
	}
	return p, true
}

func pluginNameIsCowork(pluginRoot string) (bool, bool) {
	if pluginRoot == "" {
		return false, true
	}
	pj := filepath.Join(pluginRoot, ".claude-plugin", "plugin.json")
	if !isFileFollow(pj) {
		return false, true
	}
	m, ok := loadJSONObject(pj, 4_000_000)
	if !ok {
		return false, false
	}
	name, _ := m["name"].(string)
	return pyStrip(name) == "token-optimizer-cowork", true
}

var safeMarketplaceName = regexp.MustCompile(`^[A-Za-z0-9._-]+\n?$`)

// isSafeSubdir is plugin_env._is_safe_subdir for link-free paths.
func isSafeSubdir(candidate, base string) bool {
	if !isDirFollow(candidate) || isSymlink(candidate) {
		return false
	}
	res, err := resolveStrict(candidate)
	if err != nil {
		return false
	}
	baseRes, _ := resolvePlain(base)
	return !samePath(res, baseRes) && isRelativeTo(res, baseRes)
}

// claudePluginDataIdentity is plugin_env.resolve_claude_plugin_data_env for
// the Claude runtime: the CLAUDE_PLUGIN_DATA dir when installed_plugins.json
// declares it as this plugin's, else "".
func claudePluginDataIdentity(home string) (string, bool) {
	dir, ok := claudeConfigDir()
	if !ok {
		return "", false
	}
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	envVal := os.Getenv("CLAUDE_PLUGIN_DATA")
	if envVal == "" {
		return "", true
	}
	resolved, changed := resolvePlain(envVal)
	if changed {
		return "", false
	}
	base := filepath.Join(dir, "plugins", "data")
	if !isSafeSubdir(resolved, base) {
		return "", true
	}
	registry := filepath.Join(dir, "plugins", "installed_plugins.json")
	if !isFileFollow(registry) {
		return "", true
	}
	st, err := os.Stat(registry)
	if err != nil {
		return "", false
	}
	if st.Size() > 1_048_576 {
		return "", true
	}
	data, err := os.ReadFile(registry)
	if err != nil {
		return "", false
	}
	if !utf8.Valid(data) {
		return "", true // json.load raises UnicodeDecodeError -> None
	}
	reg, ok := loadJSONObject(registry, 1_048_576)
	if !ok {
		var probe any
		if json.Unmarshal(data, &probe) == nil {
			return "", true // valid JSON, not a dict -> no candidates
		}
		return "", false
	}
	plugins := map[string]any{}
	if pv, present := reg["plugins"]; present {
		m, ok := pv.(map[string]any)
		if !ok {
			return "", true
		}
		plugins = m
	}
	for key := range plugins {
		if !strings.HasPrefix(key, "token-optimizer@") {
			continue
		}
		mkt := key[len("token-optimizer@"):]
		if !safeMarketplaceName.MatchString(mkt) {
			continue
		}
		cand := filepath.Join(base, "token-optimizer-"+mkt)
		if !isSafeSubdir(cand, base) {
			continue
		}
		cres, changed := resolvePlain(cand)
		if changed {
			return "", false
		}
		if samePath(cres, resolved) {
			return resolved, true
		}
	}
	return "", true
}

// checkConsent is run.py's _check_consent: allowed, and whether this port is
// sure it decided as run.py would (false defers to run.py).
func checkConsent() (allowed, sure bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false, false
	}
	configPath := ""
	if pd := os.Getenv("CLAUDE_PLUGIN_DATA"); pd != "" {
		res, changed := resolvePlain(pd)
		if changed {
			return false, false
		}
		if !strings.HasPrefix(res, home) {
			if strings.HasPrefix(strings.ToLower(res), strings.ToLower(home)) {
				return false, false
			}
			return true, true
		}
		identity, ok := claudePluginDataIdentity(home)
		if !ok {
			return false, false
		}
		if identity != "" {
			configPath = filepath.Join(identity, "config", "config.json")
		}
	}
	if configPath == "" {
		if os.Getenv("CODEX_HOME") != "" {
			return false, false
		}
		dir, ok := claudeConfigDir()
		if !ok {
			return false, false
		}
		if dir != "" {
			configPath = filepath.Join(dir, "token-optimizer", "config.json")
		} else {
			configPath = filepath.Join(home, ".claude", "token-optimizer", "config.json")
		}
	}
	st, err := os.Lstat(configPath)
	if err != nil || st.Mode()&os.ModeSymlink != 0 {
		return true, true
	}
	if !st.Mode().IsRegular() {
		return true, true // open() fails with an OSError -> fail open
	}
	cfg, ok := loadJSONObject(configPath, 64<<20)
	if !ok {
		return false, false
	}
	if pyTruthy(cfg["enterprise_consent_shown"]) {
		return true, true
	}
	_, hasEnterprise := cfg["enterprise_consent_shown"]
	_, hasV5 := cfg["v5_welcome_shown"]
	if pyTruthy(cfg["v5_welcome_shown"]) && !hasEnterprise {
		return false, false // run.py backfills the flag (writes the file)
	}
	if !hasEnterprise && !hasV5 {
		return true, true
	}
	return false, true
}

// runtimeIsClaude reports whether runtime_env.detect_runtime() returns
// "claude" here, as far as this port can tell without a process scan
// (Windows only: the scan is disabled there).
func runtimeIsClaude(home string) bool {
	ov := strings.ToLower(pyStrip(os.Getenv("TOKEN_OPTIMIZER_RUNTIME")))
	if ov == "claude" {
		return true
	}
	if ov != "" || runtime.GOOS != "windows" {
		return false
	}
	if os.Getenv("CODEX_HOME") != "" {
		return false
	}
	codexPlugins, changed := resolvePlain(filepath.Join(home, ".codex", "plugins"))
	if changed {
		return false
	}
	found := false
	for _, k := range []string{"CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA"} {
		if os.Getenv(k) != "" {
			found = true
		}
		raw := pyStrip(os.Getenv(k))
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "~") {
			return false
		}
		res, changed := resolvePlain(raw)
		if changed || isRelativeTo(res, codexPlugins) {
			return false
		}
	}
	return found
}

// ---- run.py main() -------------------------------------------------------------

func remoteVMEnv() bool {
	switch strings.ToLower(pyStrip(os.Getenv("CLAUDE_CODE_REMOTE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	if pyStrip(os.Getenv("CLAUDE_CODE_REMOTE_SESSION_ID")) != "" || pyStrip(os.Getenv("TOKEN_OPTIMIZER_CLOUD_KIT_VERSION")) != "" {
		return true
	}
	switch strings.ToLower(pyStrip(os.Getenv("TOKEN_OPTIMIZER_REMOTE_HOST"))) {
	case "cloud", "cowork":
		return true
	}
	return false
}

func insideGitWorkTree(p string) bool {
	cur := p
	for i := 0; i < 64; i++ {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
	return false
}

// bytecodePrefix is run.py's _bytecode_prefix_for ("" = none).
func bytecodePrefix(pluginRoot string) string {
	if pyStrip(os.Getenv("PYTHONPYCACHEPREFIX")) != "" {
		return ""
	}
	if !(remoteVMEnv() || insideGitWorkTree(pluginRoot)) {
		return ""
	}
	base := ""
	if runtime.GOOS == "windows" {
		if raw := pyStrip(os.Getenv("LOCALAPPDATA")); raw != "" && filepath.IsAbs(raw) {
			base = raw
		}
	} else if raw := pyStrip(os.Getenv("XDG_CACHE_HOME")); raw != "" && filepath.IsAbs(raw) {
		base = raw
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "token-optimizer", "pycache")
}

func setEnv(env []string, key, value string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == key || (runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+value)
}

func pathParts(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
}

// pyStem is PurePath.stem.
func pyStem(p string) string {
	base := filepath.Base(p)
	if i := strings.LastIndex(base, "."); i > 0 && i < len(base)-1 {
		return base[:i]
	}
	return base
}

// planHook mirrors run.py main() up to the Popen.
func planHook(runPy, scriptRel string, scriptArgs []string) hookPlan {
	medium := hookPlan{kind: planMedium}
	exit := hookPlan{kind: planExit}

	runPyDir, err := resolveStrict(filepath.Dir(runPy))
	if err != nil {
		return medium
	}
	selfRoot := filepath.Dir(runPyDir)
	prRaw := pyStrip(os.Getenv("CLAUDE_PLUGIN_ROOT"))
	prefixRoot := selfRoot
	if prRaw != "" {
		prefixRoot = prRaw
	}
	prefix := bytecodePrefix(prefixRoot)

	if disabled, sure := pluginDisabledByHost(prRaw); !sure {
		return medium
	} else if disabled {
		return exit
	}
	if cowork, sure := pluginNameIsCowork(prRaw); !sure || cowork {
		return medium
	}
	if scriptRel == "hooks/userpromptsubmit_runner.py" && pyStrip(os.Getenv("TOKEN_OPTIMIZER_HOOKS_USERPROMPTSUBMIT")) == "0" {
		return exit
	}
	if strings.ContainsRune(scriptRel, ':') {
		return medium // drive-relative paths: leave them to pathlib
	}
	if filepath.IsAbs(scriptRel) || strings.HasPrefix(scriptRel, "/") || strings.HasPrefix(scriptRel, "\\") {
		return exit
	}
	for _, part := range pathParts(scriptRel) {
		if part == ".." {
			return exit
		}
	}

	stale := prRaw != "" && !isDirFollow(prRaw)
	rootPath := selfRoot
	if prRaw != "" && !stale {
		rootPath = prRaw
	}
	rootResolved, err := resolveStrict(rootPath)
	if err != nil {
		return exit
	}
	candidate := filepath.Join(rootResolved, scriptRel)
	if !isFileFollow(candidate) {
		return exit
	}
	scriptPath, err := resolveStrict(candidate)
	if err != nil || !isRelativeTo(scriptPath, rootResolved) {
		return exit
	}

	// The consent check resolves the plugin data dir through runtime_env,
	// so the runtime must be known (and Claude) before it can be ported.
	home, err := os.UserHomeDir()
	if err != nil || !runtimeIsClaude(home) {
		return medium
	}

	exempt := scriptRel == "hooks/userpromptsubmit_runner.py" || scriptRel == "hooks/sessionstart_runner.py"
	for i, a := range scriptArgs {
		if i >= 2 {
			break
		}
		if a == "ensure-health" || a == "consent" || a == "v5" {
			exempt = true
		}
	}
	if !exempt {
		allowed, sure := checkConsent()
		if !sure {
			return medium
		}
		if !allowed {
			return exit
		}
	}

	if !isDirFollow(filepath.Join(rootResolved, "skills", "token-optimizer", "scripts")) {
		return medium // run.py then exports no runtime
	}

	env := os.Environ()
	env = setEnv(env, "PYTHONUTF8", "1")
	env = setEnv(env, "PYTHONIOENCODING", "utf-8")
	st := pyStem(scriptPath)
	if st == "stop_runner" || (len(scriptArgs) > 0 && scriptArgs[0] == "session-end-flush") {
		env = setEnv(env, "TOKEN_OPTIMIZER_HOOK_PARENT_PID", strconv.Itoa(os.Getppid()))
	}
	if stale {
		env = setEnv(env, "CLAUDE_PLUGIN_ROOT", rootResolved)
	}
	if prefix != "" {
		env = setEnv(env, "PYTHONPYCACHEPREFIX", prefix)
	}
	env = setEnv(env, "TOKEN_OPTIMIZER_RUNTIME", "claude")

	args := append([]string{filepath.Join(runPyDir, "module_runner.py"), filepath.Dir(scriptPath), st}, scriptArgs...)
	return hookPlan{kind: planFast, args: args, env: env}
}

// sourceDigest is native_index.source_digest(): SHA-256 over *.go, go.mod
// and go.sum of native/toidx, by name, line endings normalized.
func sourceDigest(dir string) (string, bool) {
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return "", false
	}
	for i, n := range names {
		names[i] = filepath.Base(n)
	}
	names = append(names, "go.mod", "go.sum")
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return "", false
		}
		h.Write([]byte(n + "\x00"))
		h.Write(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// binaryMatchesSources: a release copy installed by native_index carries a
// "<exe>.digest" marker; it may only run while the plugin's Go sources are
// the ones it was built from. A local build has no marker.
func binaryMatchesSources() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	want, err := os.ReadFile(exe + ".digest")
	if err != nil {
		return os.IsNotExist(err)
	}
	got, ok := sourceDigest(filepath.Join(filepath.Dir(exe), "..", "toidx"))
	return ok && strings.TrimSpace(string(want)) == got
}

// runHook: args = run.py path, script_rel, script args.
func runHook(args []string) int {
	if len(args) < 1 || !hookPlatformSupported() || !binaryMatchesSources() {
		return hookNotHandled
	}
	interp, ok := hookInterpreter()
	if !ok {
		return hookNotHandled
	}
	if len(args) < 2 {
		return 0 // run.py: nothing to dispatch
	}
	plan := planHook(args[0], args[1], args[2:])
	if trace := os.Getenv("TOIDX_HOOK_TRACE"); trace != "" {
		// Test hook (native/tests/hook_equivalence.py): which path was taken.
		_ = os.WriteFile(trace, []byte([]string{"medium", "exit", "fast"}[plan.kind]+"\n"), 0o600)
	}
	switch plan.kind {
	case planExit:
		return 0
	case planFast:
		if _, err := spawnWait(interp, plan.args, plan.env, 120); err != nil {
			return hookNotHandled // never started: the launcher's path takes over
		}
		return 0
	}
	code, err := spawnWait(interp, args, os.Environ(), 0)
	if err != nil {
		return hookNotHandled
	}
	return code
}
