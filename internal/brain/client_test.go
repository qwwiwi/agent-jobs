package brain

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "test-token-not-a-secret"

// call is one tool call the stand-in brain received.
type call struct {
	Tool string
	Args map[string]any
}

// standIn is a local MCP streamable-HTTP server shaped like the shared brain: tools return their
// result the way the brain does (an object as is, a list or null wrapped as {"result": …}), and
// every request must carry the bearer token.
type standIn struct {
	mu    sync.Mutex
	calls []call
	auths []string
	tasks map[int64]map[string]any
	delay time.Duration
}

func (b *standIn) record(tool string, args map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call{tool, args})
}

func (b *standIn) last() call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[len(b.calls)-1]
}

func result(v any) *sdk.CallToolResult {
	txt, _ := json.Marshal(v)
	r := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(txt)}}}
	if m, ok := v.(map[string]any); ok {
		r.StructuredContent = m
	} else {
		r.StructuredContent = map[string]any{"result": v}
	}
	return r
}

func toolErr(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: msg}}}
}

// serve starts the stand-in with the given tools and returns its URL.
func (b *standIn) serve(t *testing.T, tools map[string]func(args map[string]any) *sdk.CallToolResult) string {
	t.Helper()
	s := sdk.NewServer(&sdk.Implementation{Name: "brain-stand-in", Version: "0"}, nil)
	for name, h := range tools {
		s.AddTool(&sdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				var args map[string]any
				if len(req.Params.Arguments) > 0 {
					if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
						return nil, err
					}
				}
				b.record(name, args)
				if b.delay > 0 {
					select {
					case <-time.After(b.delay):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return h(args), nil
			})
	}
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.auths = append(b.auths, r.Header.Get("Authorization"))
		b.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/mcp"
}

func tokenFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func newClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	if cfg.TokenFile == "" && cfg.TokenEnv == "" {
		cfg.TokenFile = tokenFile(t, testToken+"\n", 0o600)
	}
	b, err := New(cfg, "sa-test")
	if err != nil {
		t.Fatal(err)
	}
	c, ok := b.(*Client)
	if !ok {
		t.Fatalf("New returned %T", b)
	}
	return c
}

