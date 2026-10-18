package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// fakeCodex stands in for `codex exec --json`: it records argv, stdin and the job marker and prints
// codex's JSON events. FAKE_CODEX_* words in the prompt pick other endings. As `codex mcp list --json`
// it records its argv and working dir in FAKE_CODEX_MCP_LOG (if set) and prints FAKE_CODEX_MCP_OUT
// (default []) after FAKE_CODEX_MCP_SLEEP seconds, exiting with FAKE_CODEX_MCP_EXIT.
const fakeCodex = `#!/bin/bash
set -u
if [[ ${1-} == mcp ]]; then
  if [[ -n ${FAKE_CODEX_MCP_LOG-} ]]; then printf '%s\n' "$@" "$PWD" >> "$FAKE_CODEX_MCP_LOG"; fi
  if [[ -n ${FAKE_CODEX_MCP_SLEEP-} ]]; then sleep "$FAKE_CODEX_MCP_SLEEP"; fi
  printf '%s\n' "${FAKE_CODEX_MCP_OUT-[]}"
  exit "${FAKE_CODEX_MCP_EXIT-0}"
fi
prompt=$(cat)
printf '%s\n' "$@" > ../argv.txt
printf '%s' "$prompt" > ../stdin.txt
printf '%s\n' "${AGENT_JOBS_JOB-unset}" > ../marker.txt
echo '{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}'
echo '{"type":"turn.started"}'
if [[ $prompt =~ FAKE_CODEX_SLEEP=([0-9]+) ]]; then sleep "${BASH_REMATCH[1]}"; fi
echo '{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"thinking"}}'
echo '{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"ls","status":"in_progress"}}'
echo '{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"ls","exit_code":0,"status":"completed"}}'
if [[ $prompt == *FAKE_CODEX_FAIL* ]]; then
  echo '{"type":"turn.failed","error":{"message":"model overloaded"}}'
  exit 1
fi
if [[ $prompt == *FAKE_CODEX_NOTERMINAL* ]]; then
  echo '{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"## Summary\npartial"}}'
  exit 0
fi
if [[ $prompt != *FAKE_CODEX_NOREPORT* ]]; then
  echo '{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"## Summary\ncodex ok\n\n## Findings / Changes\n-\n\n## Evidence\n-\n\n## Not done / uncertain\nnothing"}}'
fi
echo '{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":90}}'
`

