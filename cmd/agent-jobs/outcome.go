package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

const outcomeUsage = "usage: agent-jobs outcome --agent A <id> <outcome> [--note N] [--json]\n" +
	"       agent-jobs outcome --agent A --input F|- [--json]"

// cmdOutcome is `agent-jobs outcome`: job_outcome for one job (<id> <outcome> [--note]) or
// job_outcome_many for a batch (--input: a file, "-" = stdin, with a JSON array of {id, outcome, note}).
// Both go through Manager.SetOutcomes, so the validation is the MCP tools' and one failing item does not
// stop the others; every item is printed with its result and the exit status is non-zero when any failed.
func cmdOutcome(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("outcome", flag.ContinueOnError)
	agent := agentFlag(fs)
	note := fs.String("note", "", "optional note, at most 300 characters (single form)")
	input := fs.String("input", "", "JSON file (- = stdin): an array of {id, outcome, note?} (or {\"items\": [...]})")
	asJSON := fs.Bool("json", false, "print the results as JSON (the job_outcome_many output)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	var items []jobs.OutcomeItem
	switch {
	case *input != "":
		if len(pos) != 0 || *note != "" {
			return errors.New("--input replaces <id> <outcome> and --note\n" + outcomeUsage)
		}
		raw, err := readInput(*input, stdin)
		if err != nil {
			return err
		}
		if items, err = outcomeItems(raw); err != nil {
			return err
		}
	case len(pos) == 2:
		items = []jobs.OutcomeItem{{ID: pos[0], Outcome: pos[1], Note: *note}}
	default:
		return errors.New(outcomeUsage)
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	res, err := m.SetOutcomes(items)
	if err != nil {
		return err
	}
	failed := 0
	for _, r := range res {
		if !r.OK {
			failed++
		}
	}
	if *asJSON {
		if err := printJSON(struct {
			Recorded int                  `json:"recorded"`
			Failed   int                  `json:"failed"`
			Results  []jobs.OutcomeResult `json:"results"`
		}{len(res) - failed, failed, res}); err != nil {
			return err
		}
	} else {
		for _, r := range res {
			if r.OK {
				fmt.Printf("recorded %s: %s\n", r.ID, r.Outcome)
			} else {
				fmt.Printf("failed %s: %s\n", r.ID, r.Error)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d outcomes not recorded", failed, len(res))
	}
	return nil
}

// outcomeItems reads raw, a JSON array of items or an object {"items": [...]}, strictly: an unknown
// field (a typo such as "notes") and anything after the value are refused, not ignored.
func outcomeItems(raw []byte) ([]jobs.OutcomeItem, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var items []jobs.OutcomeItem
	first := bytes.TrimLeft(raw, " \t\r\n")
	if len(first) > 0 && first[0] == '{' {
		var obj struct {
			Items []jobs.OutcomeItem `json:"items"`
		}
		if err := dec.Decode(&obj); err != nil {
			return nil, fmt.Errorf("outcome input: %w", err)
		}
		items = obj.Items
	} else if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("outcome input: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("outcome input: more than one JSON value, or data after it")
	}
	return items, nil
}