func TestNewWithoutEndpointsIsDisabled(t *testing.T) {
	b, err := New(Config{}, "a")
	if err != nil || !IsDisabled(b) {
		t.Fatalf("empty config: %T %v", b, err)
	}
	ctx := context.Background()
	if _, err := b.Search(ctx, "q", 1); !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "brain not configured") {
		t.Fatalf("disabled search: %v", err)
	}
	if err := b.Ack(ctx, "x"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("disabled ack: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{RecallURL: "https://brain.example/recall/mcp", TokenEnv: "BRAIN_TOKEN"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		mod  func(*Config)
		want string
	}{
		{"no token source", func(c *Config) { c.TokenEnv = "" }, "token_file or token_env is required"},
		{"both token sources", func(c *Config) { c.TokenFile = "/x" }, "not both"},
		{"bad env name", func(c *Config) { c.TokenEnv = "A B" }, "not an environment variable name"},
		{"relative token file", func(c *Config) { c.TokenEnv, c.TokenFile = "", "token" }, "absolute"},
		{"http off loopback", func(c *Config) { c.TasksURL = "http://brain.example/task/mcp" }, "loopback"},
		{"token in url", func(c *Config) { c.SwarmURL = "https://brain.example/swarm/mcp?token=x" }, "credentials"},
		{"user in url", func(c *Config) { c.SwarmURL = "https://u:p@brain.example/swarm/mcp" }, "credentials"},
		{"ftp", func(c *Config) { c.MemoryURL = "ftp://brain.example/" }, "https"},
		{"timeout", func(c *Config) { c.TimeoutSeconds = 301 }, "timeout_seconds"},
		{"bad agent", func(c *Config) { c.Agent = "-x" }, "agent"},
	} {
		cfg := ok
		c.mod(&cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if err := (Config{TokenEnv: "X"}).Validate(); err == nil {
		t.Fatal("token without endpoint accepted")
	}
	if err := (Config{RecallURL: "http://127.0.0.1:1/mcp", TokenEnv: "X"}).Validate(); err != nil {
		t.Fatalf("loopback http refused: %v", err)
	}
}

func recallTool(args map[string]any) *sdk.CallToolResult {
	n := int(args["limit"].(float64))
	all := []any{
		map[string]any{"path": "30-decisions/a.md", "scope": "decisions", "source_type": "vault", "score": 0.9, "snippet": "A"},
		map[string]any{"path": "70-runbooks/b.md", "scope": "runbooks", "source_type": "vault", "score": 0.4, "snippet": "B"},
	}
	if n < len(all) {
		all = all[:n]
	}
	return result(all)
}

func TestClientSearchSendsTokenAndDecodesNotes(t *testing.T) {
	b := &standIn{}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{"recall": recallTool})
	c := newClient(t, Config{RecallURL: url})
	notes, err := c.Search(context.Background(), "jobs", 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 || notes[0].Path != "30-decisions/a.md" || notes[0].Score != 0.9 || notes[1].Scope != "runbooks" {
		t.Fatalf("notes: %+v", notes)
	}
	if got := b.last(); got.Tool != "recall" || got.Args["query"] != "jobs" || got.Args["limit"] != float64(6) {
		t.Fatalf("recall call: %+v", got)
	}
	for _, a := range b.auths {
		if a != "Bearer "+testToken {
			t.Fatalf("request without the token: %q", a)
		}
	}
	// A token already carrying the scheme (copied from an MCP config) is not doubled.
	c2 := newClient(t, Config{RecallURL: url, TokenFile: tokenFile(t, "Bearer "+testToken, 0o600)})
	if _, err := c2.Search(context.Background(), "x", 1); err != nil {
		t.Fatalf("Bearer-prefixed token: %v", err)
	}
	// Token from the environment.
	t.Setenv("AGENT_JOBS_TEST_BRAIN_TOKEN", testToken)
	c3 := newClient(t, Config{RecallURL: url, TokenEnv: "AGENT_JOBS_TEST_BRAIN_TOKEN"})
	if n, err := c3.Search(context.Background(), "x", 1); err != nil || len(n) != 1 {
		t.Fatalf("env token: %v %v", n, err)
	}
}

func TestClientTokenErrorsNeverLeakTheToken(t *testing.T) {
	b := &standIn{}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{"recall": recallTool})
	ctx := context.Background()
	wrong := newClient(t, Config{RecallURL: url, TokenFile: tokenFile(t, "wrong-token-value", 0o600)})
	_, err := wrong.Search(ctx, "q", 1)
	if err == nil || strings.Contains(err.Error(), "wrong-token-value") {
		t.Fatalf("wrong token: %v", err)
	}
	open := newClient(t, Config{RecallURL: url, TokenFile: tokenFile(t, testToken, 0o644)})
	if _, err := open.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), testToken) {
		t.Fatalf("world-readable token file: %v", err)
	}
	unset := newClient(t, Config{RecallURL: url, TokenEnv: "AGENT_JOBS_TEST_UNSET_TOKEN"})
	if _, err := unset.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "AGENT_JOBS_TEST_UNSET_TOKEN") {
		t.Fatalf("unset env: %v", err)
	}
	multi := newClient(t, Config{RecallURL: url, TokenFile: tokenFile(t, "a\nb", 0o600)})
	if _, err := multi.Search(ctx, "q", 1); err == nil || strings.Contains(err.Error(), "a\nb") {
		t.Fatalf("two-line token: %v", err)
	}
}

func TestClientMissingEndpointAndToolError(t *testing.T) {
	b := &standIn{}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{
		"recall": func(map[string]any) *sdk.CallToolResult { return toolErr("index offline") },
	})
	c := newClient(t, Config{RecallURL: url})
	ctx := context.Background()
	if _, err := c.CreateTask(ctx, TaskInput{Title: "t"}); !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "tasks_url") {
		t.Fatalf("no tasks endpoint: %v", err)
	}
	if _, err := c.Search(ctx, "q", 1); err == nil || !strings.Contains(err.Error(), "index offline") {
		t.Fatalf("tool error: %v", err)
	}
}

func TestClientTimeout(t *testing.T) {
	b := &standIn{delay: 5 * time.Second}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{"recall": recallTool})
	c := newClient(t, Config{RecallURL: url})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Search(ctx, "q", 1); err == nil {
		t.Fatal("slow brain answered")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout not honoured: %s", d)
	}
}

