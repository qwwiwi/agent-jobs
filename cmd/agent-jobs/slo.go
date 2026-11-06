package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// cmdStatsWakes is `stats wakes` (#170): what reached the orchestrator session, per category per day,
// and the bytes per delivery or injection (p50, p95).
func cmdStatsWakes(args []string) error {
	fs := flag.NewFlagSet("stats wakes", flag.ContinueOnError)
	agent := agentFlag(fs)
	since := fs.String("since", "", "only records since: <n>d, a duration (36h) or a date YYYY-MM-DD (default all)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("stats wakes: no positional arguments")
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	t, err := jobs.ParseSince(*since, m.Now())
	if err != nil {
		return err
	}
	rep, err := m.WakeStats(t)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(rep)
	}
	if len(rep.Categories) == 0 {
		fmt.Println("no deliveries or injections recorded")
		printIncomplete(rep)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "DAY\tCATEGORY\tCOUNT")
	for _, d := range rep.Days {
		for _, c := range append(append([]string{}, jobs.WakeCategories...), jobs.InjectionCategories...) {
			if n := d.Counts[c]; n > 0 {
				fmt.Fprintf(w, "%s\t%s\t%d\n", d.Day, c, n)
			}
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println()
	w = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tCATEGORY\tCOUNT\tBATCH MEAN\tBATCH MAX\tBYTES\tP50\tP95\tMAX")
	for _, c := range rep.Categories {
		fmt.Fprintf(w, "%s\t%s\t%d\t%.2f\t%d\t%d\t%d\t%d\t%d\n", c.Kind, c.Category, c.Count, c.MeanBatch, c.LargestRun,
			c.Bytes, c.BytesP50, c.BytesP95, c.BytesMax)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nwakes %d (%d bytes typed), hook injections %d bytes\n", rep.Wakes, rep.WakeBytes, rep.InjectionBytes)
	printIncomplete(rep)
	return nil
}

// printIncomplete says so when hook injections could not be stored: the injection numbers are then a
// lower bound.
func printIncomplete(rep jobs.WakeStatsReport) {
	if rep.InjectionsUnrecorded > 0 {
		fmt.Printf("incomplete: %d hook injections (%d bytes) could not be stored; the injection numbers are a lower bound\n",
			rep.InjectionsUnrecorded, rep.InjectionBytesUnrecorded)
	}
}

// cmdSLO is `slo record NAME SECONDS` and `slo show`.
func cmdSLO(args []string) error {
	if len(args) == 0 {
		return errors.New("slo: want the subcommand record or show")
	}
	switch args[0] {
	case "record":
		return cmdSLORecord(args[1:])
	case "show":
		return cmdSLOShow(args[1:])
	}
	return fmt.Errorf("slo: unknown subcommand %q (want record or show)", jobs.Printable(args[0], 40))
}

func cmdSLORecord(args []string) error {
	fs := flag.NewFlagSet("slo record", flag.ContinueOnError)
	agent := agentFlag(fs)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("slo record: want NAME SECONDS")
	}
	secs, err := strconv.ParseFloat(pos[1], 64)
	if err != nil || math.IsNaN(secs) || math.IsInf(secs, 0) {
		return fmt.Errorf("slo record: %q is not a number of seconds", jobs.Printable(pos[1], 40))
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	r, err := m.RecordSLO(pos[0], secs)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r)
	}
	switch {
	case r.Breach:
		fmt.Printf("recorded %s %gs: over the norm of %gs; the next delivery to the session carries a note\n", r.Name, r.Seconds, r.Target)
	case r.Target > 0:
		fmt.Printf("recorded %s %gs (norm %gs)\n", r.Name, r.Seconds, r.Target)
	default:
		fmt.Printf("recorded %s %gs (no norm configured)\n", r.Name, r.Seconds)
	}
	return nil
}

func cmdSLOShow(args []string) error {
	fs := flag.NewFlagSet("slo show", flag.ContinueOnError)
	agent := agentFlag(fs)
	since := fs.String("since", "", "only samples since: <n>d, a duration (36h) or a date YYYY-MM-DD (default all)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errors.New("slo show: no positional arguments")
	}
	m, err := open(*agent)
	if err != nil {
		return err
	}
	defer m.Close()
	t, err := jobs.ParseSince(*since, m.Now())
	if err != nil {
		return err
	}
	rows, err := m.SLOShow(t)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Println("no SLO samples recorded and no slo.targets configured")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCOUNT\tP50\tP95\tLAST\tNORM\tOVER NORM")
	for _, r := range rows {
		norm, over := "-", "-"
		if r.Target > 0 {
			norm, over = fmt.Sprintf("%gs", r.Target), strconv.Itoa(r.Over)
		}
		last := "-"
		if r.Count > 0 {
			last = fmt.Sprintf("%gs (%s)", r.Last, r.LastAt.In(m.Location()).Format(time.DateTime))
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", strings.TrimSpace(r.Name), r.Count, secs(r.Count, r.P50), secs(r.Count, r.P95), last, norm, over)
	}
	return w.Flush()
}

// secs is a seconds figure for a table, "-" when there are no samples.
func secs(count int, v float64) string {
	if count == 0 {
		return "-"
	}
	return fmt.Sprintf("%gs", v)
}
