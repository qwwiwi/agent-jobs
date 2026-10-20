package jobs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// acceptanceTok is a GitHub-shaped token; its body piece shows a leak of any part of it.
const acceptanceTokBody = "a1B2a1B2a1B2a1B2a1B2a1B2a1B2a1B2a1B2"

func acceptanceTok() string { return "ghp" + "_" + acceptanceTokBody }

// leaksTok reports whether s holds any 8-byte piece of the token body.
func leaksTok(s string) bool {
	for i := 0; i+8 <= len(acceptanceTokBody); i++ {
		if strings.Contains(s, acceptanceTokBody[i:i+8]) {
			return true
		}
	}
	return strings.Contains(s, "ghp"+"_")
}

func writeAcceptanceLog(t *testing.T, body string) string {
	t.Helper()
	state := t.TempDir()
	dir := filepath.Join(state, "jobs", "j1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, AcceptanceLog), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return state
}

// A token in acceptance output is redacted in the tail.
func TestAcceptanceLogTailRedacted(t *testing.T) {
	state := writeAcceptanceLog(t, "running\nexport TOKEN "+acceptanceTok()+"\nFAIL\n")
	tail, err := ReadAcceptanceLogTail(state, "j1", 1<<10)
	if err != nil || leaksTok(tail) || !strings.Contains(tail, "[redacted:github]") || !strings.HasSuffix(tail, "FAIL\n") {
		t.Fatalf("tail %q %v", tail, err)
	}
}

// A token straddling the tail cut is redacted before the cut, so no part of it is shown.
func TestAcceptanceLogTailCutAfterRedaction(t *testing.T) {
	line := "key " + acceptanceTok() + "\n"
	after := strings.Repeat("y", 100) + "\nEND\n"
	state := writeAcceptanceLog(t, "start\n"+line+after)
	// The cut falls inside the token.
	tail, err := ReadAcceptanceLogTail(state, "j1", int64(len(after)+20))
	if err != nil || leaksTok(tail) || !strings.HasSuffix(tail, "END\n") {
		t.Fatalf("tail %q %v", tail, err)
	}
}

// A token straddling the read cap (the file is larger than what is read) is not shown in part:
// the incomplete first line of the read window is dropped.
func TestAcceptanceLogTailReadCap(t *testing.T) {
	tok := acceptanceTok()
	// The read window starts 20 bytes before the end of the token.
	after := "\n" + strings.Repeat("z", acceptanceReadCap-20-5) + "\nEND\n"
	body := strings.Repeat("p", 100) + " " + tok + after
	if off := len(body) - acceptanceReadCap; off <= 101 || off >= 101+len(tok) {
		t.Fatalf("the read cap at %d is not inside the token", off)
	}
	state := writeAcceptanceLog(t, body)
	tail, err := ReadAcceptanceLogTail(state, "j1", int64(acceptanceReadCap))
	if err != nil || leaksTok(tail) || !strings.HasSuffix(tail, "END\n") {
		t.Fatalf("tail starts %q, err %v", tail[:min(len(tail), 80)], err)
	}
}

// The log keeps the last AcceptanceLogBytes of the output; a line cut by that is dropped whole, so a
// token straddling the cut leaves no part of itself in the log.
func TestAcceptanceLogHeadCut(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "jobs", "j1")
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	// After the token: "\n", k bytes, "\nEND\n"; the kept tail starts 20 bytes before the token's end.
	k := AcceptanceLogBytes - 20 - 1 - 5
	cmd := "printf 'x ghp" + "_%s\\n' " + acceptanceTokBody + "; head -c " + strconv.Itoa(k) + " /dev/zero | tr '\\0' a; printf '\\nEND\\n'"
	if exit, _ := runAcceptance(context.Background(), dir, cmd, time.Minute, io.Discard, nil); exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	log := readFile(t, filepath.Join(dir, AcceptanceLog))
	if leaksTok(log) || !strings.HasPrefix(log, "[... ") || !strings.HasSuffix(log, "END\n") {
		t.Fatalf("log starts %q", log[:min(len(log), 80)])
	}
}

// The acceptance note on the done line is redacted before it is cut.
func TestAcceptanceNoteRedacted(t *testing.T) {
	now := time.Now()
	j := &store.Job{ID: "261018-100000-note", Title: "t", Kind: "fix", Status: store.StatusFailed, Model: "sonnet",
		CreatedAt: now, StartedAt: &now, FinishedAt: &now, AcceptanceCmd: "make test",
		Error: "acceptance did not run " + strings.Repeat("x", 50) + " " + acceptanceTok() + " (see stderr.log)"}
	if n := acceptanceNote(j); leaksTok(n) || !strings.HasPrefix(n, "acceptance did not run") {
		t.Fatalf("note %q", n)
	}
	if line := DoneLine(j, 0); leaksTok(line) {
		t.Fatalf("done line: %s", line)
	}
}
