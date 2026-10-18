package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// `hook stop` prints the block decision while an owner reply is pending, nothing otherwise; with
// stop_hook_active it allows without opening the store; every error fails open with exit 1 (never
// exit 2, which would block the stop).
func TestHookStop(t *testing.T) {
	orch := bindableAgent(t, "test")
	opens := 0
	opener := func(agent string) (*jobs.Manager, error) {
		opens++
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
	stop := []string{"stop", "--agent", "test"}
	const event = `{"session_id":"s1","hook_event_name":"Stop","stop_hook_active":false}`

	if code, out, _ := run(stop, event); code != 0 || out != "" {
		t.Fatalf("nothing pending: %d %q", code, out)
	}

	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Enqueue(jobs.StartRequest{Title: "owner job", Kind: "general", Brief: "b", Model: "sonnet", OwnerWaits: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.MarkNotified(j.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	if code, _, errs := run([]string{"bind", "--agent", "test"}, bindEvent("s1", orch)); code != 0 {
		t.Fatalf("bind: %d %q", code, errs)
	}
	code, out, _ := run(stop, event)
	var dec jobs.StopHookOutput
	if code != 0 || json.Unmarshal([]byte(out), &dec) != nil || dec.Decision != "block" ||
		!strings.Contains(dec.Reason, "job "+j.ID+" «owner job»") {
		t.Fatalf("pending: %d %q", code, out)
	}

	before := opens
	if code, out, _ := run(stop, `{"session_id":"s1","hook_event_name":"Stop","stop_hook_active":true}`); code != 0 || out != "" || opens != before {
		t.Fatalf("stop_hook_active: %d %q opens %d→%d", code, out, before, opens)
	}

	for name, c := range map[string]struct {
		args  []string
		stdin string
	}{
		"bad json":       {stop, "{"},
		"empty input":    {stop, ""},
		"null":           {stop, "null"},
		"empty object":   {stop, "{}"},
		"no session":     {stop, `{"hook_event_name":"Stop","stop_hook_active":false}`},
		"no active flag": {stop, `{"session_id":"s1","hook_event_name":"Stop"}`},
		"no event":       {stop, `{"session_id":"s1","stop_hook_active":false}`},
		"no subcommand":  {nil, event},
		"wrong agent":    {[]string{"stop", "--agent", "other"}, event},
		"positional arg": {append(stop, "x"), event},
	} {
		if code, out, errs := run(c.args, c.stdin); code != 1 || out != "" || !strings.Contains(errs, "the stop is allowed") {
			t.Fatalf("%s: %d %q %q", name, code, out, errs)
		}
	}

	if _, err := m.OwnerReplied(""); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(stop, event); code != 0 || out != "" {
		t.Fatalf("after owner_replied: %d %q", code, out)
	}
}

func bindEvent(session, cwd string) string {
	b, _ := json.Marshal(map[string]string{"session_id": session, "hook_event_name": "SessionStart",
		"source": "startup", "cwd": cwd})
	return string(b)
}

// bindableAgent configures agent so `hook bind` can succeed: a notify tmux target whose pane a fake
// tmux resolves to %7, $TMUX_PANE=%7, and an orchestrator_dir. It returns the orchestrator dir.
func bindableAgent(t *testing.T, agent string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	orch := filepath.Join(t.TempDir(), "orch")
	if err := os.MkdirAll(filepath.Join(orch, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeAgentConfig(t, agent, map[string]any{"notify_tmux_target": "orch-session", "orchestrator_dir": orch})
	bin := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\ncase \"$*\" in\n*display-message*-t\\ orch-session*) echo %7 ;;\n*display-message*) echo \"no such target\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_JOBS_TMUX_BIN", bin)
	t.Setenv("TMUX_PANE", "%7")
	return orch
}

func writeAgentConfig(t *testing.T, agent string, cfg map[string]any) {
	t.Helper()
	dir, err := jobs.StateDir(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The Stop hook holds only the orchestrator session bound by `hook bind` (its SessionStart hook):
// before any bind every session is allowed, after it only the bound one is blocked, and a later bind
// (a new session, /clear) moves the binding. A bind with incomplete input fails and keeps the binding.
func TestHookStopSessionBinding(t *testing.T) {
	orch := bindableAgent(t, "test")
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Enqueue(jobs.StartRequest{Title: "owner job", Kind: "general", Brief: "b", Model: "sonnet", OwnerWaits: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.MarkNotified(j.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	run := func(args []string, stdin string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runHook(args, strings.NewReader(stdin), &out, &errb, jobs.Open)
		return code, out.String(), errb.String()
	}
	stopEvent := func(session string) string {
		return `{"session_id":"` + session + `","hook_event_name":"Stop","stop_hook_active":false}`
	}
	stop := []string{"stop", "--agent", "test"}
	bind := []string{"bind", "--agent", "test"}
	blocks := func(session string) bool {
		t.Helper()
		code, out, errs := run(stop, stopEvent(session))
		if code != 0 {
			t.Fatalf("stop %s: %d %q", session, code, errs)
		}
		return strings.Contains(out, `"decision":"block"`)
	}

	if blocks("s1") || blocks("s2") {
		t.Fatal("no session bound: every stop must be allowed")
	}
	if code, out, errs := run(bind, bindEvent("s1", orch)); code != 0 || out != "" {
		t.Fatalf("bind s1: %d %q %q", code, out, errs)
	}
	if !blocks("s1") || blocks("s2") {
		t.Fatal("bound to s1: only s1 is held")
	}
	for name, in := range map[string]string{
		"empty":      "",
		"no session": `{"hook_event_name":"SessionStart"}`,
		"stop event": stopEvent("s2"),
	} {
		if code, out, errs := run(bind, in); code != 1 || out != "" || !strings.Contains(errs, "agent-jobs hook bind") {
			t.Fatalf("bind %s: %d %q %q", name, code, out, errs)
		}
	}
	if !blocks("s1") || blocks("s2") {
		t.Fatal("a failed bind must keep the binding")
	}
	if code, _, errs := run(bind, bindEvent("s2", orch)); code != 0 {
		t.Fatalf("bind s2: %d %q", code, errs)
	}
	if blocks("s1") || !blocks("s2") {
		t.Fatal("rebound to s2: only s2 is held")
	}
}

// `status` without a job id ends with the owner replies pending.
func TestStatusShowsOwnerPending(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if out := captureStdout(t, func() error { return printOwnerPending(m) }); !strings.Contains(out, "no owner replies pending") {
		t.Fatalf("none:\n%s", out)
	}
	j, err := m.Enqueue(jobs.StartRequest{Title: "owner job", Kind: "general", Brief: "b", Model: "sonnet", OwnerWaits: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.MarkNotified(j.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error { return printOwnerPending(m) })
	if !strings.Contains(out, "1 owner replies pending (expire after 6h0m0s") || !strings.Contains(out, "job "+j.ID+" queued, line delivered, since") ||
		!strings.Contains(out, "no orchestrator session bound") {
		t.Fatalf("pending:\n%s", out)
	}
	if err := m.Store.BindHookSession("sess-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() error { return printOwnerPending(m) }); !strings.Contains(out, "the Stop hook holds session sess-1") {
		t.Fatalf("bound:\n%s", out)
	}
}

// `hook bind` binds only a session that runs in the notify tmux pane, with its cwd inside
// orchestrator_dir, outside any job. Every refusal keeps the existing binding, so a rotation needs a
// new session in the same pane and directory.
func TestHookBindProvenance(t *testing.T) {
	orch := bindableAgent(t, "test")
	run := func(opener func(string) (*jobs.Manager, error), stdin string) (int, string) {
		var out, errb bytes.Buffer
		code := runHook([]string{"bind", "--agent", "test"}, strings.NewReader(stdin), &out, &errb, opener)
		if out.Len() != 0 {
			t.Fatalf("bind printed %q", out.String())
		}
		return code, errb.String()
	}
	bound := func() string {
		t.Helper()
		m, err := jobs.Open("test")
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		id, _, err := m.Store.HookSession()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	refused := func(name, stdin, want string) {
		t.Helper()
		before := bound()
		code, errs := run(jobs.Open, stdin)
		if code != 1 || !strings.Contains(errs, "no session bound") || !strings.Contains(errs, want) {
			t.Fatalf("%s: %d %q (want %q)", name, code, errs, want)
		}
		if got := bound(); got != before {
			t.Fatalf("%s: binding changed %q -> %q", name, before, got)
		}
	}

	if code, errs := run(jobs.Open, bindEvent("s1", filepath.Join(orch, "sub"))); code != 0 || bound() != "s1" {
		t.Fatalf("bind from the pane and a subdirectory: %d %q", code, errs)
	}

	t.Setenv("TMUX_PANE", "%8")
	refused("other pane", bindEvent("s2", orch), "not the notify pane")
	t.Setenv("TMUX_PANE", "")
	refused("no pane", bindEvent("s2", orch), "TMUX_PANE")
	t.Setenv("TMUX_PANE", "%7")

	refused("other directory", bindEvent("s2", t.TempDir()), "not inside orchestrator_dir")
	refused("sibling with the same prefix", bindEvent("s2", orch+"-evil"), "orchestrator_dir")
	refused("parent directory", bindEvent("s2", filepath.Dir(orch)), "not inside orchestrator_dir")
	refused("relative cwd", bindEvent("s2", "orch"), "orchestrator_dir")
	refused("no cwd", `{"session_id":"s2","hook_event_name":"SessionStart"}`, "cwd")

	// A symlink into the orchestrator dir resolves inside; one out of it does not.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	refused("symlink out", bindEvent("s2", link), "not inside orchestrator_dir")

	// A job session (the job-session exclusion of open()): refused even from the pane and dir.
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Enqueue(jobs.StartRequest{Title: "job", Kind: "general", Brief: "b", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := unix.Getsid(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(j.ID, "tok", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPID(j.ID, "tok", 1, sid); err != nil {
		t.Fatal(err)
	}
	if code, errs := run(open, bindEvent("s2", orch)); code != 1 || !strings.Contains(errs, "no session bound") || bound() != "s1" {
		t.Fatalf("job session: %d %q", code, errs)
	}
	if err := m.Store.Finish(j.ID, "tok", store.StatusSucceeded, 0, 0, 0, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	t.Setenv(jobs.JobEnvVar, "j1")
	if reason := refuseInsideJob("hook", []string{"bind", "--agent", "test"}); reason == "" {
		t.Fatal("hook bind inside a job must be refused")
	}
	os.Unsetenv(jobs.JobEnvVar)

	// Rotation: a new session in the same pane and directory rebinds.
	if code, errs := run(jobs.Open, bindEvent("s3", orch)); code != 0 || bound() != "s3" {
		t.Fatalf("rotation: %d %q", code, errs)
	}

	// No tmux target or no orchestrator_dir: binding is refused, so the Stop hook never blocks.
	writeAgentConfig(t, "test", map[string]any{"orchestrator_dir": orch})
	refused("no tmux target", bindEvent("s4", orch), "notify_tmux_target")
	writeAgentConfig(t, "test", map[string]any{"notify_tmux_target": "orch-session"})
	refused("no orchestrator_dir", bindEvent("s4", orch), "orchestrator_dir")
	writeAgentConfig(t, "test", map[string]any{"notify_tmux_target": "missing", "orchestrator_dir": orch})
	refused("unresolvable target", bindEvent("s4", orch), "tmux")
}