// newCodexManager is newManager with the fake codex first on PATH (and named as the codex binary).
func newCodexManager(t *testing.T) *Manager {
	t.Helper()
	m := newManager(t)
	t.Setenv("PATH", fakeHelpr+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_JOBS_CODEX_BIN", filepath.Join(fakeHelpr, "codex"))
	// Tests never read the host's /etc/codex: a missing dir by default.
	prevDir := codexSystemConfigDir
	codexSystemConfigDir = filepath.Join(t.TempDir(), "no-etc-codex")
	t.Cleanup(func() { codexSystemConfigDir = prevDir })
	return m
}

func codexReview(r *StartRequest) { r.Kind, r.Model = "review", "gpt-6.1-sol" }

func TestCodexReadOnlyReviewWritesReportFromStream(t *testing.T) {
	m := newCodexManager(t)
	j := enqueue(t, m, "review the thing", codexReview)
	if j.Runner != store.RunnerCodex || j.ReasoningEffort != DefaultReasoningEffort {
		t.Fatalf("runner %q effort %q", j.Runner, j.ReasoningEffort)
	}
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if j.Turns != 2 || j.CostUSD != 0 || !j.CostUnknown || j.MaxTurns != 0 || j.MaxBudgetUSD != 0 {
		t.Fatalf("turns=%d cost=%v unknown=%v limits=%d/%v", j.Turns, j.CostUSD, j.CostUnknown, j.MaxTurns, j.MaxBudgetUSD)
	}
	dir := m.JobDir(j.ID)
	rep, _ := os.ReadFile(filepath.Join(dir, "report.md"))
	if !strings.Contains(string(rep), "codex ok") || !strings.Contains(string(rep), "## Not done / uncertain") {
		t.Fatalf("report.md: %q", rep)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv.txt"))
	want := "exec\n--json\n--ignore-user-config\n--ignore-rules\n--ephemeral\n--skip-git-repo-check\n" +
		"-s\nread-only\n-c\napproval_policy=\"never\"\n-c\nmcp_servers={}\n" +
		"--disable\napps\n--disable\nbrowser_use\n--disable\nbrowser_use_external\n" +
		"--disable\nbrowser_use_full_cdp_access\n--disable\ncomputer_use\n--disable\nimage_generation\n" +
		"--disable\nmulti_agent\n--disable\nplugins\n--disable\nplugin_sharing\n--disable\nremote_plugin\n" +
		"--disable\nskill_mcp_dependency_install\n--disable\ndaemon_auto_start\n" +
		"--disable\nin_app_local_automation\n--disable\ngoals\n--disable\nrealtime_conversation\n" +
		"--disable\nworktrees\n--disable\nhooks\n" +
		"-m\ngpt-6.1-sol\n-c\nmodel_reasoning_effort=high\n-\n"
	if string(argv) != want {
		t.Fatalf("codex argv:\n%s\nwant:\n%s", argv, want)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin.txt"))
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
	if len(prompt) == 0 || string(stdin) != strings.TrimRight(string(prompt), "\n") {
		t.Fatal("codex must get prompt.md on stdin")
	}
	if marker, _ := os.ReadFile(filepath.Join(dir, "marker.txt")); strings.TrimSpace(string(marker)) != j.ID {
		t.Fatalf("job marker %q", marker)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "stderr.log")); !strings.Contains(string(log), "input 1200 (cached 800), output 90 tokens") {
		t.Fatalf("usage not logged: %s", log)
	}
	// Unknown cost and the absent turn/budget caps are said as such, never as $0.00 or a number.
	if line := DoneLine(j); !strings.Contains(line, "· cost unknown ·") || strings.Contains(line, "$0.00") {
		t.Fatalf("done line %q", line)
	}
	if c := CostText(j.CostUSD, j.CostUnknown); c != "unknown" {
		t.Fatalf("cost text %q", c)
	}
	if l := LimitsText(j); l != "n/a (codex) turns · n/a (codex) budget · 45m" {
		t.Fatalf("limits text %q", l)
	}
	claude := &store.Job{Runner: store.RunnerClaude, MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45}
	if l, c := LimitsText(claude), CostText(1.234, false); l != "80 turns · $10 · 45m" || c != "$1.23" {
		t.Fatalf("claude limits %q cost %q", l, c)
	}
}

// TestCodexWithoutTerminalEventFails: exit 0 and an agent message, but no turn.completed (the stream
// ended early): the job fails and the partial message does not become the report.
func TestCodexWithoutTerminalEventFails(t *testing.T) {
	m := newCodexManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_NOTERMINAL", codexReview).ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "turn.completed") {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if hasReport(m.JobDir(j.ID)) {
		t.Fatal("a partial agent message must not become report.md")
	}
}

func TestCodexOwnReportKept(t *testing.T) {
	m := newCodexManager(t)
	j := enqueue(t, m, "FAKE_CODEX_SLEEP=2", codexReview)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	// A report.md the job wrote itself wins over the final message.
	if err := os.WriteFile(filepath.Join(m.JobDir(j.ID), "report.md"), []byte("## Summary\nown\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j = runToEnd(t, m, j.ID)
	rep, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "report.md"))
	if j.Status != store.StatusSucceeded || string(rep) != "## Summary\nown\n" {
		t.Fatalf("got %s %q, report %q", j.Status, j.Error, rep)
	}
}

func TestCodexWithoutReportFails(t *testing.T) {
	m := newCodexManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_NOREPORT", codexReview).ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "without a report") {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	j = runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_FAIL", codexReview).ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "codex exited with code 1") {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	// Resume of a codex job is refused with a clear error.
	if _, err := m.Resume(j.ID, "try again", Limits{}); !errors.Is(err, errCodexResume) {
		t.Fatalf("resume of a codex job: %v", err)
	}
}

