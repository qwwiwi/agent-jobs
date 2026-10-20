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
					bin, args = m.Cfg.codexBin(), CodexArgs(j, m.Cfg.reasoningEffort(j.ReasoningEffort))
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
