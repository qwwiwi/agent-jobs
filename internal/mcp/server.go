package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// ServerName is the MCP implementation name.
const ServerName = "agent-jobs"

// Version is reported in the MCP handshake.
const Version = "0.2.0"

// Deps is what tool handlers work on. In the job role Manager is nil: no job-role tool needs the
// job database yet, and a job's server does not open it.
type Deps struct {
	Manager *jobs.Manager
}

// toolDef binds a tool name to the function that registers it on a server.
type toolDef struct {
	name string
	add  func(s *sdk.Server, d Deps)
}

// toolDefs is every tool this binary can serve. Each name must be in exactly one audience list.
var toolDefs = []toolDef{
	{"job_start", addJobStart},
	{"job_status", addJobStatus},
	{"job_result", addJobResult},
	{"job_list", addJobList},
	{"job_cancel", addJobCancel},
}

func definitions() []toolDef { return toolDefs }

// addManagerTool registers a tool whose handler uses d.Manager. The job role runs without a manager,
// so a tool made job-visible later fails as a clean tool error instead of a nil dereference.
func addManagerTool[In, Out any](s *sdk.Server, d Deps, t *sdk.Tool, h sdk.ToolHandlerFor[In, Out]) {
	name := t.Name
	sdk.AddTool(s, t, func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		if d.Manager == nil {
			var zero Out
			return nil, zero, fmt.Errorf("tool %s needs the job database, which this server does not open (job role)", name)
		}
		return h(ctx, req, in)
	})
}

const orchestratorInstructions = `agent-jobs runs long work as background "claude -p" jobs.
job_start queues a job and returns its id at once; the daemon starts it within a minute (at most 2 run at a time).
State an explicit model or a complexity (light|normal|hard, mapped to a model by kind); with neither job_start is rejected: there is no default and no classifier.
When a job ends this session receives one line "[jobs:job-done] job <id> ..."; the report is not pushed into the session.
Read it with job_result when ready, section "Not done / uncertain" first, and check the diff before anything leaves the machine.
Route: minutes of work, do it yourself; the session needs the result now, a subagent; long, background, overnight or must survive a restart, a job.
A job may use at most 3 subagents inside its own limits and can never start jobs itself. The agent-jobs skill has the details.`

const jobInstructions = `agent-jobs job role: only the tools allow-listed for jobs are available here.`

// NewServer builds the server for role. Only tools allowed for the role are registered, and a
// receiving middleware refuses any other tools/call and filters tools/list, so the role boundary is
// enforced by the server itself, not only by what happens to be registered.
func NewServer(role Role, d Deps) (*sdk.Server, error) {
	return newServer(role, d, false)
}

// newServer with registerHidden=true also registers tools the role may not use (tests only: it
// proves the middleware refuses them on its own).
func newServer(role Role, d Deps, registerHidden bool) (*sdk.Server, error) {
	if _, err := ParseRole(string(role)); err != nil {
		return nil, err
	}
	instr := jobInstructions
	if role == RoleOrchestrator {
		if d.Manager == nil {
			return nil, errors.New("orchestrator role needs the job manager")
		}
		instr = orchestratorInstructions
	}
	s := sdk.NewServer(&sdk.Implementation{Name: ServerName, Version: Version}, &sdk.ServerOptions{Instructions: instr})
	s.AddReceivingMiddleware(enforceRole(role))
	for _, t := range definitions() {
		if _, err := audienceOf(t.name); err != nil {
			return nil, err
		}
		if Allowed(role, t.name) || registerHidden {
			t.add(s, d)
		}
	}
	return s, nil
}

// enforceRole refuses tools/call for a tool the role may not use and drops such tools from
// tools/list, whatever is registered on the server.
func enforceRole(role Role) sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			switch method {
			case "tools/call":
				name := ""
				if r, ok := req.(*sdk.CallToolRequest); ok && r.Params != nil {
					name = r.Params.Name
				}
				if !Allowed(role, name) {
					return nil, &jsonrpc.Error{
						Code:    jsonrpc.CodeInvalidParams,
						Message: fmt.Sprintf("tool %q is not available to the %s role", name, role),
					}
				}
			case "tools/list":
				res, err := next(ctx, method, req)
				if lr, ok := res.(*sdk.ListToolsResult); ok && lr != nil {
					lr.Tools = slices.DeleteFunc(lr.Tools, func(t *sdk.Tool) bool { return t == nil || !Allowed(role, t.Name) })
				}
				return res, err
			}
			return next(ctx, method, req)
		}
	}
}

// Serve runs the server for role and agent on stdin/stdout until the client disconnects.
// The orchestrator role opens the agent's job manager; the job role opens nothing.
func Serve(ctx context.Context, role Role, agent string) error {
	var d Deps
	if role != RoleJob {
		if err := jobs.CheckNotInJob(); err != nil {
			return fmt.Errorf("%s role: %w", role, err)
		}
	}
	if role == RoleOrchestrator {
		m, err := jobs.Open(agent)
		if err != nil {
			return err
		}
		defer m.Close()
		if err := m.CheckNotInJobSession(); err != nil {
			return fmt.Errorf("%s role: %w", role, err)
		}
		d.Manager = m
	} else if _, err := jobs.StateDir(agent); err != nil {
		return err // validates the agent name
	}
	s, err := NewServer(role, d)
	if err != nil {
		return err
	}
	return s.Run(ctx, &sdk.StdioTransport{})
}
