package guard

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

// Second names of protected paths: a file descriptor alias (/proc/self/fd/N, /dev/fd/N, /dev/stdin)
// reopens whatever file the descriptor holds, a hard link is the same inode under another name, and
// an exported variable ($GIT_CONFIG_GLOBAL) spells a protected path without writing it out. The
// checks below give the protected-path rule (CheckFileWrite, checkProtected) those names too.

// reFdAlias matches a path that names an open file descriptor or a process's working directory
// rather than a file: /proc/<pid>/fd/…, /proc/<pid>/task/<tid>/fd/…, /proc/<pid>/cwd, map_files,
// /dev/fd/…, /dev/stdin, /dev/stdout, /dev/stderr. Opening such a path reopens the file behind the
// descriptor (with a new access mode), chmod changes that file, `ln -L` links it.
var reFdAlias = regexp.MustCompile(`^/(proc/[^/]+/(task/[^/]+/)?(fd|fdinfo|cwd|root|map_files)|dev/fd|dev/std(in|out|err))(/|$)`)

// reStdStream matches the aliases of the standard streams a command may write to (`>/dev/stderr`,
// `tee /dev/stdout`): fds 0-2 are the tool's pipes unless the command redirects them, and opening a
// protected path for a redirection is refused by itself.
var reStdStream = regexp.MustCompile(`^/(dev/std(in|out|err)|dev/fd/[012]|proc/(self|thread-self)/fd/[012])$`)

// fdAlias reports a path (as written, cleaned) that is a file descriptor alias, and whether it is
// one of the standard streams.
func fdAlias(p string) (alias, std bool) {
	p = filepath.ToSlash(filepath.Clean(p))
	if !reFdAlias.MatchString(p) {
		return false, false
	}
	return true, reStdStream.MatchString(p)
}

// fdMeta are the programs that change a file's metadata or link it: given a descriptor alias they act
// on the file behind it (`chmod u+w /proc/self/fd/9`, `ln -L /dev/stdin alias`), never on a stream.
var fdMeta = map[string]bool{
	"chmod": true, "chown": true, "chgrp": true, "chattr": true, "setfacl": true, "ln": true, "link": true,
	"touch": true, "truncate": true, "mv": true, "rm": true, "unlink": true, "shred": true, "setfattr": true,
	"xattr": true,
}

// linkMakers may create a link (`ln -s /proc p`, `cp -s`, `rsync -l`): one to /proc, /dev or an
// ancestor of them makes a descriptor alias under a name that is not one (`p/<pid>/fd/3`) in the same
// call, before the guard can resolve the link.
var linkMakers = map[string]bool{"ln": true, "link": true, "cp": true, "rsync": true}

// reFdRoot matches a directory a descriptor alias lies below: /, /proc, /dev, /proc/<pid>,
// /proc/<pid>/task, /proc/<pid>/task/<tid>.
var reFdRoot = regexp.MustCompile(`^/((proc(/[^/]+(/task(/[^/]+)?)?)?)|dev)?$`)

// fdRootWord reports a word naming, from some directory of the call, a descriptor alias other than
// a standard stream, or a directory one lies below (as written or with symlinks resolved; an
// existing file such as /proc/cpuinfo is none).
func (s *pathScope) fdRootWord(w string) bool {
	if alias, std := s.fdWord(w); alias {
		return !std
	}
	for _, v := range []string{w, wordValue(w)} {
		exp, ok := s.expandHome(v)
		if !ok {
			continue
		}
		for _, c := range s.candidates(exp) {
			c = filepath.ToSlash(filepath.Clean(c))
			if !reFdRoot.MatchString(c) {
				continue
			}
			if fi, err := os.Stat(c); err != nil || fi.IsDir() {
				return true
			}
		}
	}
	return false
}

func fdAliasBlocked(w string) string {
	return "blocked by guard: `" + w + "` names an open file descriptor (/proc/*/fd, /dev/fd, /dev/std*), which can " +
		"stand for agent-jobs state or configuration; a job does not change, link or write through one. Write the path out."
}

// protectedForms returns the protected paths cleaned and with symlinks resolved like a write target
// (resolvePath: the longest existing prefix, a dangling link followed by its target text), both
// spellings kept: `config.json -> /outside/new.json` protects /outside/new.json before it exists.
func protectedForms(paths []string) []string {
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		out = append(out, p)
		if r := resolvePath(p, "/"); r != p {
			out = append(out, r)
		}
	}
	return out
}

// maxInodeScan bounds the entries of protected directories looked at for hard links in one call;
// past it a multiply linked target is refused (fail closed).
const maxInodeScan = 16384

type inodeKey struct{ dev, ino uint64 }

// inodeSet holds the (device, inode) of every protected regular file that has more than one link,
// collected once per call and only when a write target has more than one link itself.
type inodeSet struct {
	forms []string
	ids   map[inodeKey]string
	done  bool
	full  bool // maxInodeScan was reached: an unmatched multiply linked target is refused too
}

func statKey(fi fs.FileInfo) (inodeKey, uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return inodeKey{}, 0, false
	}
	return inodeKey{uint64(st.Dev), uint64(st.Ino)}, uint64(st.Nlink), true
}

