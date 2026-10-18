package jobs

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// claude exits non-zero without a result event while the job appended a result line of its own to
// stream.jsonl: the daemon trusts only what the wrapper read from claude's stdout, so the attempt has
// no result (cost unknown) and the forged text is not the report.
func TestForgedResultIgnoredOnFailedExit(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_FORGE FAKE_EXIT3", nil).ID)
	dir := m.JobDir(j.ID)
	if !strings.Contains(readFile(t, filepath.Join(dir, "stream.jsonl")), "FORGED") {
		t.Fatal("the forged line must be in stream.jsonl for this test")
	}
	if j.Status != store.StatusFailed || !j.CostUnknown || j.CostUSD != 0 || j.Turns != 0 {
		t.Fatalf("row trusted the forged result: %+v", j)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "report.md")); err == nil && strings.Contains(string(b), "FORGED") {
		t.Fatalf("report.md took the forged result: %s", b)
	}
	raw, ok, err := m.Store.WrapperResult(j.ID, j.LaunchToken)
	if err != nil || !ok || !strings.Contains(raw, `"found":false`) {
		t.Fatalf("wrapper record %q %v %v", raw, ok, err)
	}
}

// A clean run: the result the wrapper recorded is the one claude wrote, and it is the row's.
func TestWrapperRecordsClaudeResult(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := runToEnd(t, m, enqueue(t, m, "plain", nil).ID)
	if j.Status != store.StatusSucceeded || j.CostUnknown || j.CostUSD != 0.25 || j.Turns != 3 {
		t.Fatalf("row: %+v", j)
	}
	// A record of another launch (token) is not this launch's result.
	if _, ok, err := m.Store.WrapperResult(j.ID, "other-token"); ok || err != nil {
		t.Fatalf("record of another launch taken: %v %v", ok, err)
	}
}

// The tee keeps the last result line, also one without a trailing newline, and skips other lines.
func TestResultTee(t *testing.T) {
	var out bytes.Buffer
	tee := &resultTee{w: &out}
	in := `{"type":"system"}` + "\n" + `{"type":"result","subtype":"success","num_turns":1,"total_cost_usd":0.5,"result":"a"}` +
		"\n" + `{"type":"assistant","message":{"content":[]}}` + "\n" + `{"type":"result","subtype":"error_max_turns","is_error":true,` +
		`"num_turns":2,"total_cost_usd":0.75}`
	for i := 0; i < len(in); i += 7 { // written in small pieces, split inside lines
		if _, err := tee.Write([]byte(in[i:min(i+7, len(in))])); err != nil {
			t.Fatal(err)
		}
	}
	tee.end()
	if out.String() != in {
		t.Fatal("the tee must pass every byte through")
	}
	if r := tee.res; !r.Found || r.Subtype != "error_max_turns" || !r.IsError || r.Turns != 2 || r.CostUSD != 0.75 {
		t.Fatalf("result: %+v", r)
	}
}

// A fallback marker the job wrote in a launch that is no resume is ignored: the planted session is not
// recorded and the next resume does not continue it.
func TestFallbackMarkerIgnoredOutsideResume(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_PLANT_FALLBACK", nil).ID)
	if j.SessionID == "11111111-1111-4111-8111-111111111111" || j.RunMode != store.RunNew {
		t.Fatalf("planted fallback session recorded: %+v", j)
	}
}

