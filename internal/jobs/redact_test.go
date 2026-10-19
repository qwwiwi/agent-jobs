package jobs

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Test tokens are built from pieces, so no file of the repository holds a whole token-shaped string.
func rep(s string, n int) string { return strings.Repeat(s, n) }

func TestRedactPatterns(t *testing.T) {
	for _, c := range []struct {
		name, in, kind, keep string
	}{
		{"anthropic", "key sk-ant-" + "api03-" + rep("aB3_", 10) + " end", "anthropic", "key "},
		{"openai", "OPENAI sk-" + rep("Ab1", 12), "openai", "OPENAI "},
		{"openai project", "sk-proj-" + rep("Zx9-", 8), "openai", ""},
		{"github classic", "token ghp_" + rep("a1B2", 9), "github", "token "},
		{"github oauth", "gho_" + rep("Q", 36), "github", ""},
		{"github fine-grained", "github_pat_" + rep("11AB_", 6), "github", ""},
		{"slack", "xoxb-" + "1234567890-" + rep("ab", 8), "slack", ""},
		{"aws key id", "id AKIA" + rep("X7", 8) + ".", "aws-key-id", "id "},
		{"aws secret", "aws_secret_access_key = " + rep("wJal/+", 6) + "abcd", "aws-secret", "aws_secret_access_key = "},
		{"google", "AIza" + rep("Sy_-", 8) + "abc", "google", ""},
		{"telegram", "bot 123456789:" + rep("AAHk_-", 5) + "abcde done", "telegram", "bot 123456789"[:4]},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0." + rep("SflKxw", 3), "jwt", ""},
		{"bearer", "Authorization: Bearer " + rep("abc.DEF-", 3), "bearer", "Authorization: Bearer "},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow" + rep("Ab+/", 16) + "\n-----END RSA PRIVATE KEY-----\nafter", "private-key", "after"},
		{"pem cut off", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn" + rep("Nz", 30) + "\n" + rep("Qw", 30), "private-key", ""},
		{"generic env", "DB_PASSWORD=" + rep("hunter2!", 3), "secret", "DB_PASSWORD="},
		{"generic json", `"api_key": "` + rep("k3y", 6) + `"`, "secret", `"api_key": "`},
		{"generic token", "slack_token: " + rep("t0k", 6), "secret", "slack_token: "},
		{"generic passwd", "passwd=" + rep("p4ss", 4), "secret", "passwd="},
	} {
		out, n := Redact(c.in)
		if n == 0 || !strings.Contains(out, "[redacted:"+c.kind+"]") || !strings.Contains(out, c.keep) {
			t.Errorf("%s: %q -> %q (%d), want [redacted:%s] keeping %q", c.name, c.in, out, n, c.kind, c.keep)
		}
		// Idempotent: a redacted text is not redacted again.
		if again, k := Redact(out); k != 0 || again != out {
			t.Errorf("%s: second pass changed %q -> %q", c.name, out, again)
		}
	}
}

// What must stay: things that look random but are not secrets.
func TestRedactFalsePositives(t *testing.T) {
	for _, s := range []string{
		"commit 4c41d1544c9b3ebca9d8ee5f7ddaca6b76ce65ce",
		"base 4c41d15, ref refs/agent-jobs/jobs/261018-065424-58bd",
		"session 3f2b9c1e-7a4d-4e8f-9b6a-1c2d3e4f5a6b",
		"aGVsbG8gd29ybGQ=",                     // base64 of short text
		"dGhlIHF1aWNrIGJyb3duIGZveA==",         // base64 of short text
		"sha256:" + rep("0123456789abcdef", 4), // a digest
		"password: (none)",
		"token=short",
		"the Bearer token is passed in a header",
		"see sk-learn and sk-ant-short",
		"max_tokens: 4096",
		"-----BEGIN PUBLIC KEY-----",
		"go test -count=1 ./... PASS ok github.com/qwwiwi/agent-jobs/internal/jobs 12.3s",
		"[redacted:github]",
		"12345678:short",
		"bash task-board-" + rep("scripts-", 4),
	} {
		if out, n := Redact(s); n != 0 || out != s {
			t.Errorf("false positive: %q -> %q", s, out)
		}
	}
}

func TestRedactScanCap(t *testing.T) {
	big := rep("a", MaxRedactBytes) + " ghp_" + rep("Q", 36)
	out, n := Redact(big)
	if n == 0 || strings.Contains(out, "ghp_") || !strings.Contains(out, "unscanned") || len(out) > MaxRedactBytes+200 {
		t.Fatalf("past the cap: n=%d len=%d tail=%q", n, len(out), out[len(out)-120:])
	}
	// Pathological input stays linear (RE2): a long run of key-ish characters with no value.
	start := time.Now()
	Redact(rep("token", 200000) + rep("=", 100000))
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("redact took %v", d)
	}
}