func (s *inodeSet) load() {
	s.done, s.ids = true, map[inodeKey]string{}
	seen := 0
	for _, q := range s.forms {
		_ = filepath.WalkDir(q, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if seen++; seen > maxInodeScan {
				s.full = true
				return filepath.SkipAll
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			if k, n, ok := statKey(fi); ok && n > 1 {
				s.ids[k] = p
			}
			return nil
		})
		if s.full {
			return
		}
	}
}

// hardlink returns the protected file an existing regular file p is another hard link of (symlinks
// followed, as a write follows them). A file with one link is no other name of anything.
func (s *inodeSet) hardlink(p string) (string, bool) {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	k, n, ok := statKey(fi)
	if !ok || n < 2 {
		return "", false
	}
	if !s.done {
		s.load()
	}
	if q, ok := s.ids[k]; ok {
		return q, true
	}
	if s.full {
		return p + " (a hard link the guard cannot rule out among protected files)", true
	}
	return "", false
}

// reEnvRef is a variable reference: $NAME, ${NAME} or ${NAME<op>…} with an operator that keeps the
// value when it is set (`:-`, `-`, `:=`, `=`, `:?`, `?`).
var reEnvRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(?::?[-=?])[^}]*)?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expandEnv replaces the references to variables set in the hook's environment, other than HOME
// (expandHome handles it), with their values: the job's shell inherits the same environment, so
// `chmod u+w "$GIT_CONFIG_GLOBAL"` names the job's gitconfig and `$GIT_CONFIG_VALUE_0` its no-hooks
// dir. A variable that is unset here, or assigned in the call itself, stays as written.
func expandEnv(w string) string {
	if !strings.Contains(w, "$") {
		return w
	}
	return reEnvRef.ReplaceAllStringFunc(w, func(ref string) string {
		m := reEnvRef.FindStringSubmatch(ref)
		name := m[1] + m[2]
		if name == "HOME" {
			return ref
		}
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		return ref
	})
}

// stateWriteBlocked is the refusal of a file tool write into the state dir outside the job's folder.
func stateWriteBlocked(tool, p string) string {
	return "blocked by guard: " + tool + " into `" + p + "`: it is agent-jobs state outside this job's folder (other " +
		"jobs' files, the goals, the archive); a job writes only in its own folder and work tree."
}

// fdWord reports a word (or its `key=value` value) that names a file descriptor alias from some
// directory of the call, as written or with symlinks resolved (`p/<pid>/fd/3` with `p -> /proc`), and
// whether it is a standard stream. A standard stream is judged as written: resolved, /dev/stderr is
// the hook's own descriptor (/proc/<pid>/fd/2), which says nothing about the job's.
func (s *pathScope) fdWord(w string) (alias, std bool) {
	for _, v := range []string{w, wordValue(w)} {
		exp, ok := s.expandHome(v)
		if !ok {
			continue
		}
		for _, d := range s.dirs {
			b := exp
			if !filepath.IsAbs(b) {
				b = d + "/" + exp
			}
			if a, st := fdAlias(b); a && st {
				return true, true
			}
		}
		for _, c := range s.candidates(exp) {
			if a, _ := fdAlias(c); a {
				alias = true
			}
		}
	}
	return alias, false
}

// dirReaders only read the directory named by an argument, or list or measure what is in it: a
// directory holding a protected path is no destination for them (`find ..`, `du -sh ..`, `tree ..`).
var dirReaders = map[string]bool{
	"find": true, "du": true, "tree": true, "ls": true, "stat": true, "file": true, "realpath": true,
	"readlink": true, "basename": true, "dirname": true, "df": true, "diff": true, "cmp": true, "md5sum": true,
	"sha1sum": true, "sha256sum": true, "sha512sum": true, "b2sum": true, "cksum": true, "fd": true,
	"fdfind": true, "exa": true, "eza": true, "lsd": true, "wc": true, "head": true, "tail": true,
}

// fdExec are the options with which fd runs a command on what it finds (like find -exec).
var fdExec = []string{"-x", "--exec", "-X", "--exec-batch"}

// fdActs reports an fd/fdfind call that runs a command: an exec option, also as `--exec=…`.
func fdActs(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return slices.Contains(fdExec, a) || strings.HasPrefix(a, "--exec=") || strings.HasPrefix(a, "--exec-batch=")
	})
}

// dirReader reports a command for which the rule "the directory holding a protected path is a
// destination" (parentHit) does not apply: a directory reader (find without an action), or an
// interpreter running a script file, which the guard does not see in any case (`python3 x.py ..`).
// Every other program keeps it (fail closed): cp, mv, install, ln, rsync, tar -x, unzip -d, patch -d,
// inline code (`python3 -c '…' <dir>`).
func dirReader(name string, argv []string, c *simpleCmd) bool {
	if dirReaders[name] {
		switch name {
		case "find":
			return !findActs(argv[1:])
		case "fd", "fdfind":
			return !fdActs(argv[1:])
		}
		return true
	}
	if !stdinInterpreter(name) && !shells[name] && !awks[name] {
		return false
	}
	if c.stdin || c.heredoc != "" || len(argv) < 2 {
		return false // the program comes from stdin or a here-doc the guard checks as code
	}
	for k := 1; k < len(argv); k++ {
		if inlineCode(name, argv, k) || codeArg(argv, k) {
			return false
		}
	}
	return !awks[name] // an awk program is its first operand: inline code
}
