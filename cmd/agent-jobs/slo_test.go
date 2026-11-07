package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// #170: `slo record` and `slo show` against a configured norm, and `stats wakes` over what the
// store holds.
func TestSLOAndWakeStatsCLI(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("cli")
	if err != nil {
		t.Fatal(err)
	}
	cfg := `{"slo": {"targets": {"deploy": 600, "first_reply": 60}}}`
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.AddWake(store.Wake{At: time.Now(), Kind: store.WakeDelivery, Category: jobs.WakeJobDone, Bytes: 120, BatchSize: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.AddWake(store.Wake{At: time.Now(), Kind: store.WakeInjection, Category: jobs.InjectBriefReminder, Bytes: 40}); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()

	out := captureStdout(t, func() error { return cmdSLO([]string{"record", "deploy", "700", "--agent", "cli"}) })
	if !strings.Contains(out, "over the norm of 600s") {
		t.Fatalf("breach record: %q", out)
	}
	out = captureStdout(t, func() error { return cmdSLO([]string{"record", "deploy", "300", "--agent", "cli"}) })
	if !strings.Contains(out, "(norm 600s)") {
		t.Fatalf("record: %q", out)
	}
	out = captureStdout(t, func() error { return cmdSLO([]string{"show", "--agent", "cli", "--json"}) })
	var rows []jobs.SLOStat
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(rows) != 2 || rows[0].Name != "deploy" || rows[0].Count != 2 || rows[0].Last != 300 || rows[0].P95 != 700 ||
		rows[0].Target != 600 || rows[0].Over != 1 || rows[1].Name != "first_reply" || rows[1].Target != 60 {
		t.Fatalf("rows: %+v", rows)
	}
	out = captureStdout(t, func() error { return cmdSLO([]string{"show", "--agent", "cli"}) })
	for _, want := range []string{"NAME", "deploy", "first_reply", "600s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range [][]string{{"record", "deploy", "--agent", "cli"}, {"record", "deploy", "soon", "--agent", "cli"},
		{"record", "Bad Name", "5", "--agent", "cli"}, {"nope"}, {}} {
		if err := cmdSLO(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}

	out = captureStdout(t, func() error { return cmdStats([]string{"wakes", "--agent", "cli", "--json"}) })
	var rep jobs.WakeStatsReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Wakes != 1 || rep.WakeBytes != 120 || rep.InjectionBytes != 40 || len(rep.Categories) != 2 || rep.Categories[0].Members != 2 {
		t.Fatalf("report: %+v", rep)
	}
	out = captureStdout(t, func() error { return cmdStats([]string{"wakes", "--agent", "cli", "--since", "1d"}) })
	for _, want := range []string{"job-done", "hook:brief-reminder", "wakes 1 (120 bytes typed)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats wakes lacks %q:\n%s", want, out)
		}
	}
	if err := cmdStats([]string{"wakes", "extra", "--agent", "cli"}); err == nil {
		t.Fatal("accepted a positional argument")
	}
}
