package jobs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func withAcceptance(cmd string) func(*StartRequest) {
	return func(r *StartRequest) { r.Kind = "fix"; r.AcceptanceCmd = cmd }
}

// acceptanceManager is newManager with acceptance_allowed on, in m.Cfg and in config.json (the wrapper
// reloads it); extra is more config JSON members.
func acceptanceManager(t *testing.T, extra string) *Manager {
	t.Helper()
	m := newManager(t)
	body := `{"acceptance_allowed": true` + extra + `}`
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(m.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	m.Cfg = c
	return m
}

// A passing acceptance command keeps the job succeeded; its output tail is in acceptance.log and its
// exit code and duration in the row. It runs in the work dir, with the job marker and without the
// wrapper's other variables.
func TestAcceptancePass(t *testing.T) {
	m := acceptanceManager(t, "")
	t.Setenv("ACCEPTANCE_TEST_SECRET", "must-not-leak")
	j := enqueue(t, m, "do it", withAcceptance(`echo ACC_OK; pwd > ../acc-cwd.txt; env > ../acc-env.txt`))
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.Error != "" {
		t.Fatalf("got %s err=%q", j.Status, j.Error)
	}
	if j.AcceptanceExit == nil || *j.AcceptanceExit != 0 || j.AcceptanceStartedAt == nil {
		t.Fatalf("acceptance fields: %+v", j)
	}
	dir := m.JobDir(j.ID)
	if log := readFile(t, filepath.Join(dir, AcceptanceLog)); !strings.Contains(log, "ACC_OK") {
		t.Fatalf("acceptance.log: %q", log)
	}
	if cwd := strings.TrimSpace(readFile(t, filepath.Join(dir, "acc-cwd.txt"))); !samePath(cwd, filepath.Join(dir, "work")) {
		t.Fatalf("acceptance cwd %s", cwd)
	}
	env := readFile(t, filepath.Join(dir, "acc-env.txt"))
	if strings.Contains(env, "ACCEPTANCE_TEST_SECRET") || strings.Contains(env, "AGENT_JOBS_CLAUDE_BIN") {
		t.Fatalf("acceptance env not scrubbed:\n%s", env)
	}
	if !strings.Contains(env, JobEnvVar+"="+j.ID) || !strings.Contains(env, "GIT_CONFIG_NOSYSTEM=1") || !strings.Contains(env, "PATH=") {
		t.Fatalf("acceptance env lacks the job marker, git env or PATH:\n%s", env)
	}
	// HOME and TMPDIR are a throwaway dir in the job dir; the wrapper's own HOME is not passed on.
	home := filepath.Join(dir, acceptanceHome)
	if !envHas(env, "HOME", home) || !envHas(env, "TMPDIR", filepath.Join(home, "tmp")) {
		t.Fatalf("acceptance HOME/TMPDIR not the job's throwaway dir %s:\n%s", home, env)
	}
	if real := os.Getenv("HOME"); real != "" && envHas(env, "HOME", real) {
		t.Fatalf("acceptance env has the real HOME %s", real)
	}
	// The daemon's deadline is the one the wrapper recorded with the start (default timeout 15 min).
	if j.AcceptanceDeadline == nil || !j.AcceptanceDeadline.Equal(j.AcceptanceStartedAt.Add(15*time.Minute+acceptanceDeadlineGrace)) ||
		j.AcceptancePGID != 0 {
		t.Fatalf("recorded deadline %v (start %v), pgid %d", j.AcceptanceDeadline, j.AcceptanceStartedAt, j.AcceptancePGID)
	}
	if line := DoneLine(j, 0); strings.Contains(line, "acceptance") || !strings.Contains(line, " succeeded ") {
		t.Fatalf("done line: %s", line)
	}
}

// A failing acceptance command fails the job: error "acceptance failed (exit N)", the done line says so.
func TestAcceptanceFail(t *testing.T) {
	m := acceptanceManager(t, "")
	j := enqueue(t, m, "do it", withAcceptance(`echo ACC_BAD >&2; exit 3`))
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed || j.Error != "acceptance failed (exit 3)" {
		t.Fatalf("got %s err=%q", j.Status, j.Error)
	}
	if j.AcceptanceExit == nil || *j.AcceptanceExit != 3 {
		t.Fatalf("acceptance exit: %+v", j.AcceptanceExit)
	}
	if line := DoneLine(j, 0); !strings.Contains(line, " failed ") || !strings.Contains(line, "acceptance failed (exit 3)") {
		t.Fatalf("done line: %s", line)
	}
	if log := readFile(t, filepath.Join(m.JobDir(j.ID), AcceptanceLog)); !strings.Contains(log, "ACC_BAD") {
		t.Fatalf("acceptance.log: %q", log)
	}
	if !IsAcceptanceFailure(j.Error) {
		t.Fatal("IsAcceptanceFailure")
	}
}

// A claude run that fails never runs the acceptance command; the failure is claude's.
func TestAcceptanceNotRunAfterFailedRun(t *testing.T) {
	m := acceptanceManager(t, "")
	j := enqueue(t, m, "FAKE_EXIT3", withAcceptance(`touch ../acc-ran`))
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "exited with code 3") || j.AcceptanceExit != nil {
		t.Fatalf("got %s err=%q exit=%v", j.Status, j.Error, j.AcceptanceExit)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "acc-ran")); err == nil {
		t.Fatal("acceptance ran after a failed claude run")
	}
}

