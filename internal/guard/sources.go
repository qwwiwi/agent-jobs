package guard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Sources (G02): the job's cloned repositories (Config.Sources, <jobdir>/sources) are read-only for the
// job and their code never runs on the host; a copy under Config.Sandbox (<jobdir>/sandbox) runs only in a
// throwaway docker container that mounts nothing but that copy. Fail closed: what the guard cannot tell
// apart from running cloned code is refused.
//
//   - checkSourcesWrite: the review read-only check (readonly.go) with the sources as the protected
//     tree: redirections, in-place editors, tree writers, copier destinations, extractors into it.
//   - checkSourcesExec: a file under sources/ or sandbox/ run as a program; a runner (shell, interpreter,
//     package manager, build or test tool, an exec wrapper) whose working directory is in or above them,
//     that follows a cd the guard cannot resolve, that is given a path there (also inside an option
//     value or a `:`-separated list), or that reads its program from such a file or a pipe stage naming
//     one; find -exec over them; `awk -f` / `sed -f` with a program there; an environment assignment
//     whose value names them (PATH, LD_PRELOAD, PYTHONPATH, ...); a copy out of sources/ to anywhere but
//     sandbox/ (cp, install, ln, rsync).
//   - dockerSandbox: `docker run` is allowed only as the whole Bash command, with --rm, exactly one
//     `-v <sandbox>/<copy>:<dst>[:ro|rw]` and an allowlist of options; no env, no host namespaces or
//     network, no privileges, devices, ports, --mount or detaching.
//
// Residual gaps (not resolved): a copy made by a program the guard does not read as a copier (tar, git
// clone of the local path, cat > file, code in an interpreter) and run from there; a script the job writes
// itself; a symlink swapped in by a process the job left running between the check and docker's mount.

// sourcesWriteBlocked is the refusal of a write into the job's sources.
func sourcesWriteBlocked(what string) string {
	return "blocked by guard: the job's sources (the read-only clones under sources/) are not changed: " + what +
		" would write there. Read them; to change or run code, copy the clone into the job's sandbox dir."
}

// sourcesExecBlocked is the refusal of running cloned code on the host.
func sourcesExecBlocked(what string) string {
	return "blocked by guard: code of the job's sources never runs on the host: " + what + ". Copy the clone into the " +
		"job's sandbox dir and run it in a throwaway container, as the whole Bash command: `docker run --rm --network " +
		"none -v <sandbox>/<name>:/src -w /src <image> <command>` (the Sources section of the brief)."
}

// dockerBlocked is the refusal of a docker call outside the sandbox form.
func dockerBlocked(what string) string {
	return "blocked by guard: docker runs only the sandbox form (" + what + "): the whole Bash command is `docker run " +
		"--rm [--network none|bridge] -v <sandbox>/<copy>:<dst>[:ro] [-w dir] [-i] [--memory, --cpus, --pids-limit, " +
		"--cap-drop, --security-opt no-new-privileges, --read-only, --tmpfs, --user, --entrypoint, --init, --pull, " +
		"--platform] <image> [command...]`, with exactly one mount, a copy below the job's sandbox dir, and no env, " +
		"variables, substitutions, redirections or globs."
}

// namesProtected reports a word that mentions a protected folder by name (`$D/sources/x`).
func (s *pathScope) namesProtected(w string) bool {
	for _, p := range s.protected {
		if b := filepath.Base(p); b != "" && b != "/" && strings.Contains(w, b) {
			return true
		}
	}
	return false
}

// cwdInside reports a directory the call may run in that is, is inside or holds a protected folder.
func (s *pathScope) cwdInside() (string, bool) {
	for _, d := range s.dirs {
		if q, ok := s.match([]string{d, s.resolve(d)}, true); ok {
			return q, true
		}
	}
	return "", false
}

// argForms are the spellings of an argument a path may hide in: the word, its `--opt=` value, an attached
// short option value (`-C../x`) and its path tokens (`a:b`, `k=v`, code).
func argForms(a string) []string {
	out := []string{a, optValue(a), wordValue(a)}
	if len(a) > 2 && a[0] == '-' && a[1] != '-' {
		out = append(out, a[2:])
	}
	return append(out, pathTokens(a)...)
}