func TestCodexTimeoutKills(t *testing.T) {
	m := newCodexManager(t)
	j := enqueue(t, m, "FAKE_CODEX_SLEEP=60", func(r *StartRequest) { codexReview(r); r.TimeoutMinutes = 1 })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusRunning {
		t.Fatalf("job not running: %s", j.Status)
	}
	waitFor(t, "fake codex", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
		return err == nil
	})
	real := time.Now
	m.Now = func() time.Time { return real().Add(2 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusTimeout || !j.CostUnknown {
		t.Fatalf("status %s unknown=%v, want timeout with cost unknown", j.Status, j.CostUnknown)
	}
	waitFor(t, "process group gone", 5*time.Second, func() bool { return !groupAlive(j.PGID) })
}

func TestCodexCancelKills(t *testing.T) {
	m := newCodexManager(t)
	j := enqueue(t, m, "FAKE_CODEX_SLEEP=60", codexReview)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fake codex", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
		return err == nil
	})
	j = get(t, m, j.ID)
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusCancelled || !got.CostUnknown {
		t.Fatalf("status %s unknown=%v", got.Status, got.CostUnknown)
	}
	waitFor(t, "process group gone", 5*time.Second, func() bool { return !groupAlive(j.PGID) })
}

func TestCodexWriteJobRefusedByDefault(t *testing.T) {
	m := newCodexManager(t)
	for _, kind := range []string{"fix", "general"} {
		_, err := m.Enqueue(StartRequest{Title: "t", Kind: kind, Brief: "b", Model: "gpt-6.1-sol"})
		if err == nil || !strings.Contains(err.Error(), "codex_write_allowed") {
			t.Fatalf("%s codex job: %v", kind, err)
		}
	}
	// A queued write job is refused at launch when the config on disk no longer allows it: the launch
	// reloads config.json, the daemon's startup snapshot does not authorize it.
	writeCodexConfig(t, m, `{"codex_write_allowed":true}`)
	m.Cfg.CodexWriteAllowed = true
	j := enqueue(t, m, "fix it", func(r *StartRequest) { r.Kind, r.Model = "fix", "gpt-6.1-sol" })
	writeCodexConfig(t, m, `{"codex_write_allowed":false}`)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusFailed || !strings.Contains(got.Error, "codex_write_allowed") {
		t.Fatalf("got %s %q", got.Status, got.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt")); err == nil {
		t.Fatal("refused job must not run codex")
	}
	// A config that does not load refuses the launch too (read-only jobs included).
	writeCodexConfig(t, m, `{"codex_write_allowed":true}`)
	j = enqueue(t, m, "fix it", func(r *StartRequest) { r.Kind, r.Model = "fix", "gpt-6.1-sol" })
	ro := enqueue(t, m, "review it", codexReview)
	writeCodexConfig(t, m, `{"codex_reasoning_effort":"max"}`)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{j.ID, ro.ID} {
		if got := get(t, m, id); got.Status != store.StatusFailed || !strings.Contains(got.Error, "config") {
			t.Fatalf("invalid config: got %s %q", got.Status, got.Error)
		}
	}
	// Allowed on disk: workspace-write sandbox.
	writeCodexConfig(t, m, `{"codex_write_allowed":true}`)
	j = runToEnd(t, m, enqueue(t, m, "fix it", func(r *StartRequest) { r.Kind, r.Model = "fix", "gpt-6.1-sol" }).ID)
	argv, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "argv.txt"))
	if j.Status != store.StatusSucceeded || !strings.Contains(string(argv), "-s\nworkspace-write\n") {
		t.Fatalf("got %s %q argv %q", j.Status, j.Error, argv)
	}
}