// A succeeded verdict without a recorded acceptance result fails closed.
func TestAcceptanceMissingResultFailsClosed(t *testing.T) {
	j := &store.Job{AcceptanceCmd: "make test"}
	if got := AcceptanceFailure(j); got != "acceptance did not run (see stderr.log)" {
		t.Fatalf("missing result: %q", got)
	}
	zero, two, to := 0, 2, store.AcceptanceTimedOut
	j.AcceptanceExit = &zero
	if got := AcceptanceFailure(j); got != "" {
		t.Fatalf("pass: %q", got)
	}
	j.AcceptanceExit = &two
	if got := AcceptanceFailure(j); got != "acceptance failed (exit 2)" {
		t.Fatalf("exit 2: %q", got)
	}
	j.AcceptanceExit, j.AcceptanceDurationSec = &to, 900
	if got := AcceptanceFailure(j); got != "acceptance failed (timeout after 900s)" {
		t.Fatalf("timeout: %q", got)
	}
	if n := acceptanceNote(j); n != "acceptance failed (timeout)" {
		t.Fatalf("timeout note: %q", n)
	}
	if got := AcceptanceFailure(&store.Job{}); got != "" {
		t.Fatalf("no command: %q", got)
	}
}

// On its timeout the command's whole process group is ended (a background child holding the output
// pipe too) and the result is AcceptanceTimedOut.
func TestAcceptanceTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	exit, dur := runAcceptance(context.Background(), dir, `echo started; sleep 30 & echo $! > ../child.pid; sleep 30`,
		300*time.Millisecond, io.Discard, nil)
	if exit != store.AcceptanceTimedOut {
		t.Fatalf("exit %d, want timeout", exit)
	}
	if took := time.Since(start); took > acceptanceKillGrace+3*time.Second || dur > took {
		t.Fatalf("took %s (dur %s)", took, dur)
	}
	if log := readFile(t, filepath.Join(dir, AcceptanceLog)); !strings.Contains(log, "started") {
		t.Fatalf("acceptance.log: %q", log)
	}
	var pid int
	waitFor(t, "child pid", 2*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "child.pid"))
		return err == nil && len(strings.TrimSpace(string(b))) > 0
	})
	if _, err := fmt.Sscan(readFile(t, filepath.Join(dir, "child.pid")), &pid); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "background child gone", 3*time.Second, func() bool {
		st, _, err := procStat(pid)
		return err != nil || st == "Z"
	})
}

