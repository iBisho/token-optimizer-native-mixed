package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakePlugin builds a plugin root with hooks/run.py, a target runner, the
// manifests and a scripts dir, and a clean home. Returns (root, home).
func fakePlugin(t *testing.T) (string, string) {
	t.Helper()
	// Long form: CI runners put TEMP under an 8.3 short name
	// (C:\Users\RUNNER~1\...), which hook.go treats as "path changes when
	// resolved" and defers to run.py.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "plugin")
	home := filepath.Join(base, "home")
	for _, d := range []string{
		filepath.Join(root, "hooks"),
		filepath.Join(root, ".claude-plugin"),
		filepath.Join(root, "skills", "token-optimizer", "scripts"),
		filepath.Join(home, ".claude", "token-optimizer"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "hooks", "run.py"), "")
	write(t, filepath.Join(root, "hooks", "probe_runner.py"), "")
	write(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name": "token-optimizer"}`)
	write(t, filepath.Join(root, ".claude-plugin", "marketplace.json"), `{"name": "mkt"}`)
	for _, k := range []string{"TOKEN_OPTIMIZER_RUNTIME", "CLAUDE_PLUGIN_DATA", "CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"PYTHONPYCACHEPREFIX", "TOKEN_OPTIMIZER_HOOKS_USERPROMPTSUBMIT", "CLAUDE_CODE_REMOTE"} {
		t.Setenv(k, "")
	}
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_PLUGIN_ROOT", root)
	return root, home
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func plan(root string, rel string, args ...string) hookPlan {
	return planHook(filepath.Join(root, "hooks", "run.py"), rel, args)
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

func onlyWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("runtime detection is ported for Windows only")
	}
}

func TestFastPathWithoutConfig(t *testing.T) {
	onlyWindows(t)
	root, _ := fakePlugin(t)
	p := plan(root, "hooks/probe_runner.py", "--x")
	if p.kind != planFast {
		t.Fatalf("kind = %v, want fast", p.kind)
	}
	if got := filepath.Base(p.args[0]); got != "module_runner.py" || p.args[2] != "probe_runner" || p.args[3] != "--x" {
		t.Fatalf("args = %v", p.args)
	}
	for k, want := range map[string]string{"PYTHONUTF8": "1", "PYTHONIOENCODING": "utf-8", "TOKEN_OPTIMIZER_RUNTIME": "claude"} {
		if v, _ := envValue(p.env, k); v != want {
			t.Fatalf("%s = %q, want %q", k, v, want)
		}
	}
	if _, ok := envValue(p.env, "TOKEN_OPTIMIZER_HOOK_PARENT_PID"); ok {
		t.Fatal("parent pid only for stop_runner / session-end-flush")
	}
}

func TestConsentStates(t *testing.T) {
	onlyWindows(t)
	cases := []struct {
		cfg  string
		want hookPlanKind
	}{
		{`{"enterprise_consent_shown": true}`, planFast},
		{`{"enterprise_consent_shown": 1}`, planFast},
		{`{"enterprise_consent_shown": false, "v5_welcome_shown": true}`, planExit},
		{`{"enterprise_consent_shown": 0}`, planExit},
		{`{"v5_welcome_shown": false}`, planExit},
		{`{"other": 1}`, planFast},
		{`{"v5_welcome_shown": true}`, planMedium}, // run.py backfills (writes)
		{`[1]`, planMedium},
		{`{"a": NaN}`, planMedium},
		{"\ufeff{}", planMedium},
		{`not json`, planMedium},
	}
	for _, c := range cases {
		root, home := fakePlugin(t)
		write(t, filepath.Join(home, ".claude", "token-optimizer", "config.json"), c.cfg)
		if got := plan(root, "hooks/probe_runner.py").kind; got != c.want {
			t.Errorf("config %s: kind = %v, want %v", c.cfg, got, c.want)
		}
		// An exempt argument runs whatever the consent.
		if got := plan(root, "hooks/probe_runner.py", "ensure-health").kind; c.want == planExit && got != planFast {
			t.Errorf("config %s: exempt arg kind = %v, want fast", c.cfg, got)
		}
	}
}

func TestDisabledByHost(t *testing.T) {
	onlyWindows(t)
	root, home := fakePlugin(t)
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"enabledPlugins": {"token-optimizer@mkt": false}}`)
	if got := plan(root, "hooks/probe_runner.py").kind; got != planExit {
		t.Fatalf("disabled: kind = %v", got)
	}
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"enabledPlugins": {"token-optimizer@mkt": 0}}`)
	if got := plan(root, "hooks/probe_runner.py").kind; got != planFast {
		t.Fatalf("0 is not False: kind = %v", got)
	}
}

