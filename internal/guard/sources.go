package guard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Sources (G02): the job's cloned repositories (Config.Sources, <jobdir>/sources) are read-only for the
// job and their code never runs on the host; a copy under Config.Sandbox (<jobdir>/sandbox) runs only in a
// throwaway docker container that mounts nothing but that copy. Fail closed: what the guard cannot tell
// apart from running cloned code is refused.
//
//   - checkSourcesWrite: the review read-only check (readonly.go) with the sources as the protected
//     tree: redirections, in-place editors, tree writers, copier destinations, extractors into it.
//   - checkSourcesExec: only plain readers and file utilities (sourceReaders) are given a path under
//     sources/ or sandbox/, run there, or read a pipe, redirection or here-doc naming them; a file there
//     never runs as a program; a runner (shell, interpreter, package manager, build or test tool, exec
//     wrapper) never runs above them, after a cd the guard cannot resolve, or given an ancestor of them;
//     git never works inside sandbox/ (a container may have written its config); no environment
//     assignment names them (PATH, LD_PRELOAD, PYTHONPATH, ...); a copy, move or link out of them lands
//     only inside sandbox/.
//   - dockerSandbox: `docker run` is allowed only as the whole Bash command, spelled `docker`, with --rm,
//     --network none, --user <the job's uid>:<gid>, --cap-drop ALL, --security-opt no-new-privileges,
//     exactly one `-v <sandbox>/<copy>:<dst>[:ro|rw]` and an allowlist of other options: no env, no host
//     namespaces or network, no privileges, devices, ports, --mount or detaching.
//
// Residual gaps (not resolved): a copy made by a program the guard does not read as a copier (tar, git
// clone of the local path, code in an interpreter) and run from there; a script the job writes itself; a
// symlink swapped in by a process the job left running between the check and docker's mount; docker's
// own client config (~/.docker, DOCKER_HOST) as the job's environment sets it.

// sourcesWriteBlocked is the refusal of a write into the job's sources.
func sourcesWriteBlocked(what string) string {
	return "blocked by guard: the job's sources (the read-only clones under sources/) are not changed: " + what +
		" would write there. Read them; to change or run code, copy the clone into the job's sandbox dir."
}

// sandboxRecipe is the docker form the guard allows, as the refusals show it.
const sandboxRecipe = "`docker run --rm --network none --user <uid>:<gid> --cap-drop ALL --security-opt " +
	"no-new-privileges -v <sandbox>/<name>:/src -w /src <image> <command>`"

// sourcesExecBlocked is the refusal of running cloned code on the host.
func sourcesExecBlocked(what string) string {
	return "blocked by guard: code of the job's sources never runs on the host: " + what + ". Read the clone with " +
		"Read, Grep or plain readers (cat, grep, head, ls, find, git log); to run it, copy it into the job's sandbox " +
		"dir and run it in a throwaway container, as the whole Bash command: " + sandboxRecipe +
		" (the Sources section of the brief)."
}

