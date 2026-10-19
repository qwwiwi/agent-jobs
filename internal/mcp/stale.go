package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// ServerWatch notices, on each tool call of the long-lived orchestrator server, what changed on disk
// since it started: config.json is reloaded before the call (two stat calls when nothing changed), and
// a replaced binary or a daemon of another version adds a warning to the result. Upgrading restarts
// the daemon but not the session's MCP server, which would otherwise keep serving the old code.
type ServerWatch struct {
	m       *jobs.Manager
	started time.Time
	self    jobs.FileIdentity // the binary when the server started (with its digest)
	selfErr error
	brain   brain.Config // the brain block m.Brain (and Deps.Brain, Deps.MinScore) were built from

	// mu is held shared by every tool call and exclusively while the config is reloaded and swapped, so
	// no handler sees m.Cfg change under it.
	mu sync.RWMutex

	// cfgMu guards the fields below, which change only while mu is held exclusively, together with
	// m.Cfg: a call that sees the new identity finds the new config in force. cfgMu is never held while
	// waiting for mu: a call holding mu shared may take cfgMu (ConfigWarning), so that order would
	// deadlock.
	cfgMu      sync.Mutex
	cfgChecked bool // config.json was checked since the server started
	cfgID      jobs.FileIdentity
	cfgOK      bool   // config.json existed at the last check
	cfgErr     string // why the changed config.json was not loaded ("" = loaded)
	brainMoved bool   // the loaded config has another brain block than the one in use

	beforeSwap func() // test hook: a reload is about to wait for mu
}

// NewServerWatch records the identity and digest of m's binary now. config.json is reloaded before the
// first tool call: m.Cfg was loaded earlier, and a later identity would not prove what it holds.
func NewServerWatch(m *jobs.Manager) *ServerWatch {
	w := &ServerWatch{m: m, started: m.Now(), brain: m.Cfg.Brain}
	w.self, w.selfErr = jobs.ReadIdentity(m.SelfBin)
	return w
}

func (w *ServerWatch) configPath() string { return filepath.Join(w.m.StateDir, "config.json") }

func (w *ServerWatch) configIdentity() (jobs.FileIdentity, bool) {
	id, err := jobs.StatIdentity(w.configPath())
	return id, err == nil
}

// configChanged reports whether config.json differs from the last loaded one (cfgMu held).
func (w *ServerWatch) configChanged(id jobs.FileIdentity, ok bool) bool {
	return !w.cfgChecked || ok != w.cfgOK || (ok && !id.Same(w.cfgID))
}

// refreshConfig reloads config.json when it changed since the last check. A config that does not load
// keeps the previous one in force and is reported (ConfigWarning) until the file changes again. The
// identity is taken before the file is read, so a write in between only causes one more reload.
func (w *ServerWatch) refreshConfig() {
	id, ok := w.configIdentity()
	w.cfgMu.Lock()
	changed := w.configChanged(id, ok)
	w.cfgMu.Unlock()
	if !changed {
		return
	}
	if w.beforeSwap != nil {
		w.beforeSwap()
	}
	w.mu.Lock() // once no call runs
	defer w.mu.Unlock()
	id, ok = w.configIdentity()
	w.cfgMu.Lock()
	defer w.cfgMu.Unlock()
	if !w.configChanged(id, ok) { // another call reloaded it meanwhile
		return
	}
	w.cfgChecked, w.cfgID, w.cfgOK = true, id, ok
	cfg, err := jobs.LoadConfig(w.m.StateDir)
	if err != nil {
		w.cfgErr = err.Error()
		return
	}
	w.m.Cfg, w.cfgErr, w.brainMoved = cfg, "", cfg.Brain != w.brain
}

// ConfigWarning says that a changed config.json did not load, or that its brain block changed and is
// not applied ("" = none).
func (w *ServerWatch) ConfigWarning() string {
	w.cfgMu.Lock()
	defer w.cfgMu.Unlock()
	switch {
	case w.cfgErr != "":
		return "config.json changed but does not load (" + jobs.RedactPrintable(w.cfgErr, 200) +
			"); this MCP server keeps the config it loaded before"
	case w.brainMoved:
		return "config.json changed its brain block; this MCP server keeps the brain it started with " +
			"until the session restarts (or /mcp reconnect)"
	}
	return ""
}

// StaleBinary says that the binary on disk is not the one this server started from ("" = it is).
func (w *ServerWatch) StaleBinary() string {
	if w.selfErr != nil {
		return ""
	}
	now, err := jobs.StatIdentity(w.m.SelfBin)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Sprintf("agent-jobs MCP server is older than the installed binary (started %s, binary %s removed); "+
			"restart the session (or /mcp reconnect) to use the new version.", fmtTS(&w.started), w.m.SelfBin)
	case err != nil || now.Same(w.self):
		return ""
	}
	return fmt.Sprintf("agent-jobs MCP server is older than the installed binary (started %s, binary replaced %s); "+
		"restart the session (or /mcp reconnect) to use the new version.", fmtTS(&w.started), fmtTS(&now.ModTime))
}

// DaemonMismatch says that the daemon runs another version or build than this server ("" = same).
func (w *ServerWatch) DaemonMismatch() string {
	if w.selfErr != nil {
		return ""
	}
	return w.m.DaemonMismatch(w.self)
}

// daemonCheckedTools also compare the server with the daemon: what job_start validates here, the
// daemon runs.
var daemonCheckedTools = map[string]bool{"job_start": true, "system_doctor": true}

// Warnings are the lines a call of tool gets added to its result.
func (w *ServerWatch) Warnings(tool string) []string {
	var out []string
	for _, s := range []string{w.StaleBinary(), w.ConfigWarning()} {
		if s != "" {
			out = append(out, s)
		}
	}
	if daemonCheckedTools[tool] {
		if s := w.DaemonMismatch(); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// middleware reloads a changed config.json before each tools/call, holds mu shared while the call
// runs, and adds the call's warnings to its result as text lines.
func (w *ServerWatch) middleware() sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			w.refreshConfig()
			w.mu.RLock()
			res, err := next(ctx, method, req)
			w.mu.RUnlock()
			name := ""
			if r, ok := req.(*sdk.CallToolRequest); ok && r.Params != nil {
				name = r.Params.Name
			}
			if cr, ok := res.(*sdk.CallToolResult); ok && cr != nil {
				for _, s := range w.Warnings(name) {
					cr.Content = append(cr.Content, &sdk.TextContent{Text: s})
				}
			}
			return res, err
		}
	}
}
