package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

const inboxUsage = "usage: agent-jobs inbox pull --agent A [--ack]"

// cmdInbox is `inbox pull`: it prints the pending job-done lines of config notify.inbox_dir, one per
// line and oldest first, and with --ack removes the ones it printed. It reads only config.json and
// the inbox directory, never the jobs database, so a session hook can run it on every turn.
func cmdInbox(args []string) error {
	if len(args) == 0 || args[0] != "pull" {
		return errors.New(inboxUsage)
	}
	fs := flag.NewFlagSet("inbox pull", flag.ContinueOnError)
	agent := agentFlag(fs)
	ack := fs.Bool("ack", false, "remove the printed events from the inbox")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New(inboxUsage)
	}
	if *agent == "" {
		return errors.New("--agent is required")
	}
	state, err := jobs.StateDir(*agent)
	if err != nil {
		return err
	}
	cfg, err := jobs.LoadConfig(state)
	if err != nil {
		return err
	}
	if cfg.Notify.InboxDir == "" {
		return errors.New("notify.inbox_dir is not set in config.json: there is no inbox to pull")
	}
	events, skipped, err := jobs.InboxPull(cfg.Notify.InboxDir, *ack)
	// What was read is printed even when an acknowledgement failed: the lines are not lost.
	for _, ev := range events {
		fmt.Println(jobs.Printable(ev.Line, 0))
	}
	for _, s := range skipped {
		fmt.Fprintln(os.Stderr, "agent-jobs: inbox: skipped", jobs.Printable(s, 0))
	}
	return err
}