// In a resume launch the marker counts only when it names the fallback session the daemon generated.
func TestNoteFallbackOnlyDaemonSession(t *testing.T) {
	m := newManager(t)
	var logs bytes.Buffer
	m.Log = log.New(&logs, "", 0)
	orig := NewSessionID()
	j := insertJob(t, m, "fb", store.StatusRunning, nil, func(j *store.Job) {
		j.LaunchToken, j.SessionID, j.RunMode = "tok", orig, store.RunResume
	})
	daemon, planted := NewSessionID(), NewSessionID()
	if err := m.Store.SetFallbackSession(j.ID, "tok", daemon); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(m.JobDir(j.ID), FallbackMarker)
	writeFile(t, marker, []byte(planted+"\n"), 0o600)
	m.noteFallback(j)
	if g := get(t, m, j.ID); g.SessionID != orig || g.RunMode != store.RunResume || j.SessionID != orig {
		t.Fatalf("planted session taken: %+v", g)
	}
	if !strings.Contains(logs.String(), "not the fallback session generated for this launch") {
		t.Fatalf("log: %s", logs.String())
	}
	writeFile(t, marker, []byte(daemon+"\n"), 0o600)
	m.noteFallback(j)
	if g := get(t, m, j.ID); g.SessionID != daemon || g.RunMode != store.RunFallback {
		t.Fatalf("daemon's fallback session not taken: %+v", g)
	}
}

// The job's settings protect the claude project dir of its work dir (where its transcripts are) from
// the job's writes: the guard refuses a file tool there; a cut (long) project name protects all of
// <config>/projects.
func TestJobSettingsProtectSessionTranscripts(t *testing.T) {
	m := newManager(t)
	cfg := sessionsHome(t, false)
	j := insertJob(t, m, "tr", store.StatusRunning, nil, func(j *store.Job) { j.SessionID = NewSessionID() })
	proj := filepath.Join(cfg, "projects", ProjectDirName(filepath.Join(m.JobDir(j.ID), "work")))
	b, err := json.Marshal(m.JobSettings(j))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "--protect '"+proj+"'") || !strings.Contains(string(b), "Write(/"+proj+"/**)") {
		t.Fatalf("settings do not protect %s:\n%s", proj, b)
	}
	if r := guard.CheckFileWrite("Write", filepath.Join(proj, j.SessionID+".jsonl"), guard.Config{Protected: []string{proj}}); r == "" {
		t.Fatal("the guard lets a job write its transcript")
	}

	m.StateDir = filepath.Join(t.TempDir(), strings.Repeat("d", 220))
	if got := m.sessionProtected(j); len(got) != 1 || got[0] != filepath.Join(cfg, "projects") {
		t.Fatalf("long project name: %v", got)
	}
}

// The walk of a repo-less work dir before a resume skips an unreadable subdir (with a note) and is
// bounded by entries and time (past the bound the resume is refused).
func TestProjectClaudeWalkBounded(t *testing.T) {
	m := newManager(t)
	var logs bytes.Buffer
	m.Log = log.New(&logs, "", 0)
	j := insertJob(t, m, "walk", store.StatusStopped, nil, nil)
	work := filepath.Join(m.JobDir(j.ID), "work")
	for _, f := range []string{"a/1", "a/2", "b/3", "c/4"} {
		writeFile(t, filepath.Join(work, f), []byte("x"), 0o600)
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(work, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		if err := m.checkProjectClaude(j, work); err != nil {
			t.Fatalf("unreadable subdir refused the resume: %v", err)
		}
		if !strings.Contains(logs.String(), "skipped 1 unreadable dir(s) of work/") || !strings.Contains(logs.String(), "locked") {
			t.Fatalf("no note for the skipped dir: %s", logs.String())
		}
	}
	defer func(n int) { maxProjectWalkEntries = n }(maxProjectWalkEntries)
	maxProjectWalkEntries = 3
	if err := m.checkProjectClaude(j, work); err == nil || !strings.Contains(err.Error(), "too large to check") {
		t.Fatalf("entry bound: %v", err)
	}
	maxProjectWalkEntries = 1000
	defer func(d time.Duration) { projectWalkBudget = d }(projectWalkBudget)
	projectWalkBudget = -time.Second
	if err := m.checkProjectClaude(j, work); err == nil || !strings.Contains(err.Error(), "too large to check") {
		t.Fatalf("time bound: %v", err)
	}
}
