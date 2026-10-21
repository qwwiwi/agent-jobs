package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
)

func TestCheckCLITrustPolicyAndViews(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	var result mcp.CheckOutput
	out := captureStdout(t, func() error {
		return cmdCheck([]string{"add", "--agent", "test", "backup", "--cron", "@daily", "--allowed-schedule-ids", "7,9"})
	})
	if err := json.Unmarshal([]byte(out), &result); err != nil || !slices.Equal(result.Check.AllowedScheduleIDs, []int64{7, 9}) {
		t.Fatalf("add: %s %v", out, err)
	}
	out = captureStdout(t, func() error { return cmdCheck([]string{"set", "--agent", "test", "backup", "--grace-minutes", "30"}) })
	if err := json.Unmarshal([]byte(out), &result); err != nil || !slices.Equal(result.Check.AllowedScheduleIDs, []int64{7, 9}) {
		t.Fatalf("preserve: %s %v", out, err)
	}
	captureStdout(t, func() error { return cmdCheck([]string{"mark", "--agent", "test", "backup", "--note", "owner done"}) })
	out = captureStdout(t, func() error { return cmdChecks([]string{"--agent", "test", "--json"}) })
	if !strings.Contains(out, `"last_mark_job": "owner"`) || !strings.Contains(out, `"last_mark_untrusted_note": "owner done"`) {
		t.Fatalf("mark view: %s", out)
	}
	dir, err := jobs.StateDir("test")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	secret := "ghp_" + strings.Repeat("C", 36)
	if _, err := db.Exec(`UPDATE checks SET last_mark_note = ? WHERE name = 'backup'`, "legacy "+secret+" "+strings.Repeat("z", 500)); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"--agent", "test"}
		if asJSON {
			args = append(args, "--json")
		}
		out = captureStdout(t, func() error { return cmdChecks(args) })
		if strings.Contains(out, secret) || !strings.Contains(out, "redacted:github") || !strings.Contains(strings.ToLower(out), "untrusted_note") || !strings.Contains(out, "owner") {
			t.Fatalf("unsafe CLI: %s", out)
		}
		if asJSON {
			var list mcp.ChecksOutput
			if err := json.Unmarshal([]byte(out), &list); err != nil {
				t.Fatal(err)
			}
			if utf8.RuneCountInString(list.Checks[0].LastMarkUntrustedNote) != jobs.MaxMarkNoteRunes {
				t.Fatalf("read cap: %s", out)
			}
		}
	}
	out = captureStdout(t, func() error {
		return cmdCheck([]string{"set", "--agent", "test", "backup", "--allowed-schedule-ids", ""})
	})
	if err := json.Unmarshal([]byte(out), &result); err != nil || len(result.Check.AllowedScheduleIDs) != 0 {
		t.Fatalf("clear: %s %v", out, err)
	}
	for _, invalid := range []string{"0", "-1", "1,nope", "1,"} {
		if err := cmdCheck([]string{"set", "--agent", "test", "backup", "--allowed-schedule-ids", invalid}); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}