// The acceptance log keeps only the last AcceptanceLogBytes, with a note of what was cut.
func TestAcceptanceLogCapped(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "jobs", "j1")
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	exit, _ := runAcceptance(context.Background(), dir, `head -c 200000 /dev/zero | tr '\0' a; echo; echo TAIL_END`,
		time.Minute, io.Discard, nil)
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	log := readFile(t, filepath.Join(dir, AcceptanceLog))
	if !strings.HasPrefix(log, "[... ") || !strings.HasSuffix(log, "TAIL_END\n") || len(log) > AcceptanceLogBytes+100 {
		t.Fatalf("log: %d bytes, starts %q", len(log), log[:min(len(log), 60)])
	}
	tail, err := ReadAcceptanceLogTail(state, "j1", 9)
	if err != nil || tail != "TAIL_END\n" {
		t.Fatalf("tail %q %v", tail, err)
	}
	// A symlink in the log's place is not followed.
	if err := os.Remove(filepath.Join(dir, AcceptanceLog)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, AcceptanceLog)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAcceptanceLogTail(state, "j1", 100); err == nil {
		t.Fatal("a symlinked acceptance.log was read")
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 5}
	for _, s := range []string{"ab", "cd", "efg"} {
		_, _ = b.Write([]byte(s))
	}
	if string(b.Bytes()) != "cdefg" || b.dropped != 2 {
		t.Fatalf("%q dropped %d", b.Bytes(), b.dropped)
	}
	_, _ = b.Write([]byte("0123456789"))
	if string(b.Bytes()) != "56789" || b.dropped != 12 {
		t.Fatalf("%q dropped %d", b.Bytes(), b.dropped)
	}
}

// acceptance_cmd is refused for review and research jobs and when it is not one line of at most 1 KB.
func TestAcceptanceRefused(t *testing.T) {
	m := acceptanceManager(t, "")
	for _, kind := range []string{"review", "research"} {
		_, err := m.Enqueue(StartRequest{Title: "t", Kind: kind, Brief: "b", Model: "sonnet", AcceptanceCmd: "make test"})
		if err == nil || !strings.Contains(err.Error(), "acceptance_cmd does not apply to a "+kind+" job") {
			t.Fatalf("%s: %v", kind, err)
		}
		// Without a command the kind is fine.
		if _, err := m.Enqueue(StartRequest{Title: "t", Kind: kind, Brief: "b", Model: "sonnet", AcceptanceCmd: "  "}); err != nil {
			t.Fatalf("%s without acceptance: %v", kind, err)
		}
	}
	for cmd, want := range map[string]string{
		"make\ntest": "one line",
		"make\rtest": "one line",
		"make\x00":   "one line",
		// Boundary newlines are refused too, not trimmed away (nor a newline-only input into no command).
		"make test\n":  "one line",
		"\rmake test":  "one line",
		" make test\r": "one line",
		"\n":           "one line",
		"\r\n":         "one line",
		strings.Repeat("x", MaxAcceptanceCmdBytes+1): "at most 1024",
		"\xff": "UTF-8",
	} {
		if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "sonnet", AcceptanceCmd: cmd}); err == nil ||
			!strings.Contains(err.Error(), want) {
			t.Fatalf("%q: %v", cmd, err)
		}
	}
	j, err := m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "sonnet",
		AcceptanceCmd: " " + strings.Repeat("x", MaxAcceptanceCmdBytes) + " "})
	if err != nil || j.AcceptanceCmd != strings.Repeat("x", MaxAcceptanceCmdBytes) {
		t.Fatalf("1 KB command: %v", err)
	}
	if got := get(t, m, j.ID); got.AcceptanceCmd != j.AcceptanceCmd {
		t.Fatal("acceptance_cmd not stored")
	}
}

