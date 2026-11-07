package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

const restackUsage = "usage: agent-jobs restack --child C --parent P [--onto main] [--name N] [--repo PATH] [--json]"

// cmdRestack is `agent-jobs restack`: branch_restack for the shell. After the parent P of a stacked
// branch C was squash-merged, it creates a NEW branch (default <C>-restacked) from --onto with only C's
// own commits on top. Create-only: no existing ref moves, nothing is pushed, and any refusal (parent not
// in onto, conflict, name taken) is an error with the reason and leaves no ref behind. It needs no
// agent: it does not touch the job store.
func cmdRestack(args []string) error {
	fs := flag.NewFlagSet("restack", flag.ContinueOnError)
	child := fs.String("child", "", "the stacked branch (or commit) to carry over")
	parent := fs.String("parent", "", "the branch the child was built on, squash-merged into --onto")
	onto := fs.String("onto", "main", "the branch the squash landed on; the new branch starts from its tip")
	name := fs.String("name", "", "the new branch (default <child>-restacked); must not exist")
	repo := fs.String("repo", ".", "the repository")
	asJSON := fs.Bool("json", false, "print the result as JSON (the branch_restack output)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New(restackUsage)
	}
	if *child == "" {
		return errors.New("--child is required\n" + restackUsage)
	}
	if *parent == "" {
		return errors.New("--parent is required\n" + restackUsage)
	}
	res, err := jobs.Restack(*repo, *child, *parent, *onto, *name)
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	skipped := ""
	if len(res.Skipped) > 0 {
		skipped = fmt.Sprintf(", %d already in %s skipped", len(res.Skipped), *onto)
	}
	fmt.Printf("%s %s: %d commit(s) of %s on top of %s%s\n", res.Branch, res.Head, res.Commits, *child, *onto, skipped)
	return nil
}