func TestClientWriteNoteKinds(t *testing.T) {
	b := &standIn{}
	ok := func(args map[string]any) *sdk.CallToolResult {
		return result(map[string]any{"path": "x.md", "title": args["title"]})
	}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{
		"create_decision_note": ok, "create_runbook_note": ok, "create_error_pattern_note": ok,
	})
	c := newClient(t, Config{MemoryURL: url})
	ctx := context.Background()
	o, err := c.Write(ctx, NoteInput{Kind: KindDecision, Title: "D", Body: "b", Tags: []string{"t"}, Related: []string{"r.md"}})
	if err != nil || o["path"] != "x.md" {
		t.Fatalf("decision: %v %v", o, err)
	}
	if got := b.last(); got.Tool != "create_decision_note" || got.Args["agent"] != "sa-test" || got.Args["related"] == nil {
		t.Fatalf("decision call: %+v", got)
	}
	if _, err := c.Write(ctx, NoteInput{Kind: KindRunbook, Title: "R", Body: "b", Tags: []string{"t"}}); err != nil {
		t.Fatal(err)
	}
	if got := b.last(); got.Tool != "create_runbook_note" || got.Args["related"] != nil {
		t.Fatalf("runbook call: %+v", got)
	}
	ep := NoteInput{Kind: KindErrorPattern, Title: "E", Body: "b", Tags: []string{"t"}, Category: "c", Severity: "high",
		TriggerCondition: "when", PreventionRule: "do"}
	if _, err := c.Write(ctx, ep); err != nil {
		t.Fatal(err)
	}
	if got := b.last(); got.Tool != "create_error_pattern_note" || got.Args["prevention_rule"] != "do" || got.Args["severity"] != "high" {
		t.Fatalf("error pattern call: %+v", got)
	}
	calls := len(b.calls)
	for _, bad := range []NoteInput{
		{Kind: "essay", Title: "x", Body: "b", Tags: []string{"t"}},
		{Kind: KindRunbook, Title: "x", Body: "b"},
		{Kind: KindErrorPattern, Title: "x", Body: "b", Tags: []string{"t"}},
		{Kind: KindRunbook, Title: "x", Body: "b", Tags: []string{"t"}, Severity: "high"},
		{Kind: KindRunbook, Title: "x", Body: "b", Tags: []string{"t"}, Related: []string{"r"}},
	} {
		if _, err := c.Write(ctx, bad); err == nil {
			t.Fatalf("invalid note accepted: %+v", bad)
		}
	}
	if len(b.calls) != calls {
		t.Fatal("an invalid note reached the brain")
	}
}

