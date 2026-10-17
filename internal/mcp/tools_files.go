package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/trash"
)

// trashFor is the agent's trash: <state>/trash, roots from config (default the home directory),
// the state dir itself protected.
func trashFor(m *jobs.Manager) *trash.Trash {
	return &trash.Trash{
		Dir:       filepath.Join(m.StateDir, "trash"),
		Roots:     m.Cfg.TrashRootsOrDefault(),
		Protected: []string{m.StateDir},
		Now:       m.Now,
	}
}

// TrashMoveInput is trash_move's input.
type TrashMoveInput struct {
	Path string `json:"path" jsonschema:"absolute path of a file, directory or symlink inside an allowed root (trash_roots; default the home directory)"`
}

func addTrashMove(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "trash_move",
		Description: "Reversible deletion instead of rm: move a path into this agent's trash and return its entry " +
			"(id, original path). Use trash_restore with the id to put it back. Refused: relative paths, `..`, paths " +
			"outside the allowed roots (also after resolving symlinks in the parent), a root itself, the agent's state " +
			"dir, another filesystem. A symlink is moved as the link, never followed.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in TrashMoveInput) (*sdk.CallToolResult, trash.Entry, error) {
		e, err := trashFor(d.Manager).Move(in.Path)
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