// argHit returns the protected path an argument names (as an ancestor too when tree is set).
func (s *pathScope) argHit(a string, dyn, tree bool) (string, bool) {
	if dyn && s.namesProtected(a) {
		return a + " (expanded at run time)", true
	}
	for _, f := range argForms(a) {
		if clean := filepath.Clean(f); f == "" || clean == "/" || clean == "." && !tree {
			continue
		}
		if q, ok := s.hit(f, tree); ok {
			return q, true
		}
	}
	return "", false
}

// pointsInside reports a command running in or above a protected folder or given a path there.
func (s *pathScope) pointsInside(args []string, dyn []bool) bool {
	if _, ok := s.cwdInside(); ok {
		return true
	}
	for k, a := range args {
		if _, ok := s.argHit(a, dyn[k], false); ok {
			return true
		}
	}
	return false
}

// checkSourcesWrite refuses Bash writes into the job's sources.
func checkSourcesWrite(scripts []*script, _ string, cfg Config) string {
	if len(cfg.Sources) == 0 {
		return ""
	}
	rc := cfg
	rc.Protected = cfg.Sources
	s := newPathScope(scripts, rc)
	s.sources = true
	r := s.readOnlyScripts(scripts)
	if s.expired {
		return "blocked by guard: the command is too complex to verify against the job's read-only sources within " +
			"the guard's time budget; split it."
	}
	return r
}

// sourcesFileWrite reports a file tool path inside the job's sources (as given or resolved).
func sourcesFileWrite(p string, cfg Config) bool {
	return len(cfg.Sources) > 0 && inJobGitDir(p, cfg.Sources)
}

// sourceRunners run code they are pointed at: a script, a project directory or its build file, a test
// suite. Shells and interpreters (shells, stdinInterpreter) and execRunners count too.
var sourceRunners = map[string]bool{
	"source": true, ".": true, "exec": true, "env": true, "nohup": true, "nice": true, "timeout": true, "xargs": true,
	"java": true, "javac": true, "jshell": true, "kotlin": true, "kotlinc": true, "scala": true, "groovy": true,
	"julia": true, "tclsh": true, "wish": true, "r": true, "elixir": true, "erl": true, "escript": true, "guile": true,
	"sbcl": true, "racket": true, "swift": true, "dart": true, "flutter": true, "ghc": true, "runghc": true,
	"stack": true, "cabal": true, "zig": true, "nim": true, "crystal": true, "v": true, "gleam": true,
	"make": true, "gmake": true, "bmake": true, "cmake": true, "ctest": true, "ninja": true, "meson": true,
	"bazel": true, "bazelisk": true, "buck": true, "buck2": true, "scons": true, "just": true, "task": true,
	"npm": true, "pnpm": true, "yarn": true, "corepack": true, "tsc": true, "ts-node": true, "tsx": true,
	"esbuild": true, "vite": true, "webpack": true, "rollup": true, "jest": true, "vitest": true, "mocha": true,
	"pip": true, "pip3": true, "uv": true, "poetry": true, "pdm": true, "hatch": true, "pipenv": true, "conda": true,
	"mamba": true, "tox": true, "nox": true, "pytest": true, "py.test": true, "pre-commit": true,
	"go": true, "cargo": true, "rustc": true, "rustup": true,
	"gcc": true, "g++": true, "cc": true, "c++": true, "clang": true, "clang++": true, "ld": true,
	"mvn": true, "mvnw": true, "gradle": true, "gradlew": true, "ant": true, "sbt": true, "lein": true, "clojure": true,
	"dotnet": true, "msbuild": true, "mono": true, "xcodebuild": true, "bundle": true, "bundler": true, "gem": true,
	"rake": true, "rails": true, "rspec": true, "composer": true, "phpunit": true, "mix": true, "rebar3": true,
	"nix": true, "nix-shell": true, "nix-build": true, "direnv": true, "docker-compose": true, "conan": true,
	"vcpkg": true, "rscript": true, "deno": true, "bun": true, "node": true, "nodejs": true, "busybox": true,
}

