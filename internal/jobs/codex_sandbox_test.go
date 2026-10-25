package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
		got, err := m.codexLaunchSandbox(Config{CodexSandboxMode: c.mode, CodexWriteAllowed: true}, c.kind)
		if err != nil || got != c.want {
			t.Fatalf("%q/%s: %q, %v; want %q", c.mode, c.kind, got, err, c.want)
		}
		j := &store.Job{Kind: c.kind, Model: "gpt-6.1-sol"}
		if s := argSandbox(CodexArgs(j, "high", got)); s != c.want {
			t.Fatalf("%q/%s: codex argv -s %q, want %q", c.mode, c.kind, s, c.want)
		}
	}
	if _, err := m.codexLaunchSandbox(Config{CodexSandboxMode: CodexSandboxModeWorktreeFullAccess}, "review"); err == nil ||
		!strings.Contains(err.Error(), "needs codex_write_allowed") {
		t.Fatalf("worktree-full-access without codex_write_allowed: %v", err)
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
		!strings.Contains(err.Error(), "apparmor_restrict_unprivileged_userns") ||
		!strings.Contains(err.Error(), `codex_sandbox "worktree-full-access" is the last resort`) {
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

// A codex job whose sandbox did not start ends failed with the cause and the host fix, even though codex
// completed its turn and wrote a final message; that message does not become report.md (issue #81). Two
// signs count: a command whose output starts with a bwrap error (any job), and a repo job without a
// single command that ran successfully (codex 0.162 under read-only shows no command item at all, only
// the model's message quoting bwrap). Ordinary non-zero exits next to a success, and a job without a
// repository that ran no command, are not sandbox failures.
func TestCodexSandboxFailureFailsJob(t *testing.T) {
	m := newCodexManager(t)
	repo := newRepo(t)
	withRepo := func(r *StartRequest) { codexReview(r); r.RepoPath = repo }
	for _, c := range []struct {
		name, brief string
		start       func(*StartRequest)
		want        string // "" = succeeded
	}{
		{"bwrap output", "FAKE_CODEX_BWRAP", codexReview, `codex sandbox failed to start ("bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted")`},
		{"repo, no command", "FAKE_CODEX_NOCMD", withRepo, "codex ran no command in the repository successfully (0 commands)"},
		{"repo, every command failed", "FAKE_CODEX_ALLFAIL", withRepo, "codex ran no command in the repository successfully (2 commands, none exited 0)"},
		{"repo, rg miss and a success", "FAKE_CODEX_RGMISS", withRepo, ""},
		{"repo, one success", "review it", withRepo, ""},
		{"no repo, no command", "FAKE_CODEX_NOCMD", codexReview, ""},
		{"no repo, every command failed", "FAKE_CODEX_ALLFAIL", codexReview, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			j := runToEnd(t, m, enqueue(t, m, c.brief, c.start).ID)
			if c.want == "" {
				if j.Status != store.StatusSucceeded {
					t.Fatalf("got %s %q, want succeeded", j.Status, j.Error)
				}
				return
			}
			if j.Status != store.StatusFailed || !strings.Contains(j.Error, c.want) ||
				!strings.Contains(j.Error, "apparmor_restrict_unprivileged_userns") {
				t.Fatalf("got %s %q, want failed with %q and the AppArmor host fix", j.Status, j.Error, c.want)
			}
			if hasReport(m.JobDir(j.ID)) {
				t.Fatal("the final message of a job that read nothing must not become report.md")
			}
		})
	}
}

// An inconclusive probe is retried after codexProbeRetry; ok (and unavailable) stay cached for the
// binary. Concurrent callers share one probe run.
func TestCodexSandboxProbeRetriesInconclusive(t *testing.T) {
	forceLinuxProbe(t)
	m := newCodexManager(t)
	probes := filepath.Join(t.TempDir(), "probe.log")
	t.Setenv("FAKE_CODEX_SANDBOX_LOG", probes)
	runs := func() int {
		b, _ := os.ReadFile(probes)
		return strings.Count(string(b), "sandbox\n")
	}
	age := func() {
		m.codexProbeMu.Lock()
		m.codexProbe.At = m.codexProbe.At.Add(-11 * time.Minute)
		m.codexProbeMu.Unlock()
	}
	bin := m.Config().codexBin()
	t.Setenv("FAKE_CODEX_SANDBOX_OUT", "error: unrecognized subcommand 'sandbox'")
	t.Setenv("FAKE_CODEX_SANDBOX_EXIT", "2")
	if p := m.codexSandboxProbe(bin); p.Result != CodexProbeInconclusive {
		t.Fatalf("probe %+v", p)
	}
	m.codexSandboxProbe(bin)
	if n := runs(); n != 1 {
		t.Fatalf("a fresh inconclusive probe is cached: %d runs", n)
	}
	age()
	t.Setenv("FAKE_CODEX_SANDBOX_OUT", "")
	t.Setenv("FAKE_CODEX_SANDBOX_EXIT", "0")
	if p := m.codexSandboxProbe(bin); p.Result != CodexProbeOK || runs() != 2 {
		t.Fatalf("an old inconclusive probe must run again: %+v, %d runs", p, runs())
	}
	age()
	if p := m.codexSandboxProbe(bin); p.Result != CodexProbeOK || runs() != 2 {
		t.Fatalf("an ok probe stays cached: %+v, %d runs", p, runs())
	}

	m = newCodexManager(t)
	t.Setenv("FAKE_CODEX_SANDBOX_SLEEP", "1")
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); m.codexSandboxProbe(bin) }()
	}
	wg.Wait()
	if n := runs(); n != 3 {
		t.Fatalf("concurrent callers must share one probe: %d runs, want 3", n)
	}
}

