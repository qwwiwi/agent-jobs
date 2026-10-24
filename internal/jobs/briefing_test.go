package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

func briefingQuestion(n int, closes ...string) map[string]any {
	return map[string]any{"n": n, "closes": closes, "kind": "cost_or_vendor", "q": "Stripe or YooKassa for payments?",
		"recommended": "YooKassa: the owner's customers pay in roubles", "why": "the payment provider stalls the launch"}
}

func briefingBlock(g *store.GateInput) map[string]any {
	return map[string]any{"schema": BriefingSchema, "checklist": ref(g),
		"rounds": []map[string]any{
			{"questions": []map[string]any{briefingQuestion(1, "R01"), briefingQuestion(2, "R02", "R03i")}},
			{"questions": []map[string]any{briefingQuestion(3, "R03i")}},
		},
		"decided":     []map[string]any{{"text": "ASSUMPTION: the login page is in Russian", "closes": []string{"R01"}}},
		"adversarial": []map[string]any{briefingQuestion(4, "R02")}}
}

// The briefing end to end: StartGate reads the draft brief files (the checklist is not approved); the
// prompt carries BRIEF.md, the rows and the briefing rules, never the brief column or memory notes; the
// job runs in an empty read-only dir that never reaches the repository; the wrapper validates the JSON
// block and stores the rounds for the checklist id, version and hash.
func TestBriefingJobEndToEnd(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	j, err := m.StartGate(GateRequest{Kind: KindBriefing, Repo: repo, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	g := j.Gate
	if j.RepoPath != "" || j.Kind != KindBriefing || g == nil || g.ChecklistID != "demo" || g.Version != 2 || g.SHA != "" ||
		!strings.Contains(g.Brief, "BRIEF_TEXT_MARKER") || len(g.Rows) != 3 || j.Title != "briefing demo v2" {
		t.Fatalf("gate job: %+v %+v", j, g)
	}
	gateReport(t, briefingBlock(g))
	f := &brain.Fake{Notes: []brain.Note{{Path: "70-runbooks/n.md", Scope: "runbooks", Score: 0.9, Snippet: "NOTE_MARKER"}}}
	m.Brain = f
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s: %s", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
	for _, s := range []string{"BRIEF_TEXT_MARKER", "RATIONALE_MARKER", "SECOND_ROW_MARKER", "Checklist (kind: briefing)", BriefingSchema,
		"OWNER_RULE_MARKER", "## Not done / uncertain", g.ChecklistSHA256, "At most 4 questions per round",
		"Round 1 comes first", "Never ask for a secret", "ASSUMPTION:", "costly_to_undo"} {
		if !strings.Contains(string(prompt), s) {
			t.Fatalf("prompt.md lacks %q:\n%s", s, prompt)
		}
	}
	if strings.Contains(string(prompt), "Gate job (kind") || strings.Contains(string(prompt), "NOTE_MARKER") || len(f.Queries) != 0 {
		t.Fatalf("prompt.md carries the brief column or memory notes (queries %q):\n%s", f.Queries, prompt)
	}
	if seen, _ := os.ReadFile(filepath.Join(dir, "work-seen.txt")); strings.TrimSpace(string(seen)) != "" {
		t.Fatalf("briefing work dir is not empty: %q", seen)
	}
	settings, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	for _, s := range []string{"--read-only", "--no-read '" + repo + "'", "--read-allow", `"Edit"`, `"WebFetch"`} {
		if !strings.Contains(string(settings), s) {
			t.Fatalf("settings.json lacks %q:\n%s", s, settings)
		}
	}
	if mcpJSON, _ := os.ReadFile(filepath.Join(dir, "mcp.json")); strings.Contains(string(mcpJSON), JobMCPServerName) {
		t.Fatalf("briefing job got an MCP server: %s", mcpJSON)
	}
	r, err := m.Store.BriefingByJob(j.ID, j.Attempt)
	if err != nil || r == nil {
		t.Fatalf("no briefing result: %v", err)
	}
	if r.Version != 2 || r.ChecklistSHA256 != g.ChecklistSHA256 || r.Repo != mainWorktree(repo) || len(r.Rounds) != 2 ||
		len(r.Rounds[0].Questions) != 2 || r.Rounds[0].Questions[1].Closes[1] != "R03i" || len(r.Decided) != 1 ||
		!strings.HasPrefix(r.Decided[0].Text, AssumptionPrefix) || len(r.Adversarial) != 1 || !r.EnforcedBlind {
		t.Fatalf("briefing result: %+v", r)
	}
	bv, err := m.BriefingOf(mainWorktree(repo), "demo", 2, g.ChecklistSHA256)
	if err != nil || bv == nil || !bv.Current || bv.Result.JobID != j.ID {
		t.Fatalf("BriefingOf current: %+v %v", bv, err)
	}
	bv, err = m.BriefingOf(mainWorktree(repo), "demo", 3, "other")
	if err != nil || bv == nil || bv.Current || bv.Result.JobID != j.ID {
		t.Fatalf("BriefingOf older draft: %+v %v", bv, err)
	}
}

// An invalid briefing block fails the job and stores nothing: an unknown field, a question that asks
// for a secret, more than 4 questions in a round, a question that closes no row, no block.
func TestBriefingInvalidResultFailsJob(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	five := []map[string]any{}
	for n := 1; n <= 5; n++ {
		five = append(five, briefingQuestion(n, "R01"))
	}
	for name, mod := range map[string]func(b map[string]any){
		"unknown field": func(b map[string]any) { b["verdict"] = "fine" },
		"unknown q field": func(b map[string]any) {
			b["rounds"].([]map[string]any)[0]["questions"].([]map[string]any)[0]["prio"] = 1
		},
		"secret": func(b map[string]any) {
			b["rounds"].([]map[string]any)[0]["questions"].([]map[string]any)[0]["q"] = "What is the admin password?"
		},
		"five questions": func(b map[string]any) { b["rounds"].([]map[string]any)[1]["questions"] = five },
		"empty closes": func(b map[string]any) {
			b["rounds"].([]map[string]any)[0]["questions"].([]map[string]any)[0]["closes"] = []string{}
		},
		"no block": func(b map[string]any) { b["schema"] = "something/else" },
	} {
		j, err := m.StartGate(GateRequest{Kind: KindBriefing, Repo: repo, Model: "sonnet"})
		if err != nil {
			t.Fatal(err)
		}
		b := briefingBlock(j.Gate)
		mod(b)
		gateReport(t, b)
		j = runToEnd(t, m, j.ID)
		if j.Status != store.StatusFailed || !strings.Contains(j.Error, "gate result rejected") {
			t.Fatalf("%s: status %s error %q", name, j.Status, j.Error)
		}
		if r, _ := m.Store.BriefingByJob(j.ID, j.Attempt); r != nil {
			t.Fatalf("%s: stored %+v", name, r)
		}
	}
}

// Every rule of ValidateBriefing has a failing case; the valid block passes with its texts redacted.
func TestValidateBriefingRules(t *testing.T) {
	g := &store.GateInput{ChecklistID: "demo", Version: 2, ChecklistSHA256: "h2",
		Rows: []store.GateRow{{ID: "R01"}, {ID: "R02"}, {ID: "R03i"}}}
	report := func(b map[string]any) string {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		return "## Findings / Changes\n\n```json\n" + string(raw) + "\n```\n"
	}
	q0 := func(b map[string]any) map[string]any {
		return b["rounds"].([]map[string]any)[0]["questions"].([]map[string]any)[0]
	}
	if o, err := ValidateBriefing(report(briefingBlock(g)), g); err != nil || len(o.Rounds) != 2 || len(o.Adversarial) != 1 {
		t.Fatalf("valid block: %+v %v", o, err)
	}
	b := briefingBlock(g)
	delete(b, "adversarial")
	if o, err := ValidateBriefing(report(b), g); err != nil || o.Adversarial == nil || len(o.Adversarial) != 0 {
		t.Fatalf("no adversarial pass: %+v %v", o, err)
	}
	eight := []map[string]any{}
	for n := 10; n < 18; n++ {
		eight = append(eight, briefingQuestion(n, "R01"))
	}
	for name, c := range map[string]struct {
		mod  func(b map[string]any)
		want string
	}{
		"unknown field":      {func(b map[string]any) { b["notes"] = "x" }, "unknown field"},
		"unknown round key":  {func(b map[string]any) { b["rounds"].([]map[string]any)[0]["round"] = 1 }, "unknown field"},
		"unknown decided":    {func(b map[string]any) { b["decided"].([]map[string]any)[0]["by"] = "me" }, "unknown field"},
		"stale version":      {func(b map[string]any) { b["checklist"].(map[string]any)["version"] = 1 }, "not the job's input"},
		"null rounds":        {func(b map[string]any) { b["rounds"] = nil }, "rounds is missing"},
		"null decided":       {func(b map[string]any) { b["decided"] = nil }, "decided is missing"},
		"empty round":        {func(b map[string]any) { b["rounds"].([]map[string]any)[1]["questions"] = []map[string]any{} }, "no questions"},
		"empty closes":       {func(b map[string]any) { q0(b)["closes"] = []string{} }, "closes no row"},
		"missing closes":     {func(b map[string]any) { delete(q0(b), "closes") }, "closes no row"},
		"unknown row":        {func(b map[string]any) { q0(b)["closes"] = []string{"R99"} }, "not a row of the checklist"},
		"row twice":          {func(b map[string]any) { q0(b)["closes"] = []string{"R01", "R01"} }, "twice"},
		"bad kind":           {func(b map[string]any) { q0(b)["kind"] = "technical" }, "must be one of"},
		"n zero":             {func(b map[string]any) { q0(b)["n"] = 0 }, "numbered from 1"},
		"n twice":            {func(b map[string]any) { q0(b)["n"] = 3 }, "used twice"},
		"empty q":            {func(b map[string]any) { q0(b)["q"] = " " }, "q is empty"},
		"no recommended":     {func(b map[string]any) { delete(q0(b), "recommended") }, "recommended is empty"},
		"no why":             {func(b map[string]any) { q0(b)["why"] = "" }, "why is empty"},
		"secret password":    {func(b map[string]any) { q0(b)["q"] = "Send me the DB Password please" }, "asks for a secret"},
		"secret token":       {func(b map[string]any) { q0(b)["q"] = "Which bot token do we use?" }, "asks for a secret"},
		"secret api key":     {func(b map[string]any) { q0(b)["q"] = "Paste the Stripe API-key" }, "asks for a secret"},
		"secret key":         {func(b map[string]any) { q0(b)["q"] = "Which key opens the server?" }, "asks for a secret"},
		"secret apikey":      {func(b map[string]any) { q0(b)["q"] = "Your OpenAI apikey?" }, "asks for a secret"},
		"secret russian":     {func(b map[string]any) { q0(b)["q"] = "Пришлите пароль от админки" }, "asks for a secret"},
		"secret recommended": {func(b map[string]any) { q0(b)["recommended"] = "share the secret in the chat" }, "asks for a secret"},
		"secret adversarial": {func(b map[string]any) { b["adversarial"].([]map[string]any)[0]["q"] = "What if the token leaks?" }, "asks for a secret"},
		"five in a round": {func(b map[string]any) {
			b["rounds"].([]map[string]any)[1]["questions"] = []map[string]any{briefingQuestion(5, "R01"), briefingQuestion(6, "R01"),
				briefingQuestion(7, "R01"), briefingQuestion(8, "R01"), briefingQuestion(9, "R01")}
		}, "at most 4 per round"},
		"eight adversarial":    {func(b map[string]any) { b["adversarial"] = eight }, "at most 7"},
		"decided no prefix":    {func(b map[string]any) { b["decided"].([]map[string]any)[0]["text"] = "the page is in Russian" }, "ASSUMPTION:"},
		"decided empty closes": {func(b map[string]any) { b["decided"].([]map[string]any)[0]["closes"] = []string{} }, "closes no row"},
		"decided unknown row":  {func(b map[string]any) { b["decided"].([]map[string]any)[0]["closes"] = []string{"G01"} }, "not a row"},
		"two blocks":           {nil, "want exactly one"},
	} {
		b := briefingBlock(g)
		text := report(b)
		if c.mod != nil {
			c.mod(b)
			text = report(b)
		} else {
			text += text
		}
		if _, err := ValidateBriefing(text, g); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v (want %q)", name, err, c.want)
		}
	}
	// Words that only contain a secret word are not one: "monkey", "keynote", "tokenizer" pass.
	b = briefingBlock(g)
	q0(b)["q"] = "Monkey test the keynote page, or the tokenizer demo?"
	if _, err := ValidateBriefing(report(b), g); err != nil {
		t.Fatalf("a word containing a secret word was refused: %v", err)
	}
}

// The briefing kind is refused through job_start (Enqueue without the daemon's input); its jobs are
// refused by job_amend and job_resume; a sha does not apply.
func TestBriefingKindRefusedOutsideBriefingStart(t *testing.T) {
	m := newManager(t)
	repo := briefRepo(t)
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: KindBriefing, Model: "sonnet", Brief: "ask the owner"}); err == nil ||
		!strings.Contains(err.Error(), "briefing_start") {
		t.Fatalf("briefing via job_start: %v", err)
	}
	j, err := m.StartGate(GateRequest{Kind: KindBriefing, Repo: repo, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Amend(j.ID, "also ask about colours"); err == nil || !strings.Contains(err.Error(), "briefing job") {
		t.Fatalf("amend: %v", err)
	}
	if _, err := m.Stop(j.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(j.ID, "here is the diff", Limits{}); err == nil || !strings.Contains(err.Error(), "briefing job") ||
		!strings.Contains(err.Error(), "briefing_start") {
		t.Fatalf("resume: %v", err)
	}
	if _, err := m.StartGate(GateRequest{Kind: KindBriefing, Repo: repo, SHA: strings.Repeat("a", 40), Model: "sonnet"}); err == nil ||
		!strings.Contains(err.Error(), "sha does not apply") {
		t.Fatalf("briefing with a sha: %v", err)
	}
	if _, err := ValidateAcceptanceCmd(KindBriefing, "true", true); err == nil {
		t.Fatal("acceptance_cmd accepted for a briefing job")
	}
}
