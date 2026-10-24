package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// `hook brief-reminder` prints one reminder line for the bound session and nothing for another; it
// never blocks and never fails: every error exits 0 with nothing on stdout.
func TestHookBriefReminder(t *testing.T) {
	orch := bindableAgent(t, "test")
	opener := func(agent string) (*jobs.Manager, error) {
		if agent != "test" {
			return nil, errors.New("wrong agent " + agent)
		}
		return jobs.Open(agent)
	}
	run := func(args []string, stdin string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runHook(args, strings.NewReader(stdin), &out, &errb, opener)
		return code, out.String(), errb.String()
	}
	prompt := func(session string) string {
		b, _ := json.Marshal(map[string]string{"session_id": session, "hook_event_name": "UserPromptSubmit",
			"cwd": orch, "prompt": "hello"})
		return string(b)
	}
	reminder := []string{"brief-reminder", "--agent", "test"}
	if code, out, _ := run(reminder, prompt("s1")); code != 0 || out != "" {
		t.Fatalf("no session bound: %d %q", code, out)
	}
	if code, _, errs := run([]string{"bind", "--agent", "test"}, bindEvent("s1", orch)); code != 0 {
		t.Fatalf("bind: %d %q", code, errs)
	}
	code, out, _ := run(reminder, prompt("s1"))
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, jobs.BriefReminderPrefix+jobs.SizeRuleNote) ||
		!strings.Contains(out, "not a git work tree") {
		t.Fatalf("bound session: %d %q", code, out)
	}
	if code, out, _ := run(reminder, prompt("s2")); code != 0 || out != "" {
		t.Fatalf("another session: %d %q", code, out)
	}
	for name, c := range map[string]struct {
		args  []string
		stdin string
	}{
		"bad json":    {reminder, "{"},
		"empty":       {reminder, ""},
		"other event": {reminder, `{"hook_event_name":"Stop","session_id":"s1","stop_hook_active":false}`},
		"wrong agent": {[]string{"brief-reminder", "--agent", "nope"}, prompt("s1")},
		"positional":  {[]string{"brief-reminder", "--agent", "test", "x"}, prompt("s1")},
	} {
		code, out, errs := run(c.args, c.stdin)
		if code != 0 || out != "" || !strings.Contains(errs, "agent-jobs hook brief-reminder:") {
			t.Errorf("%s: %d %q %q", name, code, out, errs)
		}
	}
}

// Inside a job the job-role refusal ends `hook brief-reminder` with exit 0 and no output, before any
// state is opened; every other command (hook bind, status) is still refused with exit 1.
func TestHookBriefReminderInsideJob(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent-jobs")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	home := t.TempDir()
	run := func(args ...string) (int, string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "AGENT_JOBS_HOME="+home, jobs.JobEnvVar+"=review-probe")
		cmd.Stdin = strings.NewReader(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","cwd":"/"}`)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		return code, out.String(), errb.String()
	}
	if code, out, errs := run("hook", "brief-reminder", "--agent", "test"); code != 0 || out != "" || errs != "" {
		t.Errorf("brief-reminder inside a job: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	for _, args := range [][]string{{"hook", "bind", "--agent", "test"}, {"status", "--agent", "test"}} {
		if code, _, errs := run(args...); code != 1 || !strings.Contains(errs, "refused") {
			t.Errorf("%v inside a job: exit %d, stderr %q", args, code, errs)
		}
	}
	if left, err := os.ReadDir(home); err != nil || len(left) != 0 {
		t.Errorf("state opened inside a job: %v %v", left, err)
	}
}

// The brief reminder never changes the state: against a missing state dir it creates nothing; against a
// store with an older schema it runs no migration; against a store another connection holds locked it
// returns at once. Every case exits 0.
func TestHookBriefReminderReadOnly(t *testing.T) {
	orch := bindableAgent(t, "test")
	home := os.Getenv("AGENT_JOBS_HOME")
	dir, err := jobs.StateDir("test")
	if err != nil {
		t.Fatal(err)
	}
	reminder := []string{"brief-reminder", "--agent", "test"}
	b, _ := json.Marshal(map[string]string{"session_id": "s1", "hook_event_name": "UserPromptSubmit", "cwd": orch, "prompt": "hi"})
	run := func(name string) (string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		start := time.Now()
		code := runHook(reminder, bytes.NewReader(b), &out, &errb, open)
		took := time.Since(start)
		if code != 0 || took > 3*time.Second {
			t.Fatalf("%s: exit %d after %s (%q)", name, code, took, errb.String())
		}
		t.Logf("%s: %s", name, took)
		return out.String(), errb.String()
	}
	entries := func() []string {
		t.Helper()
		var names []string
		err := filepath.WalkDir(home, func(p string, _ os.DirEntry, err error) error {
			names = append(names, p)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return names
	}

	// Missing state dir: nothing is created (bindableAgent wrote only config.json).
	if err := os.Remove(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	before := entries()
	if out, _ := run("missing state dir"); out != "" {
		t.Errorf("missing state dir: stdout %q", out)
	}
	if after := entries(); !slices.Equal(before, after) {
		t.Errorf("missing state dir: files created: %v -> %v", before, after)
	}

	// An older schema (a store without the heartbeats table, the session bound): the reminder is printed,
	// no table is added.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.BindHookSession("s1", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	// tables lists the schema, optionally after dropping one table; the connection is closed again, so
	// no -wal or -shm file stays behind it.
	tables := func(drop string) []string {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(dir, "jobs.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if drop != "" {
			if _, err := db.Exec(`DROP TABLE ` + drop); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := db.Query(`SELECT name FROM sqlite_master ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			names = append(names, n)
		}
		return names
	}
	beforeTables := tables("heartbeats")
	before = entries()
	if out, _ := run("older schema"); !strings.HasPrefix(out, jobs.BriefReminderPrefix) {
		t.Errorf("older schema: no reminder for the bound session: %q", out)
	}
	// SQLite's read-only reader of a WAL database whose -wal and -shm were removed on the last close
	// recreates those two (empty) companions; nothing else may appear.
	after := slices.DeleteFunc(entries(), func(p string) bool {
		return p == filepath.Join(dir, "jobs.db-wal") || p == filepath.Join(dir, "jobs.db-shm")
	})
	if afterTables := tables(""); !slices.Equal(before, after) || !slices.Equal(beforeTables, afterTables) {
		t.Errorf("older schema: state changed: files %v -> %v, tables %v -> %v", before, after, beforeTables, afterTables)
	}

	// A full store held under an exclusive lock: the hook does not wait for it.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if m, err = jobs.Open("test"); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	lock, err := sql.Open("sqlite", filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	lock.SetMaxOpenConns(1)
	if _, err := lock.Exec(`PRAGMA locking_mode=EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(`BEGIN EXCLUSIVE; INSERT INTO hook_session (id, session_id, bound_at) VALUES (1, 's1', 0)`); err != nil {
		t.Fatal(err)
	}
	before = entries()
	if out, errs := run("locked store"); out != "" || !strings.Contains(errs, "agent-jobs hook brief-reminder:") {
		t.Errorf("locked store: %q %q", out, errs)
	}
	if after := entries(); !slices.Equal(before, after) {
		t.Errorf("locked store: files created: %v -> %v", before, after)
	}
	if _, err := lock.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}
}
