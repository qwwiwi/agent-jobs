package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Client is the shared brain over MCP streamable HTTP. Each call opens a short session to the
// endpoint, calls one tool and closes it: the daemon and the per-session MCP servers keep no
// connection open, and a brain restart costs nothing.
type Client struct {
	cfg   Config
	agent string
	// HTTP is the base HTTP client (tests may swap the transport); the Authorization header is added per call.
	HTTP *http.Client
}

var _ Brain = (*Client)(nil)

// New returns the brain for cfg: Disabled when no endpoint is configured, else a Client. agent is
// the default author name (cfg.Agent wins).
func New(cfg Config, agent string) (Brain, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Configured() {
		return Disabled{}, nil
	}
	if cfg.Agent != "" {
		agent = cfg.Agent
	}
	return &Client{cfg: cfg, agent: agent, HTTP: &http.Client{}}, nil
}

// authTransport adds the bearer token to the requests of one call that go to the configured
// endpoint (same scheme, host and port), and to nothing else.
type authTransport struct {
	header       string
	scheme, host string
	base         http.RoundTripper
}

func newAuthTransport(endpoint, header string, base http.RoundTripper) (authTransport, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return authTransport{}, err
	}
	return authTransport{header: header, scheme: strings.ToLower(u.Scheme), host: strings.ToLower(u.Host), base: base}, nil
}

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Del("Authorization")
	if strings.ToLower(r.URL.Scheme) == t.scheme && strings.ToLower(r.URL.Host) == t.host {
		r.Header.Set("Authorization", t.header)
	}
	return t.base.RoundTrip(r)
}

// maxErrText bounds a brain error message quoted back to the caller.
const maxErrText = 500

// call invokes tool on the endpoint and returns its decoded result.
func (c *Client) call(ctx context.Context, endpoint, key, tool string, args map[string]any) (any, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("%w (no %s)", ErrNotConfigured, key)
	}
	auth, err := c.cfg.token()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.timeout())
	defer cancel()
	base := c.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	at, err := newAuthTransport(endpoint, auth, base)
	if err != nil {
		return nil, fmt.Errorf("brain %s: %w", key, err)
	}
	hc := *c.HTTP
	hc.Transport = at
	// The brain's MCP endpoints need no redirect, and a redirect could lead the call (and, through a
	// transport that adds the header, the token) anywhere: the redirect response is the answer.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// Bounds the HTTP requests themselves, so a session closed in the background (below) ends too.
	hc.Timeout = c.cfg.timeout()
	tr := &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &hc, DisableStandaloneSSE: true, MaxRetries: -1}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "agent-jobs", Version: "brain"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		return nil, fmt.Errorf("brain %s: connect: %w", key, err)
	}
	// Close waits for the server's answer to an in-flight call; after a cancelled call it runs in
	// the background, so the caller's deadline holds.
	defer func() {
		if ctx.Err() != nil {
			go func() { _ = cs.Close() }()
			return
		}
		_ = cs.Close()
	}()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("brain %s: %w", tool, err)
	}
	if res.IsError {
		return nil, fmt.Errorf("brain %s: %s", tool, capText(resultText(res)))
	}
	return decode(res)
}

func resultText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func capText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxErrText {
		s = s[:maxErrText] + "…"
	}
	return s
}

// decode prefers the structured result; a non-object return arrives wrapped as {"result": …}
// and is unwrapped. Without one, the text content is parsed as JSON, or returned as a string.
func decode(res *sdk.CallToolResult) (any, error) {
	if res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		if m, ok := v.(map[string]any); ok && len(m) == 1 {
			if inner, ok := m["result"]; ok {
				return inner, nil
			}
		}
		return v, nil
	}
	text := strings.TrimSpace(resultText(res))
	if text == "" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return text, nil
	}
	return v, nil
}

func asObject(tool string, v any) (Object, error) {
	switch x := v.(type) {
	case map[string]any:
		return x, nil
	case nil:
		return nil, nil
	}
	return nil, fmt.Errorf("brain %s: unexpected result %T, want an object", tool, v)
}

func asList(tool string, v any) ([]Object, error) {
	if v == nil {
		return []Object{}, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("brain %s: unexpected result %T, want a list", tool, v)
	}
	out := make([]Object, 0, len(a))
	for _, e := range a {
		o, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("brain %s: unexpected list item %T", tool, e)
		}
		out = append(out, o)
	}
	return out, nil
}

// Search calls recall.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Note, error) {
	v, err := c.call(ctx, c.cfg.RecallURL, "recall_url", "recall", map[string]any{"query": query, "limit": limit})
	if err != nil {
		return nil, err
	}
	list, err := asList("recall", v)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	notes := []Note{}
	if err := json.Unmarshal(b, &notes); err != nil {
		return nil, fmt.Errorf("brain recall: decode notes: %w", err)
	}
	if len(notes) > limit {
		notes = notes[:limit]
	}
	for i := range notes {
		notes[i].FromJob = "" // provenance is agent-jobs' own, never the brain's word
	}
	return notes, nil
}

// noteTools maps a note kind to the memory server's create tool.
var noteTools = map[string]string{
	KindDecision:     "create_decision_note",
	KindRunbook:      "create_runbook_note",
	KindErrorPattern: "create_error_pattern_note",
}