// Job schedules carry acceptance_cmd to the jobs they start (validated like job_start); a session
// schedule refuses it.
func TestAcceptanceSchedules(t *testing.T) {
	m := acceptanceManager(t, "")
	if _, err := m.AddSchedule(ScheduleRequest{Cron: "@daily", Prompt: "p", AcceptanceCmd: "make test"}); err == nil ||
		!strings.Contains(err.Error(), "acceptance_cmd") {
		t.Fatalf("session schedule: %v", err)
	}
	if _, err := m.AddSchedule(ScheduleRequest{Cron: "@daily", Prompt: "p", Kind: "review", Model: "sonnet",
		AcceptanceCmd: "make test"}); err == nil || !strings.Contains(err.Error(), "does not apply to a review job") {
		t.Fatalf("review schedule: %v", err)
	}
	sc, err := m.AddSchedule(ScheduleRequest{Cron: "* * * * *", Prompt: "p", Kind: "fix", Model: "sonnet", AcceptanceCmd: "make test"})
	if err != nil || sc.AcceptanceCmd != "make test" {
		t.Fatalf("job schedule: %+v %v", sc, err)
	}
	if err := m.fireSchedules(); err != nil {
		t.Fatal(err)
	}
	got, err := m.Store.GetSchedule(sc.ID)
	if err != nil || got.LastJobID == "" {
		t.Fatalf("schedule did not fire: %+v %v", got, err)
	}
	if j := get(t, m, got.LastJobID); j.AcceptanceCmd != "make test" {
		t.Fatalf("fired job acceptance_cmd %q", j.AcceptanceCmd)
	}
}

// A resumed job runs its acceptance command again after the resumed run.
func TestAcceptanceRerunsOnResume(t *testing.T) {
	m := acceptanceManager(t, "")
	sessionsHome(t, true)
	// Fails on attempt 1 (no attempts dir yet), passes once attempt 1 is archived.
	j := enqueue(t, m, "do it", withAcceptance(`echo run >> ../acc-runs.txt; test -d ../attempts`))
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed || !IsAcceptanceFailure(j.Error) {
		t.Fatalf("attempt 1: %s %q", j.Status, j.Error)
	}
	if _, err := m.Resume(j.ID, "fix the test", Limits{}); err != nil {
		t.Fatal(err)
	}
	if r := get(t, m, j.ID); r.AcceptanceExit != nil || r.AcceptanceStartedAt != nil {
		t.Fatalf("resume kept the previous acceptance result: %+v", r)
	}
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.Attempt != 2 || j.AcceptanceExit == nil || *j.AcceptanceExit != 0 {
		t.Fatalf("attempt 2: %s %q %+v", j.Status, j.Error, j.AcceptanceExit)
	}
	dir := m.JobDir(j.ID)
	if n := strings.Count(readFile(t, filepath.Join(dir, "acc-runs.txt")), "run"); n != 2 {
		t.Fatalf("acceptance ran %d times, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dir, attemptsDir, "1", AcceptanceLog)); err != nil {
		t.Fatalf("attempt 1 acceptance.log not archived: %v", err)
	}
}

// Once the acceptance command started, the daemon allows the job the deadline the wrapper recorded
// with the start, never less than the job's own timeout.
func TestAcceptanceDeadline(t *testing.T) {
	start := time.Unix(1760000000, 0)
	j := &store.Job{StartedAt: &start, TimeoutMinutes: 10}
	if d := jobDeadline(j); !d.Equal(start.Add(10 * time.Minute)) {
		t.Fatalf("plain deadline %s", d)
	}
	acc := acceptanceDeadline(start.Add(9*time.Minute), 60*time.Minute)
	j.AcceptanceCmd, j.AcceptanceDeadline = "make test", &acc
	if d := jobDeadline(j); !d.Equal(start.Add(69*time.Minute + acceptanceDeadlineGrace)) {
		t.Fatalf("acceptance deadline %s", d)
	}
	early := acceptanceDeadline(start.Add(time.Minute), time.Minute)
	j.AcceptanceDeadline = &early
	if d := jobDeadline(j); !d.Equal(start.Add(10 * time.Minute)) {
		t.Fatalf("the job's own deadline is never shortened: %s", d)
	}
}

func TestAcceptanceTimeoutConfig(t *testing.T) {
	dir := t.TempDir()
	for v, ok := range map[string]bool{"0": true, "1": true, "60": true, "61": false, "-1": false} {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"acceptance_timeout_minutes": `+v+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(dir)
		if (err == nil) != ok {
			t.Fatalf("acceptance_timeout_minutes %s: %v", v, err)
		}
		if v == "0" && c.AcceptanceTimeout() != 15*time.Minute {
			t.Fatalf("default %s", c.AcceptanceTimeout())
		}
	}
}