// Before every codex launch the daemon runs `<codex bin> mcp list --json` in the job's work dir: only
// an empty JSON array lets the job run; MCP servers (listed by name), garbage, an exit code or a hang
// refuse it before codex exec starts (DEVIATIONS #5).
func TestCodexRefusedWithInheritedMCP(t *testing.T) {
	m := newCodexManager(t)
	log := filepath.Join(t.TempDir(), "mcp.log")
	t.Setenv("FAKE_CODEX_MCP_LOG", log)
	j := runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("mcp list [] must launch: %s %q", j.Status, j.Error)
	}
	calls, _ := os.ReadFile(log)
	if want := "mcp\nlist\n--json\n" + filepath.Join(m.JobDir(j.ID), "work") + "\n"; string(calls) != want {
		t.Fatalf("mcp check calls:\n%s\nwant:\n%s", calls, want)
	}
	defer func(d time.Duration) { codexMCPTimeout = d }(codexMCPTimeout)
	codexMCPTimeout = time.Second
	for _, c := range []struct {
		name, out, exit, sleep, want string
	}{
		{"servers", `[{"name":"github","enabled":true},{"name":"brain","enabled":false}]`, "0", "",
			"codex has MCP servers configured: github, brain; codex jobs refuse to run with inherited MCP servers"},
		{"unnamed", `[{}]`, "0", "", "codex has MCP servers configured: (unnamed)"},
		{"garbage", `no servers`, "0", "", "codex MCP check"},
		{"trailing", `[] []`, "0", "", "codex MCP check"},
		{"exit", `[]`, "1", "", "codex MCP check"},
		{"timeout", `[]`, "0", "5", "timed out"},
	} {
		t.Setenv("FAKE_CODEX_MCP_OUT", c.out)
		t.Setenv("FAKE_CODEX_MCP_EXIT", c.exit)
		t.Setenv("FAKE_CODEX_MCP_SLEEP", c.sleep)
		start := time.Now()
		j := enqueue(t, m, "review it", codexReview)
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		got := get(t, m, j.ID)
		if got.Status != store.StatusFailed || !strings.Contains(got.Error, c.want) {
			t.Fatalf("%s: got %s %q, want failed with %q", c.name, got.Status, got.Error, c.want)
		}
		if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt")); err == nil {
			t.Fatalf("%s: refused job must not run codex exec", c.name)
		}
		if d := time.Since(start); d > 4*time.Second {
			t.Fatalf("%s: the check took %s", c.name, d)
		}
	}
}

// A system or managed codex config layer in /etc/codex is applied after the job's CLI overrides and can
// turn `apps` back on (its codex_apps server is not in `mcp list`), so a codex launch is refused while
// that directory holds anything; a missing or empty one launches (DEVIATIONS #5).
func TestCodexRefusedWithSystemConfig(t *testing.T) {
	m := newCodexManager(t)
	etc := filepath.Join(t.TempDir(), "etc-codex")
	codexSystemConfigDir = etc
	if j := runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID); j.Status != store.StatusSucceeded {
		t.Fatalf("missing dir must launch: %s %q", j.Status, j.Error)
	}
	if err := os.Mkdir(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	if j := runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID); j.Status != store.StatusSucceeded {
		t.Fatalf("empty dir must launch: %s %q", j.Status, j.Error)
	}
	for _, c := range []struct{ file, want string }{
		{"managed_config.toml", "(managed_config.toml)"},
		{"config.toml", "(config.toml, managed_config.toml)"},
		{"requirements.toml", "(config.toml, managed_config.toml, requirements.toml)"},
	} {
		if err := os.WriteFile(filepath.Join(etc, c.file), []byte("[features]\napps = true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		j := enqueue(t, m, "review it", codexReview)
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		want := "codex has system or managed configuration in " + etc + " " + c.want +
			"; codex jobs refuse to run with it"
		if got := get(t, m, j.ID); got.Status != store.StatusFailed || !strings.Contains(got.Error, want) {
			t.Fatalf("%s: got %s %q, want failed with %q", c.file, got.Status, got.Error, want)
		}
		if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt")); err == nil {
			t.Fatalf("%s: refused job must not run codex exec", c.file)
		}
	}
}

