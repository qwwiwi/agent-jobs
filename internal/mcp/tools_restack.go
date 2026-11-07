package mcp

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// BranchRestackInput is branch_restack's input.
type BranchRestackInput struct {
	Repo   string `json:"repo" jsonschema:"absolute path of the git repository"`
	Child  string `json:"child" jsonschema:"the stacked branch (or commit) whose own commits are carried over"`
	Parent string `json:"parent" jsonschema:"the branch the child was built on; it must be an ancestor of the child and already squash-merged into onto"`
	Onto   string `json:"onto,omitempty" jsonschema:"the branch the squash landed on; the new branch starts from its tip; default main"`
	Name   string `json:"name,omitempty" jsonschema:"the new branch; default <child>-restacked; an existing name is refused"`
}

// addBranchRestack registers branch_restack. It is store-free (plain git in the given repository), so it
// needs no job database, but it is orchestrator-only: it creates a branch in the owner's repository.
func addBranchRestack(s *sdk.Server, _ Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name: "branch_restack",
		Description: "Restack a branch after its parent was squash-merged. Branch child was built on branch parent; parent landed " +
			"on onto (default main) as one squash commit, so child still carries parent's original commits and merging it " +
			"conflicts on code that is identical in substance. This creates a NEW branch (name, default <child>-restacked) from " +
			"onto's tip in a temporary worktree and cherry-picks only the child's own commits (parent..child) on it. " +
			"Returns the new branch, its head SHA, the commits carried (and any that were already in onto and skipped). " +
			"Create-only: it never moves or deletes an existing ref, never touches child, parent or the checkout, never pushes. " +
			"It refuses, with the reason and no ref left behind, when parent's change is not provably in onto (patch-id or " +
			"clean merge), when parent is not an ancestor of child, when the child has a merge commit, when the name exists, " +
			"or on a cherry-pick conflict (the error names the files).",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in BranchRestackInput) (*sdk.CallToolResult, jobs.RestackResult, error) {
		if in.Repo == "" {
			return nil, jobs.RestackResult{}, errors.New("repo is required")
		}
		res, err := jobs.Restack(in.Repo, in.Child, in.Parent, in.Onto, in.Name)
		return nil, res, err
	})
}