// worktree-full-access leaves codex jobs of every kind able to write anywhere the user can, so it needs
// codex_write_allowed too: refused at enqueue without it, and at launch when it was revoked on disk.
func TestCodexFullAccessNeedsWriteAllowed(t *testing.T) {
	m := newCodexManager(t)
	m.Cfg.CodexSandboxMode = CodexSandboxModeWorktreeFullAccess
	writeCodexConfig(t, m, `{"codex_sandbox":"worktree-full-access"}`)
	if _, err := m.Enqueue(StartRequest{Brief: "review it", Kind: "review", Model: "gpt-6.1-sol", Title: "x"}); err == nil ||
		!strings.Contains(err.Error(), "codex_write_allowed") {
		t.Fatalf("full access without codex_write_allowed: %v", err)
	}
	m.Cfg.CodexWriteAllowed = true
	writeCodexConfig(t, m, `{"codex_sandbox":"worktree-full-access","codex_write_allowed":true}`)
	j := enqueue(t, m, "review it", codexReview)
	writeCodexConfig(t, m, `{"codex_sandbox":"worktree-full-access"}`)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j = get(t, m, j.ID); j.Status != store.StatusFailed || !strings.Contains(j.Error, "refused to start") ||
		!strings.Contains(j.Error, "codex_write_allowed") {
		t.Fatalf("revoked on disk: %s %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt")); err == nil {
		t.Fatal("a refused job must not run codex exec")
	}
}

