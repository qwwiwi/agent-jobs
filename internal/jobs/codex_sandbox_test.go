package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// argSandbox is the `-s` value of a codex command line ("" = none).
func argSandbox(args []string) string {
	if i := slices.Index(args, "-s"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// forceLinuxProbe makes the probe run whatever the test host's OS (the fake codex answers it).
func forceLinuxProbe(t *testing.T) {
	t.Helper()
	prev := codexProbeGOOS
	codexProbeGOOS = "linux"
	t.Cleanup(func() { codexProbeGOOS = prev })
}

// The codex sandbox of a launch per codex_sandbox mode and kind (issue #81): auto and native take the
// kind's native sandbox, worktree-full-access danger-full-access; auto probes once (cached) and refuses
// when the probe shows bwrap cannot start, native never probes or refuses.
func TestCodexLaunchSandboxPerMode(t *testing.T) {
	forceLinuxProbe(t)
	m := newCodexManager(t)
	probes := filepath.Join(t.TempDir(), "probe.log")
	t.Setenv("FAKE_CODEX_SANDBOX_LOG", probes)
	for _, c := range []struct{ mode, kind, want string }{
		{"", "review", CodexSandboxReadOnly},
		{"auto", "research", CodexSandboxReadOnly},
		{"auto", "fix", CodexSandboxWrite},
		{"native", "review", CodexSandboxReadOnly},
		{"native", "general", CodexSandboxWrite},
		{"worktree-full-access", "review", CodexSandboxFullAccess},
		{"worktree-full-access", "fix", CodexSandboxFullAccess},
	} {
		got, err := m.codexLaunchSandbox(Config{CodexSandboxMode: c.mode}, c.kind)
		if err != nil || got != c.want {
			t.Fatalf("%q/%s: %q, %v; want %q", c.mode, c.kind, got, err, c.want)
		}
		j := &store.Job{Kind: c.kind, Model: "gpt-6.1-sol"}
		if s := argSandbox(CodexArgs(j, "high", got)); s != c.want {
			t.Fatalf("%q/%s: codex argv -s %q, want %q", c.mode, c.kind, s, c.want)
		}
	}
	if calls, _ := os.ReadFile(probes); string(calls) != "sandbox\nlinux\n--\ntrue\n" {
		t.Fatalf("auto must probe once per binary (cached), native and full-access never: %q", calls)
	}

	// Native sandbox unavailable: auto refuses with the cause and the option; native goes on unprobed.
	t.Setenv("FAKE_CODEX_SANDBOX_OUT", "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted")
	t.Setenv("FAKE_CODEX_SANDBOX_EXIT", "1")
	m = newCodexManager(t)
	_, err := m.codexLaunchSandbox(Config{}, "review")
	if err == nil || !strings.Contains(err.Error(), "bwrap: loopback: Failed RTM_NEWADDR") ||
		!strings.Contains(err.Error(), `codex_sandbox to "worktree-full-access"`) {
		t.Fatalf("auto with bwrap failing: %v", err)
	}
	if got, err := m.codexLaunchSandbox(Config{CodexSandboxMode: "native"}, "review"); err != nil || got != CodexSandboxReadOnly {
		t.Fatalf("native: %q, %v", got, err)
	}

	// Probe outcomes: another failure is inconclusive (the launch goes on natively), not linux is skipped.
	for _, c := range []struct{ out, exit, want string }{
		{"", "0", CodexProbeOK},
		{"error: unrecognized subcommand 'sandbox'", "2", CodexProbeInconclusive},
		{"warning: x\nbwrap: setting up uid map: Permission denied", "1", CodexProbeUnavailable},
	} {
		t.Setenv("FAKE_CODEX_SANDBOX_OUT", c.out)
		t.Setenv("FAKE_CODEX_SANDBOX_EXIT", c.exit)
		if p := probeCodexSandbox(m.Config().codexBin(), os.Environ()); p.Result != c.want {
			t.Fatalf("probe %q exit %s: %+v, want %s", c.out, c.exit, p, c.want)
		}
	}
	if p := probeCodexSandbox(filepath.Join(t.TempDir(), "no-codex"), os.Environ()); p.Result != CodexProbeInconclusive {
		t.Fatalf("missing binary: %+v", p)
	}
	codexProbeGOOS = "darwin"
	if p := probeCodexSandbox(m.Config().codexBin(), os.Environ()); p.Result != CodexProbeSkipped {
		t.Fatalf("darwin: %+v", p)
	}
}

// A job of the default (auto) mode on a host whose native sandbox does not start is refused at launch:
// codex exec never runs, and the error names the cause and the option.
func TestCodexAutoRefusesWithoutNativeSandbox(t *testing.T) {
	forceLinuxProbe(t)
	t.Setenv("FAKE_CODEX_SANDBOX_OUT", "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted")
	t.Setenv("FAKE_CODEX_SANDBOX_EXIT", "1")
	m := newCodexManager(t)
	j := enqueue(t, m, "review it", codexReview)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "refused to start") ||
		!strings.Contains(j.Error, "codex_sandbox") || !strings.Contains(j.Error, "RTM_NEWADDR") {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt")); err == nil {
		t.Fatal("a refused job must not run codex exec")
	}
}

// A codex job whose sandbox did not start (a bwrap error in a command's output, or every command failed)
// ends failed with the cause and the option, even though codex completed its turn and wrote a final
// message; that message does not become report.md (issue #81).
func TestCodexSandboxFailureFailsJob(t *testing.T) {
	m := newCodexManager(t)
	for _, c := range []struct{ brief, want string }{
		{"FAKE_CODEX_BWRAP", `codex sandbox failed to start ("bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted"; 1 of 1 commands failed)`},
		{"FAKE_CODEX_ALLFAIL", "every command codex ran failed (2 of 2), so nothing was read"},
	} {
		j := runToEnd(t, m, enqueue(t, m, c.brief, codexReview).ID)
		if j.Status != store.StatusFailed || !strings.Contains(j.Error, c.want) || !strings.Contains(j.Error, "codex_sandbox") {
			t.Fatalf("%s: got %s %q, want failed with %q", c.brief, j.Status, j.Error, c.want)
		}
		if hasReport(m.JobDir(j.ID)) {
			t.Fatalf("%s: the final message of a job that read nothing must not become report.md", c.brief)
		}
	}
}

// The fixture stream (bwrap fails the only command, codex still writes a BLOCK report and completes the
// turn) is a failure; a failed command whose output merely mentions bwrap, next to a command that
// worked, is not.
func TestParseCodexStreamSandboxFailure(t *testing.T) {
	r := ParseCodexStream(filepath.Join("testdata", "codex-bwrap-stream.jsonl"))
	if !r.Found || r.IsError || r.CodexCommands != 1 || r.CodexCommandsFailed != 1 ||
		r.CodexSandboxError != "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted" {
		t.Fatalf("parsed %+v", r)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("## Summary\nBLOCK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Completed, r.ReportPresent = true, true
	if st, msg := Verdict(0, r, dir); st != store.StatusFailed || !strings.Contains(msg, "codex sandbox failed to start") ||
		!strings.Contains(msg, `codex_sandbox ("worktree-full-access")`) {
		t.Fatalf("verdict %s %q", st, msg)
	}

	p := filepath.Join(t.TempDir(), "stream.jsonl")
	lines := []string{
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"rg bwrap","aggregated_output":"codex_test.go:1: bwrap: loopback\n","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"rg nothing","aggregated_output":"","exit_code":1,"status":"failed"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"cat f","aggregated_output":"x\nbwrap: from the file\n","exit_code":1,"status":"failed"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"## Summary\nok"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = ParseCodexStream(p)
	if r.CodexCommands != 3 || r.CodexCommandsFailed != 2 || r.CodexSandboxError != "" {
		t.Fatalf("parsed %+v", r)
	}
	r.Completed, r.ReportPresent = true, true
	if st, msg := Verdict(0, r, dir); st != store.StatusSucceeded {
		t.Fatalf("a job with a working command must succeed: %s %q", st, msg)
	}
}

// codex_sandbox worktree-full-access: the job runs with -s danger-full-access and no probe, without the
// GitHub tokens, the ssh agent or the owner's gh config; turning it off on disk applies at the next launch.
func TestCodexWorktreeFullAccess(t *testing.T) {
	forceLinuxProbe(t)
	m := newCodexManager(t)
	probes := filepath.Join(t.TempDir(), "probe.log")
	t.Setenv("FAKE_CODEX_SANDBOX_LOG", probes)
	t.Setenv("GH_TOKEN", "test-gh-token")
	t.Setenv("GITHUB_TOKEN", "test-github-token")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/test-agent.sock")
	m.Cfg.CodexSandboxMode = CodexSandboxModeWorktreeFullAccess
	writeCodexConfig(t, m, `{"codex_sandbox":"worktree-full-access"}`)
	j := runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	if argv, _ := os.ReadFile(filepath.Join(dir, "argv.txt")); !strings.Contains(string(argv), "-s\ndanger-full-access\n") {
		t.Fatalf("argv %q", argv)
	}
	if env, _ := os.ReadFile(filepath.Join(dir, "credenv.txt")); string(env) != "GH_CONFIG_DIR="+filepath.Join(dir, "no-gh-config")+"\n" {
		t.Fatalf("credential env of a full-access job: %q", env)
	}
	if calls, _ := os.ReadFile(probes); len(calls) != 0 {
		t.Fatalf("full-access must not probe: %q", calls)
	}

	// Revoked on disk: the next launch is native again (the daemon's active value is not enough).
	writeCodexConfig(t, m, `{}`)
	j = runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID)
	argv, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "argv.txt"))
	if j.Status != store.StatusSucceeded || !strings.Contains(string(argv), "-s\nread-only\n") {
		t.Fatalf("after revocation: %s %q argv %q", j.Status, j.Error, argv)
	}
	if env, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "credenv.txt")); !strings.Contains(string(env), "GH_TOKEN=") {
		t.Fatalf("a native job keeps its environment: %q", env)
	}
	if _, err := LoadConfig(writeConfigDir(t, `{"codex_sandbox":"off"}`)); err == nil ||
		!strings.Contains(err.Error(), "auto|native|worktree-full-access") {
		t.Fatalf("bad codex_sandbox: %v", err)
	}
}

// A codex research job is read-only in intent: one that changed its worktree (as it can under
// worktree-full-access) gets the review-changed warning, like a review job.
func TestCodexResearchChangedWorktreeWarning(t *testing.T) {
	m := newCodexManager(t)
	m.Cfg.CodexSandboxMode = CodexSandboxModeWorktreeFullAccess
	writeCodexConfig(t, m, `{"codex_sandbox":"worktree-full-access"}`)
	repo := newRepo(t)
	for _, c := range []struct {
		brief string
		warn  bool
	}{{"FAKE_CODEX_WRITE", true}, {"nothing", false}} {
		j := runToEnd(t, m, enqueue(t, m, c.brief, func(r *StartRequest) {
			r.Kind, r.Model, r.RepoPath = "research", "gpt-6.1-sol", repo
		}).ID)
		if j.Status != store.StatusSucceeded || strings.Contains(j.Error, ReviewChangedWarning) != c.warn {
			t.Fatalf("%s: %s %q, want warning %v", c.brief, j.Status, j.Error, c.warn)
		}
	}
}

// writeConfigDir writes config.json into a fresh dir and returns the dir.
func writeConfigDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
