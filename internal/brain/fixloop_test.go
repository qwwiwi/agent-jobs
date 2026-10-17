package brain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A brain (or its proxy) that redirects must never get the token forwarded: the client follows no
// redirect, and the transport adds the header only for the configured endpoint.
func TestClientNeverForwardsTokenOnRedirect(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "nope", http.StatusNotFound)
	}))
	t.Cleanup(other.Close)
	target := strings.Replace(other.URL, "127.0.0.1", "localhost", 1) + "/mcp"
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	c := newClient(t, Config{RecallURL: redirector.URL + "/mcp"})
	if _, err := c.Search(context.Background(), "q", 3); err == nil {
		t.Fatal("search through a redirect succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, h := range leaked {
		if h != "" {
			t.Fatalf("token forwarded to the redirect target (len %d)", len(h))
		}
	}
}

// The transport itself refuses to attach the header to another scheme, host or port.
func TestAuthTransportOnlyForEndpoint(t *testing.T) {
	var got []string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
	})
	tr, err := newAuthTransport("https://brain.example:8443/mcp", "Bearer x", base)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"https://brain.example:8443/mcp", "http://brain.example:8443/mcp",
		"https://evil.example:8443/mcp", "https://brain.example/mcp", "https://BRAIN.example:8443/other"} {
		req, _ := http.NewRequest(http.MethodPost, u, nil)
		if _, err := tr.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"Bearer x", "", "", "", "Bearer x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("headers %q, want %q", got, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A status change that fails after the fields were set says so, in the client and in the Fake.
func TestUpdateTaskPartialFailureIsReported(t *testing.T) {
	b := &standIn{}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{
		"task_update": func(args map[string]any) *sdk.CallToolResult { return result(map[string]any{"id": 1.0}) },
		"task_done":   func(map[string]any) *sdk.CallToolResult { return toolErr("invalid transition from new") },
	})
	c := newClient(t, Config{TasksURL: url})
	_, err := c.UpdateTask(context.Background(), TaskUpdate{ID: 1, Title: "renamed", Status: "done"})
	if err == nil || !strings.Contains(err.Error(), "fields updated") || !strings.Contains(err.Error(), "invalid transition") {
		t.Fatalf("client partial update: %v", err)
	}

	f := &Fake{}
	ctx := context.Background()
	if _, err := f.CreateTask(ctx, TaskInput{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.UpdateTask(ctx, TaskUpdate{ID: 1, Title: "renamed", Status: "done"})
	if err == nil || !strings.Contains(err.Error(), "fields updated") {
		t.Fatalf("fake partial update: %v", err)
	}
	if got, _ := f.GetTask(ctx, 1); got["title"] != "renamed" || got["status"] != "new" {
		t.Fatalf("fake after partial update: %v", got)
	}
	// Without fields there is nothing partial to report.
	if _, err := f.UpdateTask(ctx, TaskUpdate{ID: 1, Status: "done"}); err == nil || strings.Contains(err.Error(), "fields updated") {
		t.Fatalf("fake status-only failure: %v", err)
	}
}

// The payload cannot override the author.
func TestSendFromCannotBeOverridden(t *testing.T) {
	b := &standIn{}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{
		"notify": func(map[string]any) *sdk.CallToolResult { return result(map[string]any{"task_id": "d1"}) },
	})
	c := newClient(t, Config{SwarmURL: url, Agent: "sa-author"})
	if _, err := c.Send(context.Background(), "sa-x", Object{"text": "t", "from": "sa-forged"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := b.last().Args["payload"].(map[string]any); p["from"] != "sa-author" {
		t.Fatalf("from = %v", p["from"])
	}
}

// A job's note carries its provenance: job tag (caller job tags dropped, never duplicated), author
// <agent>/job:<id>, a marker line; a decision is refused. ForJob is idempotent.
func TestForJobProvenance(t *testing.T) {
	in := NoteInput{Kind: KindRunbook, Title: "t", Body: "body", Tags: []string{"x", "agent-jobs-job:other", " AGENT-JOBS-JOB:forged"}}
	n, err := ForJob(in, "sa-a", "261017-120000-beef")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(n.Tags, ",") != "x,agent-jobs-job:261017-120000-beef" || n.Author != "sa-a/job:261017-120000-beef" {
		t.Fatalf("tags %v author %q", n.Tags, n.Author)
	}
	if NoteJob(Note{Snippet: n.Body}) != "261017-120000-beef" || !strings.HasSuffix(n.Body, "\n\nbody") {
		t.Fatalf("body %q", n.Body)
	}
	again, err := ForJob(n, "sa-a", "261017-120000-beef")
	if err != nil || again.Body != n.Body || strings.Join(again.Tags, ",") != strings.Join(n.Tags, ",") {
		t.Fatalf("not idempotent: %+v %v", again, err)
	}
	if _, err := ForJob(NoteInput{Kind: KindDecision, Title: "t", Body: "b", Tags: []string{"x"}}, "sa-a", "261017-120000-beef"); err == nil {
		t.Fatal("job decision accepted")
	}
	for _, id := range []string{"", "a b", "x/y", "../x"} {
		if _, err := ForJob(in, "sa-a", id); err == nil {
			t.Fatalf("job id %q accepted", id)
		}
	}
	if got := StripJobTags([]string{"a", "agent-jobs-job:1", "Agent-Jobs-Job:2"}); strings.Join(got, ",") != "a" {
		t.Fatalf("strip: %v", got)
	}
	if NoteJob(Note{Snippet: "plain note"}) != "" {
		t.Fatal("plain note marked as a job's")
	}
}

// The client sends the note's author when one is set.
func TestClientWriteAuthor(t *testing.T) {
	b := &standIn{}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{
		"create_runbook_note": func(map[string]any) *sdk.CallToolResult { return result(map[string]any{"path": "p"}) },
	})
	c := newClient(t, Config{MemoryURL: url})
	n, err := ForJob(NoteInput{Kind: KindRunbook, Title: "t", Body: "b", Tags: []string{"x"}}, "sa-test", "261017-120000-beef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if got := b.last().Args["agent"]; got != "sa-test/job:261017-120000-beef" {
		t.Fatalf("agent = %v", got)
	}
}