// codexVerdict is the verdict of a parsed codex stream of a job (repo or not) that wrote report.md.
func codexVerdict(t *testing.T, r StreamResult, repo bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("## Summary\nBLOCK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Completed, r.ReportPresent, r.CodexRepo = true, true, repo
	return Verdict(0, r, dir)
}

// The real capture (codex 0.162.1, -s read-only, bwrap blocked by AppArmor): no command item, the bwrap
// error only in the model's message, a completed turn, exit 0. A repo job with it fails, naming the host
// fix; the message alone is no verdict, so a job without a repository is not failed by it.
func TestParseCodexStreamSandboxFailure(t *testing.T) {
	r := ParseCodexStream(filepath.Join("testdata", "codex-bwrap-stream.jsonl"))
	if !r.Found || r.IsError || r.CodexCommands != 0 || r.CodexSandboxError != "" || !r.CodexMessageBwrap ||
		r.Text != "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted" {
		t.Fatalf("parsed %+v", r)
	}
	st, msg := codexVerdict(t, r, true)
	if st != store.StatusFailed || !strings.Contains(msg, "codex ran no command in the repository successfully (0 commands)") ||
		!strings.Contains(msg, "untrusted model text") || !strings.Contains(msg, "apparmor_restrict_unprivileged_userns") ||
		!strings.Contains(msg, "AppArmor profile with `userns`") {
		t.Fatalf("repo verdict %s %q", st, msg)
	}
	if i, j := strings.Index(msg, "AppArmor"), strings.Index(msg, CodexSandboxModeWorktreeFullAccess); i < 0 || j < i {
		t.Fatalf("the host fix comes before worktree-full-access: %q", msg)
	}
	if st, msg := codexVerdict(t, r, false); st != store.StatusSucceeded {
		t.Fatalf("no-repo verdict %s %q", st, msg)
	}

	// The command_execution shape (as codex prints it under danger-full-access) with a bwrap error as the
	// output of a failed command: failed whether or not the job has a repository.
	r = ParseCodexStream(filepath.Join("testdata", "codex-bwrap-command-stream.jsonl"))
	if r.CodexCommands != 1 || r.CodexCommandsOK != 0 || r.CodexSandboxError != "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted" {
		t.Fatalf("parsed %+v", r)
	}
	for _, repo := range []bool{false, true} {
		if st, msg := codexVerdict(t, r, repo); st != store.StatusFailed || !strings.Contains(msg, "codex sandbox failed to start") {
			t.Fatalf("repo %v: verdict %s %q", repo, st, msg)
		}
	}

	// rg without a match and output that merely mentions bwrap, next to one command that worked: ok.
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
	if r.CodexCommands != 3 || r.CodexCommandsOK != 1 || r.CodexSandboxError != "" {
		t.Fatalf("parsed %+v", r)
	}
	if st, msg := codexVerdict(t, r, true); st != store.StatusSucceeded {
		t.Fatalf("a repo job with a working command must succeed: %s %q", st, msg)
	}
	// A skipped (oversized) line may be the command that worked: not judged on that rule.
	r = ParseCodexStream(filepath.Join("testdata", "codex-bwrap-stream.jsonl"))
	r.Skipped = 1
	if st, msg := codexVerdict(t, r, true); st != store.StatusSucceeded {
		t.Fatalf("skipped line: %s %q", st, msg)
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
	m.Cfg.CodexSandboxMode, m.Cfg.CodexWriteAllowed = CodexSandboxModeWorktreeFullAccess, true
	writeCodexConfig(t, m, `{"codex_sandbox":"worktree-full-access","codex_write_allowed":true}`)
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
	if sb, err := m.Store.LaunchCodexSandbox(j.ID); err != nil || sb != CodexSandboxFullAccess {
		t.Fatalf("recorded launch sandbox: %q, %v", sb, err)
	}

	// Revoked on disk: the next launch is native again (the daemon's active value is not enough).
	writeCodexConfig(t, m, `{}`)
	j = runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID)
	argv, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "argv.txt"))
	if j.Status != store.StatusSucceeded || !strings.Contains(string(argv), "-s\nread-only\n") {
		t.Fatalf("after revocation: %s %q argv %q", j.Status, j.Error, argv)
	}
	if sb, err := m.Store.LaunchCodexSandbox(j.ID); err != nil || sb != CodexSandboxReadOnly {
		t.Fatalf("recorded launch sandbox after revocation: %q, %v", sb, err)
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
	m.Cfg.CodexSandboxMode, m.Cfg.CodexWriteAllowed = CodexSandboxModeWorktreeFullAccess, true
	writeCodexConfig(t, m, `{"codex_sandbox":"worktree-full-access","codex_write_allowed":true}`)
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
