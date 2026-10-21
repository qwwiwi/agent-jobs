package guard

import (
	"path/filepath"
	"strings"
)

// noteWrite records possible changes without trusting check-time file contents.
// Directory destinations conservatively include descendants, even if absent now.
func (s *pathScope) noteWrite(w string, tree bool) {
	if w == "" || s.over() {
		return
	}
	exp, ok := s.expandHome(wordValue(w))
	if !ok || hasGlob(exp) || strings.ContainsAny(exp, "$`") || !filepath.IsAbs(exp) && s.dynamic != "" {
		s.writeUnknown = true
		return
	}
	if s.writes == nil {
		s.writes = map[string]bool{}
	}
	for _, p := range s.candidates(exp) {
		p = fold(p)
		s.writes[p] = s.writes[p] || tree
	}
}

func (s *pathScope) archiveWritten(archive, cwd string) bool {
	if s.writeUnknown || hasGlob(archive) || strings.ContainsAny(archive, "$`") {
		return true
	}
	abs := absoluteAt(archive, cwd)
	for _, p := range []string{abs, resolvePath(abs, "/"), s.futurePath(abs)} {
		p = fold(p)
		if _, ok := s.writes[p]; ok {
			return true
		}
		for parent := filepath.Dir(p); ; parent = filepath.Dir(parent) {
			if s.writes[parent] {
				return true
			}
			if parent == filepath.Dir(parent) {
				break
			}
		}
	}
	return false
}

func (s *pathScope) commandWrites(c *simpleCmd) {
	argv := argvOf(c.words)
	if len(argv) == 0 || s.over() {
		return
	}
	name, args := progName(argv[0]), argv[1:]
	if destCopiers[name] || name == "rsync" {
		ca := parseCopyArgs(name, args)
		for _, d := range ca.dests {
			s.noteWrite(d, true)
		}
		if name == "mv" {
			for _, src := range ca.srcs {
				s.noteWrite(src, true)
			}
		}
		return
	}
	if name == "tar" || name == "unzip" || name == "cpio" {
		spec := archiveExtraction(name, args)
		if spec.unknown {
			s.writeUnknown = true
		}
		if spec.writeArchive {
			s.noteWrite(spec.archive, false)
		}
		if spec.extract {
			for _, cwd := range s.dirs {
				dest := cwd
				for _, d := range spec.dirs {
					if exp, ok := s.expandHome(d); ok {
						dest = absoluteAt(exp, dest)
					} else {
						s.writeUnknown = true
					}
				}
				s.noteWrite(dest, true)
			}
		}
		return
	}
	if gitFileReaders[name] || shells[name] {
		return
	}
	switch name {
	case "cd", "pushd", "popd", "true", "false", "echo", "printf", "eval", "trap":
		return // shell bodies and output redirections are tracked separately
	case "dd":
		for _, a := range args {
			if strings.HasPrefix(a, "of=") {
				s.noteWrite(a, false)
			}
		}
		return
	case "curl", "wget":
		// Scan explicit output paths, including attached short options and long
		// values. Remote filenames and output-directory policy are not predictable.
		for i, a := range args {
			if a == "-o" && name == "curl" || a == "-O" && name == "wget" || a == "--output" || a == "--output-document" {
				if i+1 < len(args) {
					s.noteWrite(args[i+1], false)
				}
			} else if strings.HasPrefix(a, "--output=") || strings.HasPrefix(a, "--output-document=") {
				s.noteWrite(a, false)
			} else if name == "curl" && strings.HasPrefix(a, "-o") && len(a) > 2 || name == "wget" && strings.HasPrefix(a, "-O") && len(a) > 2 {
				s.noteWrite(a[2:], false)
			} else if name == "wget" || strings.HasPrefix(a, "-") {
				s.writeUnknown = true
			}
		}
		return
	}
	// Other writers (tee, sed -i, truncate, inline programs, etc.) may modify
	// any named operand or literal path. Retain this uncertainty for archives.
	for _, a := range args {
		if s.over() {
			return
		}
		if !strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
			s.noteWrite(a, true)
		}
		for _, token := range pathTokens(a) {
			s.noteWrite(token, true)
		}
	}
}