func TestClientTasks(t *testing.T) {
	b := &standIn{tasks: map[int64]map[string]any{}}
	get := func(args map[string]any) map[string]any {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.tasks[int64(args["task_id"].(float64))]
	}
	status := func(to string) func(map[string]any) *sdk.CallToolResult {
		return func(args map[string]any) *sdk.CallToolResult {
			task := get(args)
			if task == nil {
				return toolErr("task not found")
			}
			b.mu.Lock()
			task["status"] = to
			b.mu.Unlock()
			return result(task)
		}
	}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{
		"task_create": func(args map[string]any) *sdk.CallToolResult {
			b.mu.Lock()
			defer b.mu.Unlock()
			id := int64(len(b.tasks) + 1)
			task := map[string]any{"id": float64(id), "title": args["title"], "status": "new", "priority": "medium"}
			b.tasks[id] = task
			return result(task)
		},
		"task_list": func(map[string]any) *sdk.CallToolResult {
			b.mu.Lock()
			defer b.mu.Unlock()
			out := []any{}
			for i := int64(1); i <= int64(len(b.tasks)); i++ {
				out = append(out, b.tasks[i])
			}
			return result(out)
		},
		"task_get": func(args map[string]any) *sdk.CallToolResult {
			if task := get(args); task != nil {
				return result(task)
			}
			return result(nil)
		},
		"task_update": func(args map[string]any) *sdk.CallToolResult {
			task := get(args)
			b.mu.Lock()
			task["title"] = args["title"]
			b.mu.Unlock()
			return result(task)
		},
		"task_start": status("progress"), "task_block": status("blocked"),
	})
	c := newClient(t, Config{TasksURL: url})
	ctx := context.Background()
	created, err := c.CreateTask(ctx, TaskInput{Title: "first"})
	if err != nil || created["id"] != float64(1) {
		t.Fatalf("create: %v %v", created, err)
	}
	if got := b.last(); got.Args["assignee"] != nil || got.Args["priority"] != nil {
		t.Fatalf("empty fields sent (would override brain defaults): %+v", got)
	}
	list, err := c.ListTasks(ctx, TaskFilter{Status: "new", Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if got := b.last(); got.Args["status"] != "new" || got.Args["limit"] != float64(10) {
		t.Fatalf("list call: %+v", got)
	}
	up, err := c.UpdateTask(ctx, TaskUpdate{ID: 1, Title: "renamed", Status: "progress", Note: "go"})
	if err != nil || up["title"] != "renamed" || up["status"] != "progress" {
		t.Fatalf("update: %v %v", up, err)
	}
	var tools []string
	for _, cl := range b.calls[len(b.calls)-3:] {
		tools = append(tools, cl.Tool)
	}
	if strings.Join(tools, ",") != "task_update,task_start,task_get" || b.calls[len(b.calls)-2].Args["note"] != "go" {
		t.Fatalf("update calls: %v %+v", tools, b.calls[len(b.calls)-2])
	}
	if _, err := c.UpdateTask(ctx, TaskUpdate{ID: 1, Status: "blocked", Note: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if got := b.calls[len(b.calls)-2]; got.Tool != "task_block" || got.Args["reason"] != "waiting" {
		t.Fatalf("block call: %+v", got)
	}
	if _, err := c.UpdateTask(ctx, TaskUpdate{ID: 9, Status: "progress"}); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Fatalf("update unknown: %v", err)
	}
	if _, err := c.GetTask(ctx, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown: %v", err)
	}
	for _, bad := range []TaskUpdate{{ID: 0, Status: "done"}, {ID: 1}, {ID: 1, Status: "archived"}, {ID: 1, Title: "x", Note: "n"}} {
		if _, err := c.UpdateTask(ctx, bad); err == nil {
			t.Fatalf("invalid update accepted: %+v", bad)
		}
	}
}

func TestClientMessages(t *testing.T) {
	b := &standIn{}
	url := b.serve(t, map[string]func(map[string]any) *sdk.CallToolResult{
		"notify": func(args map[string]any) *sdk.CallToolResult {
			return result(map[string]any{"task_id": "d1", "status": "pending"})
		},
		"list_my_pending": func(map[string]any) *sdk.CallToolResult {
			return result([]any{map[string]any{"task_id": "d7", "payload": map[string]any{"text": "hi"}}})
		},
		"ack": func(args map[string]any) *sdk.CallToolResult {
			if args["task_id"] != "d7" {
				return toolErr("unknown delivery")
			}
			return result(map[string]any{"status": "acked"})
		},
	})
	c := newClient(t, Config{SwarmURL: url, Agent: "sa-author"})
	ctx := context.Background()
	d, err := c.Send(ctx, "sa-silvana", Object{"text": "hello"})
	if err != nil || d["task_id"] != "d1" {
		t.Fatalf("send: %v %v", d, err)
	}
	got := b.last()
	p, _ := got.Args["payload"].(map[string]any)
	if got.Tool != "notify" || got.Args["to_agent"] != "sa-silvana" || p["text"] != "hello" || p["from"] != "sa-author" {
		t.Fatalf("notify call: %+v", got)
	}
	if _, err := c.Send(ctx, "bad agent", Object{"text": "x"}); err == nil {
		t.Fatal("bad recipient accepted")
	}
	in, err := c.Inbox(ctx, 5)
	if err != nil || len(in) != 1 || in[0]["task_id"] != "d7" {
		t.Fatalf("inbox: %v %v", in, err)
	}
	if err := c.Ack(ctx, "d7"); err != nil {
		t.Fatal(err)
	}
	if err := c.Ack(ctx, "d8"); err == nil || !strings.Contains(err.Error(), "unknown delivery") {
		t.Fatalf("ack unknown: %v", err)
	}
}