// dockerBlocked is the refusal of a docker call outside the sandbox form.
func dockerBlocked(what string) string {
	return "blocked by guard: docker runs only the sandbox form (" + what + "): the whole Bash command is " +
		sandboxRecipe + ", with the job's own numeric uid, exactly one mount (a copy below the job's sandbox dir), " +
		"optionally -i, -t, --read-only, --init, -w, --memory, --memory-swap, --cpus, --pids-limit, --tmpfs, " +
		"--entrypoint, --pull, --platform, and no env, variables, substitutions, redirections, globs or tildes."
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

// cwdHit reports a directory the call may run in that is or is inside a protected folder, or, with
// tree, also one that holds one.
func (s *pathScope) cwdHit(tree bool) (string, bool) {
	for _, d := range s.dirs {
		if q, ok := s.match([]string{d, s.resolve(d)}, tree); ok {
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
	if _, ok := s.cwdHit(true); ok {
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

// sourceReaders are the only programs given a path under sources/ or sandbox/, run there, or fed from
// there: they read files or manage them and never run what they read. Left out on purpose: sed and awk
// (`e`, system()), tar (--to-command, -I), sort (--compress-program), less (preprocessors), editors.
var sourceReaders = map[string]bool{
	"cat": true, "tac": true, "head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true, "rg": true,
	"ls": true, "tree": true, "find": true, "wc": true, "stat": true, "file": true, "du": true, "diff": true,
	"cmp": true, "comm": true, "uniq": true, "cut": true, "tr": true, "nl": true, "od": true, "hexdump": true,
	"xxd": true, "strings": true, "md5sum": true, "sha1sum": true, "sha256sum": true, "sha512sum": true,
	"b2sum": true, "cksum": true, "jq": true, "realpath": true, "readlink": true, "dirname": true, "basename": true,
	"test": true, "[": true, "[[": true, "echo": true, "printf": true, "true": true, "false": true, "cloc": true,
	"tokei": true, "scc": true, "column": true, "fold": true, "fmt": true, "iconv": true, "base64": true,
	"zcat": true, "git": true, "cp": true, "rsync": true, "mv": true, "ln": true, "rm": true, "rmdir": true,
	"mkdir": true, "chmod": true, "touch": true, "cd": true, "pushd": true, "popd": true, "pwd": true,
}

// readerRuns reports a reader option that runs a program: rsync's remote shell, rg's preprocessor, find's
// actions.
func readerRuns(name string, args []string) bool {
	switch name {
	case "rsync":
		return slices.ContainsFunc(args, func(a string) bool {
			opt, _, _ := strings.Cut(a, "=")
			return opt == "--rsh" || opt == "--rsync-path" || opt == "-e" ||
				(len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'e'))
		})
	case "rg":
		return slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--pre") })
	case "find":
		return findActs(args)
	}
	return false
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
	"awk": true, "gawk": true, "mawk": true, "nawk": true, "sed": true, "gdb": true, "vim": true, "vi": true,
	"nvim": true, "emacs": true, "m4": true, "ipython": true, "jupyter": true, "pwsh": true, "octave": true,
}

// isSourceRunner reports a program that may run code of a directory it runs in or is given an ancestor of.
func isSourceRunner(name string) bool {
	return sourceRunners[name] || shells[name] || stdinInterpreter(name) || execRunners[name] ||
		strings.HasPrefix(name, "pip") || strings.HasPrefix(name, "ld-linux") || strings.HasPrefix(name, "ld.so") ||
		strings.HasPrefix(name, "qemu-")
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
	var inBox *pathScope
	if cfg.Sandbox != "" {
		box := cfg
		box.Protected = []string{cfg.Sandbox}
		inBox = newPathScope(scripts, box)
	}
	r := s.sourcesExecScripts(scripts, inBox)
	if s.expired || (inBox != nil && inBox.expired) {
		return "blocked by guard: the command is too complex to verify against the job's sources within the guard's " +
			"time budget; split it."
	}
	return r
}

func (s *pathScope) sourcesExecScripts(scripts []*script, inBox *pathScope) string {
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for i, c := range pl.cmds {
				if s.over() {
					return ""
				}
				if r := s.sourcesExecCmd(c, pl.cmds[:i], inBox); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

// sourcesExecCmd checks one command; feeders are the earlier stages of its pipeline.
func (s *pathScope) sourcesExecCmd(c *simpleCmd, feeders []*simpleCmd, inBox *pathScope) string {
	v := split(c.words)
	for k, w := range c.words[:min(v.start, len(c.words))] {
		if reAssign.MatchString(w) {
			if q, ok := s.argHit(w, c.dyn[k], false); ok {
				return sourcesExecBlocked("the assignment `" + w + "` points the environment at `" + q + "`")
			}
		}
	}
	if v.start >= len(c.words) {
		return ""
	}
	argv, dyn := c.words[v.start:], c.dyn[v.start:]
	name := progName(argv[0])
	if strings.Contains(argv[0], "/") || dyn[0] {
		if q, ok := s.argHit(argv[0], dyn[0], false); ok {
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
	if r := s.copyOut(name, argv[1:], inBox); r != "" {
		return r
	}
	// git reads the config of the repository it works in: a sandbox copy's may come from a container.
	if _, isGit := parseGit(argv); isGit && inBox != nil {
		if q, ok := inBox.cwdHit(false); ok {
			return sourcesExecBlocked("`git` runs in `" + q + "`, whose git config a container may have written")
		}
		for k, a := range argv[1:] {
			if q, ok := inBox.argHit(a, dyn[k+1], false); ok {
				return sourcesExecBlocked("`git` works on `" + q + "`, whose git config a container may have written")
			}
		}
	}
	reader := sourceReaders[name] && !readerRuns(name, argv[1:])
	if !reader {
		if q, ok := s.cwdHit(false); ok {
			return sourcesExecBlocked("`" + name + "` runs with its working directory in `" + q + "`")
		}
		for k, a := range argv[1:] {
			if q, ok := s.argHit(a, dyn[k+1], false); ok {
				return sourcesExecBlocked("`" + name + "` is given `" + q + "` (only plain readers are)")
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
		for _, f := range feeders {
			for k, w := range f.words {
				if q, ok := s.argHit(w, f.dyn[k], false); ok {
					return sourcesExecBlocked("`" + name + "` reads a pipe fed with `" + q + "`")
				}
			}
		}
	}
	if !isSourceRunner(name) && !slices.Contains(v.wrappers, "xargs") {
		return ""
	}
	if q, ok := s.cwdHit(true); ok {
		return sourcesExecBlocked("`" + name + "` runs with its working directory above `" + q + "`")
	}
	if s.dynamic != "" {
		return sourcesExecBlocked("`" + name + "` runs after `cd " + s.dynamic + "`, which the guard cannot follow")
	}
	for k, a := range argv[1:] {
		if q, ok := s.argHit(a, dyn[k+1], true); ok {
			return sourcesExecBlocked("`" + name + "` is given `" + q + "` or a folder holding it")
		}
	}
	return ""
}

// copyOut refuses a copy, move or link out of the sources or the sandbox to anywhere but the sandbox (cp,
// install, ln, mv, rsync): code copied elsewhere would run on the host past the checks.
func (s *pathScope) copyOut(name string, args []string, inBox *pathScope) string {
	var srcs, dests []string
	switch {
	case destCopiers[name]:
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
		if q, ok := s.hit(a, false); ok {
			from = q
			break
		}
	}
	if from == "" {
		return ""
	}
	for _, d := range dests {
		if inBox == nil {
			return sourcesExecBlocked("`" + name + "` copies `" + from + "` to `" + d + "` (there is no sandbox dir)")
		}
		if _, ok := inBox.hit(d, false); !ok {
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
		"-w": "", "--workdir": "", "-u": "user", "--user": "user", "-m": "", "--memory": "", "--memory-swap": "",
		"--cpus": "", "--pids-limit": "", "--cap-drop": "capdrop", "--security-opt": "security", "--entrypoint": "",
		"--pull": "pull", "--platform": "", "--tmpfs": ""}
	// reDockerUser is --user <uid>:<gid>, both numeric.
	reDockerUser = regexp.MustCompile(`^([0-9]+):([0-9]+)$`)
)

// dockerSandbox decides a Bash call that is a single docker command when the job has a sandbox dir
// (handled): "" allows the sandbox form, anything else is refused. Only the word `docker` itself (found on
// PATH) counts: a program named docker elsewhere (`./docker`, `sandbox/x/docker`) is not handled here and
// is refused by the scope check, as is docker inside a longer command.
func dockerSandbox(scripts []*script, cfg Config) (string, bool) {
	if cfg.Sandbox == "" || len(scripts) == 0 || scripts[0].depth != 0 || len(scripts[0].pipelines) != 1 ||
		len(scripts[0].pipelines[0].cmds) != 1 {
		return "", false
	}
	pl := scripts[0].pipelines[0]
	c := pl.cmds[0]
	v := split(c.words)
	if v.start >= len(c.words) || c.words[v.start] != "docker" {
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
	seen := map[string]bool{}
	volumes, image := 0, -1
	for k := 0; k < len(argv) && image < 0; k++ {
		a := argv[k]
		switch {
		case a == "--":
			image = k + 1
		case dockerFlags[a]:
			seen[a] = true
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
			seen[kind] = true
			if kind == "volume" {
				volumes++
			}
		default:
			image = k
		}
	}
	switch {
	case !seen["--rm"]:
		return dockerBlocked("--rm is required: the container is thrown away"), true
	case !seen["network"]:
		return dockerBlocked("--network none is required"), true
	case !seen["user"]:
		return dockerBlocked("--user <uid>:<gid> with the job's own uid is required"), true
	case !seen["capdrop"]:
		return dockerBlocked("--cap-drop ALL is required"), true
	case !seen["security"]:
		return dockerBlocked("--security-opt no-new-privileges is required"), true
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
		if val != "none" {
			return "network `" + val + "` (only none: a container network reaches the host's other interfaces)"
		}
	case "user":
		m := reDockerUser.FindStringSubmatch(val)
		if m == nil || m[1] != strconv.Itoa(os.Getuid()) || m[1] == "0" || m[2] == "0" {
			return "--user `" + val + "` (the job's own numeric uid:gid, " + strconv.Itoa(os.Getuid()) + ":" +
				strconv.Itoa(os.Getgid()) + "; never root, so the copy gets no root-owned or setuid files)"
		}
	case "capdrop":
		if val != "ALL" && val != "all" {
			return "--cap-drop `" + val + "` (only ALL)"
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