// isSourceRunner reports a program that may run code of a path or directory it is given.
func isSourceRunner(name string) bool {
	return sourceRunners[name] || shells[name] || stdinInterpreter(name) || execRunners[name] ||
		strings.HasPrefix(name, "pip") || strings.HasPrefix(name, "ld-linux") || strings.HasPrefix(name, "ld.so")
}

// assignBuiltins take NAME=value arguments that set the environment of later commands.
var assignBuiltins = map[string]bool{"export": true, "declare": true, "typeset": true, "readonly": true, "local": true}

// checkSourcesExec refuses running code of the job's sources or sandbox copies on the host.
func checkSourcesExec(scripts []*script, _ string, cfg Config) string {
	roots := slices.DeleteFunc(append(slices.Clone(cfg.Sources), cfg.Sandbox), func(p string) bool { return p == "" })
	if len(roots) == 0 {
		return ""
	}
	rc := cfg
	rc.Protected = roots
	s := newPathScope(scripts, rc)
	src := cfg
	src.Protected = cfg.Sources
	inSources := newPathScope(scripts, src)
	box := cfg
	box.Protected = []string{cfg.Sandbox}
	inBox := newPathScope(scripts, box)
	r := s.sourcesExecScripts(scripts, inSources, inBox, cfg.Sandbox != "")
	if s.expired || inSources.expired || inBox.expired {
		return "blocked by guard: the command is too complex to verify against the job's sources within the guard's " +
			"time budget; split it."
	}
	return r
}

func (s *pathScope) sourcesExecScripts(scripts []*script, inSources, inBox *pathScope, hasBox bool) string {
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for i, c := range pl.cmds {
				if s.over() {
					return ""
				}
				v := split(c.words)
				for k, w := range c.words[:min(v.start, len(c.words))] {
					if reAssign.MatchString(w) {
						if q, ok := s.argHit(w, c.dyn[k], false); ok {
							return sourcesExecBlocked("the assignment `" + w + "` points the environment at `" + q + "`")
						}
					}
				}
				if v.start >= len(c.words) {
					continue
				}
				argv, dyn := c.words[v.start:], c.dyn[v.start:]
				name := progName(argv[0])
				if strings.Contains(argv[0], "/") {
					if q, ok := s.hit(argv[0], false); ok {
						return sourcesExecBlocked("`" + argv[0] + "` runs `" + q + "`")
					}
				}
				if assignBuiltins[name] {
					for k, a := range argv[1:] {
						if strings.Contains(a, "=") {
							if q, ok := s.argHit(a, dyn[k+1], false); ok {
								return sourcesExecBlocked("`" + name + " " + a + "` points the environment at `" + q + "`")
							}
						}
					}
				}
				if r := s.copyOut(name, argv[1:], inSources, inBox, hasBox); r != "" {
					return r
				}
				switch {
				case name == "find" && findActs(argv[1:]):
					for k, a := range argv[1:] {
						if q, ok := s.argHit(a, dyn[k+1], true); ok {
							return sourcesExecBlocked("`find` with an action runs over `" + q + "`")
						}
					}
				case awks[name] || name == "sed":
					for k := 1; k+1 < len(argv); k++ {
						if argv[k] == "-f" || argv[k] == "--file" {
							if q, ok := s.argHit(argv[k+1], dyn[k+1], false); ok {
								return sourcesExecBlocked("`" + name + " -f` runs the program `" + q + "`")
							}
						}
					}
				}
				wrapped := slices.ContainsFunc(v.wrappers, func(w string) bool { return w == "xargs" })
				if !isSourceRunner(name) && !wrapped {
					continue
				}
				if q, ok := s.cwdInside(); ok {
					return sourcesExecBlocked("`" + name + "` runs with its working directory in or above `" + q + "`")
				}
				if s.dynamic != "" {
					return sourcesExecBlocked("`" + name + "` runs after `cd " + s.dynamic + "`, which the guard cannot follow")
				}
				for k, a := range argv[1:] {
					if q, ok := s.argHit(a, dyn[k+1], true); ok {
						return sourcesExecBlocked("`" + name + "` is given `" + q + "`")
					}
				}
				for _, t := range c.inRedirs {
					if q, ok := s.hit(t, false); ok {
						return sourcesExecBlocked("`" + name + "` reads its input from `" + q + "`")
					}
				}
				if s.namesProtected(c.heredoc) {
					return sourcesExecBlocked("`" + name + "` reads a here-doc naming the sources")
				}
				for _, feeder := range pl.cmds[:i] {
					for k, w := range feeder.words {
						if q, ok := s.argHit(w, feeder.dyn[k], false); ok {
							return sourcesExecBlocked("`" + name + "` reads a pipe fed with `" + q + "`")
						}
					}
				}
			}
		}
	}
	return ""
}

