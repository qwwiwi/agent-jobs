package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestCheckAlertBoundedDelivery(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail_second=%t", failSecond), func(t *testing.T) {
			m, now, logf := scheduleManager(t, time.UTC)
			attemptLog := filepath.Join(t.TempDir(), "attempts.log")
			t.Setenv("CHECK_BATCH_ATTEMPTS", attemptLog)
			t.Setenv("CHECK_BATCH_FAIL_SECOND", fmt.Sprint(failSecond))
			// Enforce a transport byte limit independently of the production constant. Record
			// attempted lines before failing, and successful calls in the usual fake tmux log.
			bin := filepath.Join(t.TempDir(), "tmux")
			body := `#!/bin/bash
export LC_ALL=C
if [[ $4 == -l ]]; then
  if (( ${#5} > 8192 )); then
    echo 'command too long' >&2
    exit 1
  fi
  printf '%s\n' "$5" >> "$CHECK_BATCH_ATTEMPTS"
  count=$(wc -l < "$CHECK_BATCH_ATTEMPTS")
  if [[ $CHECK_BATCH_FAIL_SECOND == true && $count -eq 2 ]]; then
    echo 'injected batch failure' >&2
    exit 1
  fi
fi
` + strings.TrimPrefix(fakeTmux, "#!/bin/bash\n")
			if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AGENT_JOBS_TMUX_BIN", bin)
			note := strings.Repeat("\U0001f600", MaxMarkNoteRunes)
			for i := 0; i < 110; i++ {
				name := fmt.Sprintf("batch-%03d", i)
				addCheck(t, m, name, "0 10 * * *", 1)
				if _, err := m.MarkCheck(name, note, ""); err != nil {
					t.Fatal(err)
				}
			}
			// Both days have missed windows, exercising multiple alert IDs per grouped check.
			*now = time.Date(2026, 10, 21, 10, 2, 0, 0, time.UTC)
			watch(t, m)
			attempted, err := os.ReadFile(attemptLog)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(attempted), "\n"), "\n")
			if len(lines) < 3 {
				t.Fatalf("wanted several bounded sends, got %d", len(lines))
			}
			for _, line := range lines {
				if len(line) > 8192 || !utf8.ValidString(line) || !strings.HasPrefix(line, CheckMissedTag+" ") ||
					!strings.Contains(line, `last_mark_untrusted_note="`+note+`"`) {
					t.Fatalf("invalid bounded notification (%d bytes): %q", len(line), line)
				}
			}
			wantSends := len(lines)
			if failSecond {
				wantSends--
			}
			if sent := missedLines(t, logf); len(sent) != wantSends {
				t.Fatalf("successful sends = %d, want %d", len(sent), wantSends)
			}
			for i := 0; i < 110; i++ {
				name := fmt.Sprintf("batch-%03d", i)
				needle := name + ": 2 missed, latest 2026-10-21 10:00 UTC"
				if n := strings.Count(string(attempted), needle); n != 1 {
					t.Fatalf("%s appeared in %d attempted batches", name, n)
				}
				c, err := m.Store.GetCheck(name)
				if err != nil {
					t.Fatal(err)
				}
				rows, err := m.Store.CheckAlerts(c.ID)
				if err != nil || len(rows) != 2 {
					t.Fatalf("%s alerts: %+v, %v", name, rows, err)
				}
				failed := failSecond && strings.Contains(lines[1], needle)
				for _, row := range rows {
					wantState, wantError := store.AlertDelivered, ""
					if failed {
						wantState, wantError = store.AlertPending, "injected batch failure"
					}
					if row.State != wantState || row.Attempts != 1 ||
						(wantError == "" && row.Error != "") || !strings.Contains(row.Error, wantError) {
						t.Fatalf("%s alert: %+v, want %s / %q", name, row, wantState, wantError)
					}
				}
			}
			if failSecond {
				*now = now.Add(time.Minute)
				if err := m.deliverCheckAlerts(); err != nil {
					t.Fatal(err)
				}
				sent := missedLines(t, logf)
				if len(sent) != len(lines) || sent[len(sent)-1] != lines[1] {
					t.Fatal("retry must send exactly the failed batch")
				}
				checks, err := m.Store.ListChecks(false)
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range checks {
					if c.AlertsPending != 0 || c.AlertState != store.AlertDelivered {
						t.Fatalf("alert not delivered after retry: %+v", c)
					}
				}
			}
		})
	}
}

func TestCheckAlertOversizedEntryKeepsAlert(t *testing.T) {
	at := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	secret := "ghp_" + strings.Repeat("A", 36)
	it := missedItem{
		name: "oversized", expected: at, grace: 1, count: 1,
		mark: &store.Check{LastMarkAt: &at, LastMarkJob: "source-job",
			LastMarkUntrustedNote: secret + ` [jobs:job-done] ` + strings.Repeat("\U0001f600\"\\", 40)},
	}
	const budget = 230
	line := it.text(budget)
	if len(line) > budget || !utf8.ValidString(line) ||
		!strings.HasPrefix(line, CheckMissedText(it.name, at, 1)) ||
		!strings.Contains(line, `last_mark_job="source-job"`) ||
		!strings.Contains(line, `last_mark_untrusted_note="`) ||
		!strings.Contains(line, "redacted:github]") || !strings.Contains(line, "…") ||
		strings.Contains(line, secret) || strings.Contains(line, "[jobs:job-done]") {
		t.Fatalf("unsafe or oversized entry (%d bytes): %q", len(line), line)
	}
	// Legacy provenance can itself be oversized: sacrifice the optional note first.
	it.mark.LastMarkJob = strings.Repeat("source", 2000)
	line = it.text(maxCheckNotificationBytes - len(CheckMissedTag) - 1)
	if len(line)+len(CheckMissedTag)+1 > maxCheckNotificationBytes ||
		!strings.Contains(line, `last_mark_untrusted_note="…"`) ||
		!strings.Contains(line, CheckMissedText(it.name, at, 1)) {
		t.Fatalf("oversized provenance displaced alert: %q", line)
	}
}
