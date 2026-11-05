package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

const orCostsUsage = "usage: agent-jobs openrouter costs --agent A [--days N] [--json] [--reconcile]"

// orCostsLookupWait bounds the generation lookup of one run during --reconcile, orCostsRetry the wait
// before a generation not found yet is asked for again.
var (
	orCostsLookupWait = 10 * time.Second
	orCostsRetry      = 2 * time.Second
)

// cmdOpenRouterCosts is `agent-jobs openrouter costs`: the agent's OpenRouter cost journal (one line per
// attempt: the proxy's cost and OpenRouter's billed cost side by side, a mismatch over 10% flagged),
// totals per day and per model and the 30-day total, headed by the key's own usage and limit from
// OpenRouter. --reconcile first reads OpenRouter's record of the runs that lack it and appends it to
// the journal. The key is read from providers.openrouter.key_file and only sent to OpenRouter.
func cmdOpenRouterCosts(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("openrouter costs", flag.ContinueOnError)
	agent := agentFlag(fs)
	days := fs.Int("days", 30, "list the runs of the last N days")
	asJSON := fs.Bool("json", false, "print the runs and totals as JSON")
	reconcile := fs.Bool("reconcile", false, "look up OpenRouter's record of the runs that lack it and append it to the journal")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return errors.New(orCostsUsage)
	}
	if *agent == "" {
		return errors.New("--agent is required\n" + orCostsUsage)
	}
	if *days < 1 || *days > 3650 {
		return errors.New("--days must be 1..3650")
	}
	dir, err := jobs.StateDir(*agent)
	if err != nil {
		return err
	}
	c, err := jobs.LoadConfig(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, jobs.OpenRouterCostsFile)
	cfg := c.Providers.OpenRouter
	var key string
	keyErr := errors.New("providers.openrouter is not configured")
	if cfg != nil {
		key, keyErr = jobs.ReadOpenRouterKey(cfg.KeyFile)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	reconciled, unresolved := 0, 0
	if *reconcile {
		if keyErr != nil {
			return fmt.Errorf("--reconcile needs the openrouter key: %v", keyErr)
		}
		runs, _, err := jobs.ReadORCosts(path)
		if err != nil {
			return err
		}
		for _, r := range runs {
			if (r.OpenRouter != nil && r.OpenRouter.CostUSD != nil) || len(r.GenerationIDs) == 0 {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), orCostsLookupWait)
			b := jobs.LookupGenerations(ctx, client, cfg.BaseURL, key, r.GenerationIDs, orCostsRetry)
			cancel()
			if b.CostUSD == nil {
				unresolved++
				continue
			}
			if err := jobs.AppendORReconcile(path, r.JobID, r.Attempt, b, key); err != nil {
				return fmt.Errorf("append to %s: %v", path, err)
			}
			reconciled++
		}
	}
	runs, malformed, err := jobs.ReadORCosts(path)
	if err != nil {
		return err
	}
	finalStatuses(*agent, runs)
	s := jobs.SummarizeORCosts(runs, malformed, time.Now(), *days)
	var usage *jobs.ORKeyUsage
	if keyErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		u, err := jobs.FetchORKeyUsage(ctx, client, cfg.BaseURL, key)
		cancel()
		if err == nil {
			usage = &u
		} else {
			keyErr = err
		}
	}
	keyNote := ""
	if keyErr != nil {
		keyNote = jobs.RedactPrintable(keyErr.Error(), 300)
	}
	if *asJSON {
		v := struct {
			Key        *jobs.ORKeyUsage `json:"key"`
			KeyError   string           `json:"key_error,omitempty"`
			Reconciled *int             `json:"reconciled,omitempty"`
			Unresolved *int             `json:"unresolved,omitempty"`
			jobs.ORCostSummary
		}{Key: usage, KeyError: keyNote, ORCostSummary: s}
		if *reconcile {
			v.Reconciled, v.Unresolved = &reconciled, &unresolved
		}
		return printJSONTo(out, v)
	}
	if usage != nil {
		fmt.Fprintln(out, jobs.FormatORKeyUsage(*usage))
	} else {
		fmt.Fprintf(out, "openrouter key: usage unavailable (%s)\n", keyNote)
	}
	if *reconcile {
		fmt.Fprintf(out, "reconciled %d run(s); %d still without OpenRouter's record\n", reconciled, unresolved)
	}
	jobs.FormatORCosts(out, s)
	return nil
}

// finalStatuses replaces each run's status by its job row's final status, when the row is terminal and
// at the run's attempt (the journal holds the wrapper's verdict; the daemon decides cancel and timeout).
func finalStatuses(agent string, runs []jobs.ORCostRunView) {
	if len(runs) == 0 {
		return
	}
	m, err := jobs.OpenReadOnly(agent)
	if err != nil {
		return // no database: the journal's own statuses
	}
	defer m.Close()
	for i := range runs {
		j, err := m.Store.Get(runs[i].JobID)
		if err != nil || j.Attempt != runs[i].Attempt || j.Status == store.StatusQueued || j.Status == store.StatusRunning {
			continue
		}
		runs[i].FinalStatus = j.Status
	}
}
