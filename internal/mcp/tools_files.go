package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/trash"
)

// trashFor is the agent's trash: <state>/trash, roots from config (none by default: closed), and
// protected everything the fleet and the guard depend on.
func trashFor(m *jobs.Manager) *trash.Trash {
	return &trash.Trash{
		Dir:       filepath.Join(m.StateDir, "trash"),
		Roots:     m.Cfg.TrashRootsOrDefault(),
		Protected: trashProtected(m),
		Now:       m.Now,
	}
}

// trashProtected are the paths trash never moves, whatever the roots: the agent-jobs home (every
// agent's state dir and job database, not only this agent's), the owner rules, the agent-jobs
// binary that every job's guard hook runs (as configured and as this process runs it), ~/.claude
// and $CLAUDE_CONFIG_DIR when absolute (settings and hooks), and ~/.claude.json (MCP servers).
func trashProtected(m *jobs.Manager) []string {
	out := []string{m.StateDir, filepath.Dir(m.StateDir), m.Cfg.OwnerRulesPath, m.SelfBin}
	if h, err := jobs.Home(); err == nil {
		out = append(out, h)
	}
	if exe, err := os.Executable(); err == nil {
		out = append(out, exe)
	}
	if h, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(h, ".claude"), filepath.Join(h, ".claude.json"))
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); filepath.IsAbs(d) {
		out = append(out, filepath.Clean(d))
	}
	return out
}

// errNoTrashRoots explains how to open the trash: there is no default root.
func errNoTrashRoots(m *jobs.Manager) error {
	return fmt.Errorf("refused: no allowed roots are configured; trash is closed until the owner sets "+
		`"trash_roots" (absolute directories, e.g. ["/home/me/projects"]) in %s and restarts the MCP server`,
		filepath.Join(m.StateDir, "config.json"))
}

// TrashMoveInput is trash_move's input.
type TrashMoveInput struct {
	Path string `json:"path" jsonschema:"absolute path of a file, directory or symlink inside an allowed root (trash_roots in config.json; no default)"`
}

func addTrashMove(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "trash_move",
		Description: "Reversible deletion instead of rm: move a path into this agent's trash and return its entry " +
			"(id, original path). Use trash_restore with the id to put it back. Works only inside the roots the owner " +
			"configured (trash_roots in config.json; none by default, so nothing can be moved until they are set). " +
			"Refused: relative paths, `..`, paths outside the allowed roots (also after resolving symlinks in the " +
			"parent), a root itself, the agent-jobs home (every agent's state), the owner rules, the agent-jobs " +
			"binary, ~/.claude, any path with a .git component, another filesystem. A symlink is moved as the link, " +
			"never followed.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in TrashMoveInput) (*sdk.CallToolResult, trash.Entry, error) {
		t := trashFor(d.Manager)
		if len(t.Roots) == 0 {
			return nil, trash.Entry{}, errNoTrashRoots(d.Manager)
		}
		e, err := t.Move(in.Path)
		if err != nil {
			return nil, trash.Entry{}, err
		}
		d.Manager.Log.Printf("trash_move: %s -> %s (%s)", e.OriginalPath, e.TrashPath, e.ID)
		return nil, e, nil
	})
}

// TrashRestoreInput is trash_restore's input.
type TrashRestoreInput struct {
	ID string `json:"id" jsonschema:"trash entry id as returned by trash_move"`
}

// recentTrash is how many entries an unknown-id error lists.
const recentTrash = 20

func addTrashRestore(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "trash_restore",
		Description: "Put a trashed path back where trash_move took it from. Refused when that path is occupied " +
			"(nothing is overwritten), its parent is gone, or it is no longer inside an allowed root. " +
			"An unknown id lists the most recent entries.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in TrashRestoreInput) (*sdk.CallToolResult, trash.Entry, error) {
		t := trashFor(d.Manager)
		if len(t.Roots) == 0 {
			return nil, trash.Entry{}, errNoTrashRoots(d.Manager)
		}
		e, err := t.Restore(strings.TrimSpace(in.ID))
		if err != nil {
			msg := err.Error()
			if list, lerr := t.List(recentTrash); lerr == nil && (strings.Contains(msg, "not found") || strings.Contains(msg, "malformed")) {
				var b strings.Builder
				for _, x := range list {
					fmt.Fprintf(&b, "\n%s  %s  %s", x.ID, x.MovedAt, x.OriginalPath)
				}
				if b.Len() == 0 {
					b.WriteString(" (the trash is empty)")
				}
				return nil, trash.Entry{}, fmt.Errorf("%w; recent entries:%s", err, b.String())
			}
			return nil, trash.Entry{}, err
		}
		d.Manager.Log.Printf("trash_restore: %s -> %s", e.ID, e.OriginalPath)
		return nil, e, nil
	})
}
