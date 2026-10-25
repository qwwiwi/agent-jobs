package jobs

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Reproduce the bypass through the actual CLI: the daemon started with both opt-ins off;
// a fresh client sees the edited file, enqueues, then tries to launch with a manual tick.
func TestManualTickCannotBypassRunningDaemonPermissions(t *testing.T) {
	for _, ownership := range []string{"lock", "state"} {
		for _, runner := range []string{"claude-acceptance", "codex-acceptance", "codex-fix", "codex-general"} {
			t.Run(ownership+"/"+runner, func(t *testing.T) {
				daemon := newCodexManager(t)
				writeReloadConfig(t, daemon, `{"codex_write_allowed":false,"acceptance_allowed":false}`)
				daemon.WatchDaemonConfig()
				if ownership == "lock" {
					unlock, err := daemon.LockDaemon()
					if err != nil {
						t.Fatal(err)
					}
					defer unlock()
				} else if err := daemon.WriteDaemonInfo(); err != nil {
					t.Fatal(err)
				}
				writeReloadConfig(t, daemon, `{"codex_write_allowed":true,"acceptance_allowed":true}`)
				client, err := Open(daemon.Agent)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				j := enqueue(t, client, "work", func(r *StartRequest) {
					r.Kind = "fix"
					if strings.HasSuffix(runner, "-acceptance") {
						r.AcceptanceCmd = "touch ../acc-ran"
					}
					if runner != "claude-acceptance" {
						r.Model = "gpt-6.1-sol"
						if runner == "codex-general" {
							r.Kind = "general"
						}
					}
				})
				out, err := exec.Command(selfBin, "tick", "--agent", daemon.Agent).CombinedOutput()
				if err == nil || !strings.Contains(string(out), "manual tick refused") {
					t.Fatalf("manual tick not refused: %v\n%s", err, out)
				}
				if got := get(t, daemon, j.ID); got.Status != store.StatusQueued || got.Attempt != 0 || got.LaunchToken != "" {
					t.Fatalf("manual tick claimed job: %+v", got)
				}
				if _, err := os.Stat(daemon.JobDir(j.ID)); !os.IsNotExist(err) {
					t.Fatalf("manual tick prepared a job: %v", err)
				}
				if ownership == "lock" {
					if err := daemon.Tick(); err != nil {
						t.Fatal(err)
					}
					if got := get(t, daemon, j.ID); got.Status != store.StatusFailed {
						t.Fatalf("daemon granted edited opt-ins: %+v", got)
					}
				}
			})
		}
	}
}