// copyOut refuses a copy out of the sources to anywhere but the sandbox (cp, install, ln, rsync): code
// copied elsewhere would run on the host past the checks.
func (s *pathScope) copyOut(name string, args []string, inSources, inBox *pathScope, hasBox bool) string {
	var srcs, dests []string
	switch {
	case destCopiers[name] && name != "mv":
		ca := parseCopyArgs(name, args)
		srcs, dests = ca.srcs, ca.dests
	case name == "rsync":
		var ops []string
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				ops = append(ops, a)
			}
		}
		if len(ops) < 2 {
			return ""
		}
		srcs, dests = ops[:len(ops)-1], ops[len(ops)-1:]
	default:
		return ""
	}
	from := ""
	for _, a := range srcs {
		if q, ok := inSources.hit(a, false); ok {
			from = q
			break
		}
	}
	if from == "" {
		return ""
	}
	for _, d := range dests {
		if _, ok := inBox.hit(d, false); !ok || !hasBox {
			return sourcesExecBlocked("`" + name + "` copies `" + from + "` to `" + d + "`, outside the sandbox dir " +
				"(a copy of cloned code goes only there)")
		}
	}
	return ""
}

var (
	// reDockerImage is an image reference: name[:tag][@digest] with registry and path.
	reDockerImage = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]*$`)
	// dockerFlags are the docker run options taking no value that the sandbox form allows.
	dockerFlags = map[string]bool{"--rm": true, "-i": true, "--interactive": true, "-t": true, "--tty": true,
		"-it": true, "-ti": true, "--read-only": true, "--init": true}
	// dockerValues are the docker run options taking a value that the sandbox form allows (checked by
	// dockerValue): long ones as `--opt v` or `--opt=v`, short ones as `-x v`.
	dockerValues = map[string]string{"--network": "network", "--net": "network", "-v": "volume", "--volume": "volume",
		"-w": "", "--workdir": "", "-u": "", "--user": "", "-m": "", "--memory": "", "--memory-swap": "", "--cpus": "",
		"--pids-limit": "", "--cap-drop": "", "--security-opt": "security", "--entrypoint": "", "--pull": "pull",
		"--platform": "", "--tmpfs": ""}
)

// dockerSandbox decides a Bash call that is a single docker command when the job has a sandbox dir
// (handled): "" allows the sandbox form, anything else is refused. A docker call inside a longer command
// is not handled here: the scope check refuses docker there as before.
func dockerSandbox(scripts []*script, cfg Config) (string, bool) {
	if cfg.Sandbox == "" || len(scripts) == 0 || scripts[0].depth != 0 || len(scripts[0].pipelines) != 1 ||
		len(scripts[0].pipelines[0].cmds) != 1 {
		return "", false
	}
	pl := scripts[0].pipelines[0]
	c := pl.cmds[0]
	v := split(c.words)
	if v.start >= len(c.words) || progName(c.words[v.start]) != "docker" {
		return "", false
	}
	switch {
	case len(scripts) != 1 || len(c.subSources) > 0 || slices.Contains(c.dyn, true):
		return dockerBlocked("no variable or substitution: the host shell would expand it"), true
	case v.lead != 0 || v.start != 0:
		return dockerBlocked("no assignment, keyword or wrapper before docker"), true
	case len(c.outRedirs) > 0 || len(c.inRedirs) > 0 || c.stdin || c.heredoc != "" || pl.piped || pl.nest > 0 ||
		pl.after == "&":
		return dockerBlocked("no redirection, pipe, subshell or background"), true
	case strings.ContainsAny(c.raw, "*?[]{}~"):
		return dockerBlocked("no glob, brace or tilde outside single quotes"), true
	case len(c.words) < 2 || c.words[1] != "run":
		return dockerBlocked("only `docker run`, without global options"), true
	}
	argv := c.words[2:]
	rm, volumes, image := false, 0, -1
	for k := 0; k < len(argv) && image < 0; k++ {
		a := argv[k]
		switch {
		case a == "--":
			image = k + 1
		case dockerFlags[a]:
			rm = rm || a == "--rm"
		case strings.HasPrefix(a, "-"):
			opt, val, eq := strings.Cut(a, "=")
			kind, ok := dockerValues[opt]
			if !ok || (eq && !strings.HasPrefix(opt, "--")) {
				return dockerBlocked("option `" + opt + "` is not in the allowlist"), true
			}
			if !eq {
				if k+1 >= len(argv) {
					return dockerBlocked("option `" + opt + "` without a value"), true
				}
				k++
				val = argv[k]
			}
			if r := dockerValue(kind, val, cfg); r != "" {
				return dockerBlocked(r), true
			}
			if kind == "volume" {
				volumes++
			}
		default:
			image = k
		}
	}
	switch {
	case !rm:
		return dockerBlocked("--rm is required: the container is thrown away"), true
	case volumes != 1:
		return dockerBlocked("exactly one -v mount, the copy below the sandbox dir"), true
	case image < 0 || image >= len(argv) || !reDockerImage.MatchString(argv[image]):
		return dockerBlocked("an image name is required"), true
	}
	return "", true
}

// dockerValue checks the value of an allowed docker run option; "" = allowed.
func dockerValue(kind, val string, cfg Config) string {
	switch kind {
	case "network":
		if val != "none" && val != "bridge" {
			return "network `" + val + "` (only none or bridge)"
		}
	case "pull":
		if val != "always" && val != "missing" && val != "never" {
			return "--pull `" + val + "`"
		}
	case "security":
		if val != "no-new-privileges" && val != "no-new-privileges:true" && val != "no-new-privileges=true" {
			return "--security-opt `" + val + "` (only no-new-privileges)"
		}
	case "volume":
		return sandboxMount(val, cfg.Sandbox)
	}
	return ""
}

// sandboxMount checks a -v value: <src>:<dst>[:ro|rw], src an existing directory strictly below the sandbox
// dir (itself a real directory, not a link), with its symlinks resolved, dst an absolute container path.
func sandboxMount(val, sandbox string) string {
	parts := strings.Split(val, ":")
	if len(parts) < 2 || len(parts) > 3 || (len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw") {
		return "mount `" + val + "` (the form is <copy>:<dst>[:ro|rw])"
	}
	src, dst := parts[0], parts[1]
	if !filepath.IsAbs(src) || hasDotDot(src) || !filepath.IsAbs(dst) {
		return "mount `" + val + "` (absolute paths without `..`; a named volume is refused)"
	}
	box := filepath.Clean(sandbox)
	if fi, err := os.Lstat(box); err != nil || !fi.IsDir() {
		return "mount `" + val + "` (the sandbox dir is not a real directory)"
	}
	rbox := resolvePath(box, "/")
	rsrc, ok := resolveBounded(src, "/")
	if !ok {
		return "mount `" + val + "` (too many symlinks)"
	}
	clean := filepath.Clean(src)
	if !within(clean, box) || clean == box || !within(rsrc, rbox) || rsrc == rbox {
		return "mount `" + val + "` (only a copy below " + box + ")"
	}
	if fi, err := os.Stat(rsrc); err != nil || !fi.IsDir() {
		return "mount `" + val + "` (the copy must be an existing directory)"
	}
	return ""
}
