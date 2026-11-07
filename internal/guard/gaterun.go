package guard

import (
	"path/filepath"
	"slices"
	"strings"
)

// The gate runner inside a job (#208, owner 2026-11-07): `agent-jobs gate run [gate flags] -- CMD...`
// runs CMD under the box-wide gate lock, so the heavy steps of jobs (test suites, type checks, builds)
// never overlap. It is the one agent-jobs form a job runs besides its own MCP server, and it is read as a
// wrapper (split): CMD is the real program and every rule checks it as if it ran directly, so a gate is
// never a way around the guard. What the prefix itself must be (checkGatePrefix):
//   - exactly `gate run`, then only the gate flags (--head, --repo, --timeout, --lock-wait; one or two
//     dashes, the value attached with = or as the next word), then `--`; anything else is not a gate
//     and stays refused as agent-jobs (checkSelf);
//   - the program a literal name, or an absolute path outside the job's own folder: a binary the job
//     built itself (`./agent-jobs`, <jobdir>/work/...) is not the installed runner;
//   - only the values of --head, --timeout and --lock-wait may come from a variable or substitution
//     (data); --repo is literal and is where CMD runs, so it must be where the shell is: omitted, `.`,
//     or the call's own directory written out when the call has no cd, never with a `..` segment (the
//     kernel resolves `link/..` after the link, filepath.Clean does not). Relative words of CMD then
//     resolve where the guard resolves them;
//   - one gate per command: a gate inside a gate would wait for the lock its own parent holds.

// gateFlags are the flags of `agent-jobs gate run` (all take a value).
var gateFlags = map[string]bool{"head": true, "repo": true, "timeout": true, "lock-wait": true}

// gateDataFlags are the gate flags whose value may be dynamic: the runner only parses it.
var gateDataFlags = map[string]bool{"head": true, "timeout": true, "lock-wait": true}

// gateForm is the allowed form, named in refusals.
const gateForm = "`agent-jobs gate run --head REV [--repo .] [--timeout D] [--lock-wait D] -- CMD...`"

// gatePrefix is one `agent-jobs gate run [flags] --` prefix of a command's words.
type gatePrefix struct {
	at     int          // index of the agent-jobs word
	end    int          // index after `--`: CMD's first word
	values map[int]bool // indices of words holding a flag value: true when it may be dynamic
	flags  []int        // indices of the flag words
	repo   int          // index of the word holding the last --repo value (-1 = none)
	repoV  string       // that value
	outer  []string     // the wrappers in front of it (split's v.wrappers so far)
}

// parseGatePrefix reads words[at:] as `agent-jobs gate run [gate flags] --` (Go flag syntax, which the
// CLI uses: `-f v`, `-f=v`, `--f v`, `--f=v`). ok is false for any other form.
func parseGatePrefix(words []string, at int) (g gatePrefix, ok bool) {
	if at+2 >= len(words) || words[at+1] != "gate" || words[at+2] != "run" {
		return g, false
	}
	g = gatePrefix{at: at, values: map[int]bool{}, repo: -1}
	for k := at + 3; k < len(words); k++ {
		w := words[k]
		if w == "--" {
			g.end = k + 1
			return g, true
		}
		if !strings.HasPrefix(w, "-") || strings.HasPrefix(w, "---") {
			return g, false
		}
		name, val, eq := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(w, "-"), "-"), "=")
		if !gateFlags[name] {
			return g, false
		}
		g.flags = append(g.flags, k)
		vi := k
		if !eq {
			if k+1 >= len(words) {
				return g, false
			}
			k++
			vi, val = k, words[k]
		}
		g.values[vi] = gateDataFlags[name]
		if name == "repo" {
			g.repo, g.repoV = vi, val
		}
	}
	return g, false
}

// checkGatePrefixes checks the gate prefixes of c (split view v) in a job; "" = allowed.
func checkGatePrefixes(c *simpleCmd, v cmdView, cfg Config, sawCD bool) string {
	if len(v.gates) == 0 {
		return ""
	}
	refuse := func(why string) string {
		return "blocked by guard: " + why + ". Inside a job agent-jobs runs only as the gate runner " + gateForm +
			", with CMD a command the job may run directly."
	}
	if len(v.gates) > 1 {
		return refuse("a gate inside a gate would wait for the gate lock its own parent holds")
	}
	g := v.gates[0]
	if slices.Contains(g.outer, "xargs") {
		return refuse("xargs in front of the gate supplies words the guard cannot see")
	}
	dyn := func(i int) bool { return i < len(c.dyn) && (c.dyn[i] || c.bare[i] || c.glob[i]) }
	// An unquoted expansion or glob anywhere in the prefix may split into words the guard never sees
	// (an injected `--`, a --repo, a whole command): only a quoted expansion stays one word.
	for i := g.at; i < g.end && i < len(c.bare); i++ {
		if c.bare[i] || c.glob[i] {
			return refuse("`" + c.words[i] + "` in the gate prefix is unquoted: the shell may split it into other words; quote it")
		}
	}
	if dyn(g.at) || dyn(g.at+1) || dyn(g.at+2) || dyn(g.end-1) {
		return refuse("the gate runner's name or subcommand comes from a variable or glob")
	}
	for _, k := range g.flags {
		if dyn(k) && !g.values[k] {
			return refuse("a gate flag comes from a variable: `" + c.words[k] + "`")
		}
	}
	for k, data := range g.values {
		if dyn(k) && !data {
			return refuse("--repo comes from a variable: write the directory out")
		}
	}
	if r := gateProgram(c.words[g.at], cfg); r != "" {
		return refuse(r)
	}
	if g.repo >= 0 {
		if r := gateRepo(g.repoV, cfg, sawCD); r != "" {
			return refuse(r)
		}
	}
	return ""
}

// gateProgram refuses a gate runner the job may have built itself: a relative path, or an absolute one
// inside the job's own folder or work tree.
func gateProgram(argv0 string, cfg Config) string {
	if !strings.Contains(argv0, "/") {
		return ""
	}
	if !filepath.IsAbs(argv0) {
		return "`" + argv0 + "` is a relative path: the gate runner is the installed agent-jobs, not a build of the job's"
	}
	p := filepath.Clean(argv0)
	for _, d := range []string{cfg.OwnDir, cfg.Dir} {
		if d != "" && within(p, filepath.Clean(d)) {
			return "`" + argv0 + "` is inside the job's own folder: the gate runner is the installed agent-jobs, not a build of the job's"
		}
	}
	return ""
}

// gateRepo allows a --repo that is where the shell is: `.`, or the call's directory written out when the
// call changes no directory (cdView would otherwise make another directory current). A `..` segment is
// refused as written, relative or absolute: filepath.Clean drops `link/..` lexically, but RunGate hands
// the raw value to the kernel, which resolves it to the parent of the link's target (#208 review).
func gateRepo(repo string, cfg Config, sawCD bool) string {
	if hasDotDot(repo) {
		return "--repo `" + repo + "` has a `..` segment: the kernel resolves it after a symlink, so CMD could run elsewhere; run the gate there with --repo ."
	}
	if filepath.Clean(repo) == "." {
		return ""
	}
	if !sawCD && filepath.IsAbs(repo) && cfg.Dir != "" && filepath.Clean(repo) == filepath.Clean(cfg.Dir) {
		return ""
	}
	return "--repo `" + repo + "` is not the current directory: CMD runs in --repo, so run the gate there with --repo ."
}