func TestWrapperRejectsUntrustedLaunchAuthorization(t *testing.T) {
	for _, runner := range []string{store.RunnerClaude, store.RunnerCodex} {
		for _, scenario := range []string{"absent", "forged file", "unreadable", "stale token", "other attempt", "other job"} {
			t.Run(runner+"/"+scenario, func(t *testing.T) {
				m := newCodexManager(t)
				writeReloadConfig(t, m, `{"codex_write_allowed":true,"acceptance_allowed":true}`)
				var err error
				m.Cfg, err = LoadConfig(m.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				j := enqueue(t, m, "work", func(r *StartRequest) {
					r.Kind, r.AcceptanceCmd = "fix", "touch ../acc-ran"
					if runner == store.RunnerCodex {
						r.Model = "gpt-6.1-sol"
					}
				})
				const token = "current-token"
				if err := m.Store.Claim(j.ID, token, m.Now()); err != nil {
					t.Fatal(err)
				}
				j = get(t, m, j.ID)
				grant := func(id string) {
					t.Helper()
					if err := m.Store.SetLaunchAuthorization(id, token, 1, store.LaunchAuthorization{CodexWriteAllowed: true, AcceptanceAllowed: true}); err != nil {
						t.Fatal(err)
					}
				}
				switch scenario {
				case "unreadable":
					grant(j.ID)
					raw, err := sql.Open("sqlite", filepath.Join(m.StateDir, "jobs.db"))
					if err != nil {
						t.Fatal(err)
					}
					_, err = raw.Exec(`UPDATE launch_authorizations SET codex_write_allowed='invalid', acceptance_allowed='invalid' WHERE job_id=?`, j.ID)
					_ = raw.Close()
					if err != nil {
						t.Fatal(err)
					}
				case "stale token", "other attempt":
					grant(j.ID)
					if err := m.Store.Finish(j.ID, token, store.StatusFailed, 0, 0, 0, "retry", m.Now()); err != nil {
						t.Fatal(err)
					}
					if err := m.Store.Requeue(j.ID, store.ResumeLimits{MaxTurns: j.MaxTurns, MaxBudgetUSD: j.MaxBudgetUSD, TimeoutMinutes: j.TimeoutMinutes}, "", ""); err != nil {
						t.Fatal(err)
					}
					nextToken := token // Reusing a token must still fail the attempt binding.
					if scenario == "stale token" {
						nextToken = "new-token"
					}
					if err := m.Store.Claim(j.ID, nextToken, m.Now()); err != nil {
						t.Fatal(err)
					}
					j = get(t, m, j.ID)
				case "other job":
					other := enqueue(t, m, "other work", nil)
					if err := m.Store.Claim(other.ID, token, m.Now()); err != nil {
						t.Fatal(err)
					}
					grant(other.ID)
				}
				if err := m.prepare(j, j.LaunchToken, m.JobDir(j.ID)); err != nil {
					t.Fatal(err)
				}
				if scenario == "forged file" {
					// A job can write these files, but neither is an authorization source.
					for _, name := range []string{"launch-authorization.json", "wrapper.json"} {
						if err := writeJSON(filepath.Join(m.JobDir(j.ID), name), map[string]any{
							"acceptance_allowed": true, "codex_write_allowed": true, "launch_token": token, "attempt": j.Attempt,
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if a, err := openAcceptance(m.JobDir(j.ID), j.LaunchToken); err == nil {
					_ = a.st.Close()
					t.Fatal("acceptance accepted untrusted authorization")
				}
				bin, args := m.Cfg.claudeBin(), ClaudeArgs(j, m.JobDir(j.ID))
				if runner == store.RunnerCodex {
					bin, args = m.Cfg.codexBin(), CodexArgs(j, m.Cfg.reasoningEffort(j.ReasoningEffort), CodexSandbox(j.Kind))
				}
				argv := []string{"run-wrapper", m.JobDir(j.ID), "--token", j.LaunchToken, "--runner", runner, "--", bin}
				cmd := exec.Command(selfBin, append(argv, args...)...)
				cmd.Dir, cmd.Env = filepath.Join(m.JobDir(j.ID), "work"), m.spawnEnv("")
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				out, err := cmd.CombinedOutput()
				if runner == store.RunnerCodex {
					if err == nil || !strings.Contains(string(out), "codex_write_allowed was not authorized") {
						t.Fatalf("codex wrapper not refused: %v\n%s", err, out)
					}
					if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt")); !os.IsNotExist(err) {
						t.Fatalf("unauthorized codex ran: %v", err)
					}
				} else if err != nil {
					t.Fatalf("claude wrapper: %v\n%s", err, out)
				}
				if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "acc-ran")); !os.IsNotExist(err) {
					t.Fatalf("unauthorized acceptance ran: %v", err)
				}
			})
		}
	}
}

func TestDaemonLaunchPersistsEffectiveAuthorization(t *testing.T) {
	for _, runner := range []string{store.RunnerClaude, store.RunnerCodex} {
		t.Run(runner, func(t *testing.T) {
			m := newCodexManager(t)
			writeReloadConfig(t, m, `{"codex_write_allowed":true,"acceptance_allowed":true}`)
			var err error
			m.Cfg, err = LoadConfig(m.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := m.LockDaemon()
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			m.WatchDaemonConfig()
			j := enqueue(t, m, "work", func(r *StartRequest) {
				r.Kind, r.AcceptanceCmd = "fix", "touch ../acc-ran"
				if runner == store.RunnerCodex {
					r.Model = "gpt-6.1-sol"
				}
			})
			j = runToEnd(t, m, j.ID)
			if j.Status != store.StatusSucceeded || j.AcceptanceExit == nil || *j.AcceptanceExit != 0 {
				t.Fatalf("legitimate daemon launch denied: %+v", j)
			}
			if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "acc-ran")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A codex launch without the codex sandbox (-s danger-full-access) runs only when the launching daemon
// recorded that sandbox and codex_write_allowed for the attempt; the wrapper refuses an argv whose
// sandbox was not authorized, or that names more than one (issue #81, L4).
func TestWrapperChecksCodexSandboxAuthorization(t *testing.T) {
	full := store.LaunchAuthorization{CodexWriteAllowed: true, CodexSandbox: CodexSandboxFullAccess}
	for _, c := range []struct {
		name  string
		auth  *store.LaunchAuthorization
		extra []string // appended to the codex argv
		ok    bool
	}{
		{"none recorded", nil, nil, false},
		{"write only", &store.LaunchAuthorization{CodexWriteAllowed: true}, nil, false},
		{"sandbox without write", &store.LaunchAuthorization{CodexSandbox: CodexSandboxFullAccess}, nil, false},
		{"authorized", &full, nil, true},
		{"second sandbox", &full, []string{"--sandbox", CodexSandboxReadOnly}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newCodexManager(t)
			j := enqueue(t, m, "review it", codexReview)
			const token = "current-token"
			if err := m.Store.Claim(j.ID, token, m.Now()); err != nil {
				t.Fatal(err)
			}
			j = get(t, m, j.ID)
			if c.auth != nil {
				if err := m.Store.SetLaunchAuthorization(j.ID, token, j.Attempt, *c.auth); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.prepare(j, token, m.JobDir(j.ID)); err != nil {
				t.Fatal(err)
			}
			args := append(CodexArgs(j, "high", CodexSandboxFullAccess), c.extra...)
			argv := []string{"run-wrapper", m.JobDir(j.ID), "--token", token, "--runner", store.RunnerCodex, "--", m.Cfg.codexBin()}
			cmd := exec.Command(selfBin, append(argv, args...)...)
			cmd.Dir, cmd.Env = filepath.Join(m.JobDir(j.ID), "work"), m.spawnEnv("")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			out, err := cmd.CombinedOutput()
			_, ranErr := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
			if c.ok {
				if err != nil || ranErr != nil {
					t.Fatalf("authorized full-access launch: %v %v\n%s", err, ranErr, out)
				}
				return
			}
			if err == nil || !strings.Contains(string(out), "was not authorized by the launching daemon") {
				t.Fatalf("wrapper not refused: %v\n%s", err, out)
			}
			if !os.IsNotExist(ranErr) {
				t.Fatalf("unauthorized codex ran: %v", ranErr)
			}
		})
	}
}

// The wrapper reads a codex argv's sandbox with codex's option boundaries: an `-s` after `--` is the
// prompt, not a sandbox; a sandbox or approval bypass flag, a `-c` sandbox override, a repeated sandbox
// or an option it does not know is refused, so a read-only review cannot launch without a sandbox
// (issue #81, L4).
func TestWrapperRefusesCodexSandboxBypass(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		ok   bool
	}{
		{"bypass with -s after --", []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "--", "-sread-only"}, false},
		{"yolo", []string{"exec", "--yolo", "-s", "read-only", "-"}, false},
		{"bypass beside read-only", []string{"exec", "-s", "read-only", "--dangerously-bypass-approvals-and-sandbox", "-"}, false},
		{"full-auto", []string{"exec", "--full-auto", "-s", "read-only", "-"}, false},
		{"profile", []string{"exec", "-p", "open", "-s", "read-only", "-"}, false},
		{"-c sandbox_mode", []string{"exec", "-s", "read-only", "-c", `sandbox_mode="danger-full-access"`, "-"}, false},
		{"-c sandbox", []string{"exec", "-s", "read-only", "-c", `sandbox="danger-full-access"`, "-"}, false},
		{"--config=sandbox_mode", []string{"exec", "-s", "read-only", "--config=sandbox_mode=danger-full-access", "-"}, false},
		{"-c attached profile sandbox", []string{"exec", "-s", "read-only", "-cprofiles.x.sandbox_mode=danger-full-access", "-"}, false},
		{"-s repeated", []string{"exec", "-s", "read-only", "-s", "danger-full-access", "-"}, false},
		{"-sX and --sandbox=X", []string{"exec", "-sread-only", "--sandbox=danger-full-access", "-"}, false},
		{"no sandbox, -s only after --", []string{"exec", "--", "-sread-only"}, false},
		{"value swallowing a flag", []string{"exec", "-s", "read-only", "-m", "--yolo", "-"}, false},
		{"unknown option", []string{"exec", "-s", "read-only", "--add-dir", "/", "-"}, false},
		{"-s X", []string{"exec", "-s", "read-only", "-"}, true},
		{"-sX", []string{"exec", "-sread-only", "-"}, true},
		{"--sandbox X", []string{"exec", "--sandbox", "read-only", "-"}, true},
		{"--sandbox=X", []string{"exec", "--sandbox=read-only", "-"}, true},
		{"daemon argv", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newCodexManager(t)
			j := enqueue(t, m, "review it", codexReview)
			const token = "current-token"
			if err := m.Store.Claim(j.ID, token, m.Now()); err != nil {
				t.Fatal(err)
			}
			j = get(t, m, j.ID)
			if err := m.prepare(j, token, m.JobDir(j.ID)); err != nil {
				t.Fatal(err)
			}
			args := c.args
			if args == nil {
				args = CodexArgs(j, "high", CodexSandbox(j.Kind))
			}
			argv := []string{"run-wrapper", m.JobDir(j.ID), "--token", token, "--runner", store.RunnerCodex, "--", m.Cfg.codexBin()}
			cmd := exec.Command(selfBin, append(argv, args...)...)
			cmd.Dir, cmd.Env = filepath.Join(m.JobDir(j.ID), "work"), m.spawnEnv("")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			out, err := cmd.CombinedOutput()
			_, ranErr := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
			if c.ok {
				if err != nil || ranErr != nil {
					t.Fatalf("sandboxed read-only launch: %v %v\n%s", err, ranErr, out)
				}
				return
			}
			if err == nil || !strings.Contains(string(out), "was not authorized by the launching daemon") {
				t.Fatalf("wrapper not refused: %v\n%s", err, out)
			}
			if !os.IsNotExist(ranErr) {
				t.Fatalf("unsandboxed codex ran: %v", ranErr)
			}
		})
	}
}