func TestRedactStats(t *testing.T) {
	st := &store.JobStats{ResultSubtype: "Bearer " + rep("x", 20), ToolCalls: map[string]int{"Bash": 2, "mcp__x__ghp_" + rep("A", 36): 1},
		Skills: map[string]int{}, GuardRules: map[string]int{}}
	redactStats(st)
	if strings.Contains(st.ResultSubtype, rep("x", 20)) || st.ToolCalls["Bash"] != 2 || st.ToolCalls["mcp__x__[redacted:github]"] != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestDoneLineRedacted(t *testing.T) {
	j := &store.Job{ID: "id", Title: "rotate ghp_" + rep("Z", 36), Status: store.StatusSucceeded}
	if l := DoneLine(j, 0); strings.Contains(l, rep("Z", 36)) || !strings.Contains(l, "[redacted:github]") {
		t.Fatalf("done line: %q", l)
	}
}

// End to end: a job whose report carries a token ends with report.md redacted in place and no raw
// copy; with keep_raw_reports the original is kept as report.raw.md (0600).
func TestReportRedactedInPlace(t *testing.T) {
	for _, keep := range []bool{false, true} {
		m := newManager(t)
		m.Cfg.KeepRawReports = keep
		j := runToEnd(t, m, enqueue(t, m, "FAKE_SECRET_REPORT", nil).ID)
		if j.Status != store.StatusSucceeded {
			t.Fatalf("status %s: %s", j.Status, j.Error)
		}
		dir := m.JobDir(j.ID)
		b, err := os.ReadFile(filepath.Join(dir, "report.md"))
		if err != nil || strings.Contains(string(b), "ghp_") || !strings.Contains(string(b), "[redacted:github]") ||
			!strings.Contains(string(b), "## Progress") {
			t.Fatalf("keep=%v report.md: %q %v", keep, b, err)
		}
		fi, err := os.Stat(filepath.Join(dir, RawReportName))
		if keep {
			raw, _ := os.ReadFile(filepath.Join(dir, RawReportName))
			if err != nil || fi.Mode().Perm() != 0o600 || !strings.Contains(string(raw), "ghp_") {
				t.Fatalf("raw report: %v %v", fi, err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("raw report kept without keep_raw_reports: %v", err)
		}
	}
}

// A report.md that is a symlink is never followed or rewritten.
func TestRedactReportSkipsSymlink(t *testing.T) {
	m := newManager(t)
	dir := m.JobDir("x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.md")
	body := "token ghp_" + rep("Q", 36)
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
	m.redactReport("x")
	if b, _ := os.ReadFile(target); string(b) != body {
		t.Fatalf("symlink target rewritten: %q", b)
	}
}

// A Telegram token in the Bot API URL form (/bot<TOKEN>/getMe) and two tokens one space apart are
// all replaced: the boundaries around a token are not consumed.
func TestRedactTelegramBoundaries(t *testing.T) {
	tok := func(c string) string { return "123456789:" + rep(c, 35) }
	for _, in := range []string{
		"curl https://api.telegram.org/bot" + tok("A") + "/getMe",
		tok("A") + " " + tok("B"),
		tok("A") + "," + tok("B") + "\n" + tok("C"),
	} {
		out, _ := Redact(in)
		if strings.Contains(out, rep("A", 35)) || strings.Contains(out, rep("B", 35)) || strings.Contains(out, rep("C", 35)) {
			t.Errorf("telegram token left: %q -> %q", in, out)
		}
	}
	if out, _ := Redact("bot" + tok("A") + "/getMe"); out != "bot[redacted:telegram]/getMe" {
		t.Errorf("url form: %q", out)
	}
}

// A credential longer than any fixed bound is replaced whole, never only a prefix.
func TestRedactLongValuesWhole(t *testing.T) {
	long := rep("x", 1500)
	for _, c := range []struct{ name, in, tail string }{
		{"bearer", "Authorization: Bearer " + long + " end", " end"},
		{"secret", "API_KEY=" + long + " end", " end"},
		{"jwt", "eyJ" + long + "." + long + "." + long + " end", " end"},
		{"anthropic", "sk-ant-" + long + " end", " end"},
		{"openai", "sk-" + long + " end", " end"},
		{"github", "ghp_" + long + " end", " end"},
		{"github pat", "github_pat_" + long + " end", " end"},
		{"slack", "xoxb-" + long + " end", " end"},
	} {
		out, n := Redact(c.in)
		if n == 0 || strings.Contains(out, "xxxx") || !strings.HasSuffix(out, c.tail) {
			t.Errorf("%s: %d value(s), out %q", c.name, n, out)
		}
	}
}

// An oversized report is never read past the scan cap: a sparse 1 GiB report.md is redacted from its
// first MaxRedactBytes (+1) bytes and the rest becomes the unscanned note.
func TestRedactReportBoundedRead(t *testing.T) {
	m := newManager(t)
	dir := m.JobDir("big")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "report.md")
	if err := os.WriteFile(p, []byte("## Summary\nused ghp_"+rep("Q", 36)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, 1<<30); err != nil { // sparse: no disk, but 1 GiB to read
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	out, n, read, err := redactFile(f)
	_ = f.Close()
	if err != nil || read > MaxRedactBytes+1 || n < 2 || strings.Contains(out, "ghp_") ||
		!strings.Contains(out, "unscanned") || len(out) > MaxRedactBytes+200 {
		t.Fatalf("redactFile: err=%v read=%d n=%d len=%d", err, read, n, len(out))
	}
	if err := m.redactReport("big"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) > MaxRedactBytes+200 || strings.Contains(string(b), "ghp_") ||
		!strings.Contains(string(b), "unscanned") {
		t.Fatalf("report.md after redaction: %d bytes, %v", len(b), err)
	}
}

// The failure text of a job (here an error result subtype carrying a token) is redacted in the
// daemon log line of the finish.
func TestFinishLogRedacted(t *testing.T) {
	m := newManager(t)
	var buf bytes.Buffer
	m.Log = log.New(&buf, "", 0)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_SECRET_SUBTYPE", nil).ID)
	if j.Status != store.StatusFailed {
		t.Fatalf("status %s", j.Status)
	}
	if l := buf.String(); strings.Contains(l, rep("W", 36)) || !strings.Contains(l, "[redacted:github]") {
		t.Fatalf("daemon log:\n%s", l)
	}
}

// Whatever sits at the old fixed temp name (here a directory) does not stop the rewrite.
func TestRedactReportTmpInTheWay(t *testing.T) {
	m := newManager(t)
	dir := m.JobDir("x")
	if err := os.MkdirAll(filepath.Join(dir, "report.md.tmp", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "report.md")
	if err := os.WriteFile(p, []byte("token ghp_"+rep("Q", 36)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.redactReport("x"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if strings.Contains(string(b), "ghp_") || fi.Mode().Perm() != 0o600 {
		t.Fatalf("report.md: %q %v", b, fi.Mode())
	}
	if ents, _ := filepath.Glob(filepath.Join(dir, ".report.md.*.tmp")); len(ents) != 0 {
		t.Fatalf("temp files left: %v", ents)
	}
}

// A failed rewrite leaves the job's redaction pending; the next tick retries until it succeeds.
func TestRedactionPendingRetried(t *testing.T) {
	m := newManager(t)
	renameFile = func(string, string) error { return errors.New("disk says no") }
	defer func() { renameFile = os.Rename }()
	j := runToEnd(t, m, enqueue(t, m, "FAKE_SECRET_REPORT", nil).ID)
	p := filepath.Join(m.JobDir(j.ID), "report.md")
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "ghp_") {
		t.Fatalf("rewrite was meant to fail: %q", b)
	}
	if ids, err := m.Store.RedactionPending(); err != nil || len(ids) != 1 || ids[0] != j.ID {
		t.Fatalf("pending: %v %v", ids, err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if ids, _ := m.Store.RedactionPending(); len(ids) != 1 {
		t.Fatalf("pending cleared although the rewrite still fails: %v", ids)
	}
	renameFile = os.Rename
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "ghp_") || !strings.Contains(string(b), "[redacted:github]") {
		t.Fatalf("not redacted on retry: %q", b)
	}
	if ids, err := m.Store.RedactionPending(); err != nil || len(ids) != 0 {
		t.Fatalf("still pending: %v %v", ids, err)
	}
}

// A daemon that died between the row turning terminal and the rewrite: the restarted daemon's first
// tick redacts the report.
func TestRedactionPendingAfterCrash(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "x", nil)
	now := time.Now()
	if err := m.Store.Claim(j.ID, "tok", now); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(j.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(m.JobDir(j.ID), "report.md")
	if err := os.WriteFile(p, []byte("## Summary\nused ghp_"+rep("Q", 36)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The crash: Finish ran, settled never did.
	if err := m.Store.Finish(j.ID, "tok", store.StatusSucceeded, 1, 0.01, 1, "", now); err != nil {
		t.Fatal(err)
	}
	m2, err := Open("test") // the restarted daemon
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	m2.SelfBin, m2.Log = selfBin, log.New(io.Discard, "", 0)
	if err := m2.Tick(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "ghp_") {
		t.Fatalf("not redacted after restart: %q", b)
	}
	if ids, err := m2.Store.RedactionPending(); err != nil || len(ids) != 0 {
		t.Fatalf("still pending: %v %v", ids, err)
	}
}

// Readers that stop at a byte cap drop the last partial line before redacting: a value cut by the
// cap is not shown in part.
func TestRedactReadTruncated(t *testing.T) {
	s := "## Summary\nok\nAPI_KEY=" + rep("k", 10)
	if out := RedactRead(s, true); strings.Contains(out, "kkkk") || out != "## Summary\nok\n" {
		t.Fatalf("truncated: %q", out)
	}
	if out := RedactRead("ok\n", false); out != "ok\n" {
		t.Fatalf("whole: %q", out)
	}
}

// Job-derived text is redacted before it is cut: a token cut by the cap to fewer characters than its
// format needs would otherwise be stored and shown almost whole.
func TestRedactBeforeCut(t *testing.T) {
	tok := "ghp_" + rep("Q", 36)
	// The token starts 29 runes before the cut (at max-1, then "…"): 25 of its characters survive the cut.
	msg := rep("a", 269) + " " + tok + " tail"
	if out := RedactPrintable(msg, 300); strings.Contains(out, rep("Q", 10)) {
		t.Fatalf("RedactPrintable leaked a cut token: %q", out[250:])
	}
	// Control characters dropped by Printable cannot join a value's parts after the redaction.
	if out := RedactPrintable("ghp_"+rep("Q", 20)+"\x00"+rep("Q", 20), 0); strings.Contains(out, rep("Q", 10)) {
		t.Fatalf("joined token left: %q", out)
	}
	status, out := scopeStopFailed(store.StatusTimeout, msg, errors.New("unit busy"))
	if status != store.StatusTimeout || strings.Contains(RedactText(out), rep("Q", 10)) {
		t.Fatalf("timeout message: %s %q", status, out)
	}
	status, out = scopeStopFailed(store.StatusSucceeded, "", errors.New(rep("e", 200)+" "+tok))
	if status != store.StatusFailed || strings.Contains(RedactText(out), rep("Q", 10)) {
		t.Fatalf("failed message: %s %q", status, out)
	}
	j := &store.Job{ID: "id", Title: rep("t", 100) + " " + tok, Status: store.StatusSucceeded}
	if l := DoneLine(j, 0); strings.Contains(l, rep("Q", 10)) {
		t.Fatalf("done line leaked a cut title token: %q", l)
	}
}

// A text cut at a cap always loses its whole incomplete final line, however long; with no line break
// at all nothing of it is kept.
func TestRedactReadDropsWholePartialLine(t *testing.T) {
	tok := "ghp_" + rep("Q", 25) // what is left of a 40-character token cut by the read cap
	for _, c := range []struct{ name, in, want string }{
		{"no newline", "API_KEY=" + rep("k", 10), ""},
		{"no newline, long", rep("a", 100<<10) + " " + tok, ""},
		{"final line past 64 KiB", "## Summary\nok\n" + rep("a", 70<<10) + " " + tok, "## Summary\nok\n"},
	} {
		if out := RedactRead(c.in, true); out != c.want {
			t.Errorf("%s: kept %d bytes %q…, want %q", c.name, len(out), out[:min(len(out), 40)], c.want)
		}
	}
}

// An oversized text or report whose last line before the scan cap is longer than 64 KiB and holds a
// token straddling the cap: the whole line goes, nothing of the token is shown.
func TestRedactOversizedStraddlingToken(t *testing.T) {
	head := "## Summary\nok\n"
	tok := "ghp_" + rep("Q", 40)
	pad := MaxRedactBytes - len(head) - 4 - 25 - 1 // 25 token characters before the cap
	big := head + rep("a", pad) + " " + tok + "\nafter\n"
	out, _ := Redact(big)
	if strings.Contains(out, rep("Q", 10)) || !strings.HasPrefix(out, head+"\n[redacted:unscanned") {
		t.Fatalf("Redact: %d bytes, head %q", len(out), out[:min(len(out), 80)])
	}
	m := newManager(t)
	dir := m.JobDir("big")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "report.md")
	if err := os.WriteFile(p, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.redactReport("big"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), rep("Q", 10)) || !strings.HasPrefix(string(b), head+"\n[redacted:unscanned") {
		t.Fatalf("report.md: %d bytes, head %q", len(b), b[:min(len(b), 80)])
	}
	// No line break at all before the cap: only the note is left.
	if out, _ := Redact(rep("a", MaxRedactBytes-29) + " " + tok); strings.Contains(out, "aaaa") || !strings.HasPrefix(out, "\n[redacted:unscanned") {
		t.Fatalf("no line break: %d bytes, head %q", len(out), out[:min(len(out), 80)])
	}
}

// The archive never holds an unredacted report: a report whose in-place redaction is still pending
// (here: never done) is redacted on the fly into the archive, earlier attempts' reports too.
func TestArchiveRedactsPendingReport(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	old := now.Add(-49 * time.Hour)
	insertJob(t, m, "old", store.StatusSucceeded, &old, nil)
	dir := m.JobDir("old")
	fillJobDir(t, dir, t.TempDir())
	tok := "ghp_" + rep("Q", 36)
	writeFile(t, filepath.Join(dir, "report.md"), []byte("## Summary\nused "+tok+"\n"), 0o600)
	writeFile(t, filepath.Join(dir, AttemptReportName(1)), []byte("## Summary\nfirst "+tok+"\n"), 0o600)
	rep0, err := m.Archive(false)
	if err != nil || len(rep0.Jobs) != 1 {
		t.Fatalf("archive: %+v %v", rep0, err)
	}
	dest := rep0.Jobs[0].Dest
	for _, name := range []string{"report.md", AttemptReportName(1)} {
		b, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || strings.Contains(string(b), "ghp_") || !strings.Contains(string(b), "[redacted:github]") {
			t.Fatalf("archived %s: %q %v", name, b, err)
		}
	}
}

// TestRedactMainPaths: the owner-waits, notify_owner and check paths redact the job's text before
// they cut it (a secret cut short no longer matches its format) or store it.
func TestRedactMainPaths(t *testing.T) {
	secret := "ghp_" + rep("Q", 36)
	leaks := func(s string) bool { return strings.Contains(s, "ghp_") || strings.Contains(s, rep("Q", 8)) }

	title := rep("t", MaxTitleRunes-10) + " " + secret
	out := StopHookDecision(StopHookInput{HookEventName: "Stop", SessionID: "s"},
		[]*store.OwnerPending{{JobID: "j1", Title: title, Status: store.StatusSucceeded, State: "delivered"}})
	if out == nil || leaks(out.Reason) {
		t.Fatalf("stop hook reason leaks the title's secret: %+v", out)
	}

	n, err := notifyText(rep("n", MaxNotifyRunes-10) + " " + secret)
	if err != nil || leaks(n) {
		t.Fatalf("notifyText = %q, %v", n, err)
	}

	note, err := MarkNote("done " + secret)
	if err != nil || leaks(note) || !strings.Contains(note, "[redacted:github]") {
		t.Fatalf("MarkNote = %q, %v", note, err)
	}

	if _, err := CheckName("x " + secret); err == nil || leaks(err.Error()) {
		t.Fatalf("CheckName error leaks the secret: %v", err)
	}
}
