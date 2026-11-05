package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// cmdOpenRouter is `agent-jobs openrouter endpoints <slug>` (issue #136, R9): the endpoints of an
// OpenRouter model ranked by the routing policy, the one it picks marked.
func cmdOpenRouter(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "endpoints" {
		return errors.New("usage: agent-jobs openrouter endpoints <slug> [--agent A] [--json] [--file F]")
	}
	fs := flag.NewFlagSet("openrouter endpoints", flag.ContinueOnError)
	agent := agentFlag(fs)
	asJSON := fs.Bool("json", false, "print the ranked endpoints as JSON")
	file := fs.String("file", "", "read the endpoints JSON from this file instead of fetching it")
	rest, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: agent-jobs openrouter endpoints <slug> [--agent A] [--json] [--file F]")
	}
	slug := rest[0]
	var cfg *jobs.OpenRouterConfig
	base := jobs.DefaultOpenRouterBaseURL
	if *agent != "" {
		dir, err := jobs.StateDir(*agent)
		if err != nil {
			return err
		}
		c, err := jobs.LoadConfig(dir)
		if err != nil {
			return err
		}
		if cfg = c.Providers.OpenRouter; cfg != nil {
			base = cfg.BaseURL
		}
	}
	var eps []jobs.Endpoint
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		if eps, err = jobs.ParseEndpoints(b); err != nil {
			return err
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if eps, err = jobs.FetchEndpoints(ctx, &http.Client{Timeout: 30 * time.Second}, base, slug); err != nil {
			return err
		}
	}
	policy := cfg.Policy(slug)
	ranked := jobs.RankEndpoints(eps, policy)
	if *asJSON {
		return printJSONTo(out, struct {
			Model     string                `json:"model"`
			Policy    jobs.EffectiveRouting `json:"policy"`
			Endpoints []jobs.RankedEndpoint `json:"endpoints"`
			Allowed   *bool                 `json:"allowed,omitempty"`
		}{slug, policy, ranked, allowedSlug(cfg, slug)})
	}
	jobs.FormatEndpoints(out, slug, policy, ranked)
	if a := allowedSlug(cfg, slug); a != nil && !*a {
		fmt.Fprintf(out, "note: %s is not in providers.openrouter.models: job_start refuses it\n", slug)
	}
	return nil
}

// allowedSlug says whether slug is on the agent's allow-list (nil without an openrouter config).
func allowedSlug(cfg *jobs.OpenRouterConfig, slug string) *bool {
	if cfg == nil {
		return nil
	}
	ok := false
	for _, m := range cfg.Models {
		ok = ok || m == slug
	}
	return &ok
}

// printJSONTo is printJSON to w.
func printJSONTo(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