// writeCodexConfig rewrites the manager's config.json on disk (the daemon's m.Cfg is left as is).
func writeCodexConfig(t *testing.T, m *Manager, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRouting(t *testing.T) {
	for _, c := range []struct {
		runner, model, want string
		ok                  bool
	}{
		{"", "gpt-6.1-sol", store.RunnerCodex, true},
		{"", "GPT-6-astra", store.RunnerCodex, true},
		{"", "opus", store.RunnerClaude, true},
		{"codex", "gpt-6-astra", store.RunnerCodex, true},
		{"claude", "sonnet", store.RunnerClaude, true},
		{"codex", "opus", "", false},
		{"claude", "gpt-6.1-sol", "", false},
		{"gemini", "x", "", false},
	} {
		got, err := ResolveRunner(c.runner, c.model)
		if (err == nil) != c.ok || got != c.want {
			t.Fatalf("ResolveRunner(%q, %q) = %q, %v", c.runner, c.model, got, err)
		}
	}
	m := newCodexManager(t)
	// Defaults are unchanged: review/hard stays on claude.
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.Model, r.Complexity = "review", "", "hard" })
	if j.Runner != store.RunnerClaude || j.Model != "fable" || j.ReasoningEffort != "" {
		t.Fatalf("default review/hard: %s %s %q", j.Runner, j.Model, j.ReasoningEffort)
	}
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "review", Brief: "b", Complexity: "hard", Runner: "codex"}); err == nil {
		t.Fatal("runner codex without a configured codex model must be rejected")
	}
	// A configured cell routes to codex; the config's effort applies.
	m.Cfg.Models = map[string]map[string]string{"review": {"hard": "gpt-6-astra"}}
	m.Cfg.CodexReasoningEffort = "medium"
	j = enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.Model, r.Complexity = "review", "", "hard" })
	if j.Runner != store.RunnerCodex || j.Model != "gpt-6-astra" || j.ReasoningEffort != "medium" {
		t.Fatalf("configured review/hard: %s %s %q", j.Runner, j.Model, j.ReasoningEffort)
	}
	j = enqueue(t, m, "x", func(r *StartRequest) { codexReview(r); r.ReasoningEffort = "low" })
	if j.ReasoningEffort != "low" {
		t.Fatalf("job effort %q", j.ReasoningEffort)
	}
	for name, r := range map[string]StartRequest{
		"effort on claude": {Model: "opus", ReasoningEffort: "high"},
		"bad effort":       {Model: "gpt-6.1-sol", ReasoningEffort: "max"},
		"turn cap":         {Model: "gpt-6.1-sol", MaxTurns: 10},
		"budget cap":       {Model: "gpt-6.1-sol", MaxBudgetUSD: 5},
	} {
		r.Title, r.Kind, r.Brief = "t", "review", "b"
		if _, err := m.Enqueue(r); err == nil {
			t.Fatalf("%s: want rejection", name)
		}
	}
	t.Setenv("AGENT_JOBS_CODEX_BIN", "")
	if b := (Config{}).codexBin(); b != "codex" {
		t.Fatalf("default codex binary %q", b)
	}
}