func TestTargetPathRules(t *testing.T) {
	onlyWindows(t)
	root, _ := fakePlugin(t)
	for _, rel := range []string{"../x.py", "hooks/../../x.py", "hooks/missing.py"} {
		if got := plan(root, rel).kind; got != planExit {
			t.Errorf("%s: kind = %v, want exit", rel, got)
		}
	}
	if got := plan(root, "C:x.py").kind; got != planMedium {
		t.Errorf("drive-relative: kind = %v, want medium", got)
	}
}

func TestOtherRuntimesDefer(t *testing.T) {
	onlyWindows(t)
	root, _ := fakePlugin(t)
	t.Setenv("TOKEN_OPTIMIZER_RUNTIME", "codex")
	if got := plan(root, "hooks/probe_runner.py").kind; got != planMedium {
		t.Fatalf("codex override: kind = %v", got)
	}
	t.Setenv("TOKEN_OPTIMIZER_RUNTIME", "")
	t.Setenv("CODEX_HOME", "x")
	if got := plan(root, "hooks/probe_runner.py").kind; got != planMedium {
		t.Fatalf("CODEX_HOME: kind = %v", got)
	}
}

func TestPromptHookOptOutAndStaleRoot(t *testing.T) {
	onlyWindows(t)
	root, _ := fakePlugin(t)
	write(t, filepath.Join(root, "hooks", "userpromptsubmit_runner.py"), "")
	t.Setenv("TOKEN_OPTIMIZER_HOOKS_USERPROMPTSUBMIT", " 0 ")
	if got := plan(root, "hooks/userpromptsubmit_runner.py").kind; got != planExit {
		t.Fatalf("opt-out: kind = %v", got)
	}
	t.Setenv("TOKEN_OPTIMIZER_HOOKS_USERPROMPTSUBMIT", "")
	t.Setenv("CLAUDE_PLUGIN_ROOT", filepath.Join(root, "gone"))
	p := plan(root, "hooks/probe_runner.py", "session-end-flush")
	if p.kind != planFast {
		t.Fatalf("stale root: kind = %v", p.kind)
	}
	if v, _ := envValue(p.env, "CLAUDE_PLUGIN_ROOT"); !samePath(v, root) {
		t.Fatalf("stale root not replaced: %q", v)
	}
	if _, ok := envValue(p.env, "TOKEN_OPTIMIZER_HOOK_PARENT_PID"); !ok {
		t.Fatal("session-end-flush needs the parent pid")
	}
}

func TestPluginDataIdentity(t *testing.T) {
	onlyWindows(t)
	root, home := fakePlugin(t)
	data := filepath.Join(home, ".claude", "plugins", "data", "token-optimizer-mkt")
	if err := os.MkdirAll(filepath.Join(data, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_DATA", data)
	write(t, filepath.Join(data, "config", "config.json"), `{"enterprise_consent_shown": false, "v5_welcome_shown": false}`)
	// Not registered yet: legacy ~/.claude config (absent) -> consent assumed.
	if got := plan(root, "hooks/probe_runner.py").kind; got != planFast {
		t.Fatalf("unregistered: kind = %v", got)
	}
	write(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), `{"plugins": {"token-optimizer@mkt": []}}`)
	if got := plan(root, "hooks/probe_runner.py").kind; got != planExit {
		t.Fatalf("registered + opted out: kind = %v", got)
	}
}

func TestInterpreterRecord(t *testing.T) {
	onlyWindows(t)
	_, home := fakePlugin(t)
	t.Setenv("PATH", `C:\x;C:\y`)
	t.Setenv("TOKEN_OPTIMIZER_PYTHON", "")
	if _, ok := hookInterpreter(); ok {
		t.Fatal("no record yet")
	}
	p, _ := hookRecordPath()
	if !strings.HasPrefix(p, home) {
		t.Fatalf("record outside home: %s", p)
	}
	interp := filepath.Join(home, "python.exe")
	write(t, interp, "MZ")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, p, "v1\n"+interp+"\n")
	if got, ok := hookInterpreter(); !ok || got != interp {
		t.Fatalf("record = %q %v", got, ok)
	}
	t.Setenv("PATH", `C:\other`)
	if _, ok := hookInterpreter(); ok {
		t.Fatal("a PATH change must miss")
	}
}

func TestSourceDigestMatchesPythonRule(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "b.go"), "package b\r\n")
	write(t, filepath.Join(dir, "a.go"), "package a\n")
	write(t, filepath.Join(dir, "go.mod"), "module m\n")
	write(t, filepath.Join(dir, "go.sum"), "")
	// Equality with native_index.source_digest() is checked in CI
	// (.github/workflows/native.yml); here only the normalization.
	got, ok := sourceDigest(dir)
	if !ok || len(got) != 64 {
		t.Fatalf("digest = %q %v", got, ok)
	}
	again, _ := sourceDigest(dir)
	write(t, filepath.Join(dir, "b.go"), "package b\n")
	same, _ := sourceDigest(dir)
	if again != got || same != got {
		t.Fatal("line endings must not change the digest")
	}
}