// Write calls create_<kind>_note.
func (c *Client) Write(ctx context.Context, n NoteInput) (Object, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	author := c.agent
	if n.Author != "" {
		author = n.Author
	}
	args := map[string]any{"title": n.Title, "body": n.Body, "tags": n.Tags, "agent": author}
	switch n.Kind {
	case KindDecision:
		if len(n.Related) > 0 {
			args["related"] = n.Related
		}
	case KindErrorPattern:
		args["category"], args["severity"] = n.Category, n.Severity
		args["trigger_condition"], args["prevention_rule"] = n.TriggerCondition, n.PreventionRule
	}
	tool := noteTools[n.Kind]
	v, err := c.call(ctx, c.cfg.MemoryURL, "memory_url", tool, args)
	if err != nil {
		return nil, err
	}
	if o, ok := v.(map[string]any); ok {
		return o, nil
	}
	return Object{"result": v}, nil
}

// setIf adds key=val to args when val is not empty (so the brain's defaults apply otherwise).
func setIf(args map[string]any, key, val string) {
	if val != "" {
		args[key] = val
	}
}

// CreateTask calls task_create.
func (c *Client) CreateTask(ctx context.Context, t TaskInput) (Object, error) {
	if strings.TrimSpace(t.Title) == "" {
		return nil, fmt.Errorf("title is required")
	}
	args := map[string]any{"title": t.Title}
	setIf(args, "description", t.Description)
	setIf(args, "assignee", t.Assignee)
	setIf(args, "priority", t.Priority)
	v, err := c.call(ctx, c.cfg.TasksURL, "tasks_url", "task_create", args)
	if err != nil {
		return nil, err
	}
	return asObject("task_create", v)
}

// ListTasks calls task_list.
func (c *Client) ListTasks(ctx context.Context, f TaskFilter) ([]Object, error) {
	args := map[string]any{"limit": f.Limit}
	setIf(args, "assignee", f.Assignee)
	setIf(args, "status", f.Status)
	v, err := c.call(ctx, c.cfg.TasksURL, "tasks_url", "task_list", args)
	if err != nil {
		return nil, err
	}
	return asList("task_list", v)
}

// UpdateTask calls task_update for the fields, then the status tool, then task_get. The two calls are
// not atomic: when the status change fails after the fields were set, the error says so.
func (c *Client) UpdateTask(ctx context.Context, u TaskUpdate) (Object, error) {
	if err := u.Validate(); err != nil {
		return nil, err
	}
	if u.hasFields() {
		args := map[string]any{"task_id": u.ID}
		setIf(args, "title", u.Title)
		setIf(args, "description", u.Description)
		setIf(args, "assignee", u.Assignee)
		setIf(args, "priority", u.Priority)
		if _, err := c.call(ctx, c.cfg.TasksURL, "tasks_url", "task_update", args); err != nil {
			return nil, err
		}
	}
	if u.Status != "" {
		args := map[string]any{"task_id": u.ID}
		key := "note"
		if u.Status == "blocked" {
			key = "reason"
		}
		setIf(args, key, u.Note)
		if _, err := c.call(ctx, c.cfg.TasksURL, "tasks_url", statusTools[u.Status], args); err != nil {
			return nil, partialUpdate(u, err)
		}
	}
	return c.GetTask(ctx, u.ID)
}

// GetTask calls task_get; a null result is ErrNotFound.
func (c *Client) GetTask(ctx context.Context, id int64) (Object, error) {
	v, err := c.call(ctx, c.cfg.TasksURL, "tasks_url", "task_get", map[string]any{"task_id": id})
	if err != nil {
		return nil, err
	}
	o, err := asObject("task_get", v)
	if err == nil && o == nil {
		return nil, fmt.Errorf("task %d: %w", id, ErrNotFound)
	}
	return o, err
}

// Send calls notify; the payload's "from" is always the author (a payload cannot set it).
func (c *Client) Send(ctx context.Context, to string, payload Object) (Object, error) {
	if err := ValidateAgentName(to); err != nil {
		return nil, err
	}
	p := Object{}
	for k, v := range payload {
		p[k] = v
	}
	p["from"] = c.agent
	v, err := c.call(ctx, c.cfg.SwarmURL, "swarm_url", "notify", map[string]any{"to_agent": to, "payload": p})
	if err != nil {
		return nil, err
	}
	return asObject("notify", v)
}

// Inbox calls list_my_pending.
func (c *Client) Inbox(ctx context.Context, limit int) ([]Object, error) {
	v, err := c.call(ctx, c.cfg.SwarmURL, "swarm_url", "list_my_pending", map[string]any{"limit": limit})
	if err != nil {
		return nil, err
	}
	return asList("list_my_pending", v)
}

// Ack calls ack.
func (c *Client) Ack(ctx context.Context, deliveryID string) error {
	if strings.TrimSpace(deliveryID) == "" {
		return fmt.Errorf("delivery id is required")
	}
	_, err := c.call(ctx, c.cfg.SwarmURL, "swarm_url", "ack", map[string]any{"task_id": deliveryID})
	return err
}