func TestCodexConfigValidated(t *testing.T) {
	for name, body := range map[string]string{
		"effort":     `{"codex_reasoning_effort":"max"}`,
		"kind":       `{"models":{"deploy":{"hard":"gpt-6.1-sol"}}}`,
		"complexity": `{"models":{"review":{"extreme":"gpt-6.1-sol"}}}`,
		"model":      `{"models":{"review":{"hard":"-x"}}}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(dir); err == nil {
			t.Fatalf("%s: config %s must be rejected", name, body)
		}
	}
}

func TestParseCodexStreamUsage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stream.jsonl")
	lines := []string{
		`{"type":"thread.started","thread_id":"t"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"i0","type":"reasoning","text":"r"}}`,
		`{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"first"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10}}`,
		`not json`,
		`{"type":"error","message":"Reconnecting... 1/5"}`,
		`{"type":"item.completed","item":{"id":"i2","type":"file_change","changes":[]}}`,
		`{"type":"item.completed","item":{"id":"i3","type":"agent_message","text":"## Summary\nlast"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":60,"output_tokens":20}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := ParseCodexStream(p)
	if !r.Found || r.IsError || r.Subtype != "success" || r.Text != "## Summary\nlast" || r.Turns != 3 {
		t.Fatalf("parsed %+v", r)
	}
	if r.InputTokens != 300 || r.CachedInputTokens != 100 || r.OutputTokens != 30 || r.CostUSD != 0 {
		t.Fatalf("usage %+v", r)
	}
	if err := os.WriteFile(p, []byte(`{"type":"turn.failed","error":{"message":"quota"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := ParseCodexStream(p); !r.Found || !r.IsError || r.Subtype != "turn_failed: quota" {
		t.Fatalf("failed turn %+v", r)
	}
	if r := ParseCodexStream(filepath.Join(t.TempDir(), "missing")); r.Found || r.Runner != store.RunnerCodex {
		t.Fatalf("missing stream %+v", r)
	}
}

// TestParseCodexStreamRealSample parses a stream recorded from codex-cli 0.162.1 on the host: an
// "error" item is a warning (unrecognized config settings), not a turn.
func TestParseCodexStreamRealSample(t *testing.T) {
	r := ParseCodexStream(filepath.Join("testdata", "codex-0.162.1-stream.jsonl"))
	if !r.Found || r.IsError || r.Subtype != "success" || r.Text != "OK" || r.Turns != 1 {
		t.Fatalf("parsed %+v", r)
	}
	if r.InputTokens != 18525 || r.CachedInputTokens != 12288 || r.OutputTokens != 5 || r.CostUSD != 0 {
		t.Fatalf("usage %+v", r)
	}
}

// TestParseCodexStreamStrict: a line over the limit is skipped and counted (the messages after it still
// count), a stream without a terminal turn.completed is not a success, and a later turn that started but
// never ended voids the earlier completion.
func TestParseCodexStreamStrict(t *testing.T) {
	old := codexMaxLineBytes
	codexMaxLineBytes = 1 << 10
	t.Cleanup(func() { codexMaxLineBytes = old })
	p := filepath.Join(t.TempDir(), "stream.jsonl")
	write := func(lines ...string) StreamResult {
		t.Helper()
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		return ParseCodexStream(p)
	}
	huge := `{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"` + strings.Repeat("x", 8<<10) + `"}}`
	first := `{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`
	last := `{"type":"item.completed","item":{"type":"agent_message","text":"last"}}`
	done := `{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}`
	r := write(`{"type":"turn.started"}`, first, huge, last, done+"\n")
	if !r.Found || r.IsError || r.Text != "last" || r.Skipped != 1 || r.Err != "" || r.Turns != 2 {
		t.Fatalf("oversized line between messages: %+v", r)
	}
	if st, msg := Verdict(0, r, t.TempDir()); st != store.StatusFailed || msg != "exit 0 without a report" {
		t.Fatalf("verdict %s %q", st, msg)
	}
	// Missing turn.completed with exit 0.
	r = write(`{"type":"turn.started"}`, first+"\n")
	if r.Found || r.Text != "first" {
		t.Fatalf("no terminal event: %+v", r)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("## Summary\nx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, msg := Verdict(0, r, dir); st != store.StatusFailed || !strings.Contains(msg, "turn.completed") {
		t.Fatalf("verdict without terminal event: %s %q", st, msg)
	}
	// Truncated stream: a later turn started and its last line is cut.
	r = write(`{"type":"turn.started"}`, first, done, `{"type":"turn.started"}`, `{"type":"item.completed","item":{"type":"agent_mess`)
	if r.Found {
		t.Fatalf("truncated stream: %+v", r)
	}
	if st, _ := Verdict(0, r, dir); st != store.StatusFailed {
		t.Fatalf("truncated stream verdict %s", st)
	}
	// The failed turn is terminal but an error.
	r = write(`{"type":"turn.started"}`, `{"type":"turn.failed","error":{"message":"quota"}}`)
	if st, msg := Verdict(0, r, dir); st != store.StatusFailed || !strings.Contains(msg, "quota") {
		t.Fatalf("failed turn verdict %s %q", st, msg)
	}
	// A read error is propagated (a directory is not a stream).
	if r := ParseCodexStream(t.TempDir()); r.Err == "" {
		t.Fatalf("read error not propagated: %+v", r)
	} else if st, msg := Verdict(0, r, dir); st != store.StatusFailed || !strings.Contains(msg, "stream") {
		t.Fatalf("read error verdict %s %q", st, msg)
	}
}
